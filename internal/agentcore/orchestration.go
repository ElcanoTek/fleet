package agentcore

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	"charm.land/fantasy"
)

// orchestrationState holds the mutable per-run enforcement + usage state that
// tool handlers and stream callbacks close over during a single Run.
//
// This is the UNION of the two front-ends' enforcement state, parameterized so
// one struct serves both modes:
//
//   - Interactive (chat): per-turn email rate-limit/dedup, repeat-call loop
//     guard, human-approval staging (send_email / risky bash / preview_email /
//     suggest_advanced_model), memory-proposal staging, cost/token ceilings.
//   - Scheduled (cutlass): audit gating (critical tools blocked until a
//     confirm_audit passes), batch critical-action commitments + retry budgets,
//     task-tracker finish enforcement.
//
// Fields used by only one mode are inert in the other (nil hooks, zero
// ceilings, empty commitment maps), so the same checkRepeatedCall /
// recordToolResult / updateUsage paths run for both. The genuine divergence —
// the wording of the loop-guard noun and which checks gate finishing — is
// expressed via config fields and the Policy seam (see policy.go), not a fork.
type orchestrationState struct {
	mu sync.Mutex

	// ── audit gating (scheduled) ──
	selfAuditRequested     bool
	auditConfirmed         bool
	selfAuditConfirmedOnce bool
	lastSuccessfulAuditFP  string
	auditTerminalFailure   bool
	// auditSummary is the user_visible_summary from the confirm_audit that set
	// auditTerminalFailure. It is the agent's own account of why it aborted —
	// the single most useful sentence about the run — and before #1151 nothing
	// downstream read it, so the task row said "Task completed successfully".
	auditSummary             string
	pendingCriticalActions   []pendingCriticalAction
	completedCriticalActions []string

	// committedCriticalActions counts outstanding critical-tool commitments per
	// tool suffix declared in the most recent successful confirm_audit. Finish
	// is refused while any count is > 0. Counting (not a bool) enables batch
	// flows like multi-record creation.
	committedCriticalActions map[string]int

	// typedCommitments is the full-fidelity ledger of commitments registered
	// from the TYPED critical_actions field, in declaration order (#715).
	// Unlike the suffix-count map above (which several MCP servers can share
	// when their tool names end in the same critical suffix), each entry is
	// keyed by the FULL tool name the audit declared (server + client-variant
	// prefix included) plus its optional record-id binding. While any
	// commitment is outstanding, checkCriticalTool refuses critical calls that
	// match no outstanding commitment, and markCommittedExecuted refuses to
	// discharge across servers/variants/records. Empty when the audit used
	// only the legacy free-text path — those commitments stay suffix-scoped.
	typedCommitments []*typedCommitment

	// typedAuditActive records that the most recent successful confirm_audit
	// supplied the TYPED critical_actions field (as opposed to the legacy
	// free-text path). When set, the commitment-binding gate engages for EVERY
	// critical call regardless of whether the ledger currently holds
	// outstanding commitments — so a typed audit that resolved to zero
	// commitments authorizes NOTHING (fail closed) rather than leaking the
	// legacy one-shot token (#715). A legacy (untyped) audit clears this and
	// keeps the one-shot semantics.
	typedAuditActive bool

	// approvedDealIDs / approvedDigest bind a BATCH confirm_audit to the exact
	// record ids (and value-set digest) the audit approved, keyed by
	// critical-tool suffix. When a tool call carries deal_ids (a server-side
	// batch), every id MUST be in approvedDealIDs[suffix]; and when the audit
	// declared a digest, the call's values_sha256 MUST equal
	// approvedDigest[suffix] — otherwise the call is blocked. Empty/absent =>
	// no batch binding, i.e. single-record flows behave exactly as before.
	// This is what stops one audit approval from silently authorizing a batch
	// over records the approver never saw.
	approvedDealIDs map[string]map[string]bool
	approvedDigest  map[string]string

	// dischargedDeals tracks, per critical-tool suffix, the record ids whose
	// commitment has ALREADY been discharged by a successful per-record batch
	// result. Discharge is therefore idempotent across resumes: a re-run where
	// already-done records report success again (idempotent skip) does NOT
	// double-discharge them, so the outstanding count reflects only the
	// records that still genuinely need work. Reset per audit envelope
	// (registerCommitted*).
	dischargedDeals map[string]map[string]bool

	// criticalToolFailureAttempts counts unsuccessful executions per
	// (toolName + argsHash) so a deterministically-broken critical call can't
	// loop endlessly under one audit envelope.
	criticalToolFailureAttempts map[string]int

	// ── repeat-call loop guard (both modes) ──
	lastCallKey     string
	lastCallRepeats int
	loopGuardTrips  int
	// The tool_call dispatcher gets its own slot: a successful dispatch
	// re-enters the guard as the real tool, so tracking the wrapper in the
	// SAME slot made the keys alternate and reset the counter every hop. But
	// a tool_call that fails BEFORE dispatch (bad JSON, unknown tool) never
	// reaches the real-tool slot, so the wrapper must still count its own
	// identical repeats or that failure loops until the run's timeout.
	lastWrapperKey     string
	lastWrapperRepeats int
	// repeatGuardNoun parameterizes the single word that differs between the
	// two front-ends' loop-guard message: chat says "reply to the user", cutlass
	// says "finish the task". See checkRepeatedCall.
	repeatGuardNoun string

	// ── email safety (both modes) ──
	sendEmailSuccessCount int
	sentEmailFingerprints map[string]struct{}

	// ── approval / memory staging (interactive) ──
	approvalSink   ApprovalStager
	memoryProposer MemoryProposer

	// stagedCriticalApprovals is every critical tool this turn staged for
	// approval that has not executed yet. A staged card freezes its arguments:
	// the human clicks Approve minutes later and the tool runs with whatever the
	// model wrote at stage time. That makes two staged writes to the same MCP
	// server actively unsafe when the second one's arguments depend on the
	// first's outcome — an optimistic-concurrency token, a record id, a version
	// pointer. A real session lost a Pages deploy exactly this way: patch_page
	// was staged, the model read "Do NOT retry" as "use a different tool",
	// staged deploy_page_upload for the same page with expected_version frozen
	// at the pre-patch version, the human approved both, and the second write
	// died on stale_version after all the work was done. checkCriticalToolApproval
	// refuses the second stage instead. Same tool name is allowed through: that
	// is the batch case (N independent records), which has no such coupling.
	stagedCriticalApprovals []stagedCriticalApproval

	// noteProposer stages agent-proposed admin-notes edits (BOTH modes), unlike
	// memoryProposer which is interactive-only. Wired by the drivers via
	// setNoteProposer; nil leaves propose_note reporting "not wired".
	noteProposer  NoteProposer
	skillProposer SkillProposer

	// ── task tracker (scheduled finish enforcement) ──
	taskTrackerUsed   bool
	latestTaskTracker taskTrackerSnapshot

	// delegatedFinish marks this run as a spawned SUB-AGENT (#1043 follow-up):
	// checkFinishEnforcement then skips the self-audit ritual blocks and keeps
	// every other finish gate. See checkFinishEnforcement for why the ritual is
	// a root-run gate, and NewDelegatedPolicy for the one constructor that sets
	// it — the flag is never model-reachable.
	delegatedFinish bool

	// ── ceilings (interactive); zero means unlimited ──
	maxCostUSD     float64
	maxTotalTokens int

	// ── step / usage tracking ──
	logSession *LogSession

	// usage counters (chat surfaced these on orch; scheduled mirrors into
	// logSession). Both are maintained so either Observer can read them.
	PromptTokens        int
	LastStepInputTokens int
	CompletionTokens    int
	CachedTokens        int
	CacheCreationTokens int
	CostUSD             float64

	// LastServedUpstream is the OpenRouter upstream that served the most recent
	// step ("" when the provider reported none). ServedFallback records that at
	// least one step in the run was served by an upstream OTHER than the one the
	// slug is pinned to — a soft pin is a preference, so this is the only signal
	// that a run silently left its canonical (cache-warm, precision-floored)
	// route. Kept on the run state so an Observer can surface it next to cost.
	LastServedUpstream string
	ServedFallback     bool
}

// pendingCriticalAction tracks a critical tool call blocked by audit gating.
type pendingCriticalAction struct {
	toolName string
	argsHash string
}

// ApprovalStager is the narrow interface the orchestration layer uses to stage
// a critical tool call for user approval (interactive only). The interactive
// driver (P3) wires an implementation that persists to the approvals table and
// emits an SSE event; in scheduled mode the sink is nil and these gates are
// inert.
type ApprovalStager interface {
	Stage(toolName, toolCallID, rawInput string) (approvalID string, err error)
	StageSuggestion(reason string) (approvalID, msg string, err error)
}

// ActionRecorder is the OPTIONAL half of ApprovalStager that a `notify`-mode
// critical tool needs (#1153): post a card recording that an action ran, rather
// than one asking whether it may. Kept as a separate, type-asserted interface so
// every existing ApprovalStager implementation compiles unchanged.
//
// A sink that does not implement it makes `notify` unavailable, and the gate
// falls back to blocking on a card. That direction is deliberate: the argument
// for executing without asking is that the user still finds out and can undo it,
// so a deployment that cannot tell them must not skip the question.
type ActionRecorder interface {
	// RecordAction posts the record card. undoHint is the bundle-authored line
	// describing how to reverse this action; it may be empty.
	RecordAction(toolName, toolCallID, rawInput, undoHint string) error
}

// Session pre-approval sentinels (#300): instead of a real approval ID, Stage
// may return one of these to signal a session-scoped pre-decision the user made
// earlier ("approve/deny all <tool> in this conversation"). They ride the normal
// (string, error) return so any stager can forward the returned string verbatim
// without a special case. The interactive gates
// interpret them: pre-approved → let the tool run normally (no approval card);
// pre-denied → block with a denial message (no card). An ApprovalStager that has
// no session registry simply never returns them, so the gates fall through to the
// normal stage-a-card path.
const (
	PreApprovedSentinel = "\x00fleet-session-preapproved\x00"
	PreDeniedSentinel   = "\x00fleet-session-predenied\x00"
)

// MemoryProposer stages a memory proposal for user confirmation (interactive).
// kind classifies the memory (#515: fact/preference/identity/constraint/
// context); implementations normalize unknown values to "fact".
type MemoryProposer interface {
	Propose(content, kind string) (proposalID string, err error)
}

// newOrchestrationState matches cutlass's constructor signature (the one the
// lifted parity tests call). The interactive driver layers on ceilings +
// approval hooks via the setters below. The trailing int param is retained for
// that signature parity only — the real iteration cap flows via the engine
// (RunConfig.MaxIterations), so the value passed here is ignored.
func newOrchestrationState(logSession *LogSession, _ int) *orchestrationState {
	return &orchestrationState{
		sentEmailFingerprints:       make(map[string]struct{}),
		committedCriticalActions:    make(map[string]int),
		approvedDealIDs:             make(map[string]map[string]bool),
		approvedDigest:              make(map[string]string),
		dischargedDeals:             make(map[string]map[string]bool),
		criticalToolFailureAttempts: make(map[string]int),
		logSession:                  logSession,
		repeatGuardNoun:             repeatGuardNounFinishTask,
	}
}

// Loop-guard nouns: the single phrase that differs between the front-ends.
const (
	repeatGuardNounFinishTask  = "finish the task"
	repeatGuardNounReplyToUser = "reply to the user"
)

// setRepeatGuardNoun overrides the loop-guard noun (interactive uses
// repeatGuardNounReplyToUser).
func (o *orchestrationState) setRepeatGuardNoun(noun string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if noun != "" {
		o.repeatGuardNoun = noun
	}
}

// setDelegatedFinish marks the run as a spawned sub-agent's, relaxing ONLY the
// self-audit finish ritual (see checkFinishEnforcement).
func (o *orchestrationState) setDelegatedFinish(v bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.delegatedFinish = v
}

// setCeilings configures the per-turn guardrails (interactive).
func (o *orchestrationState) setCeilings(maxCostUSD float64, maxTotalTokens int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.maxCostUSD = maxCostUSD
	o.maxTotalTokens = maxTotalTokens
}

// setApprovalSink wires up the stager for this run (interactive).
func (o *orchestrationState) setApprovalSink(s ApprovalStager) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.approvalSink = s
}

// setMemoryProposer wires up the proposer for this run (interactive).
func (o *orchestrationState) setMemoryProposer(p MemoryProposer) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.memoryProposer = p
}

// setNoteProposer wires up the admin-notes proposer for this run (both modes).
func (o *orchestrationState) setNoteProposer(p NoteProposer) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.noteProposer = p
}

// setSkillProposer wires up the personal-skill proposer for this run (both
// modes; docs/SKILLS.md phase 3).
func (o *orchestrationState) setSkillProposer(p SkillProposer) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.skillProposer = p
}

// checkCeilings returns (blocked, reason). Called at every tool-call boundary so
// runaway turns stop before the next paid step (interactive guardrail; a no-op
// when both ceilings are zero, i.e. scheduled mode).
func (o *orchestrationState) checkCeilings() (bool, string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.maxCostUSD > 0 && o.CostUSD >= o.maxCostUSD {
		return true, fmt.Sprintf("COST_CEILING_REACHED: this turn has accumulated $%.4f which meets or exceeds the configured ceiling of $%.2f. Stop calling tools and end the turn with what you have.",
			o.CostUSD, o.maxCostUSD)
	}
	if o.maxTotalTokens > 0 {
		total := o.PromptTokens - o.CachedTokens + o.CompletionTokens
		if total >= o.maxTotalTokens {
			return true, fmt.Sprintf("TOKEN_CEILING_REACHED: this turn has processed %d uncached tokens which meets or exceeds the configured ceiling of %d. Stop calling tools and end the turn with what you have.",
				total, o.maxTotalTokens)
		}
	}
	return false, ""
}

// BudgetState snapshots a run's cost/token ceilings and accumulated spend. It is
// the read side of the sub-agent budget split (#175): the spawn_subagent tool
// reads the PARENT's BudgetState to compute how much of the parent's REMAINING
// budget it may hand a child, so the parent ceiling stays the hard wall across
// all descendants. A zero ceiling means "unlimited" (the same convention
// checkCeilings uses). Spend already INCLUDES any prior children charged back
// via chargeChildUsage, so each successive spawn sees a smaller remaining slice.
type BudgetState struct {
	MaxCostUSD     float64 // 0 = unlimited
	SpentCostUSD   float64
	MaxTotalTokens int // 0 = unlimited
	SpentTokens    int // uncached: prompt - cached + completion (matches checkCeilings)
}

// RemainingCostUSD returns the unspent cost budget, or -1 when the ceiling is
// unlimited (0). Never returns a negative slice for a finite ceiling: an
// over-budget run reports 0 remaining.
func (b BudgetState) RemainingCostUSD() float64 {
	if b.MaxCostUSD <= 0 {
		return -1
	}
	rem := b.MaxCostUSD - b.SpentCostUSD
	if rem < 0 {
		return 0
	}
	return rem
}

// RemainingTokens returns the unspent token budget, or -1 when the ceiling is
// unlimited (0). Never negative for a finite ceiling.
func (b BudgetState) RemainingTokens() int {
	if b.MaxTotalTokens <= 0 {
		return -1
	}
	rem := b.MaxTotalTokens - b.SpentTokens
	if rem < 0 {
		return 0
	}
	return rem
}

// budgetState reads the current ceilings + accumulated spend under the orch lock.
func (o *orchestrationState) budgetState() BudgetState {
	o.mu.Lock()
	defer o.mu.Unlock()
	return BudgetState{
		MaxCostUSD:     o.maxCostUSD,
		SpentCostUSD:   o.CostUSD,
		MaxTotalTokens: o.maxTotalTokens,
		SpentTokens:    o.PromptTokens - o.CachedTokens + o.CompletionTokens,
	}
}

// chargeChildUsage folds a completed CHILD run's usage into THIS (parent) run's
// accumulated counters. This is the enforcement linchpin of the #175 budget
// split: a child runs as its own agentcore.Run with its OWN orchestrationState
// (and its OWN sliced ceiling, which the child's checkCeilings/budgetGuardedStep
// already enforce), so its spend is invisible to the parent's ceiling until it
// is charged back here. After this call the parent's checkCeilings sees the
// child's tokens+cost, so:
//
//   - the parent itself stops sooner (it has less budget left), and
//   - the NEXT sibling spawn reads a smaller remaining slice (budgetState),
//
// which together make the parent ceiling a hard wall that the collective spend
// of all children across fan-out AND depth can never breach. (Depth composes for
// free: a grandchild's spend is charged to its parent, whose own run-end usage —
// including that grandchild — is in turn charged to the grandparent here.)
//
// It deliberately does NOT touch the email/critical-action tracking that
// recordToolResult owns: this is pure usage accounting, mirroring updateUsage's
// counter math (uncached-token semantics are derived at read time in
// checkCeilings/budgetState, so only the raw counters move here).
func (o *orchestrationState) chargeChildUsage(u RunUsage) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.PromptTokens += u.PromptTokens
	o.CompletionTokens += u.CompletionTokens
	o.CachedTokens += u.CachedTokens
	o.CacheCreationTokens += u.CacheCreationTokens
	o.CostUSD += u.CostUSD
	if o.logSession != nil {
		o.logSession.mu.Lock()
		o.logSession.PromptTokens += u.PromptTokens
		o.logSession.CompletionTokens += u.CompletionTokens
		o.logSession.CachedTokens += u.CachedTokens
		o.logSession.CacheCreationTokens += u.CacheCreationTokens
		o.logSession.Cost += u.CostUSD
		o.logSession.mu.Unlock()
	}
}

// maxConsecutiveIdenticalCalls is how many times the SAME tool may run with
// byte-identical arguments back-to-back before the loop guard cuts it off.
const maxConsecutiveIdenticalCalls = 3

// checkRepeatedCall is the repeat-call loop guard. Every tool execution routes
// through it BEFORE running, so it both tracks the call sequence and gates
// degenerate repeats. Returns (blocked, msg).
//
// The single divergence between the two front-ends is the closing noun, which
// is read from o.repeatGuardNoun ("finish the task" vs "reply to the user").
func (o *orchestrationState) checkRepeatedCall(toolName, rawInput string) (bool, string) {
	// The deferred-tool dispatcher is tracked in its OWN slot (see the field
	// comment on lastWrapperKey): a successful dispatch re-enters this guard
	// as the real tool, whose slot below does the blocking; but a wrapper
	// that fails before dispatch never reaches it, so identical wrapper
	// repeats must trip on their own.
	if toolName == toolNameToolCall {
		o.mu.Lock()
		defer o.mu.Unlock()
		key := hashString(rawInput)
		if key != o.lastWrapperKey {
			o.lastWrapperKey = key
			o.lastWrapperRepeats = 1
			return false, ""
		}
		o.lastWrapperRepeats++
		if o.lastWrapperRepeats <= maxConsecutiveIdenticalCalls {
			return false, ""
		}
		o.loopGuardTrips++
		log.Printf("Enforcement: loop guard blocked %s — %d consecutive identical calls (cap %d, trip %d)",
			toolName, o.lastWrapperRepeats, maxConsecutiveIdenticalCalls, o.loopGuardTrips)
		return true, fmt.Sprintf("LOOP_GUARD (block #%d): this exact tool_call with these exact arguments has now been issued %d times in a row (execution cap: %d). Repeating it cannot produce a different result. Fix the arguments (valid JSON, a tool that exists) or take a different action.",
			o.loopGuardTrips, o.lastWrapperRepeats, maxConsecutiveIdenticalCalls)
	}
	o.mu.Lock()
	defer o.mu.Unlock()

	key := toolName + ":" + hashString(rawInput)
	if key != o.lastCallKey {
		o.lastCallKey = key
		o.lastCallRepeats = 1
		o.loopGuardTrips = 0
		return false, ""
	}
	o.lastCallRepeats++
	if o.lastCallRepeats <= maxConsecutiveIdenticalCalls {
		return false, ""
	}
	o.loopGuardTrips++
	noun := o.repeatGuardNoun
	if noun == "" {
		noun = repeatGuardNounFinishTask
	}
	log.Printf("Enforcement: loop guard blocked %s — %d consecutive identical calls (cap %d, trip %d)",
		toolName, o.lastCallRepeats, maxConsecutiveIdenticalCalls, o.loopGuardTrips)
	return true, fmt.Sprintf("LOOP_GUARD (block #%d): this exact %s call with these exact arguments has now been issued %d times in a row (execution cap: %d). Re-running identical code cannot produce new information. Change your approach: print() or inspect intermediate values, write your work to a workspace file, alter the arguments — or %s with what you have.",
		o.loopGuardTrips, toolName, o.lastCallRepeats, maxConsecutiveIdenticalCalls, noun)
}

// ── interactive approval / memory gates ──

const maxSendEmailCallsPerTurn = 3

func isEmailSendTool(toolName string) bool {
	return toolName == sendEmailToolSuffix || strings.HasSuffix(toolName, "_"+sendEmailToolSuffix)
}

// checkEmailSafety intercepts send_email calls (interactive): rate-limit, dedup,
// then stage for user approval when a sink is wired. Returns (blocked, reason).
func (o *orchestrationState) checkEmailSafety(toolName, toolCallID, rawInput string) (bool, string) {
	if !isEmailSendTool(toolName) {
		return false, ""
	}
	if hasUnresolvedToolPlaceholder(rawInput) {
		return true, "send_email argument contains an unresolved ${tool:…} placeholder. The agent runtime does NOT substitute that syntax; paste the actual value into the tool arguments instead."
	}
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.sendEmailSuccessCount >= maxSendEmailCallsPerTurn {
		log.Printf("Enforcement: blocking %s — per-turn limit %d reached", toolName, maxSendEmailCallsPerTurn)
		return true, fmt.Sprintf("Safety limit: send_email already executed %d time(s) in this turn. Further sends blocked. Ask the user before sending more.",
			maxSendEmailCallsPerTurn)
	}
	fp := emailDedupKey(rawInput)
	if _, dup := o.sentEmailFingerprints[fp]; dup {
		return true, "Safety guard: identical send_email payload already sent in this turn."
	}
	if o.approvalSink != nil {
		id, err := o.approvalSink.Stage(toolName, toolCallID, rawInput)
		if err != nil {
			log.Printf("approval stage failed: %v", err)
			return true, fmt.Sprintf("APPROVAL_REQUIRED: could not stage send for user approval (%v). Ask the user what to do.", err)
		}
		switch id {
		case PreApprovedSentinel:
			// Session pre-approval (#300): run the send without a card, but the
			// per-turn limit + dedup checks above still applied.
			o.sentEmailFingerprints[fp] = struct{}{}
			return false, ""
		case PreDeniedSentinel:
			return true, fmt.Sprintf("APPROVAL_DENIED: the user pre-denied %s for this conversation (session policy). Do NOT retry; tell the user it was blocked by their own pre-approval setting.", toolName)
		}
		return true, fmt.Sprintf("APPROVAL_REQUIRED: this send_email call has been staged for explicit user approval "+
			"(approval_id=%s). Do NOT retry. Summarize to the user what you would send and wait for them to click Send.", id)
	}
	return false, ""
}

// checkMemoryProposal intercepts propose_memory calls (interactive).
func (o *orchestrationState) checkMemoryProposal(toolName, rawInput string) (bool, string) {
	if toolName != "propose_memory" {
		return false, ""
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.memoryProposer == nil {
		return true, "MEMORY_PROPOSAL_UNAVAILABLE: saving user memories is not enabled on this transport. Do NOT retry — summarize the point for the user instead."
	}
	var args struct {
		Content string `json:"content"`
		Kind    string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(rawInput), &args); err != nil {
		return true, fmt.Sprintf("MEMORY_PROPOSAL_FAILED: invalid arguments (%v).", err)
	}
	id, err := o.memoryProposer.Propose(args.Content, args.Kind)
	if err != nil {
		return true, fmt.Sprintf("MEMORY_PROPOSAL_FAILED: could not stage proposal (%v).", err)
	}
	return true, fmt.Sprintf("MEMORY_PROPOSED: this memory has been staged for user confirmation (proposal_id=%s). Summarize what you proposed and ask the user whether to save it. Do NOT retry the tool.", id)
}

// checkNoteProposal intercepts propose_note calls (BOTH modes). Mirrors
// checkMemoryProposal; routed from the same BeforeToolCall path both Policy
// bundles use. Returns (blocked, msg) — propose_note never executes a tool, the
// staging IS the effect.
func (o *orchestrationState) checkNoteProposal(toolName, rawInput string) (bool, string) {
	if toolName != "propose_note" {
		return false, ""
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.noteProposer == nil {
		return true, "NOTE_PROPOSAL_UNAVAILABLE: note proposals are not enabled on this transport. Do NOT retry."
	}
	var args struct {
		Slug   string `json:"slug"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(rawInput), &args); err != nil {
		return true, fmt.Sprintf("NOTE_PROPOSAL_FAILED: invalid arguments (%v).", err)
	}
	id, err := o.noteProposer.Propose(args.Slug, args.Title, args.Body, args.Reason)
	if err != nil {
		return true, fmt.Sprintf("NOTE_PROPOSAL_FAILED: could not stage proposal (%v).", err)
	}
	return true, fmt.Sprintf("NOTE_PROPOSED: staged for admin review (proposal_id=%s). "+
		"An admin will publish or reject it; the change is NOT live yet. Do NOT retry the tool.", id)
}

// checkSkillProposal intercepts propose_skill calls (BOTH modes). Mirrors
// checkNoteProposal: the staging IS the effect — the tool body never executes.
func (o *orchestrationState) checkSkillProposal(toolName, rawInput string) (bool, string) {
	if toolName != "propose_skill" {
		return false, ""
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.skillProposer == nil {
		return true, "SKILL_PROPOSAL_UNAVAILABLE: skill proposals are not enabled on this transport. Do NOT retry."
	}
	var args struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Body        string `json:"body"`
		Reason      string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(rawInput), &args); err != nil {
		return true, fmt.Sprintf("SKILL_PROPOSAL_FAILED: invalid arguments (%v).", err)
	}
	id, err := o.skillProposer.Propose(args.Name, args.Description, args.Body, args.Reason)
	if err != nil {
		return true, fmt.Sprintf("SKILL_PROPOSAL_FAILED: could not stage proposal (%v).", err)
	}
	return true, fmt.Sprintf("SKILL_PROPOSED: staged for the user's review on their Skills page (proposal_id=%s). "+
		"It is NOT active yet and will not exist in later turns unless the user approves it. Do NOT retry the tool.", id)
}

// hasUnresolvedToolPlaceholder detects ${tool:…} binding tokens the model
// occasionally invents; never intentional content.
func hasUnresolvedToolPlaceholder(rawInput string) bool {
	return strings.Contains(rawInput, "${tool:") || strings.Contains(rawInput, "${TOOL:")
}

// ── usage accounting (both modes) ──

// updateUsage records token usage and cost from a fantasy step. Maintains both
// the orch-level counters (chat's UI footer) and the logSession accumulators
// (scheduled captain's-log).
//
// modelSlug is the model that produced this step; it selects a per-model price
// override when the operator configured one (#297). The step's cost is resolved
// through computeCostFromUsage: a matching manifest override prices the step
// locally from the token counts; otherwise the configured fallback applies —
// which, in the shipped default (no overrides), is the OpenRouter-returned cost,
// i.e. byte-identical to the prior behavior.
func (o *orchestrationState) updateUsage(modelSlug string, usage fantasy.Usage, metadata fantasy.ProviderMetadata) {
	o.accumulateUsage(modelSlug, usage, metadata, false)
}

// updateAuxUsage records an AUXILIARY model call made on behalf of the run
// inside its loop (#1118): the compaction summarizer and the model-calling
// native tools (suggest_*). The call's tokens/cost accumulate into the SAME
// totals checkCeilings / budgetState / Result.Usage read — that is the point —
// but it must NOT touch the LastStep* fields: LastStepInputTokens /
// LastStepPromptTokens are documented as the MAIN loop's per-call input size
// (the context-window-fill signal checkContextPressure and the chat context
// meter read), and an aux call's prompt is not the run's context fill. Without
// this split, a Stop during a proactive compaction reported the summarizer's
// prompt size as the conversation's window fill. Aux calls also skip the
// served-upstream attribution: that signal tracks the RUN's pinned model
// routing, and an aux call (often a different, cheaper model) would flap the
// transition log.
func (o *orchestrationState) updateAuxUsage(modelSlug string, usage fantasy.Usage, metadata fantasy.ProviderMetadata) {
	o.accumulateUsage(modelSlug, usage, metadata, true)
}

// accumulateUsage is the shared body behind updateUsage (main-loop steps) and
// updateAuxUsage (in-loop auxiliary calls). aux gates the per-STEP signals —
// LastStep* and the served-upstream attribution — which belong to the main
// loop only; the cumulative billing/ceiling counters accumulate for both.
func (o *orchestrationState) accumulateUsage(modelSlug string, usage fantasy.Usage, metadata fantasy.ProviderMetadata, aux bool) {
	o.mu.Lock()
	defer o.mu.Unlock()

	// Cache-token convention (#587): fantasy normalizes EVERY provider to one
	// convention before usage reaches this seam — InputTokens is the UNCACHED
	// (fresh) prompt input and CacheReadTokens is the cache-read subset. That
	// holds for OpenRouter (which reports prompt_tokens INCLUDING cached;
	// fantasy's openrouter hooks subtract the cached subset) and for the native
	// Anthropic provider (whose input_tokens already exclude cache reads) alike.
	//
	// The counters here follow the LogSession contract instead: PromptTokens is
	// the TOTAL prompt-side input including cache reads (the billing/display
	// number CumulativeCacheHitRate divides by, and the one checkCeilings /
	// budgetState subtract CachedTokens from to get uncached spend) — so the
	// cache reads are added back in ONCE, here, at the single accounting seam.
	// Accumulating bare InputTokens (the pre-#587 behavior) made those
	// subtraction sites double-discount the cache: the token ceiling and the
	// sub-agent budget slices under-counted uncached spend by the cached amount
	// of every step, so a run with a hot cache prefix was governed by a ceiling
	// that fired late (or a child budget sliced too generously).
	totalPrompt := int(usage.InputTokens + usage.CacheReadTokens)
	o.PromptTokens += totalPrompt
	// The per-STEP input size is the context-pressure / window signal, so it too
	// counts cache reads — a cached token still occupies the context window.
	// MAIN-loop steps only: an aux call's prompt is not the run's context fill
	// (see updateAuxUsage).
	if !aux {
		o.LastStepInputTokens = totalPrompt
	}
	o.CompletionTokens += int(usage.OutputTokens)
	o.CachedTokens += int(usage.CacheReadTokens)
	o.CacheCreationTokens += int(usage.CacheCreationTokens)

	cost := computeCostFromUsage(modelSlug, usage, openrouterCost(metadata), pricingConfig())
	o.CostUSD += cost

	// Attribute the step to the upstream that actually served it. A soft pin
	// (Order + AllowFallbacks) routes AWAY from the canonical upstream whenever
	// it is busy, which costs the per-upstream prompt cache and — for a family
	// whose pool mixes serving precisions — can drop below the quantization
	// floor's intent on any endpoint that ignores it. Logged once per transition
	// rather than per step: the signal is the switch, and a per-step line would
	// be noise on a long run. MAIN-loop steps only: an aux call is often a
	// different (cheaper) model whose routing says nothing about the run's pin
	// and would flap the transition detection (see updateAuxUsage).
	if served := openrouterServedProvider(metadata); !aux && served != "" {
		if served != o.LastServedUpstream {
			if preferred := preferredUpstreamFor(modelSlug); preferred != "" && served != preferred {
				o.ServedFallback = true
				log.Printf("⚠️  Upstream fallback: model=%s pinned=%s served=%s (prompt cache cold; verify serving precision if output quality is off)", modelSlug, preferred, served)
			}
		}
		o.LastServedUpstream = served
	}

	if o.logSession != nil {
		o.logSession.mu.Lock()
		o.logSession.PromptTokens += totalPrompt
		o.logSession.CompletionTokens += int(usage.OutputTokens)
		o.logSession.CachedTokens += int(usage.CacheReadTokens)
		o.logSession.CacheCreationTokens += int(usage.CacheCreationTokens)
		// The compaction trigger compares this against the model's context
		// window: it is the true size of this call's prompt (fresh + cache-read),
		// NOT fresh + cached added onto an already-inclusive total — fantasy's
		// InputTokens excludes cache reads (above), so this sum counts each
		// prompt token exactly once. MAIN-loop steps only, same invariant as
		// LastStepInputTokens above (see updateAuxUsage).
		if !aux {
			o.logSession.LastStepPromptTokens = totalPrompt
		}
		o.logSession.Cost += cost
		o.logSession.mu.Unlock()
	}
}

// markPendingCriticalDone moves ONE pending critical action for toolName to
// completed. Audited-upfront calls (never blocked) are not in the pending list,
// so this is a no-op for them. Callers must hold o.mu.
//
// An exact (toolName, argsHash) hit wins, so when the retry really is the same
// call the precise entry is the one discharged. Otherwise the OLDEST pending
// entry for that tool is discharged, because an args-hash-only rule strands the
// commitment whenever the successful retry had to differ from the blocked call —
// which is exactly what happens when the first attempt was rejected for bad
// arguments and the agent fixed them.
//
// That stranding was observed end to end: a scheduled run's send_email was
// blocked pre-audit (pending recorded against those args), the retry failed
// tool-argument validation, and the call that finally succeeded therefore
// carried CORRECTED arguments and a different hash. Nothing matched, the
// commitment stayed outstanding, and CanFinish kept answering "Execute pending
// action(s): [mcp_sendgrid_send_email]" at a run that had already sent the
// email. The agent then re-sent, hit the duplicate-send guard, and — to get past
// a guard whose whole job was to stop it — re-rendered the HTML body 110 bytes
// larger so the payload would no longer be identical. It burned ~25 of its 27
// minutes there. A commitment tracker that cannot recognize its own discharge
// turns every safety rail downstream of it into an obstacle to route around.
//
// Discharging one entry per success keeps the count honest: two distinct pending
// calls to the same tool still need two successes, exactly as before.
func (o *orchestrationState) markPendingCriticalDone(toolName, argsHash string) {
	fallback := -1
	for i, p := range o.pendingCriticalActions {
		if p.toolName != toolName {
			continue
		}
		if p.argsHash == argsHash {
			o.dischargePendingCriticalAt(i)
			return
		}
		if fallback < 0 {
			fallback = i
		}
	}
	if fallback >= 0 {
		log.Printf("Enforcement: discharging pending %s against corrected arguments (blocked-call hash no longer matches)", toolName)
		o.dischargePendingCriticalAt(fallback)
	}
}

// dischargePendingCriticalAt moves pendingCriticalActions[i] to completed.
// Callers must hold o.mu.
func (o *orchestrationState) dischargePendingCriticalAt(i int) {
	o.completedCriticalActions = append(o.completedCriticalActions, o.pendingCriticalActions[i].toolName)
	o.pendingCriticalActions = append(o.pendingCriticalActions[:i], o.pendingCriticalActions[i+1:]...)
}

// recordToolResult updates tracking state after a tool call completes. Handles
// both interactive email accounting and scheduled critical-action discharge.
func (o *orchestrationState) recordToolResult(toolName, rawInput, resultText string, succeeded bool) {
	o.mu.Lock()
	defer o.mu.Unlock()

	// A tool can return with no transport error yet REPORT failure in its
	// payload ({"success": false, ...} or a top-level "error") — e.g. an
	// upstream 400 flattened into a clean MCP response (#716). For a tool that
	// does NOT return a per-record results[] that is a FAILED execution: it
	// must not discharge a commitment and counts against the retry budget.
	// Batch tools that DO return results[] are accounted per-record below
	// (parseDealOutcomes), not via this flag.
	effectiveSucceeded := succeeded && !mcpReportedFailure(resultText)

	if isEmailTool(toolName) && succeeded {
		if sendEmailSucceeded(strings.TrimSpace(resultText)) {
			o.sendEmailSuccessCount++
			o.sentEmailFingerprints[emailDedupKey(rawInput)] = struct{}{}
			log.Printf("send_email queued successfully (%d/%d)", o.sendEmailSuccessCount, maxSendEmailCallsPerTask)
		}
	}

	if isCriticalTool(toolName) {
		argsHash := hashString(rawInput)
		key := retryBudgetKey(toolName, argsHash)
		if o.criticalToolFailureAttempts == nil {
			o.criticalToolFailureAttempts = make(map[string]int)
		}

		if outcomes, ok := parseDealOutcomes(resultText); ok {
			// Batch result: discharge one commitment per SUCCEEDED record,
			// idempotently. dischargedDeals dedups by record id, so a resume
			// that idempotently skips already-done records (reporting them
			// success again) does NOT double-discharge — the outstanding count
			// tracks only the records that still genuinely need work. Forward
			// progress (>=1 newly discharged record) resets the retry budget;
			// an attempt with failures and no new progress counts against it.
			// This lets a partial batch resume to completion without wedging
			// the budget on the unchanged full-batch args.
			suffix := criticalSuffixFor(toolName)
			done := o.dischargedDeals[suffix]
			if done == nil {
				done = make(map[string]bool)
				o.dischargedDeals[suffix] = done
			}
			// When the audit batch-bound an approved record set for this
			// suffix, ONLY a record in that set may discharge a commitment. A
			// success reported for an id the audit never approved — a server
			// results[] echoing an unexpected id, or any future path that
			// bypassed the input batch-binding gate — must NOT discharge a
			// DIFFERENT approved record's commitment (which would silently
			// mark an un-done approved record as complete and let the audit
			// auto-lock early). With no approved set (non-batch / legacy
			// audit) behavior is unchanged: discharge per succeeded record by
			// suffix.
			approved := o.approvedDealIDs[suffix]
			callDigest := valuesDigestArg(rawInput)
			newly, failed := 0, 0
			for _, oc := range outcomes {
				if len(approved) > 0 && oc.success && !approved[strings.TrimSpace(oc.dealID)] {
					log.Printf("Enforcement: ignoring batch result for unapproved record id %q on %q (not in the audit's approved set)",
						oc.dealID, toolName)
					continue
				}
				switch {
				case oc.success && !done[oc.dealID]:
					done[oc.dealID] = true
					o.markCommittedExecuted(toolName, oc.dealID, callDigest)
					newly++
				case !oc.success:
					failed++
				}
			}
			if newly > 0 {
				delete(o.criticalToolFailureAttempts, key)
				o.markPendingCriticalDone(toolName, argsHash)
				if len(o.pendingCriticalActions) == 0 {
					o.selfAuditRequested = true
				}
				log.Printf("Critical batch %s: discharged %d new record(s), %d failed", toolName, newly, failed)
			} else if failed > 0 {
				o.criticalToolFailureAttempts[key]++
				log.Printf("Critical batch %s made no forward progress (%d failed); attempt %d/%d",
					toolName, failed, o.criticalToolFailureAttempts[key], maxAttemptsPerCriticalAction)
			}
		} else if effectiveSucceeded {
			// Single-call critical tool (no per-record results[]).
			delete(o.criticalToolFailureAttempts, key)
			o.markPendingCriticalDone(toolName, argsHash)
			if len(o.pendingCriticalActions) == 0 {
				o.selfAuditRequested = true
			}
			log.Printf("Critical action succeeded: %s", toolName)
			o.markCommittedExecuted(toolName, callDealID(rawInput), valuesDigestArg(rawInput))
		} else {
			// Ran but reported failure (transport-level, resp.IsError, or a
			// payload-level failure per mcpReportedFailure) → counts against
			// the per-(tool,args) retry budget and discharges NOTHING.
			o.criticalToolFailureAttempts[key]++
			log.Printf("Critical action failed: %s (attempt %d/%d for these args)",
				toolName, o.criticalToolFailureAttempts[key], maxAttemptsPerCriticalAction)
		}

		// Consume the audit token only when no committed work remains. With no
		// commitments registered this matches legacy behavior (consume on the
		// first critical execution); with a batch audit it stays valid until
		// the last committed record is discharged, then auto-locks.
		if o.allCommitmentsExhausted() {
			o.auditConfirmed = false
		}
	}

	if toolName == toolNameTaskTracker {
		o.taskTrackerUsed = true
		o.latestTaskTracker = parseTaskTrackerSnapshot(resultText)
	}
}

const maxSendEmailCallsPerTask = 3

// parseTaskTrackerSnapshot parses task_tracker output into a snapshot. Minimal
// form sufficient for the unified runtime: structured JSON or the human
// "Summary: N total (a todo, b in progress, c done)" line. The P3 native tool
// owns the richer line-level checkpoint summary.
func parseTaskTrackerSnapshot(result string) taskTrackerSnapshot {
	result = strings.TrimSpace(result)
	if result == "" {
		return taskTrackerSnapshot{}
	}
	if strings.HasPrefix(result, "{") {
		var structured struct {
			Output  string `json:"output"`
			Summary struct {
				Total      int `json:"total"`
				Todo       int `json:"todo"`
				InProgress int `json:"in_progress"`
				Done       int `json:"done"`
			} `json:"summary"`
		}
		if err := json.Unmarshal([]byte(result), &structured); err == nil {
			if structured.Summary.Total > 0 {
				return taskTrackerSnapshot{
					Seen:       true,
					Total:      structured.Summary.Total,
					Todo:       structured.Summary.Todo,
					InProgress: structured.Summary.InProgress,
					Done:       structured.Summary.Done,
				}
			}
			if strings.TrimSpace(structured.Output) != "" {
				return parseTaskTrackerSnapshot(structured.Output)
			}
		}
	}
	return taskTrackerSnapshot{}
}
