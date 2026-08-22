// The extracted bodies of the /conversations/{id} sub-route actions the
// conversationByID dispatcher (conversations.go) routes through its
// (sub, method) table. Split out of the former ~630-line switch (#1127); each
// method keeps its branch's exact response semantics. The mcp-servers pair
// lives in mcp_servers.go with the rest of the catalog surfaces.

package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/store"
)

// handleConversationGet serves GET /conversations/{id}: the conversation row
// plus its full history, pending and resolved approval cards, and pending
// memory proposals — everything a reload needs to re-hydrate the transcript.
func (s *Server) handleConversationGet(w http.ResponseWriter, r *http.Request, user, id string, conv *store.Conversation) {
	history, err := s.store.LoadHistory(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	pending, err := s.store.ListPendingApprovals(r.Context(), user, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Shape pending approvals the same way tool.approval_required
	// events do, so the frontend reuses its render path.
	approvals := make([]map[string]any, 0, len(pending))
	for _, a := range pending {
		approvals = append(approvals, map[string]any{
			"approval_id": a.ID,
			"tool":        a.ToolName,
			"summary":     summarizeApprovalInput(a.ToolName, a.ArgsJSON, id),
			// Re-hydrate the countdown on reload (#225); 0 = no expiry.
			"expires_at": a.ExpiresAt,
			// Re-hydrate the seat badge (#167 residual 2); empty account
			// means the default bundle seat and renders no badge.
			"mcp_server":  a.MCPServer,
			"mcp_account": a.MCPAccount,
			// Anchors the card to the message holding this tool_call so a
			// reload places it where the live stream did (last-assistant
			// fallback when empty — older rows, promote cards).
			"tool_call_id": a.ToolCallID,
		})
	}
	// Resolved approvals re-hydrate too, so the transcript keeps the shape
	// it had live: the "Email sent ✓" outcome card, a timed-out card, and —
	// load-bearing for notify mode (#1153) — the "ran without asking" record
	// with its undo hint, whose only other delivery is an SSE stream the
	// away-from-page user (notify's entire audience) was not watching.
	resolved, err := s.store.ListResolvedApprovals(r.Context(), user, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resolvedCards := make([]map[string]any, 0, len(resolved))
	for _, a := range resolved {
		resolvedCards = append(resolvedCards, map[string]any{
			"approval_id":  a.ID,
			"tool":         a.ToolName,
			"summary":      summarizeApprovalInput(a.ToolName, a.ArgsJSON, id),
			"status":       a.Status,
			"result_text":  a.ResultText,
			"mcp_server":   a.MCPServer,
			"mcp_account":  a.MCPAccount,
			"tool_call_id": a.ToolCallID,
			// True for a notify-mode record (#1153): the card says the tool
			// already ran without asking, not that the user approved it.
			"recorded": isNotifyRecordResult(a.ResultText),
		})
	}
	// Pending memory proposals — same pattern as approvals. Without
	// these, the visibilitychange/focus auto-refetch in chat-experience
	// wipes the Save/Don't-Save card every time the user clicks away
	// and back.
	pendingMems, err := s.store.ListPendingMemoryProposalsForConversation(r.Context(), user, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Resolve supersede-claim targets so re-hydrated cards can render
	// "replaces: …" — one user-scoped list lookup covers every claim.
	var byID map[string]store.Memory
	for _, m := range pendingMems {
		if m.Supersedes == "" {
			continue
		}
		all, err := s.store.ListMemories(r.Context(), user)
		if err != nil {
			break // display-only enrichment; the card still renders without it
		}
		byID = make(map[string]store.Memory, len(all))
		for _, mm := range all {
			byID[mm.ID] = mm
		}
		break
	}
	memProposals := make([]map[string]any, 0, len(pendingMems))
	for _, m := range pendingMems {
		entry := map[string]any{
			"proposal_id": m.ID,
			"content":     m.Content,
			"kind":        m.Kind,
		}
		if m.Supersedes != "" {
			entry["supersedes_id"] = m.Supersedes
			if t, ok := byID[m.Supersedes]; ok {
				entry["supersedes_content"] = excerpt(t.Content, 200)
			}
		}
		memProposals = append(memProposals, entry)
	}
	writeJSON(w, map[string]any{
		"conversation":             conv,
		"history":                  history,
		"pending_approvals":        approvals,
		"resolved_approvals":       resolvedCards,
		"pending_memory_proposals": memProposals,
	})
}

// handleConversationDelete serves DELETE /conversations/{id}.
func (s *Server) handleConversationDelete(w http.ResponseWriter, r *http.Request, user, id string) {
	if err := s.store.Delete(r.Context(), user, id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Reclaim the conversation's persistent run_python sandbox (#213), if any.
	s.releasePersistentSandbox(id)
	w.WriteHeader(http.StatusNoContent)
}

// handleConversationTruncate serves POST /conversations/{id}/truncate.
//
// Retry/regenerate: drop every message after the latest user turn
// so the next turn regenerates the assistant tail from scratch.
// With ?mode=edit_last we drop the latest user turn too, which is
// what the edit-and-resend flow needs.
func (s *Server) handleConversationTruncate(w http.ResponseWriter, r *http.Request, user, id string) {
	mode := r.URL.Query().Get("mode")
	var pivot int64
	var err error
	if mode == "edit_last" {
		// Truncate after the SECOND-to-last user so the latest user
		// turn (and its assistant tail) are both removed. If no prior
		// user exists, zero is fine — everything gets wiped.
		pivot, err = s.store.SecondMaxMessageIDForRole(r.Context(), id, "user")
	} else {
		pivot, err = s.store.MaxMessageIDForRole(r.Context(), id, "user")
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.store.TruncateAfter(r.Context(), user, id, pivot); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleConversationPin serves POST /conversations/{id}/pin.
func (s *Server) handleConversationPin(w http.ResponseWriter, r *http.Request, user, id string) {
	var req struct {
		Pinned bool `json:"pinned"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if err := s.store.SetPinned(r.Context(), user, id, req.Pinned); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleConversationArchive serves POST /conversations/{id}/archive.
//
// Soft-archive / unarchive (#282). Archiving hides the conversation
// from the default sidebar (and unpins it); unarchiving restores it.
func (s *Server) handleConversationArchive(w http.ResponseWriter, r *http.Request, user, id string) {
	var req struct {
		Archived bool `json:"archived"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if err := s.store.SetArchived(r.Context(), user, id, req.Archived); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleConversationRefile serves POST /conversations/{id}/project.
//
// Re-file the conversation into a project, or unfile it (#509
// follow-up): body {"project_id": "..."} — empty unfiles. Membership
// is enforced exactly like project-bound creation (404 for missing
// AND non-member, so project ids don't leak membership state).
// Re-filing binds the project's instructions + shared memory from
// the next turn on; it does NOT retro-apply the project's curated
// connectors or default persona/model (those are creation-time
// inheritances — see SetConversationProject).
func (s *Server) handleConversationRefile(w http.ResponseWriter, r *http.Request, user, id string) {
	var req struct {
		ProjectID string `json:"project_id"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.ProjectID != "" {
		if p := s.projectForMember(w, r, user, req.ProjectID); p == nil {
			return
		}
	}
	if err := s.store.SetConversationProject(r.Context(), user, id, req.ProjectID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleConversationRename serves POST /conversations/{id}/rename.
func (s *Server) handleConversationRename(w http.ResponseWriter, r *http.Request, user, id string) {
	var req struct {
		Title string `json:"title"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		http.Error(w, "title required", http.StatusBadRequest)
		return
	}
	if len(title) > 200 {
		title = title[:200]
	}
	// A manual rename sets the title AND locks it (#302) so the background
	// auto-titler never overwrites the user's chosen name.
	if err := s.store.RenameTitle(r.Context(), user, id, title); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// If a turn is mid-flight (e.g. the user renames DURING first-turn
	// auto-titling), push the manual name over the live SSE buffer so the
	// sidebar reflects it immediately and isn't left showing a now-superseded
	// auto-title. The DB is already authoritative (the lock makes the manual
	// name win); this just keeps the screen in sync without a reload. When no
	// turn is live, no stale auto-title emit can arrive, so no emit is needed.
	if entry, ok := s.getInflight(id); ok && entry.buf != nil {
		entry.buf.Emit("conversation.title_updated", map[string]any{"id": id, "title": title})
	}
	writeJSON(w, struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}{ID: id, Title: title})
}

// handleConversationModel serves POST /conversations/{id}/model. It is
// deliberately NOT adapted through withOwnedConversation: its ownership check
// historically runs only when a non-empty model must be validated against the
// lockdown allow-list, and answers via http.NotFound ("404 page not found")
// rather than the other branches' plain-text 404 bodies — both preserved
// verbatim here.
func (s *Server) handleConversationModel(w http.ResponseWriter, r *http.Request, user, id string) {
	var req struct {
		Model string `json:"model"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	model := strings.TrimSpace(req.Model)
	if model != "" {
		conv, err := s.store.Get(r.Context(), user, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if conv == nil {
			http.NotFound(w, r)
			return
		}
		if conv.Lockdown && !s.cfg.LockdownAllows(model) {
			http.Error(w, "model not allowed in lockdown mode", http.StatusBadRequest)
			return
		}
	}
	if err := s.store.SetModel(r.Context(), user, id, model); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleApprovalTimeoutGet serves GET /conversations/{id}/approval-timeout.
//
// Per-conversation approval default-deny window override (#225).
// approval_timeout_seconds == null means "use the global default".
func (s *Server) handleApprovalTimeoutGet(w http.ResponseWriter, _ *http.Request, _, _ string, conv *store.Conversation) {
	writeJSON(w, map[string]any{
		"approval_timeout_seconds": conv.ApprovalTimeoutSeconds,
		"default_seconds":          s.cfg.LiveApprovalTimeoutSeconds(),
	})
}

// handleApprovalTimeoutSet serves POST /conversations/{id}/approval-timeout.
//
// Set or clear the per-conversation override (#225). A null/omitted
// value clears it back to the global default; a positive value (bounded
// to a sane range) sets the per-chat window. Zero/negative is rejected
// rather than silently meaning "no timeout" — an explicit error avoids an
// accidental instant-deny.
func (s *Server) handleApprovalTimeoutSet(w http.ResponseWriter, r *http.Request, user, id string) {
	var req struct {
		ApprovalTimeoutSeconds *int `json:"approval_timeout_seconds"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.ApprovalTimeoutSeconds != nil {
		v := *req.ApprovalTimeoutSeconds
		if v <= 0 || v > maxApprovalTimeoutSeconds {
			http.Error(w, "approval_timeout_seconds must be between 1 and 86400, or null to clear", http.StatusBadRequest)
			return
		}
	}
	if err := s.store.SetApprovalTimeout(r.Context(), user, id, req.ApprovalTimeoutSeconds); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleThinkingConfigGet serves GET /conversations/{id}/thinking_config.
//
// Per-conversation Claude extended-thinking override (#220). A null
// thinking_config means "inherit the global default".
func (s *Server) handleThinkingConfigGet(w http.ResponseWriter, _ *http.Request, _, _ string, conv *store.Conversation) {
	writeJSON(w, map[string]any{
		"thinking_config":       conv.ThinkingConfig,
		"default_budget_tokens": s.cfg.DefaultThinkingBudgetTokens,
	})
}

// handleThinkingConfigPut serves PUT /conversations/{id}/thinking_config.
//
// Set the per-conversation override (#220). budget_tokens must be 0 (use
// the global default budget) or within Claude's [1024, 100000] window;
// an out-of-window non-zero value is rejected rather than silently clamped
// so the caller gets a clear signal. enabled=false stores an explicit
// opt-out that overrides a global default; DELETE clears back to inherit.
func (s *Server) handleThinkingConfigPut(w http.ResponseWriter, r *http.Request, user, id string) {
	var req struct {
		Enabled      bool `json:"enabled"`
		BudgetTokens int  `json:"budget_tokens"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.BudgetTokens != 0 && (req.BudgetTokens < agentcore.MinThinkingBudgetTokens || req.BudgetTokens > agentcore.MaxThinkingBudgetTokens) {
		http.Error(w, fmt.Sprintf("budget_tokens must be 0 or between %d and %d", agentcore.MinThinkingBudgetTokens, agentcore.MaxThinkingBudgetTokens), http.StatusBadRequest)
		return
	}
	if err := s.store.SetThinkingConfig(r.Context(), user, id, &store.ThinkingConfig{Enabled: req.Enabled, BudgetTokens: req.BudgetTokens}); err != nil {
		if err.Error() == "conversation not found" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleThinkingConfigDelete serves DELETE /conversations/{id}/thinking_config.
//
// Clear the override → inherit the global default (#220).
func (s *Server) handleThinkingConfigDelete(w http.ResponseWriter, r *http.Request, user, id string) {
	if err := s.store.SetThinkingConfig(r.Context(), user, id, nil); err != nil {
		if err.Error() == "conversation not found" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleConversationExport serves GET /conversations/{id}/export.
//
// JSON export of the full conversation (metadata + history).
// Returned as a downloadable attachment so the browser triggers
// a Save dialog; reuses the same fields as GET /conversations/{id}.
func (s *Server) handleConversationExport(w http.ResponseWriter, r *http.Request, _, id string, conv *store.Conversation) {
	history, err := s.store.LoadHistory(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	exportedAt := time.Now().UTC()
	// ?format=markdown renders a human-readable transcript (#210); the default
	// (json) preserves the prior machine-readable shape exactly.
	switch strings.ToLower(r.URL.Query().Get("format")) {
	case "markdown", "md":
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set(
			"Content-Disposition",
			fmt.Sprintf(`attachment; filename="%s"`, exportFilename(conv.Title, conv.ID, "md", "chat")),
		)
		_, _ = io.WriteString(w, renderConversationMarkdown(conv, history, exportedAt))
	default:
		body := map[string]any{
			"conversation": conv,
			"history":      history,
			"exported_at":  exportedAt.Format(time.RFC3339),
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set(
			"Content-Disposition",
			fmt.Sprintf(`attachment; filename="%s"`, exportFilename(conv.Title, conv.ID, "json", "chat")),
		)
		_ = json.NewEncoder(w).Encode(body)
	}
}

// handleConversationCancel serves POST /conversations/{id}/cancel.
//
// Explicit Stop button. Owner-scoped: the withOwnedConversation gate confirms
// the conversation belongs to the caller before issuing the cancel so a token
// leak can't cancel arbitrary chats. The scope decode below is deliberately
// lenient (a malformed body means the default scope, not a 400), so it does
// not use decodeJSONBody.
func (s *Server) handleConversationCancel(w http.ResponseWriter, r *http.Request, user, id string, _ *store.Conversation) {
	// Stop semantics (#785): default scope "all" covers the active turn
	// AND every still-queued follow-up — Stop means "stop working", not
	// "stop this one and surprise me with the next". scope=turn cancels
	// only the active turn and lets the queue drain.
	scope := "all"
	if r.Body != nil {
		var body struct {
			Scope string `json:"scope"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil && strings.EqualFold(body.Scope, "turn") {
			scope = "turn"
		}
	}
	if scope == "all" {
		// Epoch BEFORE the sweep: a row claimed by a racing drain is
		// invisible to CancelQueuedInputs, but launchQueuedTurn gates on
		// the epoch, so claim-limbo rows accepted before Stop still die.
		s.markStopAll(id)
	}
	s.cancelInflight(id)
	if scope == "all" {
		// Fresh context: Stop must sweep the queue even when the client
		// aborts the request the moment the button is pressed.
		qctx, qcancel := context.WithTimeout(context.Background(), 5*time.Second)
		if n, err := s.store.CancelQueuedInputs(qctx, user, id); err != nil {
			log.Printf("cancel queued inputs (conv=%s): %v", id, err) //nolint:gosec // G706: server-generated UUIDs + internal error — no request-authored text is logged.
		} else if n > 0 {
			s.emitQueueUpdate(qctx, user, id)
		}
		qcancel()
	}
	w.WriteHeader(http.StatusNoContent)
}
