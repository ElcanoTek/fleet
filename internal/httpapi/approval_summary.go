package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/croncount"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// approvalClientFields is the review payload shared by live SSE events and
// GET /conversations/{id} pending cards so a terminal cannot see a truncated
// summary for one tool class and a full snapshot for another.
func approvalClientFields(toolName, rawInput, convID string) map[string]any {
	frozen := frozenApprovalArgs(rawInput)
	if toolName == tools.SuggestAdvancedModelToolName && suggestedModel(rawInput) == "" {
		// Legacy suggestions did not freeze their target: require a new card.
		frozen = map[string]any{"complete": false}
	}
	out := map[string]any{
		"summary":      summarizeApprovalInput(toolName, rawInput, convID),
		"pattern_args": handlerApprovalPatternArgs(toolName, rawInput),
		"frozen_args":  frozen,
		// True when the bundle lists the tool in
		// critical_tool_no_session_approval: the card must not offer
		// "apply my choice to all calls", and the POST refuses that scope.
		"no_session_approval": !agentcore.SessionApprovalAllowed(toolName),
	}
	if approvalReportsProgress(toolName) {
		// critical_tool_progress: once approved and running, the card polls
		// for the call's progress and outcome (docs/APPROVAL-PROGRESS.md).
		// Only present when true, so every other card's payload is unchanged.
		out["progress_updates"] = true
	}
	if approvalStoppable(toolName) {
		// critical_tool_stop: once approved and running, the card offers
		// Stop (docs/APPROVAL-STOP.md). Only present when true.
		out["stoppable"] = true
	}
	if resumesAfterApproval(toolName) {
		// critical_tool_resume: once this card is settled fleet starts a turn
		// on its own (docs/RESUME-AFTER-APPROVAL.md), so the client follows the
		// conversation for it. Only present when true, so every other card's
		// payload is unchanged.
		out["resume_after_approval"] = true
	}
	return out
}

// resumesAfterApproval reports whether a settled card for toolName starts a
// resume turn under the running policy (handler-only cards never do, except
// enable_mcp_connectors which always resumes so the agent can use the newly
// enabled connectors immediately).
func resumesAfterApproval(toolName string) bool {
	if toolName == tools.EnableMCPConnectorsToolName {
		return true
	}
	return agentcore.ResumeAfterApproval(toolName) && !handlerOnlyApproval(toolName)
}

func approvalRequiredEvent(approval *store.Approval, rawInput, convID string) map[string]any {
	ev := approvalClientFields(approval.ToolName, rawInput, convID)
	if summary, ok := ev["summary"].(map[string]any); ok {
		ev["summary"] = withBrowserSessionSite(summary, approval.ToolName, rawInput, approval.UserEmail)
	}
	ev["approval_id"] = approval.ID
	ev["tool"] = approval.ToolName
	ev["expires_at"] = approval.ExpiresAt
	ev["mcp_server"] = approval.MCPServer
	ev["mcp_account"] = approval.MCPAccount
	return withApprovalNarrowable(withApprovalGroup(withApprovalCard(ev, approval), approval), approval)
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
	if toolName == tools.ScheduleTaskToolName {
		return summarizeScheduleTaskInput(toolName, rawInput)
	}
	if toolName == tools.ManageTasksToolName {
		return summarizeManageTasksInput(toolName, rawInput)
	}
	if toolName == tools.EnableMCPConnectorsToolName {
		return summarizeEnableMCPConnectorsInput(toolName, rawInput)
	}
	// preview_email uses the exact same summary shape as send_email
	// so ApprovalCard's existing email-preview render path just works;
	// the only difference is the `tool` field, which drives the UI
	// branch that swaps Send for Dismiss.
	if tools.IsEmailToolName(toolName) {
		return summarizeSendEmailInput(toolName, rawInput, convID)
	}
	// Everything else — bundle-declared critical tools like a pages deploy or
	// a deal write — gets a tool-shaped summary. These used to fall through to
	// the email summary and render on the email card ("Send this email?",
	// "Email sent ✓" — for a page publish), which is exactly the wrong copy on
	// the one card class whose entire job is telling the user what is about to
	// happen.
	if toolName == tools.ConnectorStudioToolName || toolName == tools.PlaywrightBrowserToolName {
		if browser := summarizeBrowserInput(toolName, rawInput); browser != nil {
			return browser
		}
	}
	return summarizeGenericToolInput(toolName, rawInput)
}

// genericArgValueMax bounds each rendered argument value; the full args stay on
// the approval row for execution, this is display only.
const genericArgValueMax = 300

// summarizeGenericToolInput builds the card payload for a critical tool fleet
// has no tailored card for: the tool name plus its top-level arguments as
// display strings (sorted keys — JSON maps have no order), with nested values
// compacted and everything truncated to card-friendly sizes. Fleet cannot know
// what a bundle tool's arguments mean, so it shows them verbatim rather than
// guessing at a friendlier shape — the honest floor for a human review step.
func summarizeGenericToolInput(toolName, rawInput string) map[string]any {
	var args map[string]any
	if err := json.Unmarshal([]byte(rawInput), &args); err != nil || args == nil {
		return map[string]any{"tool": toolName, "raw": rawInput}
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	rows := make([]map[string]string, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, map[string]string{"key": k, "value": displayArgValue(args[k])})
	}
	return map[string]any{
		"tool": toolName,
		"args": rows,
	}
}

// displayArgValue renders one argument value for the generic card: scalars as
// themselves, nested structures as compact JSON, all truncated by RUNES so a
// multibyte payload can't be cut mid-character.
func displayArgValue(v any) string {
	var s string
	switch t := v.(type) {
	case string:
		s = t
	case nil:
		s = "null"
	default:
		b, err := json.Marshal(t)
		if err != nil {
			s = fmt.Sprintf("%v", t)
		} else {
			s = string(b)
		}
	}
	if r := []rune(s); len(r) > genericArgValueMax {
		s = string(r[:genericArgValueMax]) + "…"
	}
	return s
}

// summarizeSuggestAdvancedInput exposes the agent's reason and the
// recommended model slug so the UI card can render both without
// re-parsing the args. The server freezes the configured tier when staging;
// a later admin change must not change what this card authorizes.
func summarizeSuggestAdvancedInput(toolName, rawInput string) map[string]any {
	var args struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal([]byte(rawInput), &args)
	return map[string]any{
		"tool":            toolName,
		"reason":          args.Reason,
		"recommend_model": suggestedModel(rawInput),
		"source":          suggestionSource(rawInput),
	}
}

// Who staged a suggest_advanced_model card (docs/CHAT-ESCALATION.md): the
// chat model through the tool, a free end-of-turn struggle signal, or the
// decision-model classifier. Cards staged before the field existed read as
// the model's.
const (
	suggestionSourceModel      = "model"
	suggestionSourceSignal     = "signal"
	suggestionSourceClassifier = "classifier"
)

func suggestionSource(rawInput string) string {
	var args struct {
		Source string `json:"source"`
	}
	_ = json.Unmarshal([]byte(rawInput), &args)
	switch args.Source {
	case suggestionSourceSignal, suggestionSourceClassifier:
		return args.Source
	}
	return suggestionSourceModel
}

func suggestedModel(rawInput string) string {
	var args struct {
		Model string `json:"recommend_model"`
	}
	if json.Unmarshal([]byte(rawInput), &args) != nil {
		return ""
	}
	return strings.TrimSpace(args.Model)
}

// summarizeScheduleTaskInput builds the schedule_task approval-card payload
// (#239): the task name, a truncated prompt preview, the schedule (cron or
// one-time run_at), whether it recurs, and an estimated runs-per-month for
// recurring tasks so the user can gauge frequency before approving. Pure display
// — the underlying approval row keeps the full args for the create call.
func summarizeScheduleTaskInput(toolName, rawInput string) map[string]any {
	var p scheduleTaskStagedArgs
	if err := json.Unmarshal([]byte(rawInput), &p); err != nil {
		return map[string]any{"tool": toolName, "raw": rawInput}
	}
	const previewMax = 200
	preview := strings.TrimSpace(p.Prompt)
	// Truncate by RUNES, not bytes, so a multibyte UTF-8 prompt isn't cut
	// mid-rune (which would emit a replacement char in the card).
	if r := []rune(preview); len(r) > previewMax {
		preview = string(r[:previewMax]) + "…"
	}
	out := map[string]any{
		"tool":           toolName,
		"name":           strings.TrimSpace(p.Name),
		"prompt_preview": preview,
		// Full prompt so the card's edit mode starts from the real text (the
		// preview is rune-truncated; editing a truncated prompt would clobber
		// the staged one on approve).
		"prompt":        strings.TrimSpace(p.Prompt),
		"model":         strings.TrimSpace(p.Model),
		"allow_network": p.AllowNetwork,
		"tags":          sanitizeTaskTagSlice(p.Tags),
		// Connectors the task inherits from this conversation (ADR-0068), the
		// always-on servers every run binds anyway, and the flag the card turns
		// into a warning: approving as-is schedules a task that can reach no
		// connector at all.
		"connectors":           connectorLabels(p.Connectors),
		"always_on_connectors": append([]string{}, p.AlwaysOnConnectors...),
		"no_connectors":        len(connectorLabels(p.Connectors)) == 0 && len(p.AlwaysOnConnectors) == 0,
	}
	cron := strings.TrimSpace(p.Cron)
	switch {
	case cron != "":
		out["recurring"] = true
		out["cron"] = cron
		if n, ok := croncount.EstimateRunsPerMonth(cron); ok {
			out["runs_per_month"] = n
		}
	case strings.TrimSpace(p.RunAt) != "":
		out["recurring"] = false
		if t, ok := p.RunAtTime(); ok {
			out["run_at"] = t.Format(time.RFC3339)
		} else {
			out["run_at"] = strings.TrimSpace(p.RunAt)
		}
	default:
		out["recurring"] = false
		out["run_immediately"] = true
	}
	return out
}

// summarizeManageTasksInput builds the manage_tasks approval-card payload
// (#1152): what is about to happen, to which tasks, and — for an update — the
// change set as a list of "field → new value" lines.
//
// The card renders the SELECTOR, not a resolved list of matched tasks. Resolving
// it here would need the sched store at staging time, and the number it produced
// would be a snapshot that the executor's own resolution could disagree with by
// the time the human clicked Approve. Instead the boundaries are enforced where
// they are true: the tool refuses a filtered STOP outright, the executor caps an
// update's blast radius, and the post-approval report names every task it
// touched. What the card must never be is vague about the ACTION — so "stop"
// carries its own irreversibility line.
func summarizeManageTasksInput(toolName, rawInput string) map[string]any {
	var p tools.ManageTasksParams
	if err := json.Unmarshal([]byte(rawInput), &p); err != nil {
		return map[string]any{"tool": toolName, "raw": rawInput}
	}
	p.AddTags = sanitizeTaskTagSlice(p.AddTags)
	p.RemoveTags = sanitizeTaskTagSlice(p.RemoveTags)
	action := strings.TrimSpace(p.Action)
	ids := p.CleanTaskIDs()
	out := map[string]any{
		"tool":      toolName,
		"action":    action,
		"task_ids":  ids,
		"match":     p.MatchSummary(),
		"changes":   p.ChangeSummary(),
		"max_tasks": tools.MaxManagedTasks,
	}
	if action == tools.ManageTasksActionStop {
		out["destructive"] = true
		out["consequence"] = "A recurring job stops recurring and will not run again. A run in progress halts at its next checkpoint and keeps its partial transcript."
	}
	return out
}

// formatTaskMutationReport names every task the call touched. A bulk edit that
// reports only a count is a bulk edit nobody can check — and the operator asking
// for this was trying to fix jobs that had drifted, so "which ones" is the whole
// question.
func formatTaskMutationReport(action string, res *TaskMutationResult, link string) string {
	verb := "Updated"
	if action == tools.ManageTasksActionStop {
		verb = "Stopped"
	}
	var b strings.Builder
	if res == nil || len(res.Changed) == 0 {
		b.WriteString("Nothing changed: the selector matched no task you can edit.\n")
	} else {
		fmt.Fprintf(&b, "%s %d task(s):\n", verb, len(res.Changed))
		for _, e := range res.Changed {
			fmt.Fprintf(&b, "  • %s (%s)", e.Label, e.ID)
			if e.Detail != "" {
				fmt.Fprintf(&b, " — %s", e.Detail)
			}
			b.WriteString("\n")
		}
	}
	if res != nil && len(res.Skipped) > 0 {
		fmt.Fprintf(&b, "Skipped %d:\n", len(res.Skipped))
		for _, e := range res.Skipped {
			fmt.Fprintf(&b, "  • %s (%s) — %s\n", e.Label, e.ID, e.Detail)
		}
	}
	if link != "" {
		fmt.Fprintf(&b, "Review them on the Background tasks page: %s", link)
	}
	return b.String()
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
	// Truncate by RUNES, not bytes, so a multibyte command (a non-ASCII
	// path, an emoji in an echo) isn't cut mid-rune into a replacement char.
	preview := args.Command
	if r := []rune(preview); len(r) > 600 {
		preview = string(r[:600]) + "…[truncated]"
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
//   - `attachments` / `inline_attachments`: the frozen path/cid metadata
//     from ArgsJSON, so a terminal review can show every file that will
//     actually be sent. File bytes stay out of the summary; execution still
//     replays the original ArgsJSON.
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
	// Rune-counted like the other previews so a non-ASCII email body isn't
	// cut mid-character; the 1 MiB content cap is a BYTE budget, so it is
	// cut with capBytes (a byte slice snapped back to a rune boundary).
	preview := full
	if r := []rune(preview); len(r) > 600 {
		preview = string(r[:600]) + "…[truncated]"
	}
	const maxContentBytes = 1 << 20 // 1 MiB
	contentOverflow := false
	if len(full) > maxContentBytes {
		full = capBytes(full, maxContentBytes)
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
	out := map[string]any{
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
	if v, ok := args["attachments"]; ok {
		out["attachments"] = v
	}
	if v, ok := args["inline_attachments"]; ok {
		out["inline_attachments"] = v
	}
	return out
}

// cidPattern matches one `cid:<id>` reference. The id runs to the first
// character that cannot appear inside an unquoted/quoted attribute value,
// and the trailing \b makes the match end on a word character — so a
// reference wrapped in markup punctuation (`url(cid:logo)`) still yields
// the bare id. Compiled once at package level: it used to be rebuilt per
// attachment inside the substitution loop, which re-parsed the pattern and
// re-scanned the whole document for every inline image.
var cidPattern = regexp.MustCompile(`(?i)cid:([^\s"'<>]+)\b`)

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
	// Every read goes through an os.Root on the workspace dir: it refuses any
	// path that resolves outside it, whatever the agent supplied.
	root, err := os.OpenRoot(wsDir)
	if err != nil {
		return html
	}
	defer func() { _ = root.Close() }()
	const maxAttachmentBytes = 4 << 20

	cidMap := make(map[string]string)

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
		// Confine the symlink-resolved path to the workspace dir. filepath.Rel +
		// filepath.IsLocal is the containment check CodeQL's path-injection query
		// recognizes: Rel's nil-error result sanitizes resolvedAbs, and IsLocal
		// rejects a "../" escape that Rel would otherwise return with a nil error.
		// This keeps a hostile agent from getting /etc/passwd inlined into the
		// preview card; the root.Stat / root.ReadFile below are confined again by
		// the os.Root.
		rel, relErr := filepath.Rel(wsDir, resolvedAbs)
		if relErr != nil || !filepath.IsLocal(rel) {
			continue
		}
		info, err := root.Stat(rel)
		if err != nil || info.IsDir() || info.Size() > maxAttachmentBytes {
			continue
		}
		data, err := root.ReadFile(rel)
		if err != nil {
			continue
		}
		mimeType := mime.TypeByExtension(filepath.Ext(resolvedAbs))
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		dataURL := "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data)

		// Store the data URL mapped by the case-insensitive cid.
		cidMap[strings.ToLower(cid)] = dataURL
	}

	if len(cidMap) > 0 {
		// ONE pass over the document replaces every `cid:<id>` reference
		// (single or double quotes, case-insensitive scheme) by looking the
		// captured id up in the map, instead of one compile-and-rescan per
		// attachment. A reference with no matching attachment is returned
		// verbatim, so unresolvable cids survive exactly as before. The single
		// pass also means a substituted data URL is never itself rescanned.
		html = cidPattern.ReplaceAllStringFunc(html, func(match string) string {
			cidVal := match[len("cid:"):] // the pattern guarantees the prefix
			if dataURL, ok := cidMap[strings.ToLower(cidVal)]; ok {
				return dataURL
			}
			return match
		})
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
