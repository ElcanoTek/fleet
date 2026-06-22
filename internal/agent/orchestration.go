package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openrouter"

	"github.com/ElcanoTek/fleet/internal/tools"
)

// orchestrationState tracks the per-turn enforcement and usage state.
//
// The chat server runs multi-turn and interactive, so most of cutlass's
// one-shot audit-gating logic is intentionally absent. What remains:
//   - Email rate-limit + duplicate detection (a model stuck in a loop should
//     not spam a real human's inbox).
//   - Repeat-call loop guard: consecutive identical (tool, args) calls are
//     cut off after a small budget, because an information-free result (an
//     empty run_python stdout, say) makes a low-temperature model resample
//     the exact same call forever (OMC conv 95697a52: 34 identical calls).
//   - Human-approval gate for send_email: the tool is staged in the
//     approvals table and blocked until the user clicks Send in the UI.
//   - Token and cost tracking for the UI footer + guardrail enforcement.
type orchestrationState struct {
	mu sync.Mutex

	// email safety
	sendEmailSuccessCount int
	sentEmailFingerprints map[string]struct{}

	// repeat-call loop guard (see checkRepeatedCall)
	lastCallKey     string
	lastCallRepeats int
	loopGuardTrips  int

	// approval staging (per-turn hook)
	approvalSink ApprovalStager

	// memory proposal staging (per-turn hook)
	memoryProposer MemoryProposer

	// usage
	//
	// PromptTokens is the SUM of `usage.InputTokens` across every model
	// call (step) within a single user turn. fantasy fires one stream
	// per step in a tool-using loop, and each one returns its own
	// input-token count — accumulating them here gives the right number
	// for cost telemetry (billing IS per-step input tokens) but is
	// MISLEADING as a context-window-usage indicator: a 9-step agentic
	// turn can easily report 800K "prompt tokens" even though no single
	// step's input ever exceeded ~200K, because step N's input includes
	// every earlier message in the conversation. Dividing this sum by
	// the model's context_length produced an impossible "200%" indicator
	// in production (Jeanne, conv 3460d911).
	//
	// LastStepInputTokens is the correct signal for the context-window
	// indicator: the SINGLE most recent step's input size, overwritten
	// per step rather than accumulated. For a tool-using turn, this
	// converges on the final step's prompt (always the largest, since
	// conversation history grows monotonically within a turn), so it
	// matches "how much room is left for the next turn."
	PromptTokens        int
	LastStepInputTokens int
	CompletionTokens    int
	CachedTokens        int
	CacheCreationTokens int
	CostUSD             float64

	// ceilings — zero means unlimited.
	maxCostUSD     float64
	maxTotalTokens int
}

// ApprovalStager is the narrow interface the orchestration layer uses to
// stage a critical tool call for user approval. RunTurn wires up an
// implementation that persists to the approvals table and emits an SSE
// event on the live turn.
type ApprovalStager interface {
	// Stage records a pending approval for a tool call. toolCallID is the
	// agent-assigned id of the tool_call event in the conversation history,
	// so the post-approval resolver can write the real result back under
	// the same id (otherwise the chip is left displaying APPROVAL_REQUIRED
	// after a reload). Empty toolCallID is allowed for native paths that
	// don't have one handy, but every caller in fantasy.go should pass it.
	Stage(toolName, toolCallID, rawInput string) (approvalID string, err error)

	// StageSuggestion stages a suggest_advanced_model approval if the
	// per-conversation gate allows it. Returns:
	//   - approvalID: empty string when the suggestion is suppressed
	//     (already on advanced model, an approved suggestion already
	//     exists, or we're inside the user-turn cooldown after a recent
	//     dismissal).
	//   - msg: the agent-facing explanation, always populated. When the
	//     ID is empty, the message tells the model why we suppressed
	//     and instructs it not to retry. When the ID is set, the message
	//     is the SUGGESTION_DISPLAYED hand-off identical in shape to
	//     PREVIEW_DISPLAYED.
	StageSuggestion(reason string) (approvalID, msg string, err error)
}

// MemoryProposer is the narrow interface the orchestration layer uses to
// stage a memory proposal for user confirmation. When the model calls
// propose_memory, the tool result is intercepted and routed through this
// proposer, which creates a pending memory row and emits an SSE event.
type MemoryProposer interface {
	Propose(content string) (proposalID string, err error)
}

func newOrchestrationState() *orchestrationState {
	return &orchestrationState{
		sentEmailFingerprints: make(map[string]struct{}),
	}
}

// setCeilings configures the per-turn guardrails. Called from RunTurn so the
// ceilings come from Config rather than being hardcoded here.
func (o *orchestrationState) setCeilings(maxCostUSD float64, maxTotalTokens int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.maxCostUSD = maxCostUSD
	o.maxTotalTokens = maxTotalTokens
}

// checkCeilings returns (blocked, reason). Called at every tool-call
// boundary so runaway turns are stopped before the next paid step.
func (o *orchestrationState) checkCeilings() (bool, string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.maxCostUSD > 0 && o.CostUSD >= o.maxCostUSD {
		return true, fmt.Sprintf("COST_CEILING_REACHED: this turn has accumulated $%.4f which meets or exceeds the configured ceiling of $%.2f. Stop calling tools and end the turn with what you have.",
			o.CostUSD, o.maxCostUSD)
	}
	if o.maxTotalTokens > 0 {
		// Count REAL (uncached) tokens, not the same context re-billed every
		// step. PromptTokens is the cumulative GROSS input across all steps, so
		// a long tool loop sums the (largely identical) prompt prefix dozens of
		// times — and with prompt caching most of that is CacheReadTokens, which
		// is nearly free and represents no new context pressure. Counting it made
		// a cheap ~50-step turn read as "4M tokens" and trip a ceiling it never
		// economically deserved. Subtracting cached reads leaves uncached input +
		// output, which actually tracks work and cost. Cost (MaxCostUSD) is the
		// primary guardrail; this is a backstop for when cost telemetry is
		// missing (CostUSD stays 0). Set CHAT_MAX_TOTAL_TOKENS=0 to disable.
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
// Three is deliberately lenient — a legitimate retry-after-transient-failure
// plus a verification re-run fit inside it — while a degenerate resample loop
// (Gemini Flash at temp 0.3 replaying an empty-output run_python; OMC conv
// 95697a52 burned 34 identical calls and $1.69 before the user hit Stop)
// trips it on the fourth iteration.
const maxConsecutiveIdenticalCalls = 3

// checkRepeatedCall is the repeat-call loop guard. Every tool execution (native
// AND MCP) routes through it BEFORE running, so it both tracks the call
// sequence and gates degenerate repeats. Returns (blocked, msg).
//
// Mechanism: an identical (tool, args) call consecutively repeated more than
// maxConsecutiveIdenticalCalls times is blocked with a guard message instead
// of executing. The message embeds the running block count, so each blocked
// result is textually distinct — that matters: the loop exists because the
// model's context stopped gaining new tokens, and a byte-identical guard
// message would itself become part of a new fixed point. Any call with a
// different tool or different arguments resets the window.
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
	// Greppable, so ops can see which tools models get stuck on.
	log.Printf("Enforcement: loop guard blocked %s — %d consecutive identical calls (cap %d, trip %d)",
		toolName, o.lastCallRepeats, maxConsecutiveIdenticalCalls, o.loopGuardTrips)
	return true, fmt.Sprintf("LOOP_GUARD (block #%d): this exact %s call with these exact arguments has now been issued %d times in a row (execution cap: %d). Re-running identical code cannot produce new information. Change your approach: print() or inspect intermediate values, write your work to a workspace file, alter the arguments — or stop calling tools and reply to the user with what you have.",
		o.loopGuardTrips, toolName, o.lastCallRepeats, maxConsecutiveIdenticalCalls)
}

const maxSendEmailCallsPerTurn = 3

// isEmailSendTool matches send_email variants. Guard against accidental fan-out.
func isEmailSendTool(toolName string) bool {
	return toolName == "send_email" || strings.HasSuffix(toolName, "_send_email")
}

// checkEmailSafety intercepts send_email calls and either blocks (rate limit,
// dedup) or stages them for user approval. Returns (blocked, reason). When
// approval is required, we return a "blocked" response so fantasy treats
// the staged-but-not-fired call as a tool result — the model sees a
// structured response that says "APPROVAL_REQUIRED" and stops trying to
// send until the user clicks Send in the UI and a new turn runs.
func (o *orchestrationState) checkEmailSafety(toolName, toolCallID, rawInput string) (bool, string) {
	if !isEmailSendTool(toolName) {
		return false, ""
	}
	// Reject the same ${tool:…} placeholders preview_email catches —
	// otherwise SendGrid's own HTML validator catches them further
	// down, but only after we've staged an approval with garbage in
	// the preview.
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
	fp := hashString(rawInput)
	if _, dup := o.sentEmailFingerprints[fp]; dup {
		return true, "Safety guard: identical send_email payload already sent in this turn."
	}

	// Human-in-the-loop approval: stage the call and tell the model we're
	// waiting on the user. The model should stop retrying and end its turn
	// with a short confirmation summary; the user clicks Send in the UI to
	// actually fire the send on a subsequent, out-of-band request.
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

// checkBashSafety intercepts bash tool calls that match risky patterns
// (shared-state changes the user should consciously OK) and stages them
// for user approval. Returns (blocked, reason). Applies only when
// toolName == "bash"; other native tools pass through. The hard-blocked
// patterns (sudo, rm -rf /, etc.) are caught deeper in runBash — the
// approval gate here is for merely RISKY actions.
func (o *orchestrationState) checkBashSafety(toolName, toolCallID, rawInput string) (bool, string) {
	if toolName != "bash" {
		return false, ""
	}
	risky, reason := classifyRiskyBash(rawInput)
	if !risky {
		return false, ""
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.approvalSink != nil {
		id, err := o.approvalSink.Stage(toolName, toolCallID, rawInput)
		if err != nil {
			log.Printf("approval stage failed: %v", err)
			return true, fmt.Sprintf("APPROVAL_REQUIRED: %s. Could not stage for user approval (%v).", reason, err)
		}
		return true, fmt.Sprintf("APPROVAL_REQUIRED: %s — staged for user approval (approval_id=%s). Do NOT retry. Summarize intent and wait for the user to click Approve.", reason, id)
	}
	// No sink wired (should not happen in prod). Fail closed.
	return true, fmt.Sprintf("APPROVAL_REQUIRED: %s, but approval sink is unavailable.", reason)
}

// checkMemoryProposal intercepts propose_memory calls, creates a pending
// memory proposal via MemoryProposer, and returns a blocking response so
// the model stops and asks the user for confirmation.
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

// checkPreviewEmailSafety always stages a preview_email call for user
// approval and returns a blocking response. The tool is preview-only:
// there is no path that actually sends, so every call goes through the
// approval flow regardless of content. Keeps the contract symmetric
// with send_email and email-rate-limited bash.
func (o *orchestrationState) checkPreviewEmailSafety(toolName, toolCallID, rawInput string) (bool, string) {
	if toolName != "preview_email" {
		return false, ""
	}
	// Reject unresolved ${tool:…} placeholders BEFORE staging the
	// approval. The run_python tool used to advertise a binding
	// syntax that nothing actually resolves; if the model trusts that
	// docstring and writes `${tool:call_id.vars.content}`, the user
	// sees the literal string in the preview card. Give the model a
	// clear error instead so it can retry with the real content.
	if hasUnresolvedToolPlaceholder(rawInput) {
		return true, "preview_email argument contains an unresolved ${tool:…} placeholder. The agent runtime does NOT substitute that syntax; paste the actual value into the tool arguments instead."
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.approvalSink == nil {
		return true, "PREVIEW_FAILED: preview_email ran but the preview sink is not wired. This is a bug."
	}
	id, err := o.approvalSink.Stage(toolName, toolCallID, rawInput)
	if err != nil {
		log.Printf("preview stage failed (preview_email): %v", err)
		return true, fmt.Sprintf("PREVIEW_FAILED: could not render preview for display (%v).", err)
	}
	return true, fmt.Sprintf("PREVIEW_DISPLAYED: the user is now viewing your draft in an inbox-style preview card (preview_id=%s). Nothing was sent and no approval is needed. The card has a Dismiss button ONLY — there is no Send button. Do NOT tell the user to \"click Send\" or \"approve\" the card. Instead, describe what you drafted in your reply and wait for the user's next instruction. If they want changes, revise and call preview_email again. If they say \"send it\", call mcp_sendgrid_send_email.", id)
}

// checkSuggestAdvancedSafety intercepts suggest_advanced_model calls. The
// tool has no execution path of its own — the staged approval card *is*
// the feature, mirroring preview_email. The stager owns the per-
// conversation gate (already-on-advanced, prior-approved, cooldown)
// and returns either an approval ID + SUGGESTION_DISPLAYED message or
// an empty ID + suppression reason. Either way we block here so the
// model stops talking and waits for the user.
func (o *orchestrationState) checkSuggestAdvancedSafety(toolName, rawInput string) (bool, string) {
	if toolName != tools.SuggestAdvancedModelToolName {
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
		return true, "SUGGESTION_FAILED: suggest_advanced_model ran but the approval sink is not wired. This is a bug."
	}
	id, msg, err := o.approvalSink.StageSuggestion(args.Reason)
	if err != nil {
		log.Printf("suggestion stage failed: %v", err)
		return true, fmt.Sprintf("SUGGESTION_FAILED: could not stage suggestion (%v).", err)
	}
	if id == "" {
		// Suppressed by gate. The msg already explains why and tells the
		// model not to retry; pass it through verbatim.
		return true, msg
	}
	return true, msg
}

// hasUnresolvedToolPlaceholder detects the ${tool:…} binding tokens
// the model occasionally invents (run_python's legacy docstring
// described a resolver that never shipped). We treat any occurrence in
// tool args as an error — it's never intentional content.
func hasUnresolvedToolPlaceholder(rawInput string) bool {
	return strings.Contains(rawInput, "${tool:") || strings.Contains(rawInput, "${TOOL:")
}

// classifyRiskyBash returns (risky, reason) for a bash tool input. Reason
// is shown to the user in the approval card.
func classifyRiskyBash(rawInput string) (bool, string) {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(rawInput), &args); err != nil {
		return false, ""
	}
	// Case-insensitive substring match on the raw command.
	c := strings.ToLower(args.Command)

	// git push — visible to other humans, effectively irreversible.
	if strings.Contains(c, "git push") {
		return true, "git push to a remote"
	}

	// System package managers — mutate the host beyond the workspace.
	// We match the install/remove/update verb so `dnf list` stays free.
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

// recordToolResult updates email-safety state after a tool call completes.
func (o *orchestrationState) recordToolResult(toolName, rawInput, resultText string, succeeded bool) {
	if !isEmailSendTool(toolName) {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()

	if succeeded && sendEmailSucceeded(resultText) {
		o.sendEmailSuccessCount++
		o.sentEmailFingerprints[hashString(rawInput)] = struct{}{}
		log.Printf("send_email queued successfully (%d/%d this turn)", o.sendEmailSuccessCount, maxSendEmailCallsPerTurn)
	}
}

// openrouterCost extracts the USD cost from OpenRouter's provider metadata.
func openrouterCost(metadata fantasy.ProviderMetadata) *float64 {
	raw, ok := metadata[openrouter.Name]
	if !ok {
		return nil
	}
	opts, ok := raw.(*openrouter.ProviderMetadata)
	if !ok {
		return nil
	}
	return &opts.Usage.Cost
}

// sendEmailSucceeded looks at SendGrid's JSON response for a 2xx status_code.
// Kept deliberately loose: any non-error-looking response counts as success.
func sendEmailSucceeded(resultText string) bool {
	t := strings.TrimSpace(resultText)
	if t == "" {
		return false
	}
	low := strings.ToLower(t)
	if strings.Contains(low, `"error"`) || strings.HasPrefix(low, "error:") {
		return false
	}
	return strings.Contains(low, `"status_code": 202`) ||
		strings.Contains(low, `"status_code":202`) ||
		strings.Contains(low, `"status_code": 200`) ||
		strings.Contains(low, `"status_code":200`)
}

func hashString(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:16]
}
