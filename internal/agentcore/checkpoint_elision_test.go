package agentcore

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
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

// stubbedResults counts the tool results in msgs that are a stub.
func stubbedResults(msgs []fantasy.Message) int {
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
				!strings.Contains(stub, "recovery_action: "+elisionRecovery) || !strings.Contains(stub, "\npreview:\n") {
				t.Errorf("result %s stub = %q, want the envelope with a preview and a recovery action", tr.ToolCallID, stub)
			}
			if len(stub) > elisionStubBytes {
				t.Errorf("result %s stub is %d bytes, want at most %d", tr.ToolCallID, len(stub), elisionStubBytes)
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

	// The window counts what is sent: a recent screenshot goes out as a short
	// envelope, so it does not end the window at itself.
	withImage := elisionHistory(4, 4000)
	withImage[9] = fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
		fantasy.ToolResultPart{ToolCallID: "page-d", Output: fantasy.ToolResultOutputContentMedia{Data: strings.Repeat("i", 256_000), MediaType: "image/png"}},
	}}
	if got := recentWindowStart(withImage, 1, wireTokens(withImage[4:])); got != 4 {
		t.Errorf("room for three exchanges as sent, the newest a screenshot: window starts at %d, want 4", got)
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
	sr := &sizedRun{prefix: 140_000, toolSteps: 26, resultChars: 40_000,
		summarizer: func(context.Context, CompactionSummarizeInput) fantasy.Message {
			summaries.Add(1)
			return fantasy.NewUserMessage(compactionSummaryPrefix + "] summary")
		}}
	obs, session := sr.run(t, "elide-floor-high")

	if n := summaries.Load(); n != 0 {
		t.Fatalf("the summarizer ran %d times, want 0", n)
	}
	// The #1600 cadence holds with elision: a pause exactly when a step has
	// resent a full budget over the floor measured at the latest floor point
	// (the first step, and the first step after each elision), and the floor
	// each elision names is that floor.
	var want, floors []int
	floor := sr.sizes[0]
	for step := 2; step <= sr.toolSteps; step++ {
		if len(want) > 0 && want[len(want)-1] == step-1 {
			floor = sr.sizes[step-1]
			continue
		}
		if sr.sizes[step-1] >= floor+budget {
			want = append(want, step)
			floors = append(floors, floor)
		}
	}
	paused := checkpointedSteps(obs)
	if len(want) < 2 || fmt.Sprint(paused) != fmt.Sprint(want) {
		t.Fatalf("checkpoints after steps %v, want %v: two or more, each after a full budget over the latest floor (sizes=%v)", paused, want, sr.sizes)
	}
	compactions := payloadsOf(obs, evtContextCompacted)
	if len(compactions) != len(paused) {
		t.Fatalf("compactions %v: want one elision per pause", compactions)
	}
	for i, p := range compactions {
		if p[evtFieldTrigger] != "elision" || p[evtFieldElisionTrigger] != "resend_budget" || p[evtFieldElidedResults].(int) == 0 || p[evtFieldElidedBytes].(int) == 0 {
			t.Fatalf("want only resend-budget elisions, got %v", p)
		}
		if p[evtFieldResendBudget] != budget || p[evtFieldResendFloor] != floors[i] || p[evtFieldEffectiveBudget] != floors[i]+budget {
			t.Errorf("elision %d must name the budget, its floor %d and effective budget, got %v", i+1, floors[i], p)
		}
		if after := p[evtFieldEstimatedAfter].(int); after > floors[i]+budget-budget*3/10 {
			t.Errorf("estimated %d after elision %d, want at most %d to skip the summary", after, i+1, floors[i]+budget-budget*3/10)
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
// inputs, which are never elided — the summary is bought too, and both are
// recorded in that order. The summary is written from the full results (a
// stub's preview may have cut an identifier the run still needs), while the
// kept half carries the stubs on.
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
	msgs := make([]fantasy.Message, 0, 17)
	msgs = append(msgs, fantasy.NewUserMessage("task"))
	for i := range 8 {
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
	if n := stubbedResults(droppable); n != 0 {
		t.Errorf("the summarizer was handed %d stubs, want the full results", n)
	}
	if n := stubbedResults(res.messages); n == 0 {
		t.Error("the kept half lost its stubs")
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
	// Measure what the fixture's elision saves and what the paused step added
	// after its measured input (unseenTokens), then place used so that the
	// size after elision lands just over the 56K target, and just under it.
	history := elisionHistory(8, 40_000)
	saved := elideStaleToolResults(history, 24_000).savedTokens
	if saved < 10_000 {
		t.Fatalf("fixture: elision saves only %d tokens", saved)
	}
	const target = 80_000 - 24_000
	unseen := unseenTokens(history)
	between := run(target + saved - unseen + 1_000)
	if len(between) != 2 || between[0][evtFieldTrigger] != "elision" || between[1][evtFieldTrigger] != "resend_budget" {
		t.Fatalf("after elision 1K over the target: want the summary too, got %v", between)
	}
	if got := between[0][evtFieldEstimatedAfter]; got != target+1_000 {
		t.Fatalf("estimated after = %v, want used - saved + the paused step's own output = %d", got, target+1_000)
	}
	under := run(target + saved - unseen - 1_000)
	if len(under) != 1 || under[0][evtFieldTrigger] != "elision" {
		t.Fatalf("after elision 1K under the target: want the elision alone, got %v", under)
	}
}

// Elision is a scheduled engine's, and the knob turns it off: an interactive
// engine's window compaction and a scheduled one with the knob off summarize
// exactly as before, and no result anywhere is stubbed.
func TestCheckContextPressure_ElisionIsScheduledOnlyAndKnobGated(t *testing.T) {
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

// A history already elided has nothing left to elide outside the window: a
// stub is never stubbed again.
func TestElideStaleToolResults_NeverTwice(t *testing.T) {
	once := elideStaleToolResults(elisionHistory(8, 40_000), 0)
	if once.stubs == 0 {
		t.Fatal("fixture: nothing elided")
	}
	if again := elideStaleToolResults(once.messages, 0); again.stubs != 0 {
		t.Fatalf("a second elision made %d more stubs", again.stubs)
	}
}

// A result the tool output boundary already saved to a governed artifact keeps
// that path in its stub, and its recovery is view_file on it.
func TestElideStaleToolResults_KeepsTheArtifactPath(t *testing.T) {
	const path = ".fleet/tool-output/slot-03/artifact-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef.txt"
	bounded := "[tool output truncated]\ntool: fetch\noriginal_bytes: 500000\nshown_bytes: 60000\nformat: text\ntruncated: true\n" +
		"artifact_path: " + path + "\nrecovery_action: Use view_file.\npreview:\n" + strings.Repeat("x", 60_000)
	history := append(elisionHistory(0, 0), elisionExchange("saved", `{}`, fantasy.ToolResultOutputContentText{Text: bounded})...)
	history = append(history, elisionHistory(2, 40_000)[2:]...)
	res := elideStaleToolResults(history, 0)
	if res.stubs != 1 {
		t.Fatalf("stubs = %d, want the saved result's", res.stubs)
	}
	_, stub := resultOf(t, res.messages[3].Content[0])
	if !strings.Contains(stub, "artifact_path: "+path) || !strings.Contains(stub, fmt.Sprintf("Use view_file with path %q", path)) ||
		!strings.Contains(stub, "original_bytes: 500000") {
		t.Fatalf("stub = %q, want the artifact path, its view_file recovery and the original size", stub)
	}
}

// A subagent's answer and the task plan are conclusions the run continues
// from, never data to fetch again: they are not elided.
func TestElideStaleToolResults_KeepsSubagentAnswersAndThePlan(t *testing.T) {
	history := elisionHistory(0, 0)
	for _, tool := range []string{"spawn_subagent", toolNameTaskTracker, "fetch"} {
		ex := elisionExchange(tool+"-1", `{}`, elisionText(40_000))
		ex[0].Content[0] = fantasy.ToolCallPart{ToolCallID: tool + "-1", ToolName: tool, Input: `{}`}
		history = append(history, ex...)
	}
	history = append(history, elisionHistory(2, 40_000)[2:]...)
	res := elideStaleToolResults(history, 0)
	if res.stubs != 1 {
		t.Fatalf("stubs = %d, want only the fetch result", res.stubs)
	}
	for i, id := range []string{"spawn_subagent-1", toolNameTaskTracker + "-1"} {
		if !reflect.DeepEqual(res.messages[3+2*i], history[3+2*i]) {
			t.Errorf("%s's result was elided", id)
		}
	}
}

func TestUnseenTokens(t *testing.T) {
	history := elisionHistory(3, 4_000)
	if got, want := unseenTokens(history), estimateModelMessagesTokens(history[len(history)-2:]); got != want {
		t.Errorf("unseen = %d, want the last exchange's %d", got, want)
	}
	nudged := append(slices.Clone(history), fantasy.NewUserMessage("finish the report"))
	if got, want := unseenTokens(nudged), estimateModelMessagesTokens(nudged[len(nudged)-3:]); got != want {
		t.Errorf("unseen = %d, want the last exchange and the nudge after it, %d", got, want)
	}
	if got := unseenTokens(elisionHistory(0, 0)); got != 0 {
		t.Errorf("no assistant message: unseen = %d, want 0", got)
	}
}

// An elision that saves less than elisionMinSavingFraction of the budget is
// not made: it could not skip the summary, and changes nothing.
func TestElideBeforeSummary_SkipsATinySaving(t *testing.T) {
	const budget = 100_000
	elide := func(history []fantasy.Message) (int, bool) {
		e := newMockEngine(t, &namedMockModel{name: "elide-tiny"})
		e.envPrefix = CanonicalEnvPrefix
		e.requireCompactionOptIn = true
		saved := elideStaleToolResults(history, int(budget*elisionRecentFraction)).savedTokens
		_, ok := e.elideBeforeSummary(history, budget, true, budget, budget)
		if ok != e.resendFloorStale {
			t.Errorf("made=%t but floorStale=%t: only a made elision marks the floor", ok, e.resendFloorStale)
		}
		return saved, ok
	}
	// 3000-byte results save a few hundred tokens each: a few outside the
	// window save well under 2% of the budget, many save well over it.
	if saved, ok := elide(elisionHistory(40, 3_000)); saved == 0 || saved >= budget/50 || ok {
		t.Fatalf("saving %d of %d: made=%t, want a small saving skipped", saved, budget, ok)
	}
	if saved, ok := elide(elisionHistory(100, 3_000)); saved < budget/50 || !ok {
		t.Fatalf("saving %d of %d: made=%t, want it made", saved, budget, ok)
	}
}

// With no floor measured (so no rate), the estimate after elision falls back
// to the measured size less the savings plus the paused step's output, and is
// never below a direct estimate of the elided request: when the in-round
// guard was already stubbing old results on the wire, the measured size is
// smaller than the history and the stubs' savings would otherwise count twice.
func TestElideBeforeSummary_NeverBelowTheDirectEstimate(t *testing.T) {
	e := newMockEngine(t, &namedMockModel{name: "elide-direct"})
	e.envPrefix = CanonicalEnvPrefix
	e.requireCompactionOptIn = true
	history := elisionHistory(10, 40_000)
	el, ok := e.elideBeforeSummary(history, 30_000, true, 30_000, 30_000)
	if !ok {
		t.Fatal("fixture: no elision")
	}
	if direct := e.estimateRequestTokens(el.messages); el.usedAfter != direct || direct <= 30_000-el.savedTokens {
		t.Fatalf("estimated after = %d, want the direct estimate %d (the measured size less the savings is %d)", el.usedAfter, direct, 30_000-el.savedTokens)
	}
}

// When no step has been measured yet, the size is an estimate of the whole
// history, which already counts the latest step's output: nothing unseen is
// added, and the estimate after elision is the elided history's own.
func TestCheckContextPressure_ElisionFromAnEstimateAddsNothingUnseen(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "80000")
	model := &namedMockModel{name: "elide-estimated"}
	recordContextMax(model.name, 10_000_000)
	e := newMockEngine(t, model)
	e.envPrefix = CanonicalEnvPrefix
	e.requireCompactionOptIn = true
	msgs := elisionHistory(14, 40_000)
	obs := &payloadObserver{}
	e.checkContextPressure(context.Background(), msgs, model, newStreamSink(obs), false)
	p := obs.payloadOf(evtContextCompacted)
	if p == nil || p[evtFieldTrigger] != "elision" {
		t.Fatalf("want an elision, got %v", obs.events)
	}
	want := e.estimateRequestTokens(elideStaleToolResults(msgs, 24_000).messages)
	if p[evtFieldUsedTokens] != e.estimateRequestTokens(msgs) || p[evtFieldEstimatedAfter] != want {
		t.Fatalf("used %v, estimated after %v; want the history's estimate and the elided history's %d", p[evtFieldUsedTokens], p[evtFieldEstimatedAfter], want)
	}
}

// The rate is the provider's tokens for the history the measured call carried
// (its size less the measured prefix) per estimated token, clamped; 1 when
// there is nothing to calibrate against.
func TestHistoryTokenRate(t *testing.T) {
	e := newMockEngine(t, &namedMockModel{name: "elide-rate"})
	history := elisionHistory(4, 4_000)
	seen := estimateModelMessagesTokens(history[1 : len(history)-2]) // after the head, before the latest exchange
	if got := e.historyTokenRate(history, 50_000, true); got != 1 {
		t.Errorf("no prefix measured: rate %v, want 1", got)
	}
	e.resendPrefix = 10_000
	if got := e.historyTokenRate(history, 50_000, false); got != 1 {
		t.Errorf("an estimated size: rate %v, want 1", got)
	}
	if got, want := e.historyTokenRate(history, 10_000+2*seen, true), 2.0; got != want {
		t.Errorf("history billed at twice the estimate: rate %v, want %v", got, want)
	}
	// gpt-6-luna-pro counts dense numbers at about 7.3 times the estimate: a
	// real rate, under the cap.
	if got := e.historyTokenRate(history, 10_000+73*seen/10, true); got < 7.29 || got > 7.31 {
		t.Errorf("history billed at 7.3 times the estimate: rate %v, want 7.3 unclamped", got)
	}
	if got := e.historyTokenRate(history, 10_000+20*seen, true); got != maxHistoryTokenRate {
		t.Errorf("rate %v, want the clamp %v", got, maxHistoryTokenRate)
	}
	if got := e.historyTokenRate(history, 10_000+seen/10, true); got != minHistoryTokenRate {
		t.Errorf("rate %v, want the clamp %v", got, minHistoryTokenRate)
	}
	if got := e.historyTokenRate(history, 10_000, true); got != 1 {
		t.Errorf("a size no larger than the prefix: rate %v, want 1", got)
	}

	// A media result goes out as a short envelope on every call
	// (reduceHistoricalPayloadsToHardCap): the provider never counted its
	// bytes, so they must not dilute the rate of the dense results around it.
	image := elisionExchange("image", `{}`, fantasy.ToolResultOutputContentMedia{Data: strings.Repeat("i", 256_000), MediaType: "image/png"})
	withImage := append(append(elisionHistory(0, 0), image...), history[2:]...)
	sent := wireTokens(withImage[1 : len(withImage)-2])
	if got := e.historyTokenRate(withImage, 10_000+2*sent, true); got != 2 {
		t.Errorf("with a 256 KB image: rate %v, want 2 measured against what was sent", got)
	}
	if sent >= estimateModelMessagesTokens(withImage[1:len(withImage)-2])/2 {
		t.Fatalf("fixture: the image's bytes are not what dominates the carried history")
	}
}

// Dense output costs the provider more tokens per character than the
// estimator counts. Sized at the measured rate, the recent window holds 30%
// of the budget in the provider's tokens and the savings are counted at full
// value, so elision clears the budget where an uncalibrated one could not.
// The fixture is a live run's shape: an 80K budget, a 16K prefix, ~17 KB
// results counted at 2 characters a token.
func TestRun_Elision_SizedInTheProvidersTokens(t *testing.T) {
	t.Setenv("FLEET_SCHEDULED_AUTO_COMPACT", "")
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "80000")
	var sr *sizedRun
	var paused []int
	run := func(slug string) (elisions, summaries []map[string]any) {
		sr = &sizedRun{prefix: 16_000, toolSteps: 30, resultChars: 17_000, resultCharsPerToken: 2}
		obs, _ := sr.run(t, slug)
		paused = checkpointedSteps(obs)
		for _, p := range payloadsOf(obs, evtContextCompacted) {
			if p[evtFieldTrigger] == "elision" {
				elisions = append(elisions, p)
			} else {
				summaries = append(summaries, p)
			}
		}
		return elisions, summaries
	}
	t.Setenv("FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY", "0")
	_, summarizedOnly := run("elide-dense-off")
	t.Setenv("FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY", "")
	elisions, summaries := run("elide-dense-on")
	if len(elisions) == 0 || len(elisions) != len(paused) {
		t.Fatalf("%d elisions over pauses after steps %v: want one per pause", len(elisions), paused)
	}
	for i, p := range elisions {
		if rate := p[evtFieldHistoryTokenRate].(float64); rate < 1.6 || rate > 2.2 {
			t.Errorf("history_token_rate = %v, want about 2", rate)
		}
		// The estimate is in the provider's tokens: within 3% of what the
		// call after the pause actually resent.
		after, next := p[evtFieldEstimatedAfter].(int), sr.sizes[paused[i]]
		if diff := after - next; diff*33 > next || -diff*33 > next {
			t.Errorf("elision %d estimated %d after, the next call resent %d", i+1, after, next)
		}
	}
	if len(summaries) >= len(summarizedOnly) || len(summaries) > len(elisions)/2 {
		t.Fatalf("%d summaries with elision (%d elisions), %d without: want elision to clear the budget at most pauses", len(summaries), len(elisions), len(summarizedOnly))
	}
	t.Logf("%d pauses with the summary alone; with elision %d elisions and %d summaries", len(summarizedOnly), len(elisions), len(summaries))
}

// The 2% minimum is of the budget in the provider's tokens: a saving under 2%
// at the estimator's 4 characters a token is still made when the run's
// measured rate puts it over.
func TestElideBeforeSummary_MinimumSavingAtTheRunsRate(t *testing.T) {
	const budget = 100_000
	// The shortest history whose elision, with the window at rate 2, saves
	// 1-2% of the budget in estimated tokens: under the minimum at rate 1,
	// over it at rate 2.
	var history []fantasy.Message
	for n := 2; n < 200 && history == nil; n++ {
		h := elisionHistory(n, 3_000)
		if saved := elideStaleToolResults(h, int(budget*elisionRecentFraction/2)).savedTokens; saved < budget/50 && 2*saved >= budget/50 {
			history = h
		}
	}
	if history == nil {
		t.Fatal("fixture: no history saves 1-2% of the budget")
	}
	e := newMockEngine(t, &namedMockModel{name: "elide-min-rate"})
	e.envPrefix = CanonicalEnvPrefix
	e.requireCompactionOptIn = true
	e.resendPrefix = 10_000
	seen := wireTokens(history[1 : len(history)-2])
	el, ok := e.elideBeforeSummary(history, 10_000+2*seen, true, budget, budget)
	if !ok || el.rate != 2 {
		t.Fatalf("at rate 2 the saving clears 2%%: made=%t rate=%v", ok, el.rate)
	}
}

// The estimate after elision counts the history as it is sent: a media result
// the elision leaves alone goes out as its envelope, so its bytes are not in
// the estimate.
func TestElideBeforeSummary_EstimatesTheSentHistory(t *testing.T) {
	image := elisionExchange("image", `{}`, fantasy.ToolResultOutputContentMedia{Data: strings.Repeat("i", 256_000), MediaType: "image/png"})
	history := append(append(elisionHistory(0, 0), image...), elisionHistory(10, 40_000)[2:]...)
	e := newMockEngine(t, &namedMockModel{name: "elide-sent"})
	e.envPrefix = CanonicalEnvPrefix
	e.requireCompactionOptIn = true
	e.resendPrefix = 10_000
	used := 10_000 + 2*wireTokens(history[1:len(history)-2])
	el, ok := e.elideBeforeSummary(history, used, true, used, 80_000)
	if !ok {
		t.Fatal("fixture: no elision")
	}
	want := e.resendPrefix + int(el.rate*float64(wireTokens(el.messages[1:])))
	if el.usedAfter != want || el.usedAfter >= e.resendPrefix+int(el.rate*float64(estimateModelMessagesTokens(el.messages[1:])))-10_000 {
		t.Fatalf("estimated after %d, want %d: the image's bytes are not sent and must not be counted", el.usedAfter, want)
	}
}
