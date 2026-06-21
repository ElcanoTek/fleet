package agentcore

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"

	"charm.land/fantasy"
)

// Scheduled-mode audit gating + critical-action machinery (lifted from cutlass
// orchestration.go + the confirm_audit tool in fantasy.go).
//
// Critical tools (send_email, deal-creation across SSPs, presentation
// generation) are blocked until a confirm_audit passes. Batch audits register
// per-suffix commitment counts so a single audit can cover N deals; the audit
// token consumes only when every committed action is discharged. This whole
// subsystem is inert in interactive mode (no critical tools registered, the
// Policy's CanFinish returns true on round 1 so checkFinishEnforcement is
// never consulted).

const toolNameConfirmAudit = "confirm_audit"
const toolNameTaskTracker = "task_tracker"

// sendEmailToolSuffix is the bare tool-name suffix shared by every outbound
// email tool.
const sendEmailToolSuffix = "send_email"

// criticalActionsBeingUnblockedField is the legacy free-text confirm_audit key.
const criticalActionsBeingUnblockedField = "critical_actions_being_unblocked"

// criticalToolSuffixes are the bare tool names that require audit before
// execution. Matched by suffix so mcp_sendgrid_send_email matches "send_email".
// Longer suffixes first so longest-match-wins pins, e.g.,
// "execute_deal_from_prompt_inputs" over "create_deal".
var criticalToolSuffixes = []string{
	sendEmailToolSuffix,
	"send_template_email",
	"generate_presentation",
	"generate_wrap_up_presentation",
	"generate_standard_presentation",
	"generate_and_wait_for_presentation",
	"generate_and_wait_for_wrap_up_presentation",
	"generate_and_wait_for_standard_presentation",
	"execute_deal_from_prompt_inputs",
	"create_marketplace_deal",
	"create_curated_deal",
	"create_xandr_deal",
	"create_prepared_deal",
	"create_deal",
}

// criticalToolSubstitutes maps a committed-tool suffix to the substitute
// suffixes that may discharge its commitment (the protocol-approved fallback
// from a high-level execute_* to the SSP's lower-level create_*).
var criticalToolSubstitutes = map[string][]string{
	"execute_deal_from_prompt_inputs": {
		"create_marketplace_deal",
		"create_curated_deal",
		"create_prepared_deal",
		"create_xandr_deal",
		"create_deal",
	},
}

func substituteSatisfies(committedSuffix, executedSuffix string) bool {
	if committedSuffix == "" || executedSuffix == "" {
		return false
	}
	for _, allowed := range criticalToolSubstitutes[committedSuffix] {
		if allowed == executedSuffix {
			return true
		}
	}
	return false
}

func isCriticalTool(toolName string) bool {
	for _, suffix := range criticalToolSuffixes {
		if toolName == suffix || strings.HasSuffix(toolName, "_"+suffix) {
			return true
		}
	}
	return false
}

func isEmailTool(toolName string) bool {
	return toolName == sendEmailToolSuffix || strings.HasSuffix(toolName, "_"+sendEmailToolSuffix)
}

// maxAttemptsPerCriticalAction caps how many times a (toolName, argsHash) can be
// invoked under a single audit envelope.
const maxAttemptsPerCriticalAction = 2

func matchCriticalSuffix(declared string) string {
	needle := strings.ToLower(declared)
	best := ""
	for _, suffix := range criticalToolSuffixes {
		if !strings.Contains(needle, suffix) {
			continue
		}
		if len(suffix) > len(best) {
			best = suffix
		}
	}
	return best
}

// registerCommittedActions records the critical suffixes declared in a
// successful confirm_audit, incrementing per-suffix commitment counts. Callers
// must hold o.mu (or call before concurrent access begins, as the tests do).
func (o *orchestrationState) registerCommittedActions(declared []string) {
	if o.committedCriticalActions == nil {
		o.committedCriticalActions = make(map[string]int)
	}
	registered := 0
	for _, decl := range declared {
		if suffix := matchCriticalSuffix(decl); suffix != "" {
			o.committedCriticalActions[suffix]++
			log.Printf("Enforcement: registered committed critical action %q (from %q); %d outstanding",
				suffix, decl, o.committedCriticalActions[suffix])
			registered++
		}
	}
	if len(declared) > 0 && registered == 0 {
		log.Printf("WARNING: confirm_audit supplied %d critical-action declaration(s) "+
			"but NONE matched a known critical-tool suffix. The audit token will "+
			"consume on the first critical execution and any trailing critical call "+
			"will be blocked. Likely cause: paraphrased declarations instead of "+
			"literal tool names. Use the typed `critical_actions` field with the "+
			"exact tool name (e.g. \"mcp_openx_mcp_ox_create_prepared_deal\"). "+
			"See protocols/self-audit.md.", len(declared))
	}
}

// markCommittedExecuted decrements the commitment count for the first suffix
// that matches toolName (direct match first, then allowed substitute).
func (o *orchestrationState) markCommittedExecuted(toolName string) {
	for suffix, remaining := range o.committedCriticalActions {
		if remaining <= 0 {
			continue
		}
		if toolName == suffix || strings.HasSuffix(toolName, "_"+suffix) {
			o.committedCriticalActions[suffix] = remaining - 1
			log.Printf("Enforcement: committed %q discharged via %q (%d remaining)",
				suffix, toolName, o.committedCriticalActions[suffix])
			return
		}
	}
	executedSuffix := ""
	for _, suffix := range criticalToolSuffixes {
		if toolName == suffix || strings.HasSuffix(toolName, "_"+suffix) {
			executedSuffix = suffix
			break
		}
	}
	if executedSuffix == "" {
		return
	}
	for suffix, remaining := range o.committedCriticalActions {
		if remaining <= 0 {
			continue
		}
		if substituteSatisfies(suffix, executedSuffix) {
			o.committedCriticalActions[suffix] = remaining - 1
			log.Printf("Enforcement: committed %q discharged via substitute %q (%d remaining)",
				suffix, toolName, o.committedCriticalActions[suffix])
			return
		}
	}
}

func (o *orchestrationState) unexecutedCommitments() []string {
	var missing []string
	for suffix, remaining := range o.committedCriticalActions {
		for i := 0; i < remaining; i++ {
			missing = append(missing, suffix)
		}
	}
	sort.Strings(missing)
	return missing
}

func (o *orchestrationState) allCommitmentsExhausted() bool {
	for _, remaining := range o.committedCriticalActions {
		if remaining > 0 {
			return false
		}
	}
	return true
}

func retryBudgetKey(toolName, argsHash string) string {
	return toolName + ":" + argsHash
}

// checkCriticalTool checks audit gating + email safety before a critical tool
// executes (scheduled). Returns (blocked, response).
func (o *orchestrationState) checkCriticalTool(toolName, _ string, rawInput string) (bool, string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if isEmailTool(toolName) && o.sendEmailSuccessCount >= maxSendEmailCallsPerTask {
		log.Printf("Enforcement: Blocking send_email — limit reached (%d/%d)", o.sendEmailSuccessCount, maxSendEmailCallsPerTask)
		return true, fmt.Sprintf("Safety Limit: send_email already executed %d times. Further calls blocked.", maxSendEmailCallsPerTask)
	}

	if isEmailTool(toolName) {
		fp := emailDedupKey(rawInput)
		if _, dup := o.sentEmailFingerprints[fp]; dup {
			return true, "Safety Guard: Duplicate send_email blocked. An identical payload was already sent."
		}
	}

	if isCriticalTool(toolName) {
		key := retryBudgetKey(toolName, hashString(rawInput))
		if attempts := o.criticalToolFailureAttempts[key]; attempts >= maxAttemptsPerCriticalAction {
			log.Printf("Enforcement: Blocking %s — retry budget exhausted (%d/%d failed attempts with identical args)",
				toolName, attempts, maxAttemptsPerCriticalAction)
			return true, fmt.Sprintf("Safety Limit: '%s' has failed %d times with identical args (cap: %d). "+
				"Further retries with the same args are blocked. Either change the args (e.g. fix the failing field) "+
				"or call confirm_audit(success=false, user_visible_summary=...) to abort the task.",
				toolName, attempts, maxAttemptsPerCriticalAction)
		}
	}

	if isCriticalTool(toolName) && !o.auditConfirmed {
		log.Printf("Enforcement: Blocking %s — audit not confirmed", toolName)
		argsHash := hashString(rawInput)
		alreadyPending := false
		for _, p := range o.pendingCriticalActions {
			if p.toolName == toolName && p.argsHash == argsHash {
				alreadyPending = true
				break
			}
		}
		if !alreadyPending {
			o.pendingCriticalActions = append(o.pendingCriticalActions, pendingCriticalAction{
				toolName: toolName,
				argsHash: argsHash,
			})
		}
		return true, fmt.Sprintf("BLOCKED: '%s' requires audit first. "+
			"Read protocols/self-audit.md, call confirm_audit(...), then retry '%s'.",
			toolName, toolName)
	}

	return false, ""
}

// checkFinishEnforcement checks whether the agent is allowed to stop
// (scheduled). The interactive Policy bypasses this entirely via CanFinish.
func (o *orchestrationState) checkFinishEnforcement() (bool, []string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if !o.selfAuditRequested {
		o.selfAuditRequested = true
		log.Println("Enforcement: Self Audit not requested. Rejecting finish.")
		return false, []string{"Before finishing: read protocols/self-audit.md, verify your work, then call confirm_audit(...)."}
	}

	if !o.selfAuditConfirmedOnce {
		log.Println("Enforcement: Self Audit not confirmed. Rejecting finish.")
		return false, []string{"Audit not confirmed. Call confirm_audit(...) with evidence to proceed."}
	}

	if o.auditTerminalFailure {
		log.Println("Task ended with terminal audit failure")
		return true, nil
	}

	if o.latestTaskTracker.Seen && (o.latestTaskTracker.Todo > 0 || o.latestTaskTracker.InProgress > 0) {
		log.Println("Enforcement: Task tracker has pending work. Rejecting finish.")
		return false, []string{fmt.Sprintf("Task tracker: %d todo, %d in progress. Complete or mark done before finishing.",
			o.latestTaskTracker.Todo, o.latestTaskTracker.InProgress)}
	}

	if len(o.pendingCriticalActions) > 0 && o.auditConfirmed {
		var names []string
		for _, p := range o.pendingCriticalActions {
			names = append(names, p.toolName)
		}
		log.Printf("Enforcement: %d pending critical action(s). Rejecting finish.", len(o.pendingCriticalActions))
		return false, []string{fmt.Sprintf("Audit passed. Execute pending action(s): %v. Then finish.", names)}
	}

	if missing := o.unexecutedCommitments(); len(missing) > 0 {
		log.Printf("Enforcement: %d committed critical action(s) not yet executed: %v. Rejecting finish.", len(missing), missing)
		return false, []string{fmt.Sprintf(
			"You declared %v in your audit's critical_actions_being_unblocked but have not successfully executed them. "+
				"Execute each declared action now, or call confirm_audit(success=false, user_visible_summary=...) to abort explicitly.",
			missing)}
	}

	return true, nil
}

// ── confirm_audit tool ──

type criticalActionStruct struct {
	Tool       string `json:"tool" description:"Literal MCP tool name being unblocked, e.g. \"mcp_openx_mcp_ox_create_prepared_deal\". Copy verbatim from the tool list — substring matching against the orchestration's known suffixes will fail on paraphrased names."`
	Identifier string `json:"identifier,omitempty" description:"Optional human-readable tag distinguishing this action (deal name, recipient address, etc.). Used only for audit logging — not for matching."`
}

type confirmAuditInput struct {
	Success                       bool                   `json:"success" description:"Whether the audit passed successfully."`
	Reasoning                     string                 `json:"reasoning" description:"Brief conclusion summarizing what was checked."`
	ArtifactsChecked              []string               `json:"artifacts_checked" description:"Artifact paths reviewed during audit."`
	WorkflowSectionsChecked       []string               `json:"workflow_sections_checked" description:"Workflow contract sections checked."`
	CriticalActions               []criticalActionStruct `json:"critical_actions,omitempty" description:"Preferred typed list of {tool, identifier} entries naming each MCP tool this audit unlocks."`
	CriticalActionsBeingUnblocked []string               `json:"critical_actions_being_unblocked,omitempty" description:"Legacy free-text form (deprecated): each entry MUST contain the literal tool name so the substring matcher can extract a known suffix."`
	SendContractChecked           bool                   `json:"send_contract_checked" description:"Whether the send/delivery contract was checked."`
	AttachmentsChecked            []string               `json:"attachments_checked" description:"Attachment paths checked."`
	RemainingRisks                []string               `json:"remaining_risks" description:"Remaining known risks."`
	UserVisibleSummary            string                 `json:"user_visible_summary,omitempty" description:"When success=false, a concise final summary."`
}

// criticalActionToolNames returns the literal tool names declared in the audit,
// preferring the typed CriticalActions field.
func criticalActionToolNames(input confirmAuditInput) []string {
	if len(input.CriticalActions) > 0 {
		out := make([]string, 0, len(input.CriticalActions))
		for _, a := range input.CriticalActions {
			if a.Tool == "" {
				continue
			}
			out = append(out, a.Tool)
		}
		return out
	}
	return input.CriticalActionsBeingUnblocked
}

func buildConfirmAuditTool(orch *orchestrationState) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		toolNameConfirmAudit,
		"Confirms that the self-audit protocol has been completed. Provide structured evidence to unlock critical tools.",
		func(_ context.Context, input confirmAuditInput, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			orch.mu.Lock()
			defer orch.mu.Unlock()

			log.Printf("Tool: %s (ID: %s)", toolNameConfirmAudit, call.ID)

			argsJSON, _ := json.Marshal(input)
			var args map[string]any
			_ = json.Unmarshal(argsJSON, &args)

			if err := validateConfirmAuditArgs(args); err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Audit Rejected. %v", err)), nil
			}

			if input.Success {
				orch.selfAuditRequested = true
				fp := fingerprintConfirmAuditArgs(args)
				if fp != "" && fp == orch.lastSuccessfulAuditFP && len(orch.pendingCriticalActions) == 0 {
					return fantasy.NewTextResponse("Audit already confirmed. Finish now without further tool calls."), nil
				}

				orch.auditTerminalFailure = false
				orch.selfAuditConfirmedOnce = true
				orch.auditConfirmed = true
				orch.lastSuccessfulAuditFP = fp
				orch.registerCommittedActions(criticalActionToolNames(input))
				evidence := summarizeConfirmAuditEvidence(args)
				numPending := len(orch.pendingCriticalActions)
				numCompleted := len(orch.completedCriticalActions)

				if numPending > 0 {
					var names []string
					for _, p := range orch.pendingCriticalActions {
						names = append(names, p.toolName)
					}
					return fantasy.NewTextResponse(fmt.Sprintf("Audit Confirmed: \"%s\".\n%s\n"+
						"Pending: %d, completed: %d. Retry blocked actions: %v.",
						input.Reasoning, evidence, numPending, numCompleted, names)), nil
				}
				return fantasy.NewTextResponse(fmt.Sprintf("Audit Confirmed: \"%s\".\n%s\n"+
					"All %d critical actions executed. Finish now.",
					input.Reasoning, evidence, numCompleted)), nil
			}

			orch.selfAuditRequested = true
			orch.selfAuditConfirmedOnce = true
			orch.auditConfirmed = false
			orch.auditTerminalFailure = true
			evidence := summarizeConfirmAuditEvidence(args)
			return fantasy.NewTextResponse(fmt.Sprintf("Audit Failed Terminally.\n%s\nSummary: %s",
				evidence, strings.TrimSpace(input.UserVisibleSummary))), nil
		},
	)
}

// ── confirm_audit validation / evidence / fingerprint ──

func stringSliceArg(args map[string]interface{}, key string) []string {
	raw, ok := args[key]
	if !ok {
		return nil
	}
	items, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		text := strings.TrimSpace(fmt.Sprint(item))
		if text == "" || text == nilStringValue {
			continue
		}
		result = append(result, text)
	}
	return result
}

func criticalActionToolsArg(args map[string]interface{}, key string) []string {
	raw, ok := args[key]
	if !ok {
		return nil
	}
	items, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		obj, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		tool := strings.TrimSpace(fmt.Sprint(obj["tool"]))
		if tool == "" || tool == nilStringValue {
			continue
		}
		result = append(result, tool)
	}
	return result
}

func validateConfirmAuditArgs(args map[string]interface{}) error {
	success, _ := args["success"].(bool)
	reasoning := strings.TrimSpace(fmt.Sprint(args["reasoning"]))
	if reasoning == "" || reasoning == nilStringValue {
		return fmt.Errorf("confirm_audit requires non-empty reasoning")
	}
	artifactsChecked := stringSliceArg(args, "artifacts_checked")
	workflowSections := stringSliceArg(args, "workflow_sections_checked")
	legacyCriticalActions := stringSliceArg(args, criticalActionsBeingUnblockedField)
	structuredCriticalActions := criticalActionToolsArg(args, "critical_actions")
	attachmentsChecked := stringSliceArg(args, "attachments_checked")
	remainingRisks := stringSliceArg(args, "remaining_risks")
	_, sendContractPresent := args["send_contract_checked"]

	if len(artifactsChecked) == 0 {
		return fmt.Errorf("confirm_audit requires artifacts_checked with at least one exact artifact path or identifier")
	}
	if len(workflowSections) == 0 {
		return fmt.Errorf("confirm_audit requires workflow_sections_checked with exact workflow contract sections")
	}
	if len(legacyCriticalActions) == 0 && len(structuredCriticalActions) == 0 {
		return fmt.Errorf("confirm_audit requires critical_actions (preferred typed list) or critical_actions_being_unblocked (legacy free-text) with at least one action")
	}
	if !sendContractPresent {
		return fmt.Errorf("confirm_audit requires send_contract_checked")
	}
	if attachmentsChecked == nil {
		return fmt.Errorf("confirm_audit requires attachments_checked (use [] when none are required)")
	}
	if remainingRisks == nil {
		return fmt.Errorf("confirm_audit requires remaining_risks (use [] when none remain)")
	}
	if !success {
		summary := strings.TrimSpace(fmt.Sprint(args["user_visible_summary"]))
		if summary == "" || summary == nilStringValue {
			return fmt.Errorf("confirm_audit with success=false requires user_visible_summary")
		}
	}
	return nil
}

func summarizeConfirmAuditEvidence(args map[string]interface{}) string {
	reasoning := strings.TrimSpace(fmt.Sprint(args["reasoning"]))
	artifactsChecked := stringSliceArg(args, "artifacts_checked")
	workflowSections := stringSliceArg(args, "workflow_sections_checked")
	legacyCritical := stringSliceArg(args, criticalActionsBeingUnblockedField)
	structuredCritical := criticalActionToolsArg(args, "critical_actions")
	criticalActions := structuredCritical
	if len(criticalActions) == 0 {
		criticalActions = legacyCritical
	}
	attachmentsChecked := stringSliceArg(args, "attachments_checked")
	remainingRisks := stringSliceArg(args, "remaining_risks")
	sendContractChecked, _ := args["send_contract_checked"].(bool)

	criticalLabel := criticalActionsBeingUnblockedField
	if len(structuredCritical) > 0 {
		criticalLabel = "critical_actions"
	}
	lines := []string{
		"Audit Evidence:",
		"- reasoning: " + reasoning,
		"- artifacts_checked: " + strings.Join(artifactsChecked, ", "),
		"- workflow_sections_checked: " + strings.Join(workflowSections, ", "),
		"- " + criticalLabel + ": " + strings.Join(criticalActions, ", "),
		fmt.Sprintf("- send_contract_checked: %t", sendContractChecked),
	}
	if len(attachmentsChecked) == 0 {
		lines = append(lines, "- attachments_checked: []")
	} else {
		lines = append(lines, "- attachments_checked: "+strings.Join(attachmentsChecked, ", "))
	}
	if len(remainingRisks) == 0 {
		lines = append(lines, "- remaining_risks: []")
	} else {
		lines = append(lines, "- remaining_risks: "+strings.Join(remainingRisks, ", "))
	}
	return strings.Join(lines, "\n")
}

type confirmAuditFingerprint struct {
	Success                 bool     `json:"success"`
	ArtifactsChecked        []string `json:"artifacts_checked"`
	WorkflowSectionsChecked []string `json:"workflow_sections_checked"`
	CriticalActions         []string `json:"critical_actions_being_unblocked"`
	SendContractChecked     bool     `json:"send_contract_checked"`
	AttachmentsChecked      []string `json:"attachments_checked"`
	RemainingRisks          []string `json:"remaining_risks"`
}

func fingerprintConfirmAuditArgs(args map[string]interface{}) string {
	criticalActions := criticalActionToolsArg(args, "critical_actions")
	if len(criticalActions) == 0 {
		criticalActions = stringSliceArg(args, criticalActionsBeingUnblockedField)
	}
	fingerprint := confirmAuditFingerprint{
		Success:                 toolsBool(args["success"]),
		ArtifactsChecked:        stringSliceArg(args, "artifacts_checked"),
		WorkflowSectionsChecked: stringSliceArg(args, "workflow_sections_checked"),
		CriticalActions:         criticalActions,
		SendContractChecked:     toolsBool(args["send_contract_checked"]),
		AttachmentsChecked:      stringSliceArg(args, "attachments_checked"),
		RemainingRisks:          stringSliceArg(args, "remaining_risks"),
	}
	data, err := json.Marshal(fingerprint)
	if err != nil {
		return ""
	}
	return string(data)
}
