package agentcore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"slices"
	"strings"

	"charm.land/fantasy"
)

// Elision before the summary (#1742). When a scheduled run's compaction fires
// (the resend-budget checkpoint, or the opted-in window compaction), the run
// first replaces the bulky tool results outside a recent window with the
// stub the near-overflow reducer already builds (aggregateResultEnvelope: the
// head and tail of the output plus how to get it back), and buys the LLM
// summary only if that did not free enough. In a data-heavy run the old tool
// payloads are most of what is resent, and a summary of them costs a model
// call (about 7K completion tokens, docs/SCHEDULED-COMPACTION-CHECKPOINTS.md)
// where a stub costs nothing.
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
	// elisionMinSavingFraction is the least an elision must save, as a share of
	// the budget, to be made at all. Less cannot skip the summary, and is not
	// worth rewriting the history and recording an event for.
	elisionMinSavingFraction = 0.02
	// elisionStubBytes caps a whole stub, its preview included (about half),
	// at the near-overflow reducer's preview size.
	elisionStubBytes = innerResultPreviewBytes
)

// elisionKeptTools are the tools whose results are never elided: what they
// return is a conclusion the run continues from, not data it can fetch again.
// spawn_subagent (internal/agent) returns a child run's answer, which only a
// whole new child run could reproduce; task_tracker returns the plan the
// finish gate holds the run to (a summary re-announces it after a compaction,
// an elision does not). A2A peer tools are named per peer (<peer>_<op>), so
// they are not listed: their stubs say to re-run only a tool that reads data.
var elisionKeptTools = map[string]bool{"spawn_subagent": true, toolNameTaskTracker: true}

// elisionResult is what elideStaleToolResults did.
type elisionResult struct {
	messages    []fantasy.Message // the history with the stubs, or the input when none
	stubbedKeys []string          // the identity (callKey) of each stubbed result's call
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
// Media results are left alone for the same reason, and elisionKeptTools'
// results always.
func elideStaleToolResults(messages []fantasy.Message, recentTokens int) elisionResult {
	res := elisionResult{messages: messages}
	head := compactionHeadLen(messages)
	keepFrom := recentWindowStart(messages, head, recentTokens)
	if keepFrom <= head {
		return res
	}
	var out []fantasy.Message
	for mi := head; mi < keepFrom; mi++ {
		var parts []fantasy.MessagePart
		for pi, part := range messages[mi].Content {
			p, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
			if !ok {
				continue
			}
			call, found := callBefore(messages, mi, pi, p.ToolCallID)
			if elisionKeptTools[call.ToolName] {
				continue
			}
			stub, text, ok := elisionStub(p.Output, call.ToolName)
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
			if found {
				res.stubbedKeys = append(res.stubbedKeys, callKey(call.ToolName, call.Input))
			}
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

// callBefore is the call the tool result at messages[mi].Content[pi] answers.
// Not just any call with its ID: some providers reuse one ID across steps, or
// even within one parallel step, so a map from ID to call would name another
// call's tool, and could stub a result elisionKeptTools keeps. The call is in
// the nearest message before mi that has calls with that ID; when that message
// has several, the result takes the one in its own position among the results
// with that ID since that message (the k-th result answers the k-th call).
func callBefore(messages []fantasy.Message, mi, pi int, id string) (fantasy.ToolCallPart, bool) {
	for i := mi - 1; i >= 0; i-- {
		var calls []fantasy.ToolCallPart
		for _, part := range messages[i].Content {
			if tc, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok && tc.ToolCallID == id {
				calls = append(calls, tc)
			}
		}
		if len(calls) == 0 {
			continue
		}
		k := 0
		for j := i + 1; j <= mi; j++ {
			for q, part := range messages[j].Content {
				if j == mi && q >= pi {
					break
				}
				if tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok && tr.ToolCallID == id {
					k++
				}
			}
		}
		return calls[min(k, len(calls)-1)], true
	}
	return fantasy.ToolCallPart{}, false
}

// elisionRecovery is a stub's recovery action when the output was not saved
// to a governed artifact. The near-overflow reducer's "Re-run <tool> with
// narrower output" is wrong advice for a call with side effects, and elision
// stubs results routinely, so the stub says the call is done and to re-run
// only a tool that reads.
const elisionRecovery = "This earlier result was shortened to save context, and the call is done: do not repeat it just to see this again. " +
	"If you need more of it and the tool only reads data, re-run it with narrower output."

// elisionStub is the stub for one tool-result output and the text it replaces,
// or ok=false when the output is left as it is: media, a result the in-round
// guard already stubs, or one no longer than its own stub would be.
func elisionStub(output fantasy.ToolResultOutputContent, toolName string) (stub, text string, ok bool) {
	if _, isMedia := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](output); isMedia {
		return "", "", false
	}
	text, ok = toolResultOutputText(output)
	if !ok || len(text) <= elisionStubBytes || len(text) > HardMaxToolOutputBytes {
		return "", "", false
	}
	stub = aggregateResultEnvelopeWithRecovery(toolName, text, elisionStubBytes, elisionRecovery)
	if len(stub) >= len(text) {
		return "", "", false
	}
	return stub, text, true
}

// recentWindowStart is the index of the first message of the recent window:
// the newest messages whose estimated size as sent (wireTokens) fits
// recentTokens, extended to at least elisionMinRecentExchanges assistant
// turns, and moved back off any tool-role message onto the assistant message
// that made those calls, so an exchange is never split between the window and
// what is elided. Counted as stored, one recent screenshot would end the
// window at itself and leave the results around it to be stubbed.
func recentWindowStart(messages []fantasy.Message, head, recentTokens int) int {
	start := len(messages)
	tokens, exchanges := 0, 0
	for i := len(messages) - 1; i >= head; i-- {
		size := wireTokens(messages[i : i+1])
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

// lastAssistantIndex is the index of the latest assistant message, or
// len(messages) when there is none.
func lastAssistantIndex(messages []fantasy.Message) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == fantasy.MessageRoleAssistant {
			return i
		}
	}
	return len(messages)
}

// unseenTokens estimates what a provider-measured resent size does not yet
// include: the latest assistant message onward. The size is that call's
// input, and the call's own output (its assistant message and tool results),
// plus anything the run appended after it, are in the history the next call
// will send. When the last step's assistant message carried only reasoning,
// the replay drops it and this counts the step before as well: an
// over-estimate, which can only make the summary run, never skip it.
func unseenTokens(messages []fantasy.Message) int {
	if i := lastAssistantIndex(messages); i < len(messages) {
		return estimateModelMessagesTokens(messages[i:])
	}
	return 0
}

// Bounds on historyTokenRate: past them the measurement is degenerate (a
// history of a few tokens, a size taken across a compaction) rather than a
// property of the data. Measured through OpenRouter on 2026-10-21, the rate
// depends on the model's tokenizer as much as on the data: prose 1.1
// (gemini-3.8-flash) to 3.0 (gpt-6-luna-pro); comma-separated 4-digit
// numbers 2.4 (claude-haiku-5.5), 4.0 (gemini) and 7.3 (gpt-6-luna-pro);
// base64 2.8 (gemini) to 8.2 (gpt-6-luna-pro). An earlier cap of 4 was below
// Luna's rate on such output; its live runs measured 4.5-6.8.
const (
	minHistoryTokenRate = 0.5
	maxHistoryTokenRate = 12.0
)

// historyTokenRate is how many tokens the provider counts per token the
// in-round guard's estimator (about 4 characters a token) counts, for this
// run's history: the measured resent size used, less the prefix (the
// smallest floor, measured before any history existed, #1600), over the
// estimate of the history that call carried as it was sent (everything after
// the pinned head up to the latest assistant message, see unseenTokens and
// wireTokens). It is 1 when there is nothing to calibrate against: used is
// itself an estimate, or no floor has been measured.
//
// Providers count very differently from the estimator, by model as much as by
// data, and dense output (digits, punctuation, base64) costs far more per
// character (see minHistoryTokenRate for measured rates). A live Haiku run of
// numeric run_python output measured about 2.3: with an uncalibrated estimate
// a 30%-of-budget recent window held some 60% of the budget, and the stubs'
// savings looked half their size.
//
// Four shapes it does not see, each bounded:
//   - The rate is one average over the history. When the old results that
//     get elided cost the provider fewer tokens per character than the rest
//     (prose first, dense data later), their savings are over-counted.
//   - The prefix is the one measured first. A mid-run tool-roster rebuild or
//     a fallback model's tokenizer that grows the real prefix is taken for
//     history, shifting the estimate by part of that growth.
//   - wireTokens mirrors only the in-round guard's unconditional reduction.
//     When the call that fired was also trimmed to fit the model's input
//     allowance (compactOldToolResults, near the window: the window path, not
//     the resend budget), used holds the trimmed history and the rate is low.
//   - The measured span can hold less than the call that fired sent:
//     reasoning resent within a round, which the carried history drops, or a
//     step cut off by length, whose assistant message the replay drops
//     (messagesForReplay), so the span ends a step early. Then the rate is
//     high, not low.
//
// In the first three, the estimate after elision can come out low, and the
// run pauses again a step or two later; that pause cannot free enough by
// elision, so the summary runs. One extra pause, no more: the next one
// re-measures the rate. In the fourth, the estimate comes out high and the
// window small: more results are stubbed, and a summary may run that elision
// alone would have spared. It never lets a call past the budget.
func (e *engine) historyTokenRate(messages []fantasy.Message, used int, measured bool) float64 {
	if !measured || e.resendPrefix <= 0 || used <= e.resendPrefix {
		return 1
	}
	head := compactionHeadLen(messages)
	seenEnd := max(head, lastAssistantIndex(messages))
	est := wireTokens(messages[head:seenEnd])
	if est <= 0 {
		return 1
	}
	return min(maxHistoryTokenRate, max(minHistoryTokenRate, float64(used-e.resendPrefix)/float64(est)))
}

// wireTokens is estimateModelMessagesTokens of messages as the in-round guard
// sends them: a media result, and a result or tool-call input over the hard
// cap, go out as their stubs on every call (reduceHistoricalPayloadsToHardCap),
// so the provider's count holds the stub, not the payload. Counting the
// payload would dilute the rate: one large screenshot in the history would
// make dense results look cheap, and the recent window grow past the budget.
func wireTokens(messages []fantasy.Message) int {
	sent := cloneFantasyMessages(messages)
	reduceHistoricalPayloadsToHardCap(sent, toolNamesByCallID(sent))
	return estimateModelMessagesTokens(sent)
}

// elisionOutcome is elideBeforeSummary's result: the elision, the estimated
// resent size after it, whether that is enough to skip the summary, and the
// history token rate it was measured at.
type elisionOutcome struct {
	elisionResult
	usedAfter int
	enough    bool
	rate      float64
}

// elideBeforeSummary elides stale tool results from a scheduled run's history
// whose resent size used reached threshold, before any summary is bought.
// budget is what the trigger allows (the resend budget, or the window
// compaction point): the recent window is elisionRecentFraction of it, and
// the summary is skipped only when the estimated size after elision is at
// least elisionHeadroomFraction of it below threshold. measured reports
// whether used is the provider's count for the latest call rather than an
// estimate of the whole history.
//
// The window and the savings are sized in the provider's tokens, through
// historyTokenRate. The size after elision is then the measured prefix plus
// the elided history, as sent, at that rate; it includes what the paused step
// itself added, which used does not yet (unseenTokens). With no floor
// measured it falls back to used, less the savings, plus that unseen part,
// and never below the estimator's own count of the elided request.
//
// ok is false, and nothing changes, for an interactive engine (a chat's
// history is the user's to keep, and its windows are large), when the
// operator turned it off (FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY=0), or when it
// would save less than elisionMinSavingFraction of the budget. An elision is
// a clean reduction like proactiveCompact's: it resets the
// consecutive-compaction count, and the next step re-measures the
// resend-checkpoint floor (#1600).
func (e *engine) elideBeforeSummary(messages []fantasy.Message, used int, measured bool, threshold, budget int) (elisionOutcome, bool) {
	if !e.requireCompactionOptIn || budget <= 0 || !contextElideBeforeSummary(e.envPrefix) || e.elisionHeld() {
		return elisionOutcome{}, false
	}
	rate := e.historyTokenRate(messages, used, measured)
	res := elideStaleToolResults(messages, int(float64(budget)*elisionRecentFraction/rate))
	if res.stubs == 0 || float64(res.savedTokens)*rate < float64(budget)*elisionMinSavingFraction {
		return elisionOutcome{}, false
	}
	e.noteElidedCalls(res.stubbedKeys)
	e.consecutiveCompactions = 0
	e.resendFloorStale = true
	var after int
	switch {
	case !measured:
		after = e.estimateRequestTokens(res.messages)
	case e.resendPrefix > 0 && used > e.resendPrefix:
		history := wireTokens(res.messages[compactionHeadLen(res.messages):])
		after = e.resendPrefix + int(rate*float64(history))
	default:
		after = max(used-res.savedTokens+unseenTokens(res.messages), e.estimateRequestTokens(res.messages))
	}
	target := threshold - int(float64(budget)*elisionHeadroomFraction)
	return elisionOutcome{elisionResult: res, usedAfter: after, enough: after <= target, rate: rate}, true
}

// elisionHeld reports whether this run must not elide now, so that its
// compaction goes straight to the summary, which keeps the recent half of the
// history verbatim. Both cases were measured on a recurring scheduled task
// whose model fetched twelve ~25 KB reports and then audited its answer
// against them (docs/SCHEDULED-COMPACTION-CHECKPOINTS.md):
//
//   - While the run's self-audit is pending (selfAuditPending): that is when
//     a run re-reads the evidence for its answer, and a stub of the report it
//     is checking failed the audit. One model concluded the connector
//     truncates and aborted; another re-fetched the reports a few at a time,
//     and each pause stubbed the new ones again before the audit was done.
//   - For the rest of a run that re-ran a call whose result an elision had
//     stubbed (noteReruns): the stub cost that run a call, so eliding more is
//     not saving it anything. Its resend budget is raised too (resendBudget).
func (e *engine) elisionHeld() bool {
	if e.elisionStopped {
		return true
	}
	if e.runOrch.selfAuditPending() {
		log.Printf("context elision held: the run's self-audit is pending, so this compaction is the summary")
		return true
	}
	return false
}

// noteReruns sets elisionStopped once a call the run made since the last
// context-pressure check repeats one whose result an elision stubbed: the
// same tool with the same arguments (callKey). That also raises the run's
// resend budget (resendBudget), and noteRerunStop says so. It runs at the top
// of every check, before any rewrite. The new calls are told apart by position, past
// pressureCheckedLen, not by call ID: some providers reuse IDs. So a
// duplicate the run made before the elision is not a re-run. Positions hold
// because the history only grows between checks, except when, inside a
// round, the overflow recovery compacts it or a failed attempt's trailing
// message is dropped. A re-run there can be missed, and the run then goes on
// eliding, as it would without this stop.
func (e *engine) noteReruns(messages []fantasy.Message) {
	from := e.pressureCheckedLen
	if e.elisionStopped || len(e.elidedCallKeys) == 0 || from > len(messages) {
		return
	}
	for _, m := range messages[from:] {
		for _, part := range m.Content {
			tc, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part)
			if ok && e.elidedCallKeys[callKey(tc.ToolName, tc.Input)] {
				e.elisionStopped = true
				e.noteRerunStop(tc.ToolName)
				return
			}
		}
	}
}

// noteRerunStop records the re-run stop: a server log line, and, when the
// stop raised the run's resend budget (resendBudget), a
// [context_budget_raised] session-log breadcrumb with the budget it had and
// the one it has from now on. A run with no resend budget, or on a window
// too small for any raise, keeps its budget and gets no breadcrumb.
func (e *engine) noteRerunStop(tool string) {
	budget, raised := contextResendBudgetTokens(e.envPrefix), e.resendBudget()
	if raised <= budget {
		log.Printf("context elision off for the rest of the run: %s was re-run after its result was elided", tool)
		return
	}
	log.Printf("context elision off and the resend budget raised to %d for the rest of the run: %s was re-run after its result was elided", raised, tool)
	if e.logSession != nil {
		e.logSession.AddMessage(roleUser, fmt.Sprintf(
			"[context_budget_raised] tool=%s budget=%d raised_budget=%d — the run re-ran a call whose result an elision had replaced with a preview, a sign it needs more of its data at once than the budget holds: it stops eliding, and its resend budget is raised for the rest of the run",
			tool, budget, raised), nil, nil)
	}
}

// noteElidedCalls records the identities of an elision's stubbed calls, for
// noteReruns.
func (e *engine) noteElidedCalls(keys []string) {
	if e.elidedCallKeys == nil {
		e.elidedCallKeys = map[string]bool{}
	}
	for _, k := range keys {
		e.elidedCallKeys[k] = true
	}
}

// callKey is a call's identity: its tool name and its input, with a JSON
// input re-encoded so the same arguments in another key order or spacing
// match. Numbers keep their exact text (UseNumber), so two large record IDs
// never round to one key.
func callKey(tool, input string) string {
	dec := json.NewDecoder(strings.NewReader(input))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) == nil {
		if _, err := dec.Token(); errors.Is(err, io.EOF) {
			if canonical, err := json.Marshal(v); err == nil {
				input = string(canonical)
			}
		}
	}
	return tool + "\x00" + input
}

// noteElision records an elision the way a compaction is recorded: a
// [context_elided] breadcrumb in the session log, opening with what fired it
// (trigger, then crumbFields), and a fleet.context_compacted event with
// trigger "elision" and elision_trigger naming what fired it, built on
// payload (the firing trigger's own fields).
func (e *engine) noteElision(sink *streamSink, el elisionOutcome, used int, trigger, crumbFields string, payload map[string]any) {
	next := "that freed enough, so no summary was needed"
	if !el.enough {
		next = "that did not free enough, so the oldest half of the history is summarized next"
	}
	e.logSession.AddMessage(roleUser, fmt.Sprintf(
		"[context_elided] trigger=%s used=%d %s elided_results=%d elided_bytes=%d estimated_after=%d token_rate=%.2f — tool results older than the recent window were replaced with short previews; %s",
		trigger, used, crumbFields, el.stubs, el.elidedBytes, el.usedAfter, el.rate, next), nil, nil)
	payload[evtFieldTrigger] = "elision"
	payload[evtFieldElisionTrigger] = trigger
	payload[evtFieldUsedTokens] = used
	payload[evtFieldElidedResults] = el.stubs
	payload[evtFieldElidedBytes] = el.elidedBytes
	payload[evtFieldEstimatedAfter] = el.usedAfter
	payload[evtFieldHistoryTokenRate] = math.Round(el.rate*100) / 100
	sink.emit(evtContextCompacted, payload)
}
