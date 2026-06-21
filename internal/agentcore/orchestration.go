package agentcore

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
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
	selfAuditRequested       bool
	auditConfirmed           bool
	selfAuditConfirmedOnce   bool
	lastSuccessfulAuditFP    string
	auditTerminalFailure     bool
	pendingCriticalActions   []pendingCriticalAction
	completedCriticalActions []string

	// committedCriticalActions counts outstanding critical-tool commitments per
	// tool suffix declared in the most recent successful confirm_audit. Finish
	// is refused while any count is > 0. Counting (not a bool) enables batch
	// flows like multi-deal creation.
	committedCriticalActions map[string]int

	// criticalToolFailureAttempts counts unsuccessful executions per
	// (toolName + argsHash) so a deterministically-broken critical call can't
	// loop endlessly under one audit envelope.
	criticalToolFailureAttempts map[string]int

	// verifierRan records whether the end-of-run verifier pass already fired.
	verifierRan bool

	// ── repeat-call loop guard (both modes) ──
	lastCallKey     string
	lastCallRepeats int
	loopGuardTrips  int
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

	// noteProposer stages agent-proposed admin-notes edits (BOTH modes), unlike
	// memoryProposer which is interactive-only. Wired by the drivers via
	// setNoteProposer; nil leaves propose_note reporting "not wired".
	noteProposer NoteProposer

	// ── task tracker (scheduled finish enforcement) ──
	taskTrackerUsed   bool
	latestTaskTracker taskTrackerSnapshot

	// ── ceilings (interactive); zero means unlimited ──
	maxCostUSD     float64
	maxTotalTokens int

	// ── step / usage tracking ──
	step       int
	logSession *LogSession

	// usage counters (chat surfaced these on orch; scheduled mirrors into
	// logSession). Both are maintained so either Observer can read them.
	PromptTokens        int
	LastStepInputTokens int
	CompletionTokens    int
	CachedTokens        int
	CacheCreationTokens int
	CostUSD             float64

	// ── max iterations (informational; the loop owns the real cap) ──
	maxIterations int
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

// MemoryProposer stages a memory proposal for user confirmation (interactive).
type MemoryProposer interface {
	Propose(content string) (proposalID string, err error)
}

// newOrchestrationState matches cutlass's constructor signature (the one the
// lifted parity tests call). The interactive driver layers on ceilings +
// approval hooks via the setters below.
func newOrchestrationState(logSession *LogSession, maxIterations int) *orchestrationState {
	return &orchestrationState{
		sentEmailFingerprints:       make(map[string]struct{}),
		committedCriticalActions:    make(map[string]int),
		criticalToolFailureAttempts: make(map[string]int),
		logSession:                  logSession,
		maxIterations:               maxIterations,
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
		return true, "MEMORY_PROPOSAL_FAILED: memory proposer is not wired. This is a bug."
	}
	var args struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(rawInput), &args); err != nil {
		return true, fmt.Sprintf("MEMORY_PROPOSAL_FAILED: invalid arguments (%v).", err)
	}
	id, err := o.memoryProposer.Propose(args.Content)
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
		return true, "NOTE_PROPOSAL_FAILED: note proposer is not wired. This is a bug."
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

// hasUnresolvedToolPlaceholder detects ${tool:…} binding tokens the model
// occasionally invents; never intentional content.
func hasUnresolvedToolPlaceholder(rawInput string) bool {
	return strings.Contains(rawInput, "${tool:") || strings.Contains(rawInput, "${TOOL:")
}

// ── usage accounting (both modes) ──

// updateUsage records token usage and cost from a fantasy step. Maintains both
// the orch-level counters (chat's UI footer) and the logSession accumulators
// (scheduled captain's-log).
func (o *orchestrationState) updateUsage(usage fantasy.Usage, metadata fantasy.ProviderMetadata) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.PromptTokens += int(usage.InputTokens)
	o.LastStepInputTokens = int(usage.InputTokens)
	o.CompletionTokens += int(usage.OutputTokens)
	o.CachedTokens += int(usage.CacheReadTokens)
	o.CacheCreationTokens += int(usage.CacheCreationTokens)

	cost := openrouterCost(metadata)
	if cost != nil {
		o.CostUSD += *cost
	}

	if o.logSession != nil {
		o.logSession.mu.Lock()
		o.logSession.PromptTokens += int(usage.InputTokens)
		o.logSession.CompletionTokens += int(usage.OutputTokens)
		o.logSession.CachedTokens += int(usage.CacheReadTokens)
		o.logSession.CacheCreationTokens += int(usage.CacheCreationTokens)
		o.logSession.LastStepPromptTokens = int(usage.InputTokens + usage.CacheReadTokens)
		if cost != nil {
			o.logSession.Cost += *cost
		}
		o.logSession.mu.Unlock()
	}
}

// recordToolResult updates tracking state after a tool call completes. Handles
// both interactive email accounting and scheduled critical-action discharge.
func (o *orchestrationState) recordToolResult(toolName, rawInput, resultText string, succeeded bool) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if isEmailTool(toolName) && succeeded {
		if sendEmailSucceeded(strings.TrimSpace(resultText)) {
			o.sendEmailSuccessCount++
			o.sentEmailFingerprints[emailDedupKey(rawInput)] = struct{}{}
			log.Printf("send_email queued successfully (%d/%d)", o.sendEmailSuccessCount, maxSendEmailCallsPerTask)
		}
	}

	if isCriticalTool(toolName) {
		argsHash := hashString(rawInput)
		if succeeded {
			delete(o.criticalToolFailureAttempts, retryBudgetKey(toolName, argsHash))
			for i, p := range o.pendingCriticalActions {
				if p.toolName == toolName && p.argsHash == argsHash {
					log.Printf("Critical action succeeded: %s", toolName)
					o.completedCriticalActions = append(o.completedCriticalActions, toolName)
					o.pendingCriticalActions = append(o.pendingCriticalActions[:i], o.pendingCriticalActions[i+1:]...)
					break
				}
			}
			if len(o.pendingCriticalActions) == 0 {
				o.selfAuditRequested = true
			}
			o.markCommittedExecuted(toolName)
		} else {
			if o.criticalToolFailureAttempts == nil {
				o.criticalToolFailureAttempts = make(map[string]int)
			}
			key := retryBudgetKey(toolName, argsHash)
			o.criticalToolFailureAttempts[key]++
			log.Printf("Critical action failed: %s (attempt %d/%d for these args)",
				toolName, o.criticalToolFailureAttempts[key], maxAttemptsPerCriticalAction)
		}
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

// ── shared sort helper for finish-enforcement messaging ──
var _ = sort.Strings
