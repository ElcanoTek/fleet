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
// generation) are blocked until a confirm_audit passes. A TYPED audit (the
// structured critical_actions field) binds each approval to the full
// server-qualified tool name plus optional record ids / values digest, and
// fails closed when it resolves to zero commitments — see audit_commitment.go
// (#715). An UNTYPED audit falls back to the legacy per-suffix commitment
// counts below, so a single audit can still cover N same-suffix actions; the
// audit token consumes only when every committed action is discharged. This
// whole subsystem is inert in interactive mode (no critical tools registered,
// the Policy's CanFinish returns true on round 1 so checkFinishEnforcement is
// never consulted).

const toolNameConfirmAudit = "confirm_audit"
const toolNameTaskTracker = "task_tracker"

// sendEmailToolSuffix is the bare tool-name suffix shared by every outbound
// email tool.
const sendEmailToolSuffix = "send_email"

// criticalActionsBeingUnblockedField is the legacy free-text confirm_audit key.
const criticalActionsBeingUnblockedField = "critical_actions_being_unblocked"

// The critical tool-name suffixes and the substitute map are NOT hardcoded
// here: they come from the client bundle's AgentPolicy (see agent_policy.go,
// ConfigureAgentPolicy). The base generic suffixes (send_email,
// send_template_email) are always present even with no bundle. Matched by suffix
// so mcp_sendgrid_send_email matches "send_email"; matchCriticalSuffix picks the
// longest match so e.g. "execute_deal_from_prompt_inputs" pins over
// "create_deal" regardless of slice order.

func substituteSatisfies(committedSuffix, executedSuffix string) bool {
	if committedSuffix == "" || executedSuffix == "" {
		return false
	}
	policyMu.RLock()
	allowedList := activeCriticalSubstitutes[committedSuffix]
	policyMu.RUnlock()
	for _, allowed := range allowedList {
		if allowed == executedSuffix {
			return true
		}
	}
	return false
}

func isCriticalTool(toolName string) bool {
	policyMu.RLock()
	defer policyMu.RUnlock()
	for _, suffix := range activeCriticalSuffixes {
		if toolName == suffix || strings.HasSuffix(toolName, "_"+suffix) {
			return true
		}
	}
	return false
}

func isEmailTool(toolName string) bool {
	return toolName == sendEmailToolSuffix || strings.HasSuffix(toolName, "_"+sendEmailToolSuffix)
}

// maxAttemptsPerCriticalAction caps how many FAILED attempts a (toolName,
// argsHash) pair may accumulate under a single audit envelope before further
// retries with those identical args are blocked (checkCriticalTool). It is NOT
// an invocation cap: only unsuccessful executions count
// (criticalToolFailureAttempts increments in recordToolResult's failure branch
// only) and a success DELETES the pair's counter, so a critical action that
// succeeds is not throttled by this constant. Repeated successful invocations
// are bounded elsewhere — the audit-token consumption/commitment accounting
// (registerCommittedActions/markCommittedExecuted) and, for email, the
// maxSendEmailCallsPerTask cap + duplicate-payload fingerprint guard.
const maxAttemptsPerCriticalAction = 2

// DuplicateSendSuppressedPrefix opens the response an identical,
// already-successful send_email retry receives. Exported because the end-of-run
// verifier's result classifier must recognize it as a satisfied action, not a
// failed one: every other guard's block means the action has NOT happened,
// while this one fires precisely because it HAS.
const DuplicateSendSuppressedPrefix = "Duplicate send_email suppressed:"

func matchCriticalSuffix(declared string) string {
	needle := strings.ToLower(declared)
	best := ""
	policyMu.RLock()
	defer policyMu.RUnlock()
	for _, suffix := range activeCriticalSuffixes {
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
// successful UNTYPED confirm_audit (the legacy free-text fallback — typed
// declarations go through registerCommittedActionsTyped in
// audit_commitment.go), incrementing per-suffix commitment counts. Callers
// must hold o.mu (or call before concurrent access begins, as the tests do).
func (o *orchestrationState) registerCommittedActions(declared []string) {
	if o.committedCriticalActions == nil {
		o.committedCriticalActions = make(map[string]int)
	}
	// A fresh audit envelope REPLACES all batch-approval state (see
	// registerCommittedActionsTyped). The legacy free-text path can never
	// register deal_ids, so any surviving approvedDealIDs/approvedDigest would
	// be stale prior-envelope state — clear it fail-closed. But do so ONLY
	// when this audit actually registers something (lazy, on the first
	// matched suffix): a legacy audit whose free text matches ZERO suffixes
	// leaves auditConfirmed=true and any prior commitment outstanding, so
	// wiping the approvals would false-block the legitimately-approved
	// in-flight batch. A zero-match audit must stay a no-op for batch
	// approvals.
	registered := 0
	for _, decl := range declared {
		if suffix := matchCriticalSuffix(decl); suffix != "" {
			if registered == 0 {
				o.resetBatchApprovals()
			}
			// Fresh audit envelope for this suffix → clear any per-record
			// discharge ledger left over from a prior batch on the same suffix.
			delete(o.dischargedDeals, suffix)
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
			"exact tool name (e.g. \"mcp_myserver_create_record\"). "+
			"See protocols/self-audit.md.", len(declared))
	}
}

// markCommittedExecuted discharges one outstanding commitment for a
// successfully executed critical call targeting dealID ("" when the call
// names no record). Safe to call for non-critical tools or for tools whose
// action wasn't pre-committed (no-op in either case).
//
// Match priority (#715):
//  1. Typed commitments (markTypedExecuted): FULL-name matches with
//     record-binding compatibility. Cross-server / cross-variant /
//     cross-record discharge is REFUSED — a call on one server must never
//     discharge another server's commitment that shares its bare suffix.
//  2. Legacy (free-text) suffix commitments, direct match — but only within
//     legacy HEADROOM (aggregate count minus what typed commitments own), so
//     a call that failed typed matching can never eat a typed commitment.
//  3. Allowed substitute via the bundle's critical_tool_substitutes (e.g. a
//     committed high-level execute tool discharged by its documented
//     lower-level create fallback), again within legacy headroom; same-server
//     typed substitutes are handled in pass 1.
func (o *orchestrationState) markCommittedExecuted(toolName, dealID, callDigest string) {
	// Pass 1: typed full-name-bound commitments.
	if o.markTypedExecuted(toolName, dealID, callDigest) {
		return
	}
	executedSuffix := criticalSuffixFor(toolName)
	if executedSuffix == "" {
		return
	}
	// Pass 2: direct legacy suffix match within headroom.
	if o.legacyHeadroomFor(executedSuffix) > 0 {
		o.committedCriticalActions[executedSuffix]--
		log.Printf("Enforcement: committed %q discharged via %q (%d remaining)",
			executedSuffix, toolName, o.committedCriticalActions[executedSuffix])
		return
	}
	// Pass 3: discharge a legacy substitute if the executed tool is an
	// allowed fallback for an outstanding free-text commitment.
	for suffix := range o.committedCriticalActions {
		if substituteSatisfies(suffix, executedSuffix) && o.legacyHeadroomFor(suffix) > 0 {
			o.committedCriticalActions[suffix]--
			log.Printf("Enforcement: committed %q discharged via substitute %q (%d remaining)",
				suffix, toolName, o.committedCriticalActions[suffix])
			return
		}
	}
	if len(o.typedCommitments) > 0 && !o.allCommitmentsExhausted() {
		log.Printf("Enforcement: REFUSED to discharge any commitment via %s (record %q) — no outstanding "+
			"commitment matches this server/variant/record; cross-server discharge is not allowed (#715)",
			toolName, dealID)
	}
}

// retireOutstandingCommitments zeroes every declared-but-unexecuted
// commitment (typed and legacy) and drops blocked calls awaiting retry. Used
// by an explicit abort: the model has said the declared work will not be done,
// so nothing may keep demanding it afterwards. Returns the retired summary for
// the response text. Callers must hold o.mu.
func (o *orchestrationState) retireOutstandingCommitments() []string {
	retired := o.outstandingCommitmentSummary()
	for _, c := range o.typedCommitments {
		c.remaining = 0
	}
	for suffix := range o.committedCriticalActions {
		o.committedCriticalActions[suffix] = 0
	}
	o.pendingCriticalActions = nil
	return retired
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
			// This is the one guard whose block means "already done" rather
			// than "not done": the fingerprint is recorded exclusively on a
			// successful send, so an identical payload here has provably
			// reached the provider. It must therefore discharge the same
			// commitments a successful call would — a blocked call never
			// reaches recordToolResult, and a commitment tracker that keeps
			// demanding an action this guard exists to prevent leaves the run
			// only two exits: abort a task whose work is done, or mutate the
			// content until the fingerprint no longer matches (observed: a
			// body re-rendered 110 bytes larger purely to get past this line).
			o.markPendingCriticalDone(toolName, hashString(rawInput))
			if len(o.pendingCriticalActions) == 0 {
				o.selfAuditRequested = true
			}
			o.markCommittedExecuted(toolName, callDealID(rawInput), valuesDigestArg(rawInput))
			if o.allCommitmentsExhausted() {
				o.auditConfirmed = false
			}
			return true, DuplicateSendSuppressedPrefix + " an identical payload was already sent successfully by this run, so this send is complete and its commitment is discharged. Do NOT send it again and do NOT alter the content to make it look different; report the send as done and move on."
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

	if isCriticalTool(toolName) && o.auditConfirmed {
		// Batch binding (#715): a call carrying deal_ids may only target the
		// records (and, when declared, the exact value-set digest) the audit
		// approved. Legacy audits never register batch approvals, so under
		// them every server-side batch fails closed until a typed re-audit
		// declares its ids.
		if blocked, msg := o.checkBatchBinding(toolName, rawInput); blocked {
			return true, msg
		}
		// Commitment binding (#715): the audit token is NOT a bearer token —
		// every critical call must map to an outstanding commitment. Without
		// this, one successful confirm_audit let a drifted (or
		// prompt-injected) agent immediately execute ANY critical tool — a
		// different MCP server, a different client variant's seat, a
		// different record — while the approved commitment stayed open, or
		// worse, was silently discharged by the wrong-server call's shared
		// bare suffix.
		//
		// The gate engages when a TYPED audit is active (typedAuditActive),
		// OR while any commitment is outstanding. Keying on typedAuditActive
		// — not merely on the ledger being non-empty — is the fail-closed
		// half: a typed audit that resolved to zero commitments (refused up
		// front in confirm_audit) must still find NO outstanding commitment
		// for any call, never leaking the legacy one-shot token. Legacy
		// audits that declared NO critical_actions at all leave
		// typedAuditActive false and keep one-shot semantics: the gate only
		// engages while their declared suffix commitments remain outstanding.
		if o.typedAuditActive || !o.allCommitmentsExhausted() {
			if allowed, msg := o.commitmentAuthorizes(toolName, rawInput); !allowed {
				return true, msg
			}
		}
	}

	return false, ""
}

// checkFinishEnforcement checks whether the agent is allowed to stop
// (scheduled). The interactive Policy bypasses this entirely via CanFinish.
//
// A DELEGATED run (a spawned sub-agent, #1043 follow-up) skips the two
// self-audit blocks below and nothing else: the self-audit ritual is the
// TOP-LEVEL run's deliverable gate — it re-reads protocols/self-audit.md and
// re-checks the task's contract, neither of which a scoped child was given.
// Forcing a child through it made every delegation cost several extra rounds
// of "read protocols/self-audit.md" flailing (a child usually cannot even see
// that file), polluted the answer the parent gets back with audit narration,
// and — when the child ran out of iterations mid-ritual — returned no answer
// at all. The child's real gate is its PARENT's audit: the parent still has to
// confirm_audit over the delegated work before IT can finish. Every other
// enforcement below (task tracker, pending critical actions, undischarged
// commitments) still applies to a child unchanged, so a child that DID unlock
// a critical action through confirm_audit is held to it exactly like a root
// run.
// auditVerdict reports what the run's own self-audit concluded, for the driver
// to carry into the task record. Before #1151 this state died inside the run:
// checkFinishEnforcement returned (true, nil) on a terminal audit failure — the
// agent is ALLOWED to finish after declaring one — and nothing downstream ever
// learned it happened, so a run that printed ABORTED_WITH_FLAGS and called
// confirm_audit(success=false) landed as status: success.
//
// executedCritical counts critical tools the run actually ran. Zero means the
// run touched nothing outside itself, which for a daily refresh is a real,
// healthy, and repeatable outcome — and the one an operator most needs to see
// repeated, because N of them in a row means the upstream is dead.
func (o *orchestrationState) auditVerdict() (aborted bool, summary string, executedCritical int) {
	if o == nil {
		return false, "", 0
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.auditTerminalFailure, o.auditSummary, len(o.completedCriticalActions)
}

func (o *orchestrationState) checkFinishEnforcement() (bool, []string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if !o.delegatedFinish {
		if !o.selfAuditRequested {
			o.selfAuditRequested = true
			log.Println("Enforcement: Self Audit not requested. Rejecting finish.")
			// The audit wording (#990, borrowed from Prime Agent's goal
			// completion audit) names the two rationalizations unattended runs
			// actually fail on: declaring done on intent, and declaring done on
			// a plausible-sounding final answer nothing verified.
			return false, []string{"Before finishing: read protocols/self-audit.md and audit the current state against every requirement of the original task — do not treat intent, partial progress, or a plausible final answer as proof of completion. Then call confirm_audit(...)."}
		}

		if !o.selfAuditConfirmedOnce {
			log.Println("Enforcement: Self Audit not confirmed. Rejecting finish.")
			return false, []string{"Audit not confirmed. Call confirm_audit(...) with evidence to proceed."}
		}
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

// criticalActionStruct is a typed entry in confirm_audit's preferred
// structured `critical_actions` field. Each entry names a literal MCP tool
// the audit unlocks, optionally bound to specific record id(s) and a
// value-set digest. The JSON field names (deal_id / deal_ids / values_digest)
// are the wire contract the client bundles' protocols emit — keep them
// verbatim even though fleet treats the values as opaque record identifiers.
type criticalActionStruct struct {
	Tool         string   `json:"tool" description:"Literal MCP tool name being unblocked, e.g. \"mcp_myserver_create_record\". Copy verbatim from the tool list — a bare suffix or paraphrased name is refused. Execution is BOUND to this exact name (server and client-variant prefix included): a same-suffix call on a different server or variant is blocked and cannot discharge this commitment."`
	Identifier   string   `json:"identifier,omitempty" description:"Optional human-readable tag distinguishing this action (record name, recipient address, etc.). Used only for audit logging — not for matching; bind a single-record mutation with deal_id instead."`
	DealID       string   `json:"deal_id,omitempty" description:"For a SINGLE-record mutation on one existing record: the exact record id this audit authorizes — the value the call will pass as its record-id argument (deal_id or a sibling key). The orchestration BINDS the unblocked call to it — a same-tool call targeting any other record is blocked and discharges nothing. Omit for creation tools and non-record actions; use deal_ids for server-side batches."`
	DealIDs      []string `json:"deal_ids,omitempty" description:"For a server-side BATCH mutation (a tool call that carries a deal_ids array): the EXACT record ids this audit authorizes, as strings. The orchestration registers one commitment per id and BINDS the batch — a call targeting any unlisted record is blocked. Omit for single-record actions."`
	ValuesDigest string   `json:"values_digest,omitempty" description:"For a by-reference batch mutation: the sha256 of the value file (sha256sum output). When supplied, a batch call whose values_sha256 differs is blocked — proving the audit approved this exact value list."`
}

type confirmAuditInput struct {
	Success                       bool                   `json:"success" description:"Whether the audit passed successfully."`
	Reasoning                     string                 `json:"reasoning" description:"Brief conclusion summarizing what was checked."`
	ArtifactsChecked              []string               `json:"artifacts_checked" description:"Artifact paths reviewed during audit."`
	WorkflowSectionsChecked       []string               `json:"workflow_sections_checked" description:"Workflow contract sections checked."`
	CriticalActions               []criticalActionStruct `json:"critical_actions,omitempty" description:"Preferred typed list of {tool, identifier} entries naming each MCP tool this audit unlocks. Required when success=true; optional on an abort (success=false), which unlocks nothing."`
	CriticalActionsBeingUnblocked []string               `json:"critical_actions_being_unblocked,omitempty" description:"Legacy free-text form (deprecated): each entry MUST contain the literal tool name so the substring matcher can extract a known suffix."`
	SendContractChecked           bool                   `json:"send_contract_checked" description:"Whether the send/delivery contract was checked."`
	AttachmentsChecked            []string               `json:"attachments_checked" description:"Attachment paths checked."`
	RemainingRisks                []string               `json:"remaining_risks" description:"Remaining known risks."`
	UserVisibleSummary            string                 `json:"user_visible_summary,omitempty" description:"When success=false, a concise final summary. Abort ONLY while declared critical work is still undone: once every declared action has executed, an abort is refused — finish and put reservations in your final summary instead."`
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

				// Register commitments BEFORE granting the token so a
				// malformed/injected typed declaration can fail the audit
				// closed (#715). A typed audit that resolves to ZERO
				// commitments (e.g. only paraphrases, or bare suffixes dropped
				// fail-closed) must NOT grant the one-shot token — doing so
				// would authorize one arbitrary critical mutation on any
				// server/record. registerCommittedActionsTyped adds nothing
				// when it returns 0, so returning here leaves state ungranted.
				// An UNTYPED audit keeps the legacy suffix-scoped fallback.
				typedProvided := len(input.CriticalActions) > 0
				if typedProvided {
					if registered := orch.registerCommittedActionsTyped(input.CriticalActions); registered == 0 {
						// Distinguish a MALFORMED critical declaration from an
						// explicit no-op. An entry whose text names (or
						// paraphrases) a known critical suffix meant to unlock
						// a real tool but failed the full-name requirement —
						// refuse it so the agent fixes the name rather than
						// believing the action is unlocked. An entry with no
						// critical-tool reference at all ("none" — the shape
						// the schema forces on tasks with no critical work)
						// declares that nothing needs unlocking: accept the
						// audit for completion, and let the EMPTY typed gate
						// below make it authorize nothing (fail closed).
						for _, a := range input.CriticalActions {
							if matchCriticalSuffix(a.Tool) == "" {
								continue
							}
							return fantasy.NewTextErrorResponse(
								"Audit Rejected: the typed critical_actions named no full server-qualified critical MCP " +
									"tool. Each entry's `tool` MUST be the literal tool name copied verbatim (e.g. " +
									"\"mcp_myserver_create_record\") — a bare suffix or paraphrase is refused so the " +
									"audit cannot silently unlock an unbound mutation. Fix the tool names and re-run " +
									"confirm_audit. See protocols/self-audit.md."), nil
						}
					}
				} else {
					orch.registerCommittedActions(input.CriticalActionsBeingUnblocked)
				}
				orch.auditTerminalFailure = false
				orch.auditSummary = ""
				orch.selfAuditConfirmedOnce = true
				orch.auditConfirmed = true
				orch.typedAuditActive = typedProvided
				orch.lastSuccessfulAuditFP = fp
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
				// The trailer must describe the ledger, not assume it: a fresh
				// audit that just DECLARED work used to end "All 0 critical
				// actions executed. Finish now." — telling the model to finish
				// before it had made the write it had just been authorized for.
				if outstanding := orch.outstandingCommitmentSummary(); len(outstanding) > 0 {
					return fantasy.NewTextResponse(fmt.Sprintf("Audit Confirmed: \"%s\".\n%s\n"+
						"Declared and not yet executed: %s. Execute exactly those call(s) now — the same tool "+
						"name(s) and record(s) as declared — then finish.",
						input.Reasoning, evidence, strings.Join(outstanding, ", "))), nil
				}
				return fantasy.NewTextResponse(fmt.Sprintf("Audit Confirmed: \"%s\".\n%s\n"+
					"All %d critical actions executed. Finish now.",
					input.Reasoning, evidence, orch.criticalExecutedCount)), nil
			}

			// An abort after the declared critical work has ALL executed is a
			// contradiction, not a failure: the write/send already happened and
			// aborting cannot un-happen it. Refuse it and steer to finish. Field
			// case: a managed-data publish succeeded on its retry, a phantom
			// commitment (since fixed in registerCommittedActionsTyped) kept
			// finish enforcement demanding more, and the model's only exit was
			// confirm_audit(success=false) — which landed a run whose page WAS
			// live as status: error. Leaving the terminal-failure flag unset
			// here keeps the task record honest; reservations belong in the
			// final summary as flags, which the model is told to write.
			if orch.criticalExecutedCount > 0 && len(orch.pendingCriticalActions) == 0 && orch.allCommitmentsExhausted() {
				orch.selfAuditRequested = true
				orch.selfAuditConfirmedOnce = true
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"Audit Abort Refused: every declared critical action already executed successfully "+
						"(%d critical call(s) completed in this run) and nothing is outstanding. There is nothing to "+
						"abort — the work is done and aborting cannot undo it. Finish now and report the completed "+
						"result; put any reservations in your final summary as quality flags. Do NOT repeat the action "+
						"and do NOT call confirm_audit(success=false) again.",
					orch.criticalExecutedCount)), nil
			}

			// An abort says the declared critical work will NOT be done, so the
			// declarations themselves are retired here. Field case: an audit
			// bound the inline Pages write, the payload had gone by reference,
			// the upload variant was BLOCKED, the model aborted (correctly:
			// nothing had run) and re-audited the upload tool, which then
			// published — but the retired-in-spirit inline declaration stayed
			// on the ledger, finish enforcement demanded it, and the model's
			// only exit was a second abort that landed a live page as status:
			// error. A later confirm_audit(success=true) already clears the
			// terminal flag; with the ledger cleared too, the run is judged on
			// what executes after the re-audit.
			retired := orch.retireOutstandingCommitments()
			orch.selfAuditRequested = true
			orch.selfAuditConfirmedOnce = true
			orch.auditConfirmed = false
			orch.auditTerminalFailure = true
			orch.auditSummary = strings.TrimSpace(input.UserVisibleSummary)
			evidence := summarizeConfirmAuditEvidence(args)
			retiredNote := ""
			if len(retired) > 0 {
				retiredNote = fmt.Sprintf("\nRetired declared-but-unexecuted: %s. If the task can still be completed "+
					"another way, re-run confirm_audit declaring the tool you will actually call, then execute it; "+
					"otherwise finish and report the blocker.", strings.Join(retired, ", "))
			}
			return fantasy.NewTextResponse(fmt.Sprintf("Audit Failed Terminally.\n%s\nSummary: %s%s",
				evidence, strings.TrimSpace(input.UserVisibleSummary), retiredNote)), nil
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
	// The critical-actions declaration is what a PASSING audit unlocks; an
	// abort unlocks nothing, so demanding it there was pure friction. Every
	// observed abort in the field was first rejected on exactly this line —
	// the model omits the list because it is not unlocking anything — and
	// only the second attempt landed, after a wasted round trip.
	if success && len(legacyCriticalActions) == 0 && len(structuredCriticalActions) == 0 {
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

// criticalActionFingerprints renders each typed critical_actions entry as a
// stable string carrying its record binding (deal_id / deal_ids /
// values_digest), so an audit re-approving the SAME tool over DIFFERENT
// records or values hashes differently and is not swallowed by the
// "already confirmed" shortcut.
func criticalActionFingerprints(args map[string]interface{}) []string {
	raw, ok := args["critical_actions"]
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
		entry := strings.TrimSpace(fmt.Sprint(obj["tool"]))
		if entry == "" || entry == nilStringValue {
			continue
		}
		if id := strings.TrimSpace(fmt.Sprint(obj["deal_id"])); id != "" && id != nilStringValue {
			entry += "#single:" + id
		}
		if ids, ok := obj["deal_ids"].([]interface{}); ok && len(ids) > 0 {
			parts := make([]string, 0, len(ids))
			for _, v := range ids {
				parts = append(parts, strings.TrimSpace(fmt.Sprint(v)))
			}
			sort.Strings(parts)
			entry += "#batch:" + strings.Join(parts, ",")
		}
		if d := strings.TrimSpace(fmt.Sprint(obj["values_digest"])); d != "" && d != nilStringValue {
			entry += "#digest:" + strings.ToLower(d)
		}
		result = append(result, entry)
	}
	return result
}

func fingerprintConfirmAuditArgs(args map[string]interface{}) string {
	criticalActions := criticalActionFingerprints(args)
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
