package agentcore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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

// A stub tells the model that making a call that only read data again is how
// to get the data back, with the same arguments when the tool takes no
// narrower ones, and where the data is not. Told only "do not repeat it" and
// "narrower output", Gemini 3.8 Flash, whose report tool took no narrower
// arguments, searched the environment and the workspace for the data instead
// (the store-KPI bench). The longer text must not cost a report-shaped JSON
// result any of its preview.
func TestElisionStub_SaysToCallAReadingToolAgain(t *testing.T) {
	items := make([]string, 0, 300)
	for i := range 300 {
		items = append(items, fmt.Sprintf(`{"txn_id":"S01-20261031-%04d","sku":"NW-PACK-%03d","qty":%d,"unit_price":89.99,"discount_pct":12.5}`, i, i%40, i%5+1))
	}
	report := `{"store_id":"S01","date":"2026-10-31","line_items":[` + strings.Join(items, ",") + `]}`
	for name, output := range map[string]fantasy.ToolResultOutputContent{
		"text": elisionText(9000),
		"json": fantasy.ToolResultOutputContentText{Text: report},
	} {
		stub, _, ok := elisionStub(output, "mcp_store_metrics_get_store_daily_report")
		if !ok {
			t.Fatalf("%s: fixture: the result was not elided", name)
		}
		for _, want := range []string{
			"the call itself is done",
			"the call only read data, make it again with the same arguments (or narrower ones, if the tool takes them)",
			"Do not look for it in the environment, /tmp or other tasks' directories; a file this run wrote is fine to read.",
			"Do not repeat a call that changed something",
		} {
			if !strings.Contains(stub, want) {
				t.Errorf("%s: stub lacks %q:\n%s", name, want, stub)
			}
		}
		if strings.Contains(stub, "do not repeat it just to see this again") {
			t.Errorf("%s: stub still tells the model not to make a reading call again:\n%s", name, stub)
		}
		if len(stub) > elisionStubBytes {
			t.Errorf("%s: stub is %d bytes, want at most %d", name, len(stub), elisionStubBytes)
		}
		if name != "json" {
			continue
		}
		var env toolOutputEnvelope
		if err := json.Unmarshal([]byte(stub), &env); err != nil {
			t.Fatalf("json stub is not JSON: %v\n%s", err, stub)
		}
		if env.RecoveryAction != elisionRecovery {
			t.Errorf("json stub recovery = %q, want elisionRecovery", env.RecoveryAction)
		}
		if want := initialPreviewBudget(elisionStubBytes); env.ShownBytes != want {
			t.Errorf("json stub shows %d bytes of the report, want the full preview, %d", env.ShownBytes, want)
		}
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
	t.Setenv("FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY", "true") // on, whatever the shell or a legacy alias says
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
	// The estimate after each elision is within 0.5% of what the next call
	// actually resent.
	for i, p := range compactions {
		if diff := p[evtFieldEstimatedAfter].(int) - sr.sizes[paused[i]]; diff*200 > sr.sizes[paused[i]] || -diff*200 > sr.sizes[paused[i]] {
			t.Errorf("elision %d estimated %d, the next call resent %d", i+1, p[evtFieldEstimatedAfter], sr.sizes[paused[i]])
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
	t.Setenv("FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY", "true")
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
	t.Setenv("FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY", "true") // on, whatever the shell or a legacy alias says
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "80000")
	model := &namedMockModel{name: "elide-not-enough"}
	recordContextMax(model.name, 10_000_000)
	e := newMockEngine(t, model)
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
	t.Setenv("FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY", "true") // on, whatever the shell or a legacy alias says
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "80000")
	run := func(used int) []map[string]any {
		model := &namedMockModel{name: "elide-headroom"}
		recordContextMax(model.name, 10_000_000)
		e := newMockEngine(t, model)
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
		model := &namedMockModel{name: "elide-knob-off"}
		recordContextMax(model.name, 10_000_000)
		e := newMockEngine(t, model)
		e.requireCompactionOptIn = true
		e.logSession.LastStepPromptTokens = 150_000
		obs := &payloadObserver{}
		res := e.checkContextPressure(context.Background(), msgs, model, newStreamSink(obs), false)
		compactions := payloadsOf(obs, evtContextCompacted)
		if len(compactions) != 1 || compactions[0][evtFieldTrigger] != "resend_budget" || stubbedResults(res.messages) != 0 {
			t.Fatalf("with the knob off a resend compaction must be the summary alone, got %v", compactions)
		}
	})

	// The legacy CHAT_/CUTLASS_ aliases are read when FLEET_ is empty: clear
	// them, so the "" case reads the default whatever the shell has.
	t.Setenv("CHAT_CONTEXT_ELIDE_BEFORE_SUMMARY", "")
	t.Setenv("CUTLASS_CONTEXT_ELIDE_BEFORE_SUMMARY", "")
	for _, tc := range []struct {
		value string
		want  bool
	}{{"", true}, {"1", true}, {"true", true}, {"0", false}, {"false", false}, {"FALSE", false}, {"not-a-bool", true}} {
		t.Setenv("FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY", tc.value)
		if got := contextElideBeforeSummary(); got != tc.want {
			t.Errorf("FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY=%q: %t, want %t", tc.value, got, tc.want)
		}
	}
}

// A scheduled run that compacts only near the window (resend budget off,
// auto-compaction on) elides first as well, against the compaction point,
// and an elision that frees enough clears the stalled flag, resets the
// consecutive-compaction count and marks the floor for re-measurement.
func TestCheckContextPressure_ScheduledWindowCompactionElidesFirst(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY", "true") // on, whatever the shell or a legacy alias says
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "0")
	t.Setenv("FLEET_SCHEDULED_AUTO_COMPACT", "1")
	model := &namedMockModel{name: "elide-window"}
	recordContextMax(model.name, 200_000)
	e := newMockEngine(t, model)
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
	if strings.Contains(stub, elisionRecovery) {
		t.Fatalf("stub = %q, want only the view_file recovery: the saved file is where the output is", stub)
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
	t.Setenv("FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY", "true") // on, whatever the shell or a legacy alias says
	const budget = 100_000
	elide := func(history []fantasy.Message) (int, bool) {
		e := newMockEngine(t, &namedMockModel{name: "elide-tiny"})
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
	t.Setenv("FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY", "true") // on, whatever the shell or a legacy alias says
	e := newMockEngine(t, &namedMockModel{name: "elide-direct"})
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
	t.Setenv("FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY", "true") // on, whatever the shell or a legacy alias says
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "80000")
	model := &namedMockModel{name: "elide-estimated"}
	recordContextMax(model.name, 10_000_000)
	e := newMockEngine(t, model)
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
	t.Setenv("FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY", "true")
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
	t.Setenv("FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY", "true") // on, whatever the shell or a legacy alias says
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
	t.Setenv("FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY", "true") // on, whatever the shell or a legacy alias says
	image := elisionExchange("image", `{}`, fantasy.ToolResultOutputContentMedia{Data: strings.Repeat("i", 256_000), MediaType: "image/png"})
	history := append(append(elisionHistory(0, 0), image...), elisionHistory(10, 40_000)[2:]...)
	e := newMockEngine(t, &namedMockModel{name: "elide-sent"})
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

// The issue's prompt-cache constraint: elision never rewrites history per
// step, only when the run already rewrites it (a compaction pause), and what
// it writes then is carried on unchanged. Between pauses every call's history
// is an exact prefix of the next call's, so the provider's cache holds; and a
// stub, once written, is byte-identical in every later call, so an elision at
// a later pause rewrites only the results that left the window since.
func TestRun_Elision_KeepsThePromptCachePrefix(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY", "true") // on, whatever the shell or a legacy alias says
	t.Setenv("FLEET_SCHEDULED_AUTO_COMPACT", "")
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "80000")
	sr := &sizedRun{prefix: 140_000, toolSteps: 26, resultChars: 40_000}
	obs, _ := sr.run(t, "elide-cache-prefix")
	paused := map[int]bool{}
	for _, step := range checkpointedSteps(obs) {
		paused[step] = true // the call after step s is the first after a rewrite
	}
	stubs := map[string]string{} // call ID -> the stub first sent for it
	rewrites := 0
	for i := 0; i+1 < len(sr.sent); i++ {
		prev, next := sr.sent[i], sr.sent[i+1]
		if !paused[i+1] {
			if len(next) < len(prev) || !reflect.DeepEqual(next[:len(prev)], prev) {
				t.Fatalf("call %d rewrote the history call %d sent, with no pause between them", i+2, i+1)
			}
			continue
		}
		rewrites++
		// Across a pause the next call carries the paused step's own call and
		// result (two messages), and of what the paused call sent, only tool
		// results may differ, each keeping its call ID.
		if len(next) != len(prev)+2 {
			t.Fatalf("the call after the pause at call %d sent %d messages, want %d", i+1, len(next), len(prev)+2)
		}
		for k := range prev {
			if next[k] == prev[k] {
				continue
			}
			id, _, _ := strings.Cut(strings.TrimPrefix(prev[k], "tool|result:"), ":")
			if !strings.HasPrefix(prev[k], "tool|result:") || !strings.HasPrefix(next[k], "tool|result:"+id+":") {
				t.Fatalf("the elision at call %d rewrote message %d, which is not a tool result:\n was %.80q\n now %.80q", i+2, k, prev[k], next[k])
			}
		}
	}
	for _, sent := range sr.sent {
		for _, line := range sent {
			_, rest, ok := strings.Cut(line, "|result:")
			if !ok {
				continue
			}
			id, text, _ := strings.Cut(rest, ":")
			if !strings.HasPrefix(text, "[tool output truncated]") {
				continue
			}
			if first, seen := stubs[id]; seen && first != text {
				t.Fatalf("the stub for %s changed after it was written", id)
			}
			stubs[id] = text
		}
	}
	// Checked last, so a per-step rewrite fails above on what it changed.
	if len(paused) < 2 || len(payloadsOf(obs, evtContextCompacted)) != len(paused) {
		t.Fatalf("fixture: want two or more elision pauses and no summary, got pauses %v, compactions %v", paused, payloadsOf(obs, evtContextCompacted))
	}
	if rewrites != len(paused) || len(stubs) == 0 {
		t.Fatalf("rewrites %d (pauses %d), stubs %d", rewrites, len(paused), len(stubs))
	}
}

// heldEngine is a scheduled engine with elision on and the given run state.
func heldEngine(t *testing.T, name string, orch *orchestrationState) *engine {
	t.Helper()
	t.Setenv("FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY", "true") // on, whatever the shell or a legacy alias says
	e := newMockEngine(t, &namedMockModel{name: name})
	e.requireCompactionOptIn = true
	e.bindRunUsage(orch)
	return e
}

// While the self-audit is pending (asked for, no confirm_audit accepted yet)
// the run does not elide, and the compaction is the summary: the audit
// re-reads the evidence elision would stub. Before the audit is asked for,
// and once it is accepted, elision works as before.
func TestElideBeforeSummary_HeldWhileTheAuditIsPending(t *testing.T) {
	orch := newOrchestrationState(NewLogSession(), 0)
	e := heldEngine(t, "elide-audit", orch)
	history := elisionHistory(10, 40_000)
	if _, ok := e.elideBeforeSummary(history, 30_000, true, 30_000, 30_000); !ok {
		t.Fatal("fixture: no elision before the audit is asked for")
	}

	e = heldEngine(t, "elide-audit", orch)
	orch.mu.Lock()
	orch.selfAuditRequested = true
	orch.mu.Unlock()
	if _, ok := e.elideBeforeSummary(history, 30_000, true, 30_000, 30_000); ok {
		t.Fatal("elided while the self-audit was pending")
	}

	orch.mu.Lock()
	orch.selfAuditConfirmedOnce = true
	orch.mu.Unlock()
	if _, ok := e.elideBeforeSummary(history, 30_000, true, 30_000, 30_000); !ok {
		t.Fatal("no elision once the audit was accepted")
	}
}

// At the checkpoint, a pending audit sends the run straight to the summary:
// one resend_budget compaction, no elision event or breadcrumb.
func TestCheckContextPressure_PendingAuditSummarizesWithoutEliding(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "80000")
	orch := newOrchestrationState(NewLogSession(), 0)
	orch.selfAuditRequested = true
	e := heldEngine(t, "elide-audit-pressure", orch)
	recordContextMax("elide-audit-pressure", 10_000_000)
	e.compactionSummarizer = func(_ context.Context, _ CompactionSummarizeInput) fantasy.Message {
		return fantasy.NewUserMessage(compactionSummaryPrefix + "] summary")
	}
	msgs := elisionHistory(14, 40_000)
	e.logSession.LastStepPromptTokens = 100_000
	obs := &payloadObserver{}
	e.checkContextPressure(context.Background(), msgs, e.model, newStreamSink(obs), false)

	compactions := payloadsOf(obs, evtContextCompacted)
	if len(compactions) != 1 || compactions[0][evtFieldTrigger] != "resend_budget" {
		t.Fatalf("want one resend_budget summary and no elision, got %v", compactions)
	}
	if crumbs := sessionLines(e.logSession, "[context_elided]"); len(crumbs) != 0 {
		t.Fatalf("elision breadcrumbs: %v", crumbs)
	}
}

// A run that re-runs a call whose result an elision stubbed never elides
// again: the stub cost it a call. The re-run is matched by tool and
// arguments, in any key order, among the calls made since the last check; a
// duplicate the run made before the elision, and a new call to other
// arguments, are not re-runs.
func TestElideBeforeSummary_StopsAfterARerun(t *testing.T) {
	e := heldEngine(t, "elide-rerun", nil)
	history := elisionHistory(10, 40_000)
	// A duplicate of the first call, made before any elision.
	history = append(history[:2:2], append(elisionExchange("dup", `{"page":"page-a"}`, elisionText(10)), history[2:]...)...)
	el, ok := e.elideBeforeSummary(history, 30_000, true, 30_000, 30_000)
	if !ok || !e.elidedCallKeys[callKey("fetch", `{"page":"page-a"}`)] {
		t.Fatalf("fixture: elided=%t keys=%v, want page-a's call recorded", ok, e.elidedCallKeys)
	}
	e.pressureCheckedLen = len(el.messages) // what the check hands back

	other := append(slices.Clone(el.messages), elisionExchange("next", `{"page":"page-z"}`, elisionText(40_000))...)
	if e.noteReruns(other); e.elisionStopped {
		t.Fatal("a call to other arguments, or the duplicate made before the elision, counted as a re-run")
	}
	rerun := append(slices.Clone(other), elisionExchange("again", `{ "page" : "page-a" }`, elisionText(40_000))...)
	if e.noteReruns(rerun); !e.elisionStopped {
		t.Fatal("the re-run of a stubbed call was not noticed")
	}
	if _, ok := e.elideBeforeSummary(rerun, 30_000, true, 30_000, 30_000); ok {
		t.Fatal("elided after the run had re-run a stubbed call")
	}
	// It stays off, even for a history the re-run has left.
	if _, ok := e.elideBeforeSummary(history, 30_000, true, 30_000, 30_000); ok {
		t.Fatal("elision came back after the run had re-run a stubbed call")
	}
}

// Some providers reuse one call ID for every call. The elided identities are
// still exactly the stubbed calls' (each result's own call), and a re-run
// under the reused ID is still noticed.
func TestElideBeforeSummary_RerunWithAReusedCallID(t *testing.T) {
	e := heldEngine(t, "elide-reused-id", nil)
	history := make([]fantasy.Message, 0, 21)
	history = append(history, fantasy.NewUserMessage("refresh every page"))
	for i := range 10 {
		history = append(history, elisionExchange("call_0", fmt.Sprintf(`{"page":%d}`, i), elisionText(40_000))...)
	}
	el, ok := e.elideBeforeSummary(history, 30_000, true, 30_000, 30_000)
	if !ok {
		t.Fatal("fixture: no elision")
	}
	for i := range 10 {
		_, text := resultOf(t, el.messages[2+2*i].Content[0])
		stubbed := strings.HasPrefix(text, "[tool output truncated]")
		if recorded := e.elidedCallKeys[callKey("fetch", fmt.Sprintf(`{"page":%d}`, i))]; recorded != stubbed {
			t.Fatalf("page %d: stubbed=%t but recorded=%t", i, stubbed, recorded)
		}
	}
	e.pressureCheckedLen = len(el.messages)
	if e.noteReruns(append(slices.Clone(el.messages), elisionExchange("call_0", `{"page":0}`, elisionText(40_000))...)); !e.elisionStopped {
		t.Fatal("a re-run under a reused call ID was not noticed")
	}
}

// With a reused call ID, each result is paired with its own call, the nearest
// before it: a subagent's answer is kept, and a stub names the tool that made
// the call, not the last tool to use that ID.
func TestElideStaleToolResults_ReusedCallIDKeepsEachResultsTool(t *testing.T) {
	history := make([]fantasy.Message, 0, 13)
	history = append(history,
		fantasy.NewUserMessage("refresh every page"),
		fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: "call_0", ToolName: "spawn_subagent", Input: `{"task":"a"}`}}},
		fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "call_0", Output: elisionText(40_000)}}},
		fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: "call_0", ToolName: "read_page", Input: `{"page":1}`}}},
		fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.ToolResultPart{ToolCallID: "call_0", Output: elisionText(40_000)}}},
	)
	for i := range 4 {
		history = append(history, elisionExchange("call_0", fmt.Sprintf(`{"page":%d}`, 10+i), elisionText(40_000))...)
	}
	res := elideStaleToolResults(history, 1)
	if _, text := resultOf(t, res.messages[2].Content[0]); strings.HasPrefix(text, "[tool output truncated]") {
		t.Fatal("the subagent's answer was stubbed under a reused call ID")
	}
	_, text := resultOf(t, res.messages[4].Content[0])
	if !strings.Contains(text, "read_page") || strings.Contains(text, "fetch") {
		t.Fatalf("the read_page result's stub names the wrong tool: %.300q", text)
	}
	if !slices.Contains(res.stubbedKeys, callKey("read_page", `{"page":1}`)) || slices.Contains(res.stubbedKeys, callKey("spawn_subagent", `{"task":"a"}`)) {
		t.Fatalf("stubbed keys %q", res.stubbedKeys)
	}
}

// Calls sharing one ID within a parallel step: each result answers the call in
// its own position, so a subagent's answer in the batch is still kept.
func TestElideStaleToolResults_ParallelCallsSharingAnID(t *testing.T) {
	history := make([]fantasy.Message, 0, 11)
	history = append(history,
		fantasy.NewUserMessage("refresh every page"),
		fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{
			fantasy.ToolCallPart{ToolCallID: "call_0", ToolName: "read_page", Input: `{"page":1}`},
			fantasy.ToolCallPart{ToolCallID: "call_0", ToolName: "spawn_subagent", Input: `{"task":"a"}`},
		}},
		fantasy.Message{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{
			fantasy.ToolResultPart{ToolCallID: "call_0", Output: elisionText(40_000)},
			fantasy.ToolResultPart{ToolCallID: "call_0", Output: elisionText(40_000)},
		}},
	)
	for i := range 4 {
		history = append(history, elisionExchange("call_0", fmt.Sprintf(`{"page":%d}`, 10+i), elisionText(40_000))...)
	}
	res := elideStaleToolResults(history, 1)
	if _, text := resultOf(t, res.messages[2].Content[0]); !strings.Contains(text, "read_page") {
		t.Fatalf("the first result should be read_page's stub, got %.200q", text)
	}
	if _, text := resultOf(t, res.messages[2].Content[1]); strings.HasPrefix(text, "[tool output truncated]") {
		t.Fatal("the subagent's answer in a batch sharing one call ID was stubbed")
	}
}

// Through checkContextPressure: the first pause elides, and the run re-runs a
// stubbed call. From then on it no longer elides, and its resend budget is
// three times larger: a pause under that compacts nothing, and one over it
// summarizes.
func TestCheckContextPressure_RerunStopsElision(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "80000")
	e := heldEngine(t, "elide-rerun-pressure", nil)
	recordContextMax("elide-rerun-pressure", 10_000_000)
	e.compactionSummarizer = func(_ context.Context, _ CompactionSummarizeInput) fantasy.Message {
		return fantasy.NewUserMessage(compactionSummaryPrefix + "] summary")
	}
	e.logSession.LastStepPromptTokens = 100_000
	obs := &payloadObserver{}
	res := e.checkContextPressure(context.Background(), elisionHistory(14, 40_000), e.model, newStreamSink(obs), false)
	if c := payloadsOf(obs, evtContextCompacted); len(c) != 1 || c[0][evtFieldTrigger] != "elision" {
		t.Fatalf("fixture: want one elision, got %v", c)
	}

	more := func(history []fantasy.Message, first fantasy.Message, rest ...fantasy.Message) []fantasy.Message {
		out := append(slices.Clone(history), first)
		out = append(out, rest...)
		for range 6 {
			out = append(out, elisionExchange(fmt.Sprintf("new-%d", len(out)), fmt.Sprintf(`{"page":"new-%d"}`, len(out)), elisionText(40_000))...)
		}
		return out
	}
	// New calls only: the calls the first elision stubbed are not taken for
	// re-runs of themselves, and the next pause elides again.
	again := elisionExchange("again", `{"page":"page-a"}`, elisionText(40_000))
	fresh := elisionExchange("fresh", `{"page":"fresh"}`, elisionText(40_000))
	obs = &payloadObserver{}
	res = e.checkContextPressure(context.Background(), more(res.messages, fresh[0], fresh[1]), e.model, newStreamSink(obs), false)
	if c := payloadsOf(obs, evtContextCompacted); len(c) != 1 || c[0][evtFieldTrigger] != "elision" || e.elisionStopped {
		t.Fatalf("with no re-run, want a second elision, got %v (stopped=%t)", c, e.elisionStopped)
	}
	if crumbs := sessionLines(e.logSession, "[context_budget_raised]"); len(crumbs) != 0 {
		t.Fatalf("budget raised with no re-run: %v", crumbs)
	}
	obs = &payloadObserver{}
	rerun := more(res.messages, again[0], again[1])
	res = e.checkContextPressure(context.Background(), rerun, e.model, newStreamSink(obs), false)
	if c := payloadsOf(obs, evtContextCompacted); len(c) != 0 || !e.elisionStopped || len(res.messages) != len(rerun) {
		t.Fatalf("after a re-run, 100K is under the raised budget: want no compaction and elision stopped, got %v (stopped=%t)", c, e.elisionStopped)
	}
	crumbs := sessionLines(e.logSession, "[context_budget_raised]")
	if len(crumbs) != 1 || !strings.HasPrefix(crumbs[0], "[context_budget_raised] tool=fetch budget=80000 raised_budget=240000 — ") {
		t.Fatalf("[context_budget_raised] breadcrumbs = %v", crumbs)
	}

	e.logSession.LastStepPromptTokens = 250_000
	obs = &payloadObserver{}
	e.checkContextPressure(context.Background(), res.messages, e.model, newStreamSink(obs), false)
	c := payloadsOf(obs, evtContextCompacted)
	if len(c) != 1 || c[0][evtFieldTrigger] != "resend_budget" || c[0][evtFieldResendBudget] != 240_000 {
		t.Fatalf("over the raised budget, want one resend_budget summary against 240000, got %v", c)
	}
	summary := sessionLines(e.logSession, "[context_compacted]")
	if len(summary) != 1 || !strings.Contains(summary[0], "budget=240000 ") ||
		!strings.Contains(summary[0], "exceeded the run's resend budget (240000, raised from FLEET_CONTEXT_RESEND_BUDGET_TOKENS after it re-ran an elided call)") {
		t.Fatalf("[context_compacted] breadcrumbs = %v", summary)
	}
	if crumbs := sessionLines(e.logSession, "[context_budget_raised]"); len(crumbs) != 1 {
		t.Fatalf("the raise was recorded %d times, want once", len(crumbs))
	}
}

// resendBudget is the configured budget until the run re-runs an elided call,
// then three times it; a budget the operator turned off stays off. Whether
// the floor rule applies is still judged against the configured budget, so a
// raise never lowers the point a run pauses at.
func TestResendBudget_RaisedAfterARerun(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "80000")
	e := heldEngine(t, "raise-budget", nil)
	recordContextMax("raise-budget", 10_000_000)
	if got := e.resendBudget(); got != 80_000 {
		t.Fatalf("resendBudget before a re-run = %d, want 80000", got)
	}
	if got := e.checkpointThreshold(e.model); got != 80_000 {
		t.Fatalf("checkpoint before a re-run at %d, want 80000", got)
	}
	e.elisionStopped = true
	if got := e.resendBudget(); got != 240_000 {
		t.Fatalf("resendBudget after a re-run = %d, want 240000", got)
	}
	if got := e.checkpointThreshold(e.model); got != 240_000 {
		t.Fatalf("checkpoint after a re-run at %d, want 240000", got)
	}

	// A prefix of 100K crowds the configured 80K budget but not 240K. Judged
	// against the raised budget the threshold would fall from floor + 80K to
	// a plain 240K; it is floor + 240K.
	for _, stopped := range []bool{false, true} {
		e.elisionStopped = stopped
		e.resendPrefix, e.resendFloor = 100_000, 170_000
		want := 250_000
		if stopped {
			want = 410_000
		}
		if got := e.effectiveResendBudget(e.resendBudget()); got != want {
			t.Errorf("stopped=%t: effective budget %d, want %d", stopped, got, want)
		}
	}

	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "0")
	if got := e.resendBudget(); got != 0 {
		t.Fatalf("a budget turned off came back as %d after a re-run", got)
	}
}

// A run without a resend budget (the window checkpoint) has nothing to raise:
// the re-run stops elision, and no [context_budget_raised] breadcrumb claims
// otherwise.
func TestNoteReruns_NoBudgetNoRaise(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "0")
	e := heldEngine(t, "raise-no-budget", nil)
	e.noteElidedCalls([]string{callKey("fetch", `{"page":"page-a"}`)})
	e.noteReruns(elisionExchange("again", `{"page":"page-a"}`, elisionText(10)))
	if !e.elisionStopped {
		t.Fatal("the re-run was not noticed")
	}
	if crumbs := sessionLines(e.logSession, "[context_budget_raised]"); len(crumbs) != 0 {
		t.Fatalf("raised a budget that is off: %v", crumbs)
	}
}

// A whole scheduled run: the first pause elides, the model then fetches one of
// the stubbed pages again, and the run takes no further pause until its
// prompt reaches three times the budget, where it summarizes rather than
// elides. Before the fix, a pause at every 80K summarized, and the model
// re-fetched what each summary dropped.
func TestRun_Elision_RerunRaisesTheBudget(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY", "true") // on, whatever the shell or a legacy alias says
	t.Setenv("FLEET_SCHEDULED_AUTO_COMPACT", "")
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "80000")
	const rerunAt = 10 // after the first pause, which elides page 1
	sr := &sizedRun{prefix: 10_000, toolSteps: 36, resultChars: 40_000,
		arg: func(n int32) int32 {
			if n == rerunAt {
				return 1
			}
			return n
		}}
	obs, session := sr.run(t, "elide-rerun-raise")

	checkpoints := payloadsOf(obs, evtContextCheckpoint)
	compactions := payloadsOf(obs, evtContextCompacted)
	if len(compactions) < 2 || compactions[0][evtFieldTrigger] != "elision" {
		t.Fatalf("want an elision first, then a summary: %v", compactions)
	}
	if paused := checkpointedSteps(obs); len(paused) == 0 || paused[0] >= rerunAt {
		t.Fatalf("fixture: the first pause came after step %v, want it before the re-run at %d", paused, rerunAt)
	}
	raised := sessionLines(session, "[context_budget_raised]")
	if len(raised) != 1 || !strings.HasPrefix(raised[0], "[context_budget_raised] tool=tick budget=80000 raised_budget=240000 ") {
		t.Fatalf("[context_budget_raised] breadcrumbs = %v", raised)
	}
	// The pause that noticed the re-run was taken at the configured budget and
	// compacted nothing; every later one waited for the raised budget.
	if len(checkpoints) != len(compactions)+1 {
		t.Fatalf("checkpoints %v, compactions %v: want exactly one pause that compacted nothing", checkpoints, compactions)
	}
	for i, p := range checkpoints[2:] {
		if p[evtFieldResendBudget] != 240_000 || p[evtFieldUsedTokens].(int) < 240_000 {
			t.Errorf("checkpoint %d after the raise: %v, want it at 240000 or more", i+3, p)
		}
	}
	for i, p := range compactions[1:] {
		if p[evtFieldTrigger] != "resend_budget" || p[evtFieldResendBudget] != 240_000 {
			t.Errorf("compaction %d after the raise: %v, want a summary against 240000", i+2, p)
		}
	}
	crumbs := sessionLines(session, "[context_checkpoint] ")
	if last := crumbs[len(crumbs)-1]; !strings.Contains(last, "reached the run's resend budget (240000, raised from FLEET_CONTEXT_RESEND_BUDGET_TOKENS after it re-ran an elided call)") {
		t.Errorf("the last [context_checkpoint] does not name the raised budget: %q", last)
	}
	// The run grew past twice the configured budget before it paused again.
	if top := slices.Max(sr.sizes); top < 2*80_000 {
		t.Errorf("the largest call resent %d, want the run to grow past 160000 after the raise (sizes=%v)", top, sr.sizes)
	}
}

// A re-run of a call whose bulky result a resend-budget summary dropped raises
// the budget, with elision off: the run fetched again what the summary took,
// and at the configured budget the next pause would take it again. A repeat
// of a call whose result the summary kept, and a new call, change nothing.
func TestCheckContextPressure_RerunOfASummarizedCallRaisesTheBudget(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "80000")
	e := heldEngine(t, "summary-rerun-pressure", nil)
	t.Setenv("FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY", "false")
	recordContextMax("summary-rerun-pressure", 10_000_000)
	e.compactionSummarizer = func(_ context.Context, _ CompactionSummarizeInput) fantasy.Message {
		return fantasy.NewUserMessage(compactionSummaryPrefix + "] summary")
	}
	e.logSession.LastStepPromptTokens = 100_000
	obs := &payloadObserver{}
	history := elisionHistory(14, 40_000)
	res := e.checkContextPressure(context.Background(), history, e.model, newStreamSink(obs), false)
	if c := payloadsOf(obs, evtContextCompacted); len(c) != 1 || c[0][evtFieldTrigger] != "resend_budget" {
		t.Fatalf("fixture: want one resend_budget summary, got %v", c)
	}
	// Exactly the pages whose exchanges the summary dropped are recorded.
	keptCalls := map[string]bool{}
	for _, m := range res.messages {
		for _, part := range m.Content {
			if tc, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
				keptCalls[callKey(tc.ToolName, tc.Input)] = true
			}
		}
	}
	want := map[string]bool{}
	for i := range 14 {
		if k := callKey("fetch", `{"page":"page-`+string(rune('a'+i))+`"}`); !keptCalls[k] {
			want[k] = true
		}
	}
	if len(want) == 0 || len(want) == 14 || !maps.Equal(e.summarizedCallKeys, want) {
		t.Fatalf("summarized calls = %v, want the dropped pages %v", e.summarizedCallKeys, want)
	}

	kept := elisionExchange("kept-again", `{"page":"page-n"}`, elisionText(40_000))
	fresh := elisionExchange("fresh", `{"page":"fresh"}`, elisionText(40_000))
	obs = &payloadObserver{}
	next := append(slices.Clone(res.messages), kept...)
	next = append(next, fresh...)
	res = e.checkContextPressure(context.Background(), next, e.model, newStreamSink(obs), false)
	if c := payloadsOf(obs, evtContextCompacted); len(c) != 1 || e.summaryRerun {
		t.Fatalf("with no re-run of a summarized call, want another summary, got %v (raised=%t)", c, e.summaryRerun)
	}
	if crumbs := sessionLines(e.logSession, "[context_budget_raised]"); len(crumbs) != 0 {
		t.Fatalf("budget raised with no re-run of a summarized call: %v", crumbs)
	}

	again := elisionExchange("again", `{"page":"page-a"}`, elisionText(40_000))
	rerun := append(slices.Clone(res.messages), again...)
	obs = &payloadObserver{}
	res = e.checkContextPressure(context.Background(), rerun, e.model, newStreamSink(obs), false)
	if c := payloadsOf(obs, evtContextCompacted); len(c) != 0 || !e.summaryRerun || e.elisionStopped || len(res.messages) != len(rerun) {
		t.Fatalf("after a re-run, 100K is under the raised budget: want no compaction and elision left alone, got %v (raised=%t, stopped=%t)", c, e.summaryRerun, e.elisionStopped)
	}
	crumbs := sessionLines(e.logSession, "[context_budget_raised]")
	if len(crumbs) != 1 || crumbs[0] != "[context_budget_raised] tool=fetch budget=80000 raised_budget=240000 — the run re-ran a call whose result a summary had dropped, "+
		"a sign it needs more of its data at once than the budget holds: its resend budget is raised for the rest of the run" {
		t.Fatalf("[context_budget_raised] breadcrumbs = %v", crumbs)
	}

	e.logSession.LastStepPromptTokens = 250_000
	obs = &payloadObserver{}
	e.checkContextPressure(context.Background(), res.messages, e.model, newStreamSink(obs), false)
	if c := payloadsOf(obs, evtContextCompacted); len(c) != 1 || c[0][evtFieldResendBudget] != 240_000 {
		t.Fatalf("over the raised budget, want one summary against 240000, got %v", c)
	}
	summaries := sessionLines(e.logSession, "[context_compacted]")
	if last := summaries[len(summaries)-1]; !strings.Contains(last, "exceeded the run's resend budget (240000, raised from FLEET_CONTEXT_RESEND_BUDGET_TOKENS after it re-ran a summarized call)") {
		t.Fatalf("the last [context_compacted] does not name the raised budget: %q", last)
	}
}

// What a summary records: only a dropped result elision would have stubbed,
// so not a small one, one past the hard cap (always sent as a preview), a
// kept tool's (the plan), media, or one whose call still appears in what the
// summary kept.
func TestNoteSummarizedCalls_OnlyBulkyDataWhoseCopiesAllWent(t *testing.T) {
	e := heldEngine(t, "summary-record", nil)
	media := elisionExchange("shot", `{"page":"shot"}`, fantasy.ToolResultOutputContentMedia{Data: strings.Repeat("A", 40_000), MediaType: "image/png"})
	plan := elisionExchange("plan", `{"command":"view"}`, elisionText(40_000))
	plan[0].Content[0] = fantasy.ToolCallPart{ToolCallID: "plan", ToolName: toolNameTaskTracker, Input: `{"command":"view"}`}
	var dropped []fantasy.Message
	for _, ex := range [][]fantasy.Message{
		elisionExchange("big", `{"page":"big"}`, elisionText(40_000)),
		elisionExchange("small", `{"page":"small"}`, elisionText(elisionStubBytes)),
		elisionExchange("huge", `{"page":"huge"}`, elisionText(HardMaxToolOutputBytes+1)),
		elisionExchange("twin", `{"page":"twin"}`, elisionText(40_000)),
		media, plan,
	} {
		dropped = append(dropped, ex...)
	}
	kept := elisionExchange("twin-2", `{"page":"twin"}`, elisionText(40_000))
	e.noteSummarizedCalls(dropped, kept)
	if want := map[string]bool{callKey("fetch", `{"page":"big"}`): true}; !maps.Equal(e.summarizedCallKeys, want) {
		t.Fatalf("summarized calls = %v, want only the big page's", e.summarizedCallKeys)
	}
}

// With elision on, a summarized re-run leaves elision on (no stub cost the
// run anything) and the elided kind wins when a call was both: the crumb then
// says elision stops.
func TestNoteReruns_SummarizedLeavesElisionOnElidedWins(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "80000")
	e := heldEngine(t, "summary-rerun-kinds", nil)
	recordContextMax("summary-rerun-kinds", 10_000_000)
	e.summarizedCallKeys = map[string]bool{callKey("fetch", `{"page":"a"}`): true, callKey("fetch", `{"page":"b"}`): true}
	e.noteReruns(elisionExchange("a2", `{"page":"a"}`, elisionText(10)))
	if !e.summaryRerun || e.elisionStopped || e.elisionHeld() {
		t.Fatalf("after a summarized re-run: raised=%t stopped=%t held=%t, want raised and elision on", e.summaryRerun, e.elisionStopped, e.elisionHeld())
	}

	e = heldEngine(t, "summary-rerun-kinds", nil)
	e.summarizedCallKeys = map[string]bool{callKey("fetch", `{"page":"b"}`): true}
	e.noteElidedCalls([]string{callKey("fetch", `{"page":"b"}`)})
	e.noteReruns(elisionExchange("b2", `{"page":"b"}`, elisionText(10)))
	crumbs := sessionLines(e.logSession, "[context_budget_raised]")
	if !e.elisionStopped || len(crumbs) != 1 || !strings.Contains(crumbs[0], "whose result an elision had replaced with a preview, ") ||
		!strings.HasSuffix(crumbs[0], "it stops eliding, and its resend budget is raised for the rest of the run") {
		t.Fatalf("a call both elided and summarized: stopped=%t crumbs=%v, want the elided kind", e.elisionStopped, crumbs)
	}
}

// The order of the kinds: the first re-run raises the budget and writes the
// one breadcrumb, whichever kind it is, in one check or across two; a later
// elided re-run still stops elision, and the budget keeps the kind that raised
// it. Two summarized re-runs in one check raise it once.
func TestNoteReruns_TheFirstKindRaisesOnce(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "80000")
	call := func(page string) []fantasy.Message {
		return elisionExchange(page+"-again", `{"page":"`+page+`"}`, elisionText(10))
	}
	fresh := func() *engine {
		e := heldEngine(t, "rerun-kinds", nil)
		recordContextMax("rerun-kinds", 10_000_000)
		e.summarizedCallKeys = map[string]bool{callKey("fetch", `{"page":"a"}`): true, callKey("fetch", `{"page":"c"}`): true}
		e.noteElidedCalls([]string{callKey("fetch", `{"page":"b"}`)})
		return e
	}
	check := func(name string, e *engine, wantStopped bool) {
		t.Helper()
		crumbs := sessionLines(e.logSession, "[context_budget_raised]")
		if len(crumbs) != 1 || !strings.Contains(crumbs[0], "whose result a summary had dropped, ") || e.raisedBy != rerunSummarized ||
			e.elisionStopped != wantStopped || !strings.Contains(e.resendBudgetName(), "after it re-ran a summarized call") {
			t.Errorf("%s: crumbs=%v raisedBy=%q stopped=%t name=%q", name, crumbs, e.raisedBy, e.elisionStopped, e.resendBudgetName())
		}
	}

	e := fresh()
	e.noteReruns(append(call("a"), call("b")...))
	check("summarized then elided, one check", e, true)

	e = fresh()
	first := call("a")
	e.noteReruns(first)
	e.pressureCheckedLen = len(first)
	e.noteReruns(append(slices.Clone(first), call("b")...))
	check("summarized then elided, two checks", e, true)

	e = fresh()
	e.noteReruns(append(call("a"), call("c")...))
	check("two summarized re-runs, one check", e, false)
}

// Elision stays on after a summarized re-run, and from then on works against
// the raised budget: past it, the pause trims the older results first, as it
// would at the configured budget, instead of buying a summary.
func TestCheckContextPressure_ElisionAgainstTheRaisedBudget(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "80000")
	e := heldEngine(t, "summary-rerun-elide", nil)
	recordContextMax("summary-rerun-elide", 10_000_000)
	e.compactionSummarizer = func(_ context.Context, _ CompactionSummarizeInput) fantasy.Message {
		return fantasy.NewUserMessage(compactionSummaryPrefix + "] summary")
	}
	e.summaryRerun, e.raisedBy = true, rerunSummarized
	e.logSession.LastStepPromptTokens = 250_000
	obs := &payloadObserver{}
	e.checkContextPressure(context.Background(), elisionHistory(20, 40_000), e.model, newStreamSink(obs), false)
	c := payloadsOf(obs, evtContextCompacted)
	if len(c) == 0 || c[0][evtFieldTrigger] != "elision" || c[0][evtFieldResendBudget] != 240_000 {
		t.Fatalf("past the raised budget, want an elision against 240000 first, got %v", c)
	}
}

// The window compaction (a run with no resend budget) records nothing: there
// is no budget to raise.
func TestCheckContextPressure_WindowSummaryRecordsNothing(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "0")
	t.Setenv("FLEET_SCHEDULED_AUTO_COMPACT", "1")
	e := heldEngine(t, "summary-window", nil)
	t.Setenv("FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY", "false")
	recordContextMax("summary-window", 200_000)
	e.compactionSummarizer = func(_ context.Context, _ CompactionSummarizeInput) fantasy.Message {
		return fantasy.NewUserMessage(compactionSummaryPrefix + "] summary")
	}
	e.logSession.LastStepPromptTokens = 190_000
	obs := &payloadObserver{}
	e.checkContextPressure(context.Background(), elisionHistory(14, 40_000), e.model, newStreamSink(obs), false)
	if c := payloadsOf(obs, evtContextCompacted); len(c) != 1 || c[0][evtFieldTrigger] != "window" {
		t.Fatalf("fixture: want one window summary, got %v", c)
	}
	if len(e.summarizedCallKeys) != 0 {
		t.Fatalf("the window summary recorded %v, want nothing", e.summarizedCallKeys)
	}
}

// The whole loop with elision off: the first pause summarizes the oldest
// pages away, the model fetches page 1 again, and from the next pause on the
// run waits for the raised budget instead of summarizing at every 80K.
func TestRun_Summary_RerunRaisesTheBudget(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY", "false")
	t.Setenv("FLEET_SCHEDULED_AUTO_COMPACT", "")
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "80000")
	const rerunAt = 10 // after the first pause, which summarizes page 1 away
	sr := &sizedRun{prefix: 10_000, toolSteps: 36, resultChars: 40_000,
		arg: func(n int32) int32 {
			if n == rerunAt {
				return 1
			}
			return n
		}}
	obs, session := sr.run(t, "summary-rerun-raise")

	checkpoints := payloadsOf(obs, evtContextCheckpoint)
	compactions := payloadsOf(obs, evtContextCompacted)
	if len(compactions) < 2 {
		t.Fatalf("want a summary before the re-run and one after the raise: %v", compactions)
	}
	for i, c := range compactions {
		if c[evtFieldTrigger] != "resend_budget" {
			t.Fatalf("compaction %d: %v, want only summaries with elision off", i+1, c)
		}
	}
	if paused := checkpointedSteps(obs); len(paused) == 0 || paused[0] >= rerunAt {
		t.Fatalf("fixture: the first pause came after step %v, want it before the re-run at %d", paused, rerunAt)
	}
	raised := sessionLines(session, "[context_budget_raised]")
	if len(raised) != 1 || !strings.HasPrefix(raised[0], "[context_budget_raised] tool=tick budget=80000 raised_budget=240000 — the run re-ran a call whose result a summary had dropped, ") {
		t.Fatalf("[context_budget_raised] breadcrumbs = %v", raised)
	}
	if len(checkpoints) != len(compactions)+1 {
		t.Fatalf("checkpoints %v, compactions %v: want exactly one pause that compacted nothing", checkpoints, compactions)
	}
	for i, p := range checkpoints[2:] {
		if p[evtFieldResendBudget] != 240_000 || p[evtFieldUsedTokens].(int) < 240_000 {
			t.Errorf("checkpoint %d after the raise: %v, want it at 240000 or more", i+3, p)
		}
	}
	if top := slices.Max(sr.sizes); top < 2*80_000 {
		t.Errorf("the largest call resent %d, want the run to grow past 160000 after the raise (sizes=%v)", top, sr.sizes)
	}
}

// The window compaction is held the same way while the audit is pending.
func TestCheckContextPressure_PendingAuditHoldsTheWindowElision(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "0")
	t.Setenv("FLEET_SCHEDULED_AUTO_COMPACT", "1")
	orch := newOrchestrationState(NewLogSession(), 0)
	orch.selfAuditRequested = true
	e := heldEngine(t, "elide-window-audit", orch)
	recordContextMax("elide-window-audit", 200_000)
	e.compactionSummarizer = func(_ context.Context, _ CompactionSummarizeInput) fantasy.Message {
		return fantasy.NewUserMessage(compactionSummaryPrefix + "] summary")
	}
	e.logSession.LastStepPromptTokens = 180_000
	obs := &payloadObserver{}
	e.checkContextPressure(context.Background(), elisionHistory(14, 40_000), e.model, newStreamSink(obs), false)
	if c := payloadsOf(obs, evtContextCompacted); len(c) != 1 || c[0][evtFieldTrigger] != "window" {
		t.Fatalf("want one window summary and no elision, got %v", c)
	}
}

func TestCallKey(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{`{"store_id":"S01","date":"2026-10-21"}`, `{"date": "2026-10-21", "store_id": "S01"}`, true},
		{`{"store_id":"S01"}`, `{"store_id":"S02"}`, false},
		{`not json`, `not json`, true},
		{`not json`, `not  json`, false},
		{`{"id":9007199254740993}`, `{"id":9007199254740992}`, false},
		{`{"a":1} trailing`, `{"a":1}`, false},
	} {
		if got := callKey("fetch", tc.a) == callKey("fetch", tc.b); got != tc.same {
			t.Errorf("callKey(%q) == callKey(%q): %t, want %t", tc.a, tc.b, got, tc.same)
		}
	}
	if callKey("fetch", `{}`) == callKey("read", `{}`) {
		t.Error("two tools with the same input share a key")
	}
}

// On a window too small for three budgets, the raise stops 30% of the budget
// short of the model's input limit, so a call can still reach the pause
// before the in-round guard trims it; on a window with too little room for a
// raise of at least that much, the run keeps its budget and records no raise.
// Under the floor rule the raised threshold stops the same margin short.
func TestResendBudget_RaiseStopsShortOfTheWindow(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "80000")
	e := heldEngine(t, "raise-200k", nil)
	recordContextMax("raise-200k", 200_000)
	limit := e.resendWindowLimit()
	if limit <= 80_000 || limit >= 240_000 {
		t.Fatalf("fixture: a 200K window's limit is %d", limit)
	}
	e.resendPrefix, e.resendFloor = 50_000, 50_000 // crowds the configured 80K
	before := e.effectiveResendBudget(e.resendBudget())
	e.elisionStopped = true
	if got, want := e.resendBudget(), limit-24_000; got != want {
		t.Fatalf("raised budget on a 200K window = %d, want %d (the limit %d less 30%% of the budget)", got, want, limit)
	}
	// Under the floor rule the raised threshold stops the same margin short
	// of the limit (floor + raised budget is past it)...
	if after := e.effectiveResendBudget(e.resendBudget()); after != limit-24_000 || after < before {
		t.Fatalf("floor rule: threshold %d before the raise, %d after; want %d", before, after, limit-24_000)
	}
	// ...but never below where the configured budget pauses: once the floor
	// alone is near the limit, that is the limit, as without a raise.
	e.resendFloor = 150_000
	e.elisionStopped = false
	before = e.effectiveResendBudget(e.resendBudget())
	e.elisionStopped = true
	if after := e.effectiveResendBudget(e.resendBudget()); before != limit || after != limit {
		t.Fatalf("a 150K floor: threshold %d before the raise, %d after; want the limit %d both times", before, after, limit)
	}

	// On 128K the cap is 105216 − 24000 = 81216: too little room to be
	// worth a raise.
	mid := heldEngine(t, "raise-128k", nil)
	recordContextMax("raise-128k", 128_000)
	mid.elisionStopped = true
	if got := mid.resendBudget(); got != 80_000 {
		t.Fatalf("a 128K window leaves no room for a raise worth making, yet the budget is %d", got)
	}

	small := heldEngine(t, "raise-64k", nil)
	recordContextMax("raise-64k", 64_000)
	small.noteElidedCalls([]string{callKey("fetch", `{"page":"page-a"}`)})
	small.noteReruns(elisionExchange("again", `{"page":"page-a"}`, elisionText(10)))
	if !small.elisionStopped {
		t.Fatal("the re-run was not noticed")
	}
	if got := small.resendBudget(); got != 80_000 {
		t.Fatalf("a 64K window has no room for a raise, yet the budget is %d", got)
	}
	if crumbs := sessionLines(small.logSession, "[context_budget_raised]"); len(crumbs) != 0 {
		t.Fatalf("recorded a raise that did not happen: %v", crumbs)
	}
}

// The whole run on a 200K window: after the re-run it pauses at the capped
// budget, and the in-round guard never has to trim a call. The 10K-token task
// message is a real one, so the guard counts it as the model does.
func TestRun_Elision_RerunRaiseOnASmallWindow(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY", "true") // on, whatever the shell or a legacy alias says
	t.Setenv("FLEET_SCHEDULED_AUTO_COMPACT", "")
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "80000")
	// The input limit is 200000 − 16384 (completion) − 10000 (provider
	// reserve) = 173616; the raise stops 24000 short of it.
	const raised = 149_616
	sr := &sizedRun{headChars: 40_000, toolSteps: 40, resultChars: 40_000, window: 200_000,
		arg: func(n int32) int32 {
			if n == 10 {
				return 1
			}
			return n
		}}
	obs, session := sr.run(t, "elide-rerun-raise-200k")

	crumbs := sessionLines(session, "[context_budget_raised]")
	if len(crumbs) != 1 || !strings.HasPrefix(crumbs[0], fmt.Sprintf("[context_budget_raised] tool=tick budget=80000 raised_budget=%d ", raised)) {
		t.Fatalf("[context_budget_raised] breadcrumbs = %v", crumbs)
	}
	checkpoints := payloadsOf(obs, evtContextCheckpoint)
	if len(checkpoints) < 3 {
		t.Fatalf("checkpoints %v: want pauses after the raise", checkpoints)
	}
	for i, p := range checkpoints[2:] {
		if p[evtFieldResendBudget] != raised || p[evtFieldUsedTokens].(int) < raised {
			t.Errorf("checkpoint %d after the raise: %v, want it at %d or more", i+3, p, raised)
		}
	}
	if n := len(payloadsOf(obs, "fleet.tool_context_reduced")); n != 0 {
		t.Errorf("the in-round guard trimmed %d calls; the pause should come first", n)
	}
	if top := slices.Max(sr.sizes); top > 173_616 {
		t.Errorf("a call resent %d, past the model's input limit 173616", top)
	}
}
