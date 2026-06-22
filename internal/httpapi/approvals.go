// Approvals: human-in-the-loop gate for high-risk tool calls.
//
// The agent never calls mcp_sendgrid_send_email directly. Orchestration
// intercepts the call, stages it in the approvals table, and emits a
// tool.approval_required SSE event during the live turn. The assistant
// message finishes with a summary of what it *would* send, and an inline
// card in the UI offers Send / Cancel buttons.
//
// When the user clicks Send, the UI POSTs here. The server calls the
// actual MCP tool (one-shot, outside the agent loop), writes the real
// tool result into both the approvals row and the conversation history,
// and returns it to the UI. No new agent turn runs — this is purely a
// resolution of a staged action.

package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// mcpToolCaller is the subset of *mcp.Client needed for pre-validation.
type mcpToolCaller interface {
	CallTool(ctx context.Context, toolName string, arguments map[string]interface{}) (*mcp.ToolResult, error)
}

// approvalStager implements agent.ApprovalStager. It persists a pending
// approval and emits the tool.approval_required event on the live SSE sink
// so the frontend renders its approval card immediately.
type approvalStager struct {
	ctx            context.Context
	store          *store.Store
	conversationID string
	userEmail      string
	sink           agent.EventSink
	mcpClient      mcpToolCaller
}

func (a *approvalStager) Stage(toolName, toolCallID, rawInput string) (string, error) {
	// For email tools that accept content_file, inline the file bytes
	// now. run_python writes to workspace/<convID>/, but the Go server
	// and the sendgrid MCP process each have their own cwd, so a bare
	// filename the agent passes never resolves downstream — the user
	// sees the filename rendered in the preview card, and the MCP call
	// errors out with "Content file not found" when they click Send.
	// Materializing here (the one place that has convID + the file is
	// still on disk) closes both gaps at once. See logs/Email-to-Kyle-*.
	if tools.IsEmailToolName(toolName) {
		inlined, err := tools.MaterializeContentFile(a.conversationID, rawInput)
		if err != nil {
			return "", err
		}
		rawInput = inlined
		// Same cwd-mismatch problem as content_file: the sendgrid MCP
		// subprocess does os.path.abspath against ITS cwd, so a bare
		// filename like "chart.png" written by run_python into
		// workspace/<convID>/ never resolves at send time. Rewrite
		// relative attachment paths to absolute now so the args
		// replayed post-approval carry resolvable paths.
		rewritten, err := tools.MaterializeAttachmentPaths(a.conversationID, rawInput)
		if err != nil {
			return "", err
		}
		rawInput = rewritten
		// Hard-reject empty content. Without this, preview_email stages a
		// card with an empty iframe (user sees nothing and reports "I
		// can't see the preview"), and send_email stages an approval
		// that the actual SendGrid call will reject AFTER the user has
		// clicked Send — wasting their time. The agent gets a clear
		// retry signal instead. See logs/Email-Report-Analysis-and-Summary
		// for the failure mode this catches.
		if err := validateEmailHasContent(toolName, rawInput); err != nil {
			return "", err
		}
		if a.mcpClient != nil {
			if err := a.prevalidateEmail(rawInput); err != nil {
				return "", err
			}
		}
	}

	// Supersede any older pending approvals for this same tool in this
	// conversation. Keeps the UI clean when the agent retries — e.g.
	// a preview_email that staged with a broken body, then re-staged
	// with the real body. Without this, both cards render and the
	// user sees the broken one linger forever. Best-effort: a failure
	// here shouldn't block the new approval.
	if n, err := a.store.SupersedePendingApprovals(a.ctx, a.conversationID, toolName); err != nil {
		log.Printf("supersede approvals (tool=%s conv=%s): %v", toolName, a.conversationID, err)
	} else if n > 0 {
		// Tell the UI to mark the old card as resolved. The client
		// already handles rejected approvals — this just nudges any
		// open stream to refresh its approval state.
		a.sink.Emit("tool.approval_superseded", map[string]any{
			"tool":  toolName,
			"count": n,
		})
	}

	approval, err := a.store.CreateApproval(a.ctx, a.conversationID, a.userEmail, toolName, toolCallID, rawInput)
	if err != nil {
		return "", err
	}

	// Extract the display-relevant fields so the UI can render a readable card
	// without re-parsing the whole payload.
	summary := summarizeApprovalInput(toolName, rawInput, a.conversationID)

	a.sink.Emit("tool.approval_required", map[string]any{
		"approval_id": approval.ID,
		"tool":        toolName,
		"summary":     summary,
	})
	return approval.ID, nil
}

// validateEmailHasContent rejects a preview_email / send_email call
// whose body is empty after content_file materialization. Returns an
// agent-readable error pointing at the missing field. Empty here means
// either content was never set OR content_file pointed at an empty
// file. We deliberately don't trim-whitespace the body itself — a
// whitespace-only email is probably wrong but isn't our call to make
// at the staging layer; SendGrid will accept and the operator can
// reject in the preview card. We DO trim the parsed string before
// the empty check so a single space ` ` doesn't sneak through as
// "non-empty" when the agent likely meant nothing.
//
// rawInput must already have run through materializeContentFile so
// content_file → content inlining has happened.
func validateEmailHasContent(toolName, rawInput string) error {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(rawInput), &parsed); err != nil {
		// Non-JSON: nothing useful to check. Stage and let downstream
		// handle the error — same behavior as prevalidateEmail.
		return nil //nolint:nilerr // intentional: a non-JSON body is not a validation failure here; downstream handles the malformed input.
	}
	if strings.TrimSpace(firstString(parsed, "content")) != "" {
		return nil
	}
	return fmt.Errorf(
		"%s called without an email body. Pass `content` (HTML/text string) "+
			"or `content_file` (path to a file in your workspace, e.g. "+
			"`email_draft.html` or `/opt/chat/workspace/<convID>/email_draft.html`). "+
			"Both are empty in this call",
		toolName,
	)
}

// memoryProposer implements agent.MemoryProposer. It persists a pending
// memory proposal and emits a memory.proposed SSE event so the frontend
// renders a Save/Don't Save card. The conversationID scopes the proposal
// so loadConversation can re-hydrate it on focus/refresh — without that,
// the visibilitychange handler's auto-refetch wipes the card.
type memoryProposer struct {
	ctx            context.Context
	store          *store.Store
	conversationID string
	userEmail      string
	sink           agent.EventSink
}

func (m *memoryProposer) Propose(content string) (string, error) {
	memory, err := m.store.CreateMemoryProposal(m.ctx, m.userEmail, m.conversationID, content)
	if err != nil {
		return "", err
	}
	m.sink.Emit("memory.proposed", map[string]any{
		"proposal_id": memory.ID,
		"content":     memory.Content,
	})
	return memory.ID, nil
}

// StageSuggestion stages a suggest_advanced_model approval if the
// per-conversation gate allows it. Suppression rules:
//
//   - Conversation already pinned to ADVANCED → skip; the user is already
//     there and a card would be silly.
//   - A prior suggest_advanced_model approval is approved → skip; the
//     user explicitly switched at some point. Don't re-nag even if they
//     later moved off advanced; one explicit yes is enough.
//   - A prior suggestion is still pending → skip; a card is already up.
//   - A prior suggestion is rejected and fewer than
//     agentcore.SuggestAdvancedCooldownTurns subsequent user turns have passed
//     → skip (cooldown).
//
// The agent-facing message reflects the suppression reason so the model
// knows not to retry.
func (a *approvalStager) StageSuggestion(reason string) (string, string, error) {
	conv, err := a.store.Get(a.ctx, a.userEmail, a.conversationID)
	if err != nil {
		return "", "", fmt.Errorf("lookup conversation: %w", err)
	}
	if conv != nil && conv.Model == agentcore.AdvancedModelSlug {
		return "", "SUGGESTION_SUPPRESSED: this conversation is already pinned to the advanced model. Do not call suggest_advanced_model again — proceed with the user's request.", nil
	}

	prior, err := a.store.LatestApprovalByTool(a.ctx, a.conversationID, tools.SuggestAdvancedModelToolName)
	if err != nil {
		return "", "", fmt.Errorf("lookup prior suggestion: %w", err)
	}
	if prior != nil {
		switch prior.Status {
		case "approved":
			return "", "SUGGESTION_SUPPRESSED: the user has already accepted a model-switch suggestion in this conversation. Do not stage another — keep working with the current model.", nil
		case "pending":
			return "", "SUGGESTION_SUPPRESSED: a model-switch suggestion is already pending the user's response. Do not stage another.", nil
		case "rejected":
			n, err := a.store.CountUserMessagesAfterTimestamp(a.ctx, a.conversationID, prior.CreatedAt)
			if err != nil {
				return "", "", fmt.Errorf("count user turns: %w", err)
			}
			if n < int64(agentcore.SuggestAdvancedCooldownTurns) {
				return "", fmt.Sprintf(
					"SUGGESTION_SUPPRESSED: the user dismissed a model-switch suggestion %d user turn(s) ago; cooldown is %d turn(s). Do not stage another suggestion right now — keep working with the current model.",
					n, agentcore.SuggestAdvancedCooldownTurns,
				), nil
			}
		}
	}

	// Persist the reason as the args payload so the approval card can
	// render it on page reload and the audit trail is intact.
	rawInput, err := json.Marshal(map[string]any{"reason": reason})
	if err != nil {
		return "", "", fmt.Errorf("encode reason: %w", err)
	}

	// Supersede any older pending cards for this same tool so the UI
	// stays clean if a rare race writes two in quick succession.
	if n, err := a.store.SupersedePendingApprovals(a.ctx, a.conversationID, tools.SuggestAdvancedModelToolName); err != nil {
		log.Printf("supersede suggestions (conv=%s): %v", a.conversationID, err)
	} else if n > 0 {
		a.sink.Emit("tool.approval_superseded", map[string]any{
			"tool":  tools.SuggestAdvancedModelToolName,
			"count": n,
		})
	}

	// suggest_advanced_model has no agent-emitted tool_call to thread
	// back to — it's a server-staged card. Empty toolCallID is fine;
	// resolutionCallID falls back to the approval id at write time.
	approval, err := a.store.CreateApproval(a.ctx, a.conversationID, a.userEmail,
		tools.SuggestAdvancedModelToolName, "", string(rawInput))
	if err != nil {
		return "", "", err
	}

	a.sink.Emit("tool.approval_required", map[string]any{
		"approval_id": approval.ID,
		"tool":        tools.SuggestAdvancedModelToolName,
		"summary": map[string]any{
			"tool":            tools.SuggestAdvancedModelToolName,
			"reason":          reason,
			"recommend_model": agentcore.AdvancedModelSlug,
		},
	})

	msg := fmt.Sprintf(
		"SUGGESTION_DISPLAYED: the user is now seeing your model-switch suggestion (suggestion_id=%s). The card has three actions — Switch & retry (default), Just switch, Dismiss — and the user picks. Do NOT call suggest_advanced_model again. Briefly summarize what you've done so far and stop iterating; the user's choice will arrive on the next turn.",
		approval.ID,
	)
	return approval.ID, msg, nil
}

// prevalidateEmail calls the MCP validate_email_content tool to catch
// structural errors or unresolved tokens before the approval row is created.
// If the MCP tool itself errors, the error is logged and staging continues;
// the real send path will still validate. This prevents broken emails from
// reaching the user's approval card.
func (a *approvalStager) prevalidateEmail(rawInput string) error {
	var args map[string]any
	if err := json.Unmarshal([]byte(rawInput), &args); err != nil {
		return nil // can't parse, let downstream handle
	}
	content := firstString(args, "content")
	subject := firstString(args, "subject")
	if content == "" {
		return nil // nothing to validate, let downstream handle
	}

	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()

	result, err := a.mcpClient.CallTool(ctx, "validate_email_content", map[string]interface{}{
		"content": content,
		"subject": subject,
	})
	if err != nil {
		log.Printf("prevalidateEmail: validation tool error: %v", err)
		return nil
	}
	if result == nil || len(result.Content) == 0 {
		return nil
	}

	var vr validationResult
	if err := json.Unmarshal([]byte(result.Content[0].Text), &vr); err != nil {
		log.Printf("prevalidateEmail: unmarshal validation result: %v", err)
		return nil
	}
	if !vr.Valid && len(vr.Errors) > 0 {
		return fmt.Errorf("email validation failed:\n  - %s", strings.Join(vr.Errors, "\n  - "))
	}
	return nil
}

type validationResult struct {
	Valid    bool     `json:"valid"`
	Errors   []string `json:"errors"`
	Warnings []string `json:"warnings"`
}

// summarizeApprovalInput dispatches on tool name to build a display
// payload for the approval card. convID is used by the email
// summarizer to resolve workspace-relative inline attachment paths
// when expanding `cid:` image references to data: URLs for the
// preview iframe — pass "" when no per-conversation context is
// available (the substitution becomes a no-op).
func summarizeApprovalInput(toolName, rawInput, convID string) map[string]any {
	if toolName == "bash" {
		return summarizeBashInput(toolName, rawInput)
	}
	if toolName == tools.SuggestAdvancedModelToolName {
		return summarizeSuggestAdvancedInput(toolName, rawInput)
	}
	// preview_email uses the exact same summary shape as send_email
	// so ApprovalCard's existing email-preview render path just works;
	// the only difference is the `tool` field, which drives the UI
	// branch that swaps Send for Dismiss.
	return summarizeSendEmailInput(toolName, rawInput, convID)
}

// summarizeSuggestAdvancedInput exposes the agent's reason and the
// recommended model slug so the UI card can render both without
// re-parsing the args. The recommended slug is server-authoritative
// (agentcore.AdvancedModelSlug) — the agent doesn't choose it.
func summarizeSuggestAdvancedInput(toolName, rawInput string) map[string]any {
	var args struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal([]byte(rawInput), &args)
	return map[string]any{
		"tool":            toolName,
		"reason":          args.Reason,
		"recommend_model": agentcore.AdvancedModelSlug,
	}
}

// summarizeBashInput extracts the command, working directory, and
// timeout from a staged bash invocation so the UI can render a plain
// preview.
func summarizeBashInput(toolName, rawInput string) map[string]any {
	var args struct {
		Command        string `json:"command"`
		WorkingDir     string `json:"working_dir"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	_ = json.Unmarshal([]byte(rawInput), &args)
	preview := args.Command
	if len(preview) > 600 {
		preview = preview[:600] + "…[truncated]"
	}
	return map[string]any{
		"tool":            toolName,
		"command":         args.Command,
		"preview":         preview,
		"working_dir":     args.WorkingDir,
		"timeout_seconds": args.TimeoutSeconds,
	}
}

// summarizeSendEmailInput pulls the display-relevant fields out of a
// send_email payload. Carries:
//   - `preview`: a truncated (600 char) plain-string snippet used by older
//     UI paths and as a lightweight fallback.
//   - `content`: the FULL content, so the approval card can render an
//     HTML iframe preview exactly matching what SendGrid will receive.
//   - `content_type`: "text/html" vs "text/plain" so the UI picks the
//     right renderer without sniffing the body.
//
// The full content is capped at 1 MiB — SendGrid tolerates much more, but
// anything beyond that in an approval payload is almost certainly a bug
// and blowing up a browser state blob to multi-megabytes is a bigger UX
// regression than hiding the preview behind a "too large" notice.
func summarizeSendEmailInput(toolName, rawInput, convID string) map[string]any {
	var args map[string]any
	if err := json.Unmarshal([]byte(rawInput), &args); err != nil {
		return map[string]any{"tool": toolName, "raw": rawInput}
	}
	// Stage() inlines content_file into content before we get here, so
	// reading content alone is enough.
	full := firstString(args, "content")
	// Substitute `src="cid:foo"` refs with data: URLs sourced from the
	// matching inline_attachments entries. The preview iframe has no
	// SMTP envelope so cid: URLs would otherwise render as broken
	// images; the user sees a chart-shaped void in the draft and
	// reports "I clicked send blindly". The actual approval.args_json
	// keeps the original cid: + inline_attachments so the real
	// SendGrid call still attaches the images and the recipient's
	// inbox renders them via the normal MIME path.
	if convID != "" {
		full = expandCidImagesToDataURLs(full, args, convID)
	}
	preview := full
	if preview != "" && len(preview) > 600 {
		preview = preview[:600] + "…[truncated]"
	}
	const maxContentBytes = 1 << 20 // 1 MiB
	contentOverflow := false
	if len(full) > maxContentBytes {
		full = full[:maxContentBytes]
		contentOverflow = true
	}
	contentType := firstString(args, "content_type")
	if contentType == "" {
		// Match sendgrid_server.py's sniff: presence of <html/<body/<table
		// or a doctype → assume HTML, otherwise plain. Cheap and close
		// enough for preview routing; the real detection runs server-side
		// before send.
		contentType = sniffContentType(full)
	}
	return map[string]any{
		"tool":             toolName,
		"to":               args["to_email"],
		"cc":               args["cc_emails"],
		"bcc":              args["bcc_emails"],
		"subject":          args["subject"],
		"from":             firstString(args, "from_email"),
		"preview":          preview,
		"content":          full,
		"content_type":     contentType,
		"content_overflow": contentOverflow,
	}
}

// expandCidImagesToDataURLs rewrites `src="cid:<id>"` references in
// the supplied HTML into `src="data:image/png;base64,..."` data URLs
// sourced from the matching inline_attachments entry. Used only for
// the preview pane — the underlying approval row keeps the original
// cid: form so SendGrid's MIME assembler still attaches the images
// for the real send.
//
// Path validation mirrors the workspace file API: relative paths
// resolve under the per-conversation workspace root, EvalSymlinks +
// has-prefix check rejects escapes. We refuse to read anything
// larger than 4 MiB (the same per-attachment cap the email pipeline
// enforces elsewhere) so a misbehaving agent can't blow up the
// approval row's summary payload.
func expandCidImagesToDataURLs(html string, args map[string]any, convID string) string {
	atts, ok := args["inline_attachments"].([]any)
	if !ok || len(atts) == 0 || html == "" {
		return html
	}
	wsDir, err := filepath.Abs(tools.WorkspaceDirForConversation(convID))
	if err != nil {
		return html
	}
	const maxAttachmentBytes = 4 << 20

	for _, raw := range atts {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		cid := strings.TrimSpace(firstString(entry, "cid", "content_id"))
		path := strings.TrimSpace(firstString(entry, "path", "file"))
		if cid == "" || path == "" {
			continue
		}
		// Resolve to an absolute path safely. Refuse anything that
		// escapes the workspace dir so a hostile agent can't get
		// /etc/passwd inlined into the preview card.
		var full string
		if filepath.IsAbs(path) {
			full = path
		} else {
			full = filepath.Join(wsDir, filepath.FromSlash(path))
		}
		resolved, err := filepath.EvalSymlinks(full)
		if err != nil {
			continue
		}
		resolvedAbs, err := filepath.Abs(resolved)
		if err != nil {
			continue
		}
		if resolvedAbs != wsDir && !strings.HasPrefix(resolvedAbs, wsDir+string(filepath.Separator)) {
			continue
		}
		info, err := os.Stat(resolvedAbs)
		if err != nil || info.IsDir() || info.Size() > maxAttachmentBytes {
			continue
		}
		data, err := os.ReadFile(resolvedAbs) //nolint:gosec // path was validated above to live under wsDir
		if err != nil {
			continue
		}
		mimeType := mime.TypeByExtension(filepath.Ext(resolvedAbs))
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		dataURL := "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)

		// Replace every `cid:<id>` reference (single or double quotes,
		// case-insensitive scheme). The cid value itself is treated as
		// a literal — it's already constrained to email-safe chars
		// (RFC 2392) so a regex special isn't a concern here, but
		// QuoteMeta keeps us safe regardless.
		quoted := regexp.QuoteMeta(cid)
		re := regexp.MustCompile(`(?i)cid:` + quoted + `\b`)
		html = re.ReplaceAllString(html, dataURL)
	}
	return html
}

// sniffContentType returns "text/html" if the body looks like HTML, else
// "text/plain". Checked case-insensitively against a few structural tags
// that almost every styled email carries. Mirrors the agent-side default
// "always content_type=text/html" convention while still letting plain
// drafts preview as text.
func sniffContentType(body string) string {
	if body == "" {
		return "text/plain"
	}
	lc := strings.ToLower(body)
	markers := []string{"<!doctype", "<html", "<body", "<table", "<div"}
	for _, m := range markers {
		if strings.Contains(lc, m) {
			return "text/html"
		}
	}
	return "text/plain"
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

// ── HTTP handler: POST /conversations/{id}/approvals/{approvalId} ───────

// approvalRequest is the body the UI sends when the user clicks Send,
// Cancel, or — for suggest_advanced_model — Switch & retry / Just
// switch / Dismiss. The `action` field is optional and only inspected
// for tool kinds that distinguish multiple positive resolutions
// (currently just suggest_advanced_model). For send_email / preview_email /
// bash the boolean `approved` alone is authoritative.
type approvalRequest struct {
	Approved bool   `json:"approved"`
	Action   string `json:"action,omitempty"` // "switch_and_retry" | "switch_only" | "dismiss"
}

// approvalHandler runs the staged tool when the user approves, or marks it
// rejected. Returns the MCP result or the rejection confirmation.
//
// Requires the agent manager's MCP client so we can fire the send outside
// the agent loop. For that reason it lives on Server (which already holds
// *agent.Manager).
func (s *Server) handleApproval(w http.ResponseWriter, r *http.Request, convID, approvalID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := userFromCtx(r.Context())

	var req approvalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}

	approval, err := s.store.GetApproval(r.Context(), user, approvalID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if approval == nil {
		http.Error(w, "approval not found", http.StatusNotFound)
		return
	}
	if approval.ConversationID != convID {
		http.Error(w, "approval/conversation mismatch", http.StatusBadRequest)
		return
	}
	if approval.Status != "pending" {
		// Idempotent: return the already-resolved state without re-firing.
		writeJSON(w, map[string]any{
			"status":      approval.Status,
			"result_text": approval.ResultText,
		})
		return
	}

	// suggest_advanced_model has its own resolution shape (three actions
	// instead of approve/reject) and a side effect that's pure metadata
	// — flipping conversations.model — rather than firing an MCP tool.
	// Branch before the generic Send path so we don't accidentally
	// route it through runStagedTool.
	if approval.ToolName == tools.SuggestAdvancedModelToolName {
		s.handleSuggestAdvancedApproval(w, r, user, approval, req)
		return
	}

	if !req.Approved {
		claimed, err := s.store.ClaimApproval(r.Context(), user, approvalID, "rejected",
			"User declined to send.")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !claimed {
			s.writeResolvedApprovalState(w, r, user, approvalID)
			return
		}
		// Also write a tool_result into the conversation so the NEXT turn
		// sees the rejection and the model knows not to retry. Use the
		// original tool_call id so the chip in the UI updates instead
		// of orphaning a second result row keyed off the approval id.
		appendToolResultToHistory(r.Context(), s.store, convID, approval.ToolName,
			resolutionCallID(approval), "User declined to send this email.", false)
		writeJSON(w, map[string]any{"status": "rejected"})
		return
	}

	// User clicked Send. Claim the approval BEFORE firing the tool:
	// the pending→approved flip is the only atomic gate, so executing
	// first would let two concurrent requests (double-click, mobile
	// retry) both send the email and only collide on the status write
	// afterwards. Losing the claim means someone else is running it —
	// return their resolved state instead of re-firing.
	claimed, err := s.store.ClaimApproval(r.Context(), user, approvalID, "approved",
		"Approved — executing…")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !claimed {
		s.writeResolvedApprovalState(w, r, user, approvalID)
		return
	}

	// Fire the MCP tool. Mock mode short-circuits here so Playwright can
	// exercise the approval UX without real SendGrid credentials. The
	// user already committed to the send, so detach from the request
	// context — a tab close mid-flight must not abandon a claimed
	// approval half-executed (runStagedTool applies its own 60s cap).
	execCtx := context.WithoutCancel(r.Context())
	var (
		text    string
		toolErr error
	)
	if s.cfg.MockMode {
		text = `{"status_code":202,"message":"mock send ok"}`
	} else {
		text, toolErr = runStagedTool(execCtx, s.agent, approval)
	}
	resultText := text
	if toolErr != nil {
		// The approval stays approved so the user knows the send was
		// attempted; surface the failure in result_text so the UI can
		// show it.
		resultText = fmt.Sprintf("send failed: %v", toolErr)
	}
	if err := s.store.SetApprovalResult(execCtx, user, approvalID, resultText); err != nil {
		log.Printf("SetApprovalResult: %v", err)
	}
	// Write the real tool_result into history so the next turn's model
	// sees what happened — and so the existing chip in the UI updates
	// from "APPROVAL_REQUIRED..." to the real outcome on reload.
	isErr := toolErr != nil
	appendToolResultToHistory(execCtx, s.store, convID, approval.ToolName,
		resolutionCallID(approval), resultText, isErr)

	writeJSON(w, map[string]any{
		"status":      "approved",
		"result_text": resultText,
		"is_err":      isErr,
	})
}

// writeResolvedApprovalState answers a request that lost the claim race
// (or arrived after resolution) with the current state of the approval,
// mirroring the idempotent already-resolved response above.
func (s *Server) writeResolvedApprovalState(w http.ResponseWriter, r *http.Request, user, approvalID string) {
	latest, err := s.store.GetApproval(r.Context(), user, approvalID)
	if err != nil || latest == nil {
		http.Error(w, "approval already resolved", http.StatusConflict)
		return
	}
	writeJSON(w, map[string]any{
		"status":      latest.Status,
		"result_text": latest.ResultText,
	})
}

// handleSuggestAdvancedApproval resolves a suggest_advanced_model card.
// Three valid actions:
//   - "switch_and_retry" / "switch_only": mark approved, pin the
//     conversation to the advanced model. The UI distinguishes the two
//     by whether it re-submits the prior user turn after the response;
//     the server only cares about the model flip.
//   - "dismiss" (or req.Approved == false): mark rejected. The
//     conversation's model stays as-is.
//
// The response carries `model` (the new effective slug) so the UI can
// update its local conversation state without a separate refetch.
func (s *Server) handleSuggestAdvancedApproval(w http.ResponseWriter, r *http.Request, user string, approval *store.Approval, req approvalRequest) {
	switchAction := req.Action == "switch_and_retry" || req.Action == "switch_only" ||
		(req.Action == "" && req.Approved)
	if !switchAction {
		// Treat anything else as a dismissal. Record the rejection so
		// the cooldown gate in StageSuggestion sees it.
		if err := s.store.ResolveApproval(r.Context(), user, approval.ID, "rejected",
			"User dismissed the model-switch suggestion."); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		appendToolResultToHistory(r.Context(), s.store, approval.ConversationID, approval.ToolName, resolutionCallID(approval),
			"User dismissed the model-switch suggestion. Continue working with the current model.", false)
		writeJSON(w, map[string]any{
			"status": "rejected",
			"action": "dismiss",
		})
		return
	}

	// User accepted. Pin the conversation to the advanced model first;
	// the resolution row is only useful if the side effect succeeds.
	if err := s.store.SetModel(r.Context(), user, approval.ConversationID, agentcore.AdvancedModelSlug); err != nil {
		log.Printf("SetModel (suggest_advanced approval): %v", err)
		http.Error(w, "could not pin conversation model: "+err.Error(), http.StatusInternalServerError)
		return
	}
	resultText := fmt.Sprintf("User accepted the suggestion. Conversation pinned to %s.", agentcore.AdvancedModelSlug)
	if err := s.store.ResolveApproval(r.Context(), user, approval.ID, "approved", resultText); err != nil {
		log.Printf("ResolveApproval (suggest_advanced): %v", err)
	}
	appendToolResultToHistory(r.Context(), s.store, approval.ConversationID, approval.ToolName, resolutionCallID(approval),
		resultText, false)

	action := req.Action
	if action == "" {
		action = "switch_only"
	}
	writeJSON(w, map[string]any{
		"status":      "approved",
		"action":      action,
		"model":       agentcore.AdvancedModelSlug,
		"result_text": resultText,
	})
}

// resolutionCallID returns the id under which a post-approval
// tool_result row should be written. Prefer the original tool_call id
// the orchestration layer captured at stage time so the chip the UI
// already rendered (keyed off that id) gets its resultText updated
// from "APPROVAL_REQUIRED..." to the real outcome on reload. Older
// approval rows predating the migration won't have one — fall back
// to the approval id, matching pre-fix behavior.
func resolutionCallID(a *store.Approval) string {
	if a != nil && a.ToolCallID != "" {
		return a.ToolCallID
	}
	if a != nil {
		return a.ID
	}
	return ""
}

// runStagedTool executes the staged tool one-shot outside the agent
// loop. Supports MCP tools (send_email) and the native bash tool (risky
// shell commands gated by orchestration.checkBashSafety).
func runStagedTool(ctx context.Context, mgr turnEngine, approval *store.Approval) (string, error) {
	if mgr == nil {
		return "", errors.New("agent manager unavailable")
	}
	// Native bash: run inline via the tools package. Same safety checks
	// apply (hard-blocked patterns still rejected) since runBash is the
	// single entry point.
	if approval.ToolName == "bash" {
		return runStagedBash(ctx, mgr, approval)
	}
	// preview_email is preview-only by design: there is no send path.
	// When the user clicks Dismiss, we just mark it resolved with a
	// canned acknowledgment so the history reflects "user saw it".
	if approval.ToolName == "preview_email" {
		return "Preview dismissed by user. No email was sent.", nil
	}
	client := mgr.MCPClient()
	if client == nil {
		return "", errors.New("MCP client not initialized (mock mode?)")
	}
	// Our internal naming convention is mcp_<server>_<tool>. Route by the
	// full prefixed name so the call lands on the server that staged it —
	// bare names collide across servers (sendgrid and mailbux both
	// export send_email).
	if !strings.HasPrefix(approval.ToolName, "mcp_") {
		return "", fmt.Errorf("unsupported tool for approval: %s", approval.ToolName)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(approval.ArgsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	// Give the send a generous but bounded timeout — SendGrid is usually
	// <1s, but we don't want to hold the approval request open forever.
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	result, err := client.CallToolPrefixed(ctx, approval.ToolName, args)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, block := range result.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
			sb.WriteString("\n")
		}
	}
	return strings.TrimSpace(sb.String()), nil
}

// runStagedBash executes an approved bash invocation directly. We call
// the tools package rather than going through the fantasy agent loop so
// the approval is a truly one-shot execution; no new LLM turn runs
// here. Hard-blocked safety rules still apply inside runBash.
func runStagedBash(ctx context.Context, mgr turnEngine, approval *store.Approval) (string, error) {
	var params tools.BashParams
	if err := json.Unmarshal([]byte(approval.ArgsJSON), &params); err != nil {
		return "", fmt.Errorf("parse bash args: %w", err)
	}
	// Bound the approval handler; the configured per-call timeout still
	// applies inside runBash, but we also want the HTTP request to
	// return in reasonable time.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	// Pull a sandbox from the warm pool so the approval runs through
	// the same container boundary an agent-driven bash call would.
	// Production agent.New always wires a pool; a nil pool here is a
	// programmer error and we surface it rather than silently dropping
	// to host execution (which would let approved-but-malicious bash
	// reach the chat-server's filesystem and credentials).
	pool := mgr.SandboxPool()
	if pool == nil {
		return "", fmt.Errorf("approval handler: no sandbox pool wired (chat-server boot misconfigured?)")
	}
	sb, cleanup, err := pool.Take()
	if err != nil {
		return "", fmt.Errorf("take sandbox: %w", err)
	}
	defer cleanup()
	return tools.RunBashForApproval(ctx, sb, params)
}

// appendToolResultToHistory writes a synthetic tool_result row so the
// conversation transcript reflects the outcome of the (async) approval.
func appendToolResultToHistory(ctx context.Context, st *store.Store, convID, toolName, callID, text string, isErr bool) {
	entry := agent.HistoryEntry{
		Role: "tool",
		Type: "tool_result",
	}
	payload, _ := json.Marshal(map[string]any{
		"id":     callID,
		"name":   toolName,
		"text":   text,
		"is_err": isErr,
	})
	entry.Content = payload
	if err := st.AppendHistory(ctx, convID, []agent.HistoryEntry{entry}); err != nil {
		log.Printf("AppendHistory (approval result): %v", err)
	}
}

// ── helper on Manager: expose the MCP client to the approval path ──
// (added via a small method on Manager in agent/agent.go)

var _ = mcp.Tool{}
