package agentcore

import (
	"fmt"
	"slices"

	"charm.land/fantasy"
)

// Elision before the summary (#1742). When a scheduled run's compaction fires
// (the resend-budget checkpoint, or the opted-in window compaction), the run
// first replaces the bulky tool results outside a recent window with the
// stub the near-overflow reducer already uses (aggregateResultEnvelope: a
// preview plus how to get the full output back), and buys the LLM summary only
// if that did not free enough. In a data-heavy run the old tool payloads are
// most of what is resent, and a summary of them costs a model call (about 7K
// completion tokens, docs/SCHEDULED-COMPACTION-CHECKPOINTS.md) where a stub
// costs nothing.
//
// The stubs are written into the carried history at the moment the run
// already rewrites it, never per step: a mid-history rewrite on every step
// would miss the prompt cache every step. Only tool-result outputs change.
// Assistant text, tool-call inputs, the pinned head and any summary message
// are left byte-identical, and every result keeps its call ID, so each call
// stays paired with its result (#1106).
const (
	// elisionRecentFraction is the share of the budget kept verbatim as the
	// recent window, newest messages first.
	elisionRecentFraction = 0.3
	// elisionMinRecentExchanges is the fewest assistant turns the recent window
	// keeps, however large their results: the model's latest work stays whole.
	elisionMinRecentExchanges = 2
	// elisionHeadroomFraction is how far, as a share of the budget, elision
	// alone must bring the resent size below the threshold for the summary to
	// be skipped. Just under the threshold, the next step would pause again.
	elisionHeadroomFraction = 0.3
	// elisionPreviewBytes is the stub's preview: the near-overflow reducer's.
	elisionPreviewBytes = innerResultPreviewBytes
)

// elisionResult is what elideStaleToolResults did.
type elisionResult struct {
	messages    []fantasy.Message // the history with the stubs, or the input when none
	stubs       int               // tool results replaced
	elidedBytes int               // output bytes removed
	savedTokens int               // estimated tokens removed (estimateModelMessagesTokens' terms)
}

// elideStaleToolResults replaces each tool result that lies after the pinned
// head and before the recent window (recentWindowStart), and whose stub is
// shorter than it, with that stub. It copies what it changes: the caller's
// slice and the messages' Content are never written.
//
// A result over HardMaxToolOutputBytes is left alone: the in-round context
// guard already sends the provider the same stub in its place on every call
// (reduceHistoricalPayloadsToHardCap), so replacing it would save nothing.
// Media results are left alone for the same reason.
func elideStaleToolResults(messages []fantasy.Message, recentTokens int) elisionResult {
	res := elisionResult{messages: messages}
	head := compactionHeadLen(messages)
	keepFrom := recentWindowStart(messages, head, recentTokens)
	if keepFrom <= head {
		return res
	}
	toolNames := toolNamesByCallID(messages)
	var out []fantasy.Message
	for mi := head; mi < keepFrom; mi++ {
		var parts []fantasy.MessagePart
		for pi, part := range messages[mi].Content {
			p, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
			if !ok {
				continue
			}
			stub, text, ok := elisionStub(p.Output, toolNames[p.ToolCallID])
			if !ok {
				continue
			}
			if parts == nil {
				parts = slices.Clone(messages[mi].Content)
			}
			before := estimateToolResultTokens(p.Output)
			p.Output = replaceToolResultOutput(p.Output, stub)
			parts[pi] = p
			res.stubs++
			res.elidedBytes += len(text) - len(stub)
			res.savedTokens += before - estimateToolResultTokens(p.Output)
		}
		if parts != nil {
			if out == nil {
				out = slices.Clone(messages)
			}
			out[mi].Content = parts
		}
	}
	if out != nil {
		res.messages = out
	}
	return res
}

// elisionStub is the stub for one tool-result output and the text it replaces,
// or ok=false when the output is left as it is: media, a result the in-round
// guard already stubs, or one no longer than its own stub would be.
func elisionStub(output fantasy.ToolResultOutputContent, toolName string) (stub, text string, ok bool) {
	if _, isMedia := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](output); isMedia {
		return "", "", false
	}
	text, ok = toolResultOutputText(output)
	if !ok || len(text) <= elisionPreviewBytes || len(text) > HardMaxToolOutputBytes {
		return "", "", false
	}
	stub = aggregateResultEnvelope(toolName, text, elisionPreviewBytes)
	if len(stub) >= len(text) {
		return "", "", false
	}
	return stub, text, true
}

// recentWindowStart is the index of the first message of the recent window:
// the newest messages whose estimated size fits recentTokens, extended to at
// least elisionMinRecentExchanges assistant turns, and moved back off any
// tool-role message onto the assistant message that made those calls, so an
// exchange is never split between the window and what is elided.
func recentWindowStart(messages []fantasy.Message, head, recentTokens int) int {
	start := len(messages)
	tokens, exchanges := 0, 0
	for i := len(messages) - 1; i >= head; i-- {
		size := estimateModelMessagesTokens(messages[i : i+1])
		if exchanges >= elisionMinRecentExchanges && tokens+size > recentTokens {
			break
		}
		tokens += size
		if messages[i].Role == fantasy.MessageRoleAssistant {
			exchanges++
		}
		start = i
	}
	return max(head, snapCutBackward(messages, start))
}

// elisionOutcome is elideBeforeSummary's result: the elision, the estimated
// resent size after it, and whether that is enough to skip the summary.
type elisionOutcome struct {
	elisionResult
	usedAfter int
	enough    bool
}

// elideBeforeSummary elides stale tool results from a scheduled run's history
// whose resent size used reached threshold, before any summary is bought.
// budget is what the trigger allows (the resend budget, or the window
// compaction point): the recent window is elisionRecentFraction of it, and
// the summary is skipped only when the estimated size after elision is at
// least elisionHeadroomFraction of it below threshold. used is the provider's
// own count; the estimate subtracts the tokens the stubs removed from it.
//
// ok is false, and nothing changes, for an interactive engine (a chat's
// history is the user's to keep, and its windows are large), when the
// operator turned it off (FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY=0), or when no
// result was worth a stub. An elision is a clean reduction like
// proactiveCompact's: it resets the consecutive-compaction count, and the
// next step re-measures the resend-checkpoint floor (#1600).
func (e *engine) elideBeforeSummary(messages []fantasy.Message, used, threshold, budget int) (elisionOutcome, bool) {
	if !e.requireCompactionOptIn || budget <= 0 || !contextElideBeforeSummary(e.envPrefix) {
		return elisionOutcome{}, false
	}
	res := elideStaleToolResults(messages, int(float64(budget)*elisionRecentFraction))
	if res.stubs == 0 {
		return elisionOutcome{}, false
	}
	e.consecutiveCompactions = 0
	e.resendFloorStale = true
	after := max(0, used-res.savedTokens)
	target := threshold - int(float64(budget)*elisionHeadroomFraction)
	return elisionOutcome{elisionResult: res, usedAfter: after, enough: after <= target}, true
}

// noteElision records an elision the way a compaction is recorded: a
// [context_elided] breadcrumb in the session log, opening with crumbFields
// (what fired it), and a fleet.context_compacted event with trigger
// "elision", built on payload (the firing trigger's own fields).
func (e *engine) noteElision(sink *streamSink, el elisionOutcome, used int, crumbFields string, payload map[string]any) {
	next := "that freed enough, so no summary was needed"
	if !el.enough {
		next = "that did not free enough, so the oldest half of the history is summarized next"
	}
	e.logSession.AddMessage(roleUser, fmt.Sprintf(
		"[context_elided] %s elided_results=%d elided_bytes=%d estimated_after=%d — tool results older than the recent window were replaced with short previews that say how to get the full output back; %s",
		crumbFields, el.stubs, el.elidedBytes, el.usedAfter, next), nil, nil)
	payload[evtFieldTrigger] = "elision"
	payload[evtFieldUsedTokens] = used
	payload[evtFieldElidedResults] = el.stubs
	payload[evtFieldElidedBytes] = el.elidedBytes
	payload[evtFieldEstimatedAfter] = el.usedAfter
	sink.emit(evtContextCompacted, payload)
}
