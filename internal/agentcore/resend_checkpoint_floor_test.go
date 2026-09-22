package agentcore

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"
)

// Tests for the resend-budget checkpoint's floor rule (#1600): when what every
// call resends before any history exists takes more than half the budget, the
// budget applies to the history alone.

// sizedPromptTokens sizes a call the way a provider bills it, for fixtures that
// need a compaction to actually shrink the resent prompt: a fixed prefix (the
// system prompt and tool schemas, which no compaction can shed) plus ~4 chars
// per token of every message's text, tool-call input and tool-result output.
func sizedPromptTokens(prefix int, call fantasy.Call) int {
	chars := 0
	for _, m := range call.Prompt {
		if m.Role == fantasy.MessageRoleSystem {
			continue
		}
		for _, part := range m.Content {
			if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
				chars += len(tp.Text)
			}
			if tc, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
				chars += len(tc.Input)
			}
			if tr, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
				if out, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](tr.Output); ok {
					chars += len(out.Text)
				}
			}
		}
	}
	return prefix + chars/4
}

// sizedRun is a scheduled run whose model reports sizedPromptTokens(prefix)
// for every call and asks for toolSteps tick calls (each returning resultChars
// of output) before it answers.
type sizedRun struct {
	prefix, toolSteps, resultChars int

	mu    sync.Mutex
	sizes []int // the resent size of each call, in call order
}

func (sr *sizedRun) run(t *testing.T, slug string) (*payloadObserver, *LogSession) {
	t.Helper()
	var ticks atomic.Int32
	tick := fantasy.NewAgentTool("tick", "fetch the next page of data",
		func(context.Context, tickInput, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			ticks.Add(1)
			return fantasy.NewTextResponse(strings.Repeat("d", sr.resultChars)), nil
		})
	// A window far above anything the fixture sends, so neither the window-
	// pressure path nor the pre-call context reducer touches the history.
	recordContextMax(slug, 10_000_000)
	model := &namedMockModel{name: slug}
	var calls atomic.Int32
	model.streamFunc = func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
		n := calls.Add(1)
		size := sizedPromptTokens(sr.prefix, call)
		sr.mu.Lock()
		sr.sizes = append(sr.sizes, size)
		sr.mu.Unlock()
		if int(n) <= sr.toolSteps {
			return toolStep(fantasy.Usage{InputTokens: int64(size), OutputTokens: 10}, fmt.Sprintf("page-%d", n), n), nil
		}
		return streamTextThenFinish("every page refreshed"), nil
	}
	obs := &payloadObserver{}
	session := NewLogSession()
	res, err := Run(context.Background(), ModeScheduled,
		RunConfig{EnvPrefix: CanonicalEnvPrefix, RequireCompactionOptIn: true, NativeTools: []fantasy.AgentTool{tick}},
		Deps{
			Input:      historyInput{system: "s", msgs: fillerMessages(1, 400), label: "refresh"},
			Observer:   obs,
			Policy:     finishableScheduledPolicy(session),
			Model:      model,
			LogSession: session,
		})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := int(ticks.Load()); got != sr.toolSteps {
		t.Fatalf("tool ran %d times, want %d — a checkpoint must never drop or replay a step", got, sr.toolSteps)
	}
	if !strings.Contains(res.FinalText, "every page refreshed") {
		t.Fatalf("final text = %q, want the model's closing answer", res.FinalText)
	}
	return obs, session
}

// checkpointedSteps returns, for each resend-budget checkpoint event, the
// 1-based tool step it paused after (the tool.call events seen before it).
func checkpointedSteps(obs *payloadObserver) []int {
	var out []int
	steps := 0
	for i, e := range obs.events {
		switch e {
		case "tool.call":
			steps++
		case evtContextCheckpoint:
			if obs.payloads[i][evtFieldTrigger] == "resend_budget" {
				out = append(out, steps)
			}
		}
	}
	return out
}

func payloadsOf(obs *payloadObserver, eventType string) []map[string]any {
	var out []map[string]any
	for i, e := range obs.events {
		if e == eventType {
			out = append(out, obs.payloads[i])
		}
	}
	return out
}

func sessionLines(session *LogSession, marker string) []string {
	var out []string
	for _, m := range session.Messages {
		if strings.Contains(m.Content, marker) {
			out = append(out, m.Content)
		}
	}
	return out
}

// A Pages refresh resends ~140K before any history exists (system prompt, a
// 44-tool catalog, the task prompt) against the 80K default budget, and before
// #1600 paused on EVERY tool step until the 40-pause cap. With the floor rule
// the run takes no pause until the history alone has grown by a full budget,
// then exactly one pause and one compaction — and the floor is re-measured
// after it, so the post-compaction steps, which resend well past the ORIGINAL
// floor + budget, run on without pausing again.
func TestRun_ResendCheckpoint_FloorAboveBudgetWaitsForABudgetOfHistory(t *testing.T) {
	t.Setenv("FLEET_SCHEDULED_AUTO_COMPACT", "")
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "80000")
	const budget = 80_000
	sr := &sizedRun{prefix: 140_000, toolSteps: 16, resultChars: 40_000} // ~10K tokens per result
	obs, session := sr.run(t, "cp-floor-high")

	floor := sr.sizes[0]
	if floor <= budget {
		t.Fatalf("fixture: the first step must already resend over the budget, got %d", floor)
	}
	paused := checkpointedSteps(obs)
	if len(paused) != 1 {
		t.Fatalf("checkpoints after steps %v, want exactly one (sizes=%v)", paused, sr.sizes)
	}
	at := paused[0]
	for step := 1; step < at; step++ {
		if grown := sr.sizes[step-1] - floor; grown >= budget {
			t.Fatalf("step %d had grown %d over the floor without a pause", step, grown)
		}
	}
	if grown := sr.sizes[at-1] - floor; grown < budget {
		t.Fatalf("the pause after step %d came before the history grew by a budget (grown %d)", at, grown)
	}
	cp := payloadsOf(obs, evtContextCheckpoint)[0]
	if cp[evtFieldUsedTokens] != sr.sizes[at-1] || cp[evtFieldResendBudget] != budget ||
		cp[evtFieldResendFloor] != floor || cp[evtFieldEffectiveBudget] != floor+budget {
		t.Errorf("checkpoint payload must carry the resent size, budget, floor and effective budget, got %v", cp)
	}

	compactions := payloadsOf(obs, evtContextCompacted)
	if len(compactions) != 1 || compactions[0][evtFieldTrigger] != "resend_budget" ||
		compactions[0][evtFieldEffectiveBudget] != floor+budget {
		t.Fatalf("want exactly one resend_budget compaction naming the effective budget, got %v", compactions)
	}

	// The floor reset: the compaction shrank the prompt, and later steps grew
	// past the ORIGINAL floor + budget — a floor that stayed at the first
	// step's size would have paused again there.
	if sr.sizes[at] >= sr.sizes[at-1] {
		t.Fatalf("the compaction did not shrink the prompt: %d -> %d", sr.sizes[at-1], sr.sizes[at])
	}
	pastOriginal := false
	for _, size := range sr.sizes[at:sr.toolSteps] {
		if size >= floor+budget {
			pastOriginal = true
		}
	}
	if !pastOriginal {
		t.Fatalf("fixture: no post-compaction step reached the original floor + budget (%d), so the reset is untested (sizes=%v)", floor+budget, sr.sizes)
	}

	crumbs := sessionLines(session, "[context_checkpoint_floor]")
	if len(crumbs) != 1 {
		t.Fatalf("want exactly one [context_checkpoint_floor] breadcrumb, got %d: %v", len(crumbs), crumbs)
	}
	if want := fmt.Sprintf("floor=%d budget=%d effective_budget=%d", floor, budget, floor+budget); !strings.Contains(crumbs[0], want) {
		t.Errorf("floor breadcrumb = %q, want it to state %q", crumbs[0], want)
	}
	if lines := sessionLines(session, "[context_checkpoint] "); len(lines) != 1 || !strings.Contains(lines[0], fmt.Sprintf("over the prompt floor (floor=%d effective_budget=%d)", floor, floor+budget)) {
		t.Errorf("checkpoint breadcrumb must name the floor rule, got %v", lines)
	}
}

// A small prefix keeps the pre-#1600 rule exactly: the checkpoint fires after
// every tool step whose resent prompt reached the plain budget and after no
// other, and its payload and breadcrumb are the pre-#1600 ones. The fixture's
// compactions are real (the sized model reports what it was sent), so this is
// the plain rule cycling through several pause/compact/resume rounds.
func TestRun_ResendCheckpoint_SmallPrefixKeepsThePlainBudget(t *testing.T) {
	t.Setenv("FLEET_SCHEDULED_AUTO_COMPACT", "")
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "20000")
	const budget = 20_000
	sr := &sizedRun{prefix: 2_000, toolSteps: 20, resultChars: 12_000} // ~3K tokens per result
	obs, session := sr.run(t, "cp-floor-small")

	var want []int
	for step := 1; step <= sr.toolSteps; step++ {
		if sr.sizes[step-1] >= budget {
			want = append(want, step)
		}
	}
	got := checkpointedSteps(obs)
	if len(want) < 3 {
		t.Fatalf("fixture: want several over-budget steps, got %v (sizes=%v)", want, sr.sizes)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("checkpoints after steps %v, want exactly the steps that reached the plain budget %v (sizes=%v)", got, want, sr.sizes)
	}
	for _, p := range payloadsOf(obs, evtContextCheckpoint) {
		if len(p) != 4 || p[evtFieldResendBudget] != budget {
			t.Errorf("plain-budget checkpoint payload changed: %v", p)
		}
	}
	for _, p := range payloadsOf(obs, evtContextCompacted) {
		if _, ok := p[evtFieldResendFloor]; ok {
			t.Errorf("plain-budget compaction payload gained floor fields: %v", p)
		}
	}
	if crumbs := sessionLines(session, "[context_checkpoint_floor]"); len(crumbs) != 0 {
		t.Errorf("a small prefix must not engage the floor rule, got %v", crumbs)
	}
	lines := sessionLines(session, "[context_checkpoint] ")
	if len(lines) != len(want) {
		t.Fatalf("checkpoint breadcrumbs = %d, want %d", len(lines), len(want))
	}
	first := fmt.Sprintf("[context_checkpoint] resent prompt %d tokens reached FLEET_CONTEXT_RESEND_BUDGET_TOKENS; the tool loop paused after %d step(s) so the history can be compacted before the next call (checkpoint 1)", sr.sizes[want[0]-1], want[0])
	if lines[0] != first {
		t.Errorf("checkpoint breadcrumb changed:\n got  %q\n want %q", lines[0], first)
	}
}

// The checkpoint never fires on a history too short to summarize: fewer than
// minResendCheckpointHistory messages after the pinned head — counting the
// round's input and the steps' own messages — however far over the budget the
// step is. Both the StopCondition and the run loop's re-evaluation hold it.
func TestResendBudgetCheckpoint_NeedsHistoryToSummarize(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "1000")
	e := newMockEngine(t, &namedMockModel{name: "cp-history"})
	e.envPrefix = CanonicalEnvPrefix
	e.requireCompactionOptIn = true
	overStep := fantasy.StepResult{
		Response: fantasy.Response{FinishReason: fantasy.FinishReasonToolCalls, Usage: fantasy.Usage{InputTokens: 50_000}},
		Messages: []fantasy.Message{fantasy.NewUserMessage("call"), fantasy.NewUserMessage("result")},
	}
	result := &fantasy.AgentResult{Steps: []fantasy.StepResult{overStep}}

	// head + 1 message + the step's 2 = 3 droppable messages.
	short := fillerMessages(2, 10)
	if e.resendBudgetCheckpoint(short)(result.Steps) {
		t.Fatal("the StopCondition paused with 3 droppable messages")
	}
	if e.consumeResendCheckpoint(result, 1, short) {
		t.Fatal("the run loop counted a pause with 3 droppable messages")
	}
	if e.resendCheckpoints != 0 {
		t.Fatalf("resendCheckpoints = %d, want 0", e.resendCheckpoints)
	}

	// head + 2 messages + the step's 2 = 4: enough to summarize.
	enough := fillerMessages(3, 10)
	if !e.resendBudgetCheckpoint(enough)(result.Steps) {
		t.Fatal("the StopCondition must pause with 4 droppable messages")
	}
	if !e.consumeResendCheckpoint(result, 1, enough) {
		t.Fatal("the run loop must count a pause with 4 droppable messages")
	}
}

// The floor state machine: the prefix is the smallest floor ever measured, the
// floor is re-measured at every floor point, and the rule is decided on the
// prefix — so a small-prefix run whose post-compaction prompt is large keeps
// the plain budget.
func TestEffectiveResendBudget_FloorRule(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "80000")
	const budget = 80_000
	usage := func(in, cached int64) fantasy.Usage { return fantasy.Usage{InputTokens: in, CacheReadTokens: cached} }
	scheduled := func() *engine {
		e := newMockEngine(t, &namedMockModel{name: "cp-floor-rule"})
		e.envPrefix = CanonicalEnvPrefix
		e.requireCompactionOptIn = true
		return e
	}

	e := scheduled()
	if got := e.effectiveResendBudget(budget); got != budget {
		t.Fatalf("unmeasured: effective = %d, want the plain budget", got)
	}
	e.noteResendFloor(usage(0, 0))
	if e.resendPrefix != 0 {
		t.Fatal("a step with no reported usage must not set the floor")
	}
	e.noteResendFloor(usage(40_000, 100_000)) // cache reads count: the whole request is resent
	if e.resendFloor != 140_000 || e.resendPrefix != 140_000 || e.effectiveResendBudget(budget) != 220_000 {
		t.Fatalf("first step 140K: floor=%d prefix=%d effective=%d, want 140000/140000/220000", e.resendFloor, e.resendPrefix, e.effectiveResendBudget(budget))
	}
	e.noteResendFloor(usage(230_000, 0))
	if e.resendFloor != 140_000 {
		t.Fatal("a step that is not a floor point must not move the floor")
	}
	e.resendFloorStale = true // a compaction happened
	e.noteResendFloor(usage(190_000, 0))
	if e.resendFloor != 190_000 || e.resendPrefix != 140_000 || e.effectiveResendBudget(budget) != 270_000 {
		t.Fatalf("post-compaction 190K: floor=%d prefix=%d effective=%d, want 190000/140000/270000", e.resendFloor, e.resendPrefix, e.effectiveResendBudget(budget))
	}
	if crumbs := sessionLines(e.logSession, "[context_checkpoint_floor]"); len(crumbs) != 1 {
		t.Fatalf("want exactly one floor breadcrumb across floor points, got %d", len(crumbs))
	}
	e.resendFloorStale = true
	e.noteResendFloor(usage(30_000, 0)) // e.g. a smaller tool roster after a rebuild
	if e.resendPrefix != 30_000 || e.effectiveResendBudget(budget) != budget {
		t.Fatalf("a floor under half the budget lowers the prefix and restores the plain budget: prefix=%d effective=%d", e.resendPrefix, e.effectiveResendBudget(budget))
	}

	small := scheduled()
	small.noteResendFloor(usage(20_000, 0))
	small.resendFloorStale = true
	small.noteResendFloor(usage(95_000, 0)) // a large kept half, over the budget itself
	if got := small.effectiveResendBudget(budget); got != budget {
		t.Fatalf("small prefix, large post-compaction floor: effective = %d, want the plain budget", got)
	}
	if crumbs := sessionLines(small.logSession, "[context_checkpoint_floor]"); len(crumbs) != 0 {
		t.Fatalf("a small prefix must not write the floor breadcrumb, got %v", crumbs)
	}

	interactive := newMockEngine(t, &namedMockModel{name: "cp-floor-chat"})
	interactive.noteResendFloor(usage(140_000, 0))
	if interactive.resendPrefix != 0 {
		t.Fatal("an interactive engine has no checkpoint and must not track a floor")
	}
}

// The pre-round resend-budget compaction uses the same threshold as the
// checkpoint: under the floor rule a prompt over the plain budget but under
// floor + budget is left alone, and one past it compacts, says why, and marks
// the floor for re-measurement.
func TestCheckContextPressure_ResendBudgetFollowsTheFloorRule(t *testing.T) {
	t.Setenv("FLEET_CONTEXT_RESEND_BUDGET_TOKENS", "80000")
	slug := "cp-floor-pressure"
	recordContextMax(slug, 10_000_000)
	model := &namedMockModel{name: slug}
	e := newMockEngine(t, model)
	e.envPrefix = CanonicalEnvPrefix
	e.requireCompactionOptIn = true
	e.noteResendFloor(fantasy.Usage{InputTokens: 140_000})
	msgs := fillerMessages(8, 400)

	obs := &payloadObserver{}
	e.logSession.LastStepPromptTokens = 150_000
	if res := e.checkContextPressure(context.Background(), msgs, model, newStreamSink(obs), false); len(obs.events) != 0 || len(res.messages) != len(msgs) {
		t.Fatalf("150K is over the plain budget but under floor + budget: nothing to do, events=%v", obs.events)
	}

	e.logSession.LastStepPromptTokens = 225_000
	res := e.checkContextPressure(context.Background(), msgs, model, newStreamSink(obs), false)
	p := obs.payloadOf(evtContextCompacted)
	if p == nil || len(res.messages) >= len(msgs) {
		t.Fatalf("225K is past floor + budget: must compact, events=%v", obs.events)
	}
	if p[evtFieldResendFloor] != 140_000 || p[evtFieldEffectiveBudget] != 220_000 {
		t.Errorf("compaction payload must name the floor and effective budget, got %v", p)
	}
	if lines := sessionLines(e.logSession, "[context_compacted] trigger=resend_budget used=225000 budget=80000 floor=140000 effective_budget=220000"); len(lines) != 1 {
		t.Errorf("compaction breadcrumb must name the floor rule, got %v", e.logSession.Messages)
	}
	if !e.resendFloorStale {
		t.Error("a compaction must mark the floor for re-measurement at the next step")
	}
}
