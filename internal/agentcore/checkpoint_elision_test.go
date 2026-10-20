package agentcore

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
)

// Tests for eliding stale tool results before the compaction summary (#1742).

// elisionExchange is one assistant tool call and the tool message carrying its
// result, as fantasy carries them in a run's history.
func elisionExchange(id, input string, output fantasy.ToolResultOutputContent) []fantasy.Message {
	return []fantasy.Message{
		{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: id, ToolName: "fetch", Input: input}}},
		{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: id, Output: output}}},
	}
}

func elisionText(n int) fantasy.ToolResultOutputContent {
	return fantasy.ToolResultOutputContentText{Text: strings.Repeat("d", n)}
}

// elisionHistory is a pinned head, a summary, and size exchanges whose results
// are resultChars of text each.
func elisionHistory(exchanges, resultChars int) []fantasy.Message {
	msgs := make([]fantasy.Message, 0, 2+2*exchanges)
	msgs = append(msgs,
		fantasy.NewUserMessage("refresh every page"),
		fantasy.NewUserMessage(compactionSummaryPrefix+"] pages 1-3 already refreshed"),
	)
	for i := range exchanges {
		id := "page-" + string(rune('a'+i))
		msgs = append(msgs, elisionExchange(id, `{"page":"`+id+`"}`, elisionText(resultChars))...)
	}
	return msgs
}

func resultOf(t *testing.T, part fantasy.MessagePart) (fantasy.ToolResultPart, string) {
	t.Helper()
	tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
	if !ok {
		t.Fatalf("part %T is not a tool result", part)
	}
	text, _ := toolResultOutputText(tr.Output)
	return tr, text
}

// Only the bulky results before the recent window change. Every other part —
// the pinned head, the summary, assistant text, every tool-call input, and
// every result in the recent window — is byte-identical, every result keeps
// its call ID, and the caller's history is not written.
func TestElideStaleToolResults(t *testing.T) {
	history := make([]fantasy.Message, 0, 17)
	history = append(history,
		fantasy.NewUserMessage("refresh every page"),
		fantasy.NewUserMessage(compactionSummaryPrefix+"] pages 1-3 already refreshed"),
	)
	history = append(history, elisionExchange("big", `{"page":1}`, elisionText(8000))...)
	history = append(history, elisionExchange("failed", `{"page":2}`,
		fantasy.ToolResultOutputContentError{Error: errors.New(strings.Repeat("e", 6000))})...)
	history = append(history, elisionExchange("small", `{"page":3}`, elisionText(1500))...)
	history = append(history, elisionExchange("huge", `{"page":4}`, elisionText(HardMaxToolOutputBytes+1))...)
	history = append(history, elisionExchange("image", `{"page":5}`,
		fantasy.ToolResultOutputContentMedia{Data: strings.Repeat("i", 9000), MediaType: "image/png"})...)
	history = append(history, fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "Halfway there."}}})
	history = append(history, elisionExchange("recent-1", `{"page":6}`, elisionText(8000))...)
	history = append(history, elisionExchange("recent-2", `{"page":7}`, elisionText(8000))...)
	before := cloneFantasyMessages(history)

	// A zero budget for the window: it holds only the minimum two exchanges.
	res := elideStaleToolResults(history, 0)

	if !reflect.DeepEqual(history, before) {
		t.Fatal("the caller's history was written")
	}
	if res.stubs != 2 {
		t.Fatalf("stubs = %d, want 2 (the 8000-byte result and the 6000-byte error)", res.stubs)
	}
	if len(res.messages) != len(history) {
		t.Fatalf("messages = %d, want %d: elision never adds or drops a message", len(res.messages), len(history))
	}
	elided := map[string]bool{"big": true, "failed": true}
	wantBytes := 0
	for mi, m := range res.messages {
		if m.Role != history[mi].Role || len(m.Content) != len(history[mi].Content) {
			t.Fatalf("message %d changed shape", mi)
		}
		for pi, part := range m.Content {
			tr, isResult := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
			if !isResult || !elided[tr.ToolCallID] {
				if !reflect.DeepEqual(part, history[mi].Content[pi]) {
					t.Errorf("message %d part %d changed: only stale bulky results may", mi, pi)
				}
				continue
			}
			orig, origText := resultOf(t, history[mi].Content[pi])
			_, stub := resultOf(t, part)
			if tr.ToolCallID != orig.ToolCallID {
				t.Errorf("result %s lost its call ID", orig.ToolCallID)
			}
			if !strings.HasPrefix(stub, "[tool output truncated]\ntool: fetch\n") ||
				!strings.Contains(stub, "recovery_action: Re-run fetch with narrower output.") || !strings.Contains(stub, "\npreview:\n") {
				t.Errorf("result %s stub = %q, want the envelope with a preview and a recovery action", tr.ToolCallID, stub)
			}
			if len(stub) > elisionPreviewBytes {
				t.Errorf("result %s stub is %d bytes, want at most %d", tr.ToolCallID, len(stub), elisionPreviewBytes)
			}
			_, wasErr := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](orig.Output)
			_, isErr := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](tr.Output)
			if wasErr != isErr {
				t.Errorf("result %s changed between error and text", tr.ToolCallID)
			}
			wantBytes += len(origText) - len(stub)
		}
	}
	if res.elidedBytes != wantBytes {
		t.Errorf("elidedBytes = %d, want %d", res.elidedBytes, wantBytes)
	}
	if want := estimateModelMessagesTokens(history) - estimateModelMessagesTokens(res.messages); res.savedTokens != want {
		t.Errorf("savedTokens = %d, want the estimate's difference %d", res.savedTokens, want)
	}

	// A window that holds everything leaves nothing to elide.
	if all := elideStaleToolResults(history, 1_000_000); all.stubs != 0 || &all.messages[0] != &history[0] {
		t.Errorf("a window over the whole history elided %d results", all.stubs)
	}
}

// The pinned head is never elided, whatever it holds.
func TestElideStaleToolResults_NeverThePinnedHead(t *testing.T) {
	head := fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
		fantasy.ToolResultPart{ToolCallID: "head", Output: elisionText(9000)},
	}}
	history := append([]fantasy.Message{head}, elisionHistory(6, 8000)[2:]...)
	res := elideStaleToolResults(history, 0)
	if res.stubs == 0 {
		t.Fatal("fixture: nothing after the head was elided")
	}
	if !reflect.DeepEqual(res.messages[0], head) {
		t.Fatal("the pinned head was elided")
	}
}

func TestRecentWindowStart(t *testing.T) {
	msgs := elisionHistory(4, 4000) // head, summary, then 4 exchanges at indexes 2..9
	exchange := estimateModelMessagesTokens(msgs[2:4])
	if got := recentWindowStart(msgs, 1, 0); got != 6 {
		t.Errorf("zero budget: window starts at %d, want 6 (never fewer than two exchanges)", got)
	}
	if got := recentWindowStart(msgs, 1, 3*exchange); got != 4 {
		t.Errorf("room for three exchanges: window starts at %d, want 4", got)
	}
	if got := recentWindowStart(msgs, 1, 1_000_000); got != 1 {
		t.Errorf("room for everything: window starts at %d, want the head boundary 1", got)
	}

	// A boundary on a tool message moves back onto the assistant message that
	// made the calls: a result is never in the window without its call.
	wordy := elisionExchange("x", `{}`, elisionText(100))
	wordy[0].Content = append([]fantasy.MessagePart{fantasy.TextPart{Text: strings.Repeat("w", 20_000)}}, wordy[0].Content...)
	split := append([]fantasy.Message{fantasy.NewUserMessage("task")}, wordy...)
	split = append(split, elisionExchange("y", `{}`, elisionText(100))...)
	split = append(split, elisionExchange("z", `{}`, elisionText(100))...)
	fits := estimateModelMessagesTokens(split[2:]) // x's result and the two exchanges after it
	if got := recentWindowStart(split, 1, fits); got != 1 {
		t.Errorf("boundary on x's result: window starts at %d, want x's call at 1", got)
	}
}

// The issue's acceptance case: a run whose history is mostly large tool
// results gets back under the effective budget with zero summarizer calls.
// The fixture is TestRun_ResendCheckpoint_FloorAboveBudgetWaitsForABudgetOfHistory's
// (a 140K prefix over the 80K budget), with elision on.
func TestRun_Elision_ClearsTheBudgetWithoutASummary(t *testing.T) {
	t.Setenv("FLEET_SCHEDULED_AUTO_COMPACT", "")
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "80000")
	const budget = 80_000
	var summaries atomic.Int32
	sr := &sizedRun{prefix: 140_000, toolSteps: 16, resultChars: 40_000,
		summarizer: func(context.Context, CompactionSummarizeInput) fantasy.Message {
			summaries.Add(1)
			return fantasy.NewUserMessage(compactionSummaryPrefix + "] summary")
		}}
	obs, session := sr.run(t, "elide-floor-high")

	if n := summaries.Load(); n != 0 {
		t.Fatalf("the summarizer ran %d times, want 0", n)
	}
	paused := checkpointedSteps(obs)
	compactions := payloadsOf(obs, evtContextCompacted)
	if len(paused) == 0 || len(compactions) != len(paused) {
		t.Fatalf("checkpoints after steps %v, compactions %v: want one elision per pause", paused, compactions)
	}
	floor := sr.sizes[0]
	for _, p := range compactions {
		if p[evtFieldTrigger] != "elision" || p[evtFieldElidedResults].(int) == 0 || p[evtFieldElidedBytes].(int) == 0 {
			t.Fatalf("want only elisions, got %v", p)
		}
		if p[evtFieldResendBudget] != budget || p[evtFieldResendFloor] != floor || p[evtFieldEffectiveBudget] != floor+budget {
			t.Errorf("an elision must name the budget, floor and effective budget, got %v", p)
		}
		if after := p[evtFieldEstimatedAfter].(int); after > floor+budget-budget*3/10 {
			t.Errorf("estimated %d after elision, want at most %d to skip the summary", after, floor+budget-budget*3/10)
		}
	}
	at := paused[0]
	if sr.sizes[at] >= floor+budget {
		t.Fatalf("the step after the elision resent %d, want it back under floor + budget %d", sr.sizes[at], floor+budget)
	}
	// The stubs were carried on, not applied to one request: the step after
	// is still well under the size that paused.
	if sr.sizes[at+1] >= sr.sizes[at-1] {
		t.Fatalf("the stubs did not persist: %d paused, then %d, %d", sr.sizes[at-1], sr.sizes[at], sr.sizes[at+1])
	}
	crumbs := sessionLines(session, "[context_elided] ")
	if len(crumbs) != len(paused) || !strings.HasPrefix(crumbs[0], "[context_elided] trigger=resend_budget used=") ||
		!strings.Contains(crumbs[0], " floor=") || !strings.HasSuffix(crumbs[0], "that freed enough, so no summary was needed") {
		t.Fatalf("[context_elided] breadcrumbs = %v", crumbs)
	}
	if lines := sessionLines(session, "[context_compacted]"); len(lines) != 0 {
		t.Errorf("no summary compaction, yet %v", lines)
	}
}

// A run that elides settles on a lower floor than one that only summarizes:
// the stubs are smaller than the kept recent half they replace (#1600).
func TestRun_Elision_LowersThePostCompactionFloor(t *testing.T) {
	t.Setenv("FLEET_SCHEDULED_AUTO_COMPACT", "")
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "80000")
	floorAfterFirstPause := func(slug string) int {
		sr := &sizedRun{prefix: 140_000, toolSteps: 16, resultChars: 40_000}
		obs, _ := sr.run(t, slug)
		paused := checkpointedSteps(obs)
		if len(paused) == 0 {
			t.Fatalf("%s: no checkpoint (sizes=%v)", slug, sr.sizes)
		}
		return sr.sizes[paused[0]]
	}
	t.Setenv("FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY", "0")
	summarized := floorAfterFirstPause("elide-off")
	t.Setenv("FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY", "")
	elided := floorAfterFirstPause("elide-on")
	t.Logf("resent size after the first pause: %d with the summary alone, %d with elision", summarized, elided)
	if elided >= summarized {
		t.Fatalf("floor after the first pause: %d with elision, %d with the summary alone; want it lower", elided, summarized)
	}
}

// When elision does not free enough — here the history is mostly tool-call
// inputs, which are never elided — the summary is bought too, on the elided
// history, and both are recorded in that order.
func TestCheckContextPressure_ElisionThenSummaryWhenNotEnough(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "80000")
	model := &namedMockModel{name: "elide-not-enough"}
	recordContextMax(model.name, 10_000_000)
	e := newMockEngine(t, model)
	e.envPrefix = CanonicalEnvPrefix
	e.requireCompactionOptIn = true
	var droppable []fantasy.Message
	e.compactionSummarizer = func(_ context.Context, in CompactionSummarizeInput) fantasy.Message {
		droppable = in.Droppable
		return fantasy.NewUserMessage(compactionSummaryPrefix + "] summary")
	}
	msgs := make([]fantasy.Message, 0, 13)
	msgs = append(msgs, fantasy.NewUserMessage("task"))
	for i := range 6 {
		id := string(rune('a' + i))
		msgs = append(msgs, elisionExchange(id, `{"rows":"`+strings.Repeat("r", 40_000)+`"}`, elisionText(4_000))...)
	}
	e.logSession.LastStepPromptTokens = 100_000
	obs := &payloadObserver{}
	res := e.checkContextPressure(context.Background(), msgs, model, newStreamSink(obs), false)

	compactions := payloadsOf(obs, evtContextCompacted)
	if len(compactions) != 2 || compactions[0][evtFieldTrigger] != "elision" || compactions[1][evtFieldTrigger] != "resend_budget" {
		t.Fatalf("want an elision then a resend_budget summary, got %v", compactions)
	}
	if len(res.messages) >= len(msgs) {
		t.Fatal("the summary did not shorten the history")
	}
	stubbed := false
	for _, m := range droppable {
		for _, part := range m.Content {
			if tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
				if text, _ := toolResultOutputText(tr.Output); strings.HasPrefix(text, "[tool output truncated]") {
					stubbed = true
				}
			}
		}
	}
	if !stubbed {
		t.Error("the summarizer was handed the history before elision")
	}
	crumbs := sessionLines(e.logSession, "[context_")
	if len(crumbs) != 2 || !strings.HasPrefix(crumbs[0], "[context_elided] trigger=resend_budget used=100000 budget=80000 elided_results=") ||
		!strings.HasSuffix(crumbs[0], "the oldest half of the history is summarized next") ||
		!strings.HasPrefix(crumbs[1], "[context_compacted] trigger=resend_budget used=100000 budget=80000 ") {
		t.Fatalf("breadcrumbs = %v", crumbs)
	}
}

// Just under the threshold is not enough: the next step would pause again. The
// summary is skipped only when elision freed elisionHeadroomFraction of the
// budget below the threshold.
func TestCheckContextPressure_ElisionJustUnderTheBudgetStillSummarizes(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "80000")
	run := func(used int) []map[string]any {
		model := &namedMockModel{name: "elide-headroom"}
		recordContextMax(model.name, 10_000_000)
		e := newMockEngine(t, model)
		e.envPrefix = CanonicalEnvPrefix
		e.requireCompactionOptIn = true
		e.logSession.LastStepPromptTokens = used
		obs := &payloadObserver{}
		e.checkContextPressure(context.Background(), elisionHistory(8, 40_000), model, newStreamSink(obs), false)
		return payloadsOf(obs, evtContextCompacted)
	}
	// Measure what the fixture's elision saves, then place used so that the
	// size after it lands between the target and the threshold, and under it.
	saved := elideStaleToolResults(elisionHistory(8, 40_000), 24_000).savedTokens
	if saved < 10_000 {
		t.Fatalf("fixture: elision saves only %d tokens", saved)
	}
	between := run(80_000 - 24_000 + saved + 1_000) // after: 57K, over the 56K target
	if len(between) != 2 || between[0][evtFieldTrigger] != "elision" || between[1][evtFieldTrigger] != "resend_budget" {
		t.Fatalf("after elision 1K over the target: want the summary too, got %v", between)
	}
	under := run(80_000 - 24_000 + saved - 1_000) // after: 55K, under the target
	if len(under) != 1 || under[0][evtFieldTrigger] != "elision" {
		t.Fatalf("after elision 1K under the target: want the elision alone, got %v", under)
	}
}

// Elision is a scheduled engine's, and the knob turns it off: an interactive
// engine's window compaction and a scheduled one with the knob off summarize
// exactly as before, and no result anywhere is stubbed.
func TestCheckContextPressure_ElisionIsScheduledOnlyAndKnobGated(t *testing.T) {
	stubbedResults := func(msgs []fantasy.Message) int {
		n := 0
		for _, m := range msgs {
			for _, part := range m.Content {
				if tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
					if text, _ := toolResultOutputText(tr.Output); strings.HasPrefix(text, "[tool output truncated]") {
						n++
					}
				}
			}
		}
		return n
	}
	msgs := elisionHistory(14, 40_000)

	t.Run("interactive", func(t *testing.T) {
		model := &namedMockModel{name: "elide-chat"}
		recordContextMax(model.name, 200_000)
		e := newMockEngine(t, model)
		e.envPrefix = CanonicalEnvPrefix
		e.logSession.LastStepPromptTokens = 190_000
		obs := &payloadObserver{}
		res := e.checkContextPressure(context.Background(), msgs, model, newStreamSink(obs), false)
		compactions := payloadsOf(obs, evtContextCompacted)
		if len(compactions) != 1 || compactions[0][evtFieldTrigger] != "window" || stubbedResults(res.messages) != 0 {
			t.Fatalf("an interactive window compaction must be the summary alone, got %v", compactions)
		}
	})
	t.Run("scheduled, knob off", func(t *testing.T) {
		t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "80000")
		t.Setenv("FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY", "false")
		model := &namedMockModel{name: "elide-off"}
		recordContextMax(model.name, 10_000_000)
		e := newMockEngine(t, model)
		e.envPrefix = CanonicalEnvPrefix
		e.requireCompactionOptIn = true
		e.logSession.LastStepPromptTokens = 150_000
		obs := &payloadObserver{}
		res := e.checkContextPressure(context.Background(), msgs, model, newStreamSink(obs), false)
		compactions := payloadsOf(obs, evtContextCompacted)
		if len(compactions) != 1 || compactions[0][evtFieldTrigger] != "resend_budget" || stubbedResults(res.messages) != 0 {
			t.Fatalf("with the knob off a resend compaction must be the summary alone, got %v", compactions)
		}
	})

	for _, tc := range []struct {
		value string
		want  bool
	}{{"", true}, {"1", true}, {"true", true}, {"0", false}, {"false", false}, {"FALSE", false}, {"not-a-bool", true}} {
		t.Setenv("FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY", tc.value)
		if got := contextElideBeforeSummary(CanonicalEnvPrefix); got != tc.want {
			t.Errorf("FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY=%q: %t, want %t", tc.value, got, tc.want)
		}
	}
}

// A scheduled run that compacts only near the window (resend budget off,
// auto-compaction on) elides first as well, against the compaction point,
// and an elision that frees enough clears the stalled flag, resets the
// consecutive-compaction count and marks the floor for re-measurement.
func TestCheckContextPressure_ScheduledWindowCompactionElidesFirst(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "0")
	t.Setenv("FLEET_SCHEDULED_AUTO_COMPACT", "1")
	model := &namedMockModel{name: "elide-window"}
	recordContextMax(model.name, 200_000)
	e := newMockEngine(t, model)
	e.envPrefix = CanonicalEnvPrefix
	e.requireCompactionOptIn = true
	e.windowCompactionStalled = true
	e.consecutiveCompactions = 2
	e.logSession.LastStepPromptTokens = 180_000
	msgs := elisionHistory(14, 40_000)
	obs := &payloadObserver{}
	res := e.checkContextPressure(context.Background(), msgs, model, newStreamSink(obs), false)

	compactions := payloadsOf(obs, evtContextCompacted)
	if len(compactions) != 1 || compactions[0][evtFieldTrigger] != "elision" || compactions[0][evtFieldWindowSize] != 200_000 {
		t.Fatalf("want one elision naming the window and no summary, got %v", compactions)
	}
	if len(res.messages) != len(msgs) {
		t.Fatal("an elision that freed enough must not summarize")
	}
	if e.windowCompactionStalled || e.consecutiveCompactions != 0 || !e.resendFloorStale {
		t.Errorf("stalled=%t consecutive=%d floorStale=%t, want false/0/true", e.windowCompactionStalled, e.consecutiveCompactions, e.resendFloorStale)
	}
	if crumbs := sessionLines(e.logSession, "[context_elided] trigger=window used=180000 window=200000 "); len(crumbs) != 1 {
		t.Fatalf("want one window [context_elided] breadcrumb, got %v", e.logSession.Messages)
	}
}
