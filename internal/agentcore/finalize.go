package agentcore

import (
	"context"

	"charm.land/fantasy"
)

// Interactive-only finalize hook (the chat leaked-tool-call retry +
// forced-final-summary recovery). This file defines the seam TYPES; the
// interactive driver in internal/agent supplies the real implementation
// (stripLeakedToolCalls + the nudge text live there). The loop calls the hook
// only when one is wired (scheduled mode passes nil).

// FinalizeInput is what the loop hands the finalize hook when a run is about to
// finish. The hook may produce recovered final text (e.g. after forcing a
// summary out of a model that ended with tool calls and no prose).
type FinalizeInput struct {
	Mode      Mode
	FinalText string
	// Messages is the conversation as it stands when the run finishes: the
	// finishing round's INPUT plus that round's completed assistant/tool
	// transcript (carryRoundMessages — the same carry the enforcement loop and
	// the terminal structured-output phase use). A recovery call replays it so
	// it sees the current user question and the tool results it is asked to
	// write up; the round input alone lacks the tool transcript (it lives in
	// the round's AgentResult.Steps), which left the forced summary fabricating
	// from stale context (#1117).
	Messages []fantasy.Message
	// RoundToolEvents counts the tool call/result events the finishing round
	// committed to the run sink (0 = the round produced text/reasoning only).
	// A finalize retry that re-drives from Messages with the governed roster
	// could re-issue calls the round already executed and repeat their side
	// effects — the exact hazard ADR-0035's side-effect mark gate suppresses
	// in streamRoundWithResilience — so a hook must treat any committed tool
	// event as "no blind re-drive" and degrade to a tool-less recovery path.
	RoundToolEvents int
	// Tools is the already-governed final roster used by the main run. A finalize
	// retry must reuse it rather than rebuilding raw driver tools outside policy,
	// credential, output-screening, and panic boundaries.
	Tools        []fantasy.AgentTool
	Observer     Observer
	SystemPrompt string
	// OnToolCall/OnToolResult route a finalize retry's tool events back into the
	// SAME run sink as the ordinary loop. A retry that executes (or contains a
	// panic from) a tool must persist one call/result pair and participate in the
	// run's side-effect gate; it cannot become an unaudited auxiliary loop.
	OnToolCall   fantasy.OnToolCallFunc
	OnToolResult fantasy.OnToolResultFunc
	// GuardStep wraps a finalize retry's PrepareStep with the run's
	// pre-completion cost/token ceiling check (the same budgetGuardedStep the
	// main loop streams under), so a recovery stream cannot keep buying paid
	// completions past the ceiling. Nil-safe: when unset the inner step runs
	// unguarded.
	GuardStep func(fantasy.PrepareStepFunction) fantasy.PrepareStepFunction
	// StopWhen carries the run's step-cap conditions (MaxIterations) into a
	// finalize retry's stream — the retry re-runs WITH tools, so without it a
	// model that keeps issuing (policy-blocked) tool calls loops paid steps
	// unboundedly.
	StopWhen []fantasy.StopCondition
	// RecordUsage meters a recovery model call's tokens/cost into the SAME run
	// accounting the main loop uses. It is a capability closure over the run's
	// orchestration state (the state itself never escapes Run), so a finalize
	// hook that makes its own model call (the interactive leaked-call retry /
	// forced summary) is not invisible to the cost chip. Nil-safe; the loop wires
	// it unconditionally, so this is NOT a mode branch in the trunk.
	RecordUsage UsageSink
}

// UsageSink records one model step's usage (+ provider metadata, which carries
// the OpenRouter cost) into the run accounting. See FinalizeInput.RecordUsage.
type UsageSink func(usage fantasy.Usage, metadata fantasy.ProviderMetadata)

// FinalizeHook is the interactive recovery hook. It returns recovered final text
// (empty to keep the loop's text) and an error. Scheduled mode passes nil.
type FinalizeHook func(ctx context.Context, in FinalizeInput) (recovered string, err error)
