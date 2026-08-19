package agentcore

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

// Interactive-only native approval gates: risky bash, preview_email, and
// suggest_advanced_model. These mirror chat's agent-package orchestration gates
// (ported here so the unified InteractivePolicy enforces them through the SAME
// BeforeToolCall path as send_email / propose_memory). They are inert in
// scheduled mode (the ScheduledPolicy never calls them).
//
// The tool-name constants are inlined (not imported from internal/tools) to keep
// agentcore dependency-free of the driver tool package.

const (
	toolNameBash                 = "bash"
	toolNamePreviewEmail         = "preview_email"
	toolNameSuggestAdvancedModel = "suggest_advanced_model"
	toolNameScheduleTask         = "schedule_task"
	toolNameManageTasks          = "manage_tasks"
)

// stagedCriticalApproval is one critical tool call this turn parked on an
// approval card. See orchestrationState.stagedCriticalApprovals for why the set
// is tracked rather than just counted.
type stagedCriticalApproval struct {
	tool       string
	approvalID string
}

// checkBashSafety stages risky bash commands (git push, system package-manager
// actions) for user approval. Non-risky bash passes through. Inert when no
// approval sink is wired (scheduled mode).
func (o *orchestrationState) checkBashSafety(toolName, toolCallID, rawInput string) (bool, string) {
	if toolName != toolNameBash {
		return false, ""
	}
	risky, reason := classifyRiskyBash(rawInput)
	if !risky {
		return false, ""
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.approvalSink == nil {
		return false, ""
	}
	id, err := o.approvalSink.Stage(toolName, toolCallID, rawInput)
	if err != nil {
		log.Printf("approval stage failed (bash): %v", err)
		return true, fmt.Sprintf("APPROVAL_REQUIRED: %s. Could not stage for user approval (%v).", reason, err)
	}
	switch id {
	case PreApprovedSentinel:
		// Session pre-approval (#300): run the command without a card.
		return false, ""
	case PreDeniedSentinel:
		return true, fmt.Sprintf("APPROVAL_DENIED: %s — the user pre-denied bash for this conversation (session policy). Do NOT retry.", reason)
	}
	return true, fmt.Sprintf("APPROVAL_REQUIRED: %s — staged for user approval (approval_id=%s). Do NOT retry. Summarize intent and wait for the user to click Approve.", reason, id)
}

// checkCriticalToolApproval stages bundle-declared critical tools (the
// manifest's agent_policy.critical_tools suffixes, e.g. SSP deal-creation and
// deal-mutation tools) for explicit user approval in INTERACTIVE mode. It is
// the interactive counterpart of the scheduled confirm_audit gate
// (checkCriticalTool): scheduled runs self-audit because no human is present;
// interactive turns have a human, so the same suffixes route through the
// approval-card UX that already gates send_email and risky bash. Without this
// gate a suffix a bundle marked critical would execute un-gated in chat —
// contradicting the manifest contract ("critical tools require audit gating
// before execution") and leaving agent_policy.critical_tool_timeouts dead for
// those tools.
//
// Exemptions keep single ownership of the tailored flows that run EARLIER in
// the interactive gate chain: outbound-email tools stay with checkEmailSafety
// (rate-limit + dedup + staging), and the natively-gated tool names keep their
// dedicated gates. Inert when no approval sink is wired (mirrors
// checkBashSafety: a transport with no approval UI cannot stage a card, and
// hard-blocking would brick those tools without offering a decision path).
func (o *orchestrationState) checkCriticalToolApproval(toolName, toolCallID, rawInput string) (bool, string) {
	if !isCriticalTool(toolName) {
		return false, ""
	}
	if isEmailSendTool(toolName) {
		return false, "" // checkEmailSafety owns send_email end-to-end
	}
	switch toolName {
	case toolNameBash, toolNamePreviewEmail, toolNameScheduleTask, toolNameSuggestAdvancedModel:
		return false, "" // dedicated gates own these
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.approvalSink == nil {
		return false, ""
	}
	// Per-tool approval mode (#1153). `notify` runs the tool now and posts a
	// card recording what happened, for operations whose undo is cheap and
	// complete — a page publish against a store that keeps immutable versions,
	// not a sent email. The window it replaces is 300 seconds beginning at an
	// unpredictable moment many minutes into a run the user started and then
	// reasonably stopped watching, so the thing it most reliably blocked was the
	// final, wanted action of a long analysis.
	//
	// Recording is REQUIRED, not best-effort: a sink that cannot post the card,
	// or one that fails to, falls back to blocking. The whole case for executing
	// without asking is that the user still finds out.
	if approvalPolicy, undoHint := ApprovalModeForTool(toolName); approvalPolicy == ApprovalModeNotify {
		if recorder, ok := o.approvalSink.(ActionRecorder); ok {
			if err := recorder.RecordAction(toolName, toolCallID, rawInput, undoHint); err != nil {
				log.Printf("notify-mode record failed (%s): %v — falling back to a blocking approval", toolName, err)
			} else {
				return false, ""
			}
		} else {
			log.Printf("notify-mode configured for %s but this transport cannot record actions — falling back to a blocking approval", toolName)
		}
	}
	if pending, ok := o.pendingApprovalOnSameServer(toolName); ok {
		return true, fmt.Sprintf(
			"APPROVAL_BLOCKED: %s already has a critical action awaiting the user's approval in this turn (%s, approval_id=%s), "+
				"and its arguments are frozen until the user clicks Approve. A second write staged now would be computed against "+
				"state that pending action is about to change, so it would land wrong or be rejected outright. Do NOT stage it and "+
				"do NOT look for another tool that does the same thing. Stop here, summarize what is waiting, and let the user approve it first.",
			toolName, pending.tool, pending.approvalID)
	}
	id, err := o.approvalSink.Stage(toolName, toolCallID, rawInput)
	if err != nil {
		log.Printf("approval stage failed (%s): %v", toolName, err)
		return true, fmt.Sprintf("APPROVAL_REQUIRED: %s is a critical action. Could not stage it for user approval (%v). Ask the user what to do.", toolName, err)
	}
	switch id {
	case PreApprovedSentinel:
		// Session pre-approval (#300): run the tool without a card.
		return false, ""
	case PreDeniedSentinel:
		return true, fmt.Sprintf("APPROVAL_DENIED: the user pre-denied %s for this conversation (session policy). Do NOT retry; tell the user it was blocked by their own pre-approval setting.", toolName)
	}
	o.stagedCriticalApprovals = append(o.stagedCriticalApprovals, stagedCriticalApproval{tool: toolName, approvalID: id})
	// "Do NOT retry" was read by at least one production model as a rule about
	// this tool name only, so it went and staged a second, competing write
	// through a different tool. Name the loophole.
	return true, fmt.Sprintf(
		"APPROVAL_REQUIRED: %s is a critical action and has been staged for explicit user approval (approval_id=%s). "+
			"Do NOT retry, and do NOT attempt the same change through a different tool — every write you stage now is frozen "+
			"with the arguments it has and cannot see what this one does. Summarize what the action would do and wait for the "+
			"user to click Approve.", toolName, id)
}

// pendingApprovalOnSameServer reports an already-staged critical action from the
// same MCP server (and client variant) as toolName, excluding toolName itself so
// batch flows over one tool keep working. Caller holds o.mu.
func (o *orchestrationState) pendingApprovalOnSameServer(toolName string) (stagedCriticalApproval, bool) {
	for _, staged := range o.stagedCriticalApprovals {
		if staged.tool == toolName {
			continue
		}
		if sameToolServer(staged.tool, toolName) {
			return staged, true
		}
	}
	return stagedCriticalApproval{}, false
}

// checkPreviewEmailSafety always stages a preview_email call for display (the
// approval card is the feature; the tool has no execution path).
func (o *orchestrationState) checkPreviewEmailSafety(toolName, toolCallID, rawInput string) (bool, string) {
	if toolName != toolNamePreviewEmail {
		return false, ""
	}
	if hasUnresolvedToolPlaceholder(rawInput) {
		return true, "preview_email argument contains an unresolved ${tool:…} placeholder. The agent runtime does NOT substitute that syntax; paste the actual value into the tool arguments instead."
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.approvalSink == nil {
		return true, "PREVIEW_UNAVAILABLE: email preview is not enabled on this transport. Do NOT retry — describe the draft in your reply instead."
	}
	id, err := o.approvalSink.Stage(toolName, toolCallID, rawInput)
	if err != nil {
		log.Printf("preview stage failed (preview_email): %v", err)
		return true, fmt.Sprintf("PREVIEW_FAILED: could not render preview for display (%v).", err)
	}
	return true, fmt.Sprintf("PREVIEW_DISPLAYED: the user is now viewing your draft in an inbox-style preview card (preview_id=%s). Nothing was sent and no approval is needed. The card has a Dismiss button ONLY — there is no Send button. Do NOT tell the user to \"click Send\" or \"approve\" the card. Instead, describe what you drafted in your reply and wait for the user's next instruction. If they want changes, revise and call preview_email again. If they say \"send it\", call mcp_sendgrid_send_email.", id)
}

// checkScheduleTaskSafety intercepts schedule_task (interactive, #239): it always
// stages the call for explicit user approval. Like preview_email, the tool has no
// execution path of its own — its Run is a guarded error and the actual
// orchestrator task creation happens in the approval-resolution handler when the
// user clicks Approve. Inert (unavailable, never an infinite stage loop) when no
// approval sink is wired; schedule_task is registered only in the interactive
// tool set, so the no-sink branch is a defensive backstop, not a normal path.
//
// Unlike send_email/bash, this gate does NOT handle the pre-approve/pre-deny
// session sentinels: schedule_task has no apply-all card chrome, so the session
// registry never holds a policy for it and Stage never returns a sentinel here.
// A pre-approval would be meaningless anyway — the work runs handler-side, not in
// the tool's (error-only) Run.
func (o *orchestrationState) checkScheduleTaskSafety(toolName, toolCallID, rawInput string) (bool, string) {
	if toolName != toolNameScheduleTask {
		return false, ""
	}
	if hasUnresolvedToolPlaceholder(rawInput) {
		return true, "schedule_task argument contains an unresolved ${tool:…} placeholder. The agent runtime does NOT substitute that syntax; paste the actual value into the tool arguments instead."
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.approvalSink == nil {
		return true, "SCHEDULE_TASK_UNAVAILABLE: creating scheduled tasks from chat requires an approval-enabled interactive session. Do NOT retry — tell the user to create the task from the Operations Center instead."
	}
	id, err := o.approvalSink.Stage(toolName, toolCallID, rawInput)
	if err != nil {
		log.Printf("approval stage failed (schedule_task): %v", err)
		return true, fmt.Sprintf("APPROVAL_REQUIRED: could not stage schedule_task for user approval (%v). Ask the user what to do.", err)
	}
	return true, fmt.Sprintf("APPROVAL_REQUIRED: the scheduled task has been staged for explicit user approval (approval_id=%s). Do NOT retry. Summarize the task you would create (name, what it does, when it runs) and wait for the user to click Approve.", id)
}

// checkManageTasksSafety intercepts manage_tasks (#1152), the sibling of
// schedule_task for tasks that already exist. Same shape and same reason: the
// tool has no execution path of its own, and the card the human reads IS the
// safety mechanism — one call can rewrite the schedule on a dozen jobs or stop
// a recurring one for good, and neither is something to discover afterwards.
func (o *orchestrationState) checkManageTasksSafety(toolName, toolCallID, rawInput string) (bool, string) {
	if toolName != toolNameManageTasks {
		return false, ""
	}
	if hasUnresolvedToolPlaceholder(rawInput) {
		return true, "manage_tasks argument contains an unresolved ${tool:…} placeholder. The agent runtime does NOT substitute that syntax; paste the actual value into the tool arguments instead."
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.approvalSink == nil {
		return true, "MANAGE_TASKS_UNAVAILABLE: changing scheduled tasks from chat requires an approval-enabled interactive session. Do NOT retry — tell the user to edit the task in the Operations Center instead."
	}
	id, err := o.approvalSink.Stage(toolName, toolCallID, rawInput)
	if err != nil {
		log.Printf("approval stage failed (manage_tasks): %v", err)
		return true, fmt.Sprintf("APPROVAL_REQUIRED: could not stage manage_tasks for user approval (%v). Ask the user what to do.", err)
	}
	return true, fmt.Sprintf("APPROVAL_REQUIRED: the change has been staged for explicit user approval (approval_id=%s). Do NOT retry. Say plainly which tasks you are about to change and what changes, then wait for the user to click Approve.", id)
}

// checkSuggestAdvancedSafety intercepts suggest_advanced_model — the staged
// approval card IS the feature (mirrors preview_email). The stager owns the
// per-conversation gate (already-on-advanced, prior-approved, cooldown).
func (o *orchestrationState) checkSuggestAdvancedSafety(toolName, rawInput string) (bool, string) {
	if toolName != toolNameSuggestAdvancedModel {
		return false, ""
	}
	var args struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(rawInput), &args); err != nil {
		return true, "suggest_advanced_model: could not parse arguments. Pass {\"reason\": \"<one-line user-facing rationale>\"}."
	}
	args.Reason = strings.TrimSpace(args.Reason)
	if args.Reason == "" {
		return true, "suggest_advanced_model: reason is required and must be non-empty. Pass a one-line user-facing rationale."
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.approvalSink == nil {
		return true, "SUGGESTION_UNAVAILABLE: model-switch suggestions are not surfaced on this transport. Do NOT retry — proceed with the current model."
	}
	id, msg, err := o.approvalSink.StageSuggestion(args.Reason)
	if err != nil {
		log.Printf("suggestion stage failed: %v", err)
		return true, fmt.Sprintf("SUGGESTION_FAILED: could not stage suggestion (%v).", err)
	}
	// id == "" means the gate suppressed the suggestion; msg explains why.
	_ = id
	return true, msg
}

// classifyRiskyBash returns (risky, reason) for a bash tool input. Reason is
// shown to the user in the approval card. Ported verbatim from chat's
// orchestration.go.
func classifyRiskyBash(rawInput string) (bool, string) {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(rawInput), &args); err != nil {
		return false, ""
	}
	c := strings.ToLower(args.Command)

	if strings.Contains(c, "git push") {
		return true, "git push to a remote"
	}

	pkgOps := []string{
		"dnf install", "dnf remove", "dnf erase", "dnf update", "dnf upgrade",
		"dnf autoremove", "dnf downgrade", "dnf reinstall",
		"yum install", "yum remove", "yum update", "yum upgrade",
		"apt install", "apt remove", "apt upgrade", "apt full-upgrade",
		"apt-get install", "apt-get remove", "apt-get upgrade", "apt-get dist-upgrade",
		"pacman -s", "pacman -r", "pacman -u",
		"zypper install", "zypper remove", "zypper update",
		"snap install", "snap remove",
		"flatpak install", "flatpak uninstall",
	}
	for _, op := range pkgOps {
		if strings.Contains(c, op) {
			return true, "system package-manager action (" + op + ")"
		}
	}
	return false, ""
}
