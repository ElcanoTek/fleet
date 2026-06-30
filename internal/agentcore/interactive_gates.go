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
)

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
