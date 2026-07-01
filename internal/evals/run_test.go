package evals

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/fakellm"
)

// fakeTurnRunner is an evals.TurnRunner double: RunTurn returns a canned final
// text per case prompt (or an error), Resolve delegates to a real
// agentcore.ModelResolver pointed at the fake LLM so llm_judge scorers run the
// genuine judge path.
type fakeTurnRunner struct {
	replies  map[string]string // prompt → final text
	failFor  map[string]error  // prompt → run error
	resolver *agentcore.ModelResolver
	turns    []agent.TurnInput
}

func (f *fakeTurnRunner) RunTurn(_ context.Context, in agent.TurnInput, _ agent.EventSink) (*agent.TurnResult, error) {
	f.turns = append(f.turns, in)
	if err, ok := f.failFor[in.UserMessage]; ok {
		return nil, err
	}
	return &agent.TurnResult{
		FinalText:        f.replies[in.UserMessage],
		Model:            in.Model,
		CostUSD:          0.01,
		PromptTokens:     10,
		CompletionTokens: 5,
	}, nil
}

func (f *fakeTurnRunner) Resolve(ctx context.Context, slug string) (fantasy.LanguageModel, error) {
	if f.resolver == nil {
		return nil, errors.New("no resolver wired")
	}
	return f.resolver.Resolve(ctx, slug)
}

func newFakeResolver(t *testing.T, fake *fakellm.Server) *agentcore.ModelResolver {
	t.Helper()
	ts := httptest.NewServer(fake.Handler())
	t.Cleanup(ts.Close)
	t.Setenv("OPENROUTER_BASE_URL", ts.URL+"/api/v1")
	r, err := agentcore.NewModelResolver("test-key", agentcore.DefaultProviderHeaders)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func floatPtr(v float64) *float64 { return &v }

func TestRunSet_DeterministicScoringAndGate(t *testing.T) {
	set := &Set{
		Name:      "smoke",
		Threshold: floatPtr(0.5),
		Cases: []Case{
			{Name: "passes", Prompt: "p1", Model: "m", Scorers: []ScorerSpec{{Contains: "hello"}, {Regex: "(?i)WORLD"}}},
			{Name: "fails", Prompt: "p2", Model: "m", Scorers: []ScorerSpec{{Equals: "exact"}}},
			{Name: "errors", Prompt: "p3", Model: "m", Scorers: []ScorerSpec{{Contains: "x"}}},
		},
	}
	tr := &fakeTurnRunner{
		replies: map[string]string{"p1": "hello world", "p2": "not exact"},
		failFor: map[string]error{"p3": errors.New("model exploded")},
	}
	res, err := RunSet(context.Background(), tr, set, Options{RunID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 3 || res.Passed != 1 {
		t.Fatalf("got total=%d passed=%d", res.Total, res.Passed)
	}
	// 1/3 passed < 0.5 threshold → gate fails.
	if res.Pass {
		t.Fatal("gate must fail below threshold")
	}
	if res.Cases[0].Score != 1.0 || !res.Cases[0].Pass {
		t.Fatalf("case 1: %+v", res.Cases[0])
	}
	if res.Cases[1].Pass || res.Cases[1].Score != 0.0 {
		t.Fatalf("case 2: %+v", res.Cases[1])
	}
	if res.Cases[2].Error == "" || res.Cases[2].Pass {
		t.Fatalf("case 3 must record the run error: %+v", res.Cases[2])
	}
	if res.CostUSD != 0.02 { // two successful turns × $0.01
		t.Fatalf("cost aggregation: got %v", res.CostUSD)
	}
	// Each case must have run in its own conversation workspace.
	if tr.turns[0].ConversationID == tr.turns[1].ConversationID {
		t.Fatal("cases must not share a conversation id")
	}
	// Threshold met when enough pass.
	set.Threshold = floatPtr(1.0 / 3.0)
	res, err = RunSet(context.Background(), tr, set, Options{RunID: "t2"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Pass {
		t.Fatal("gate must pass at threshold")
	}
}

func TestRunSet_JudgeScorer(t *testing.T) {
	fake := fakellm.New()
	tr := &fakeTurnRunner{
		resolver: newFakeResolver(t, fake),
		replies: map[string]string{
			// The echo marker rides the ANSWER text: the judge prompt embeds the
			// answer, so the fake streams back exactly the JSON verdict.
			"good": `[[echo:{"pass": true, "score": 0.9, "reasoning": "solid"}]]`,
			"bad":  `[[echo:{"pass": false, "score": 0.2}]]`,
		},
	}
	set := &Set{
		Name:       "judged",
		JudgeModel: "openai/gpt-5.2",
		Cases: []Case{
			{Name: "good", Prompt: "good", Model: "m", Expected: "ref",
				Scorers: []ScorerSpec{{LLMJudge: &JudgeSpec{Rubric: "faithful?"}}}},
			{Name: "bad", Prompt: "bad", Model: "m",
				Scorers: []ScorerSpec{{LLMJudge: &JudgeSpec{Rubric: "faithful?"}}}},
		},
	}
	res, err := RunSet(context.Background(), tr, set, Options{RunID: "t3"})
	if err != nil {
		t.Fatal(err)
	}
	good, bad := res.Cases[0], res.Cases[1]
	if !good.Pass || good.Score != 0.9 || good.Scorers[0].Label != "judge:0.90" {
		t.Fatalf("good case: %+v", good)
	}
	if good.Scorers[0].Reasoning != "solid" {
		t.Fatalf("judge reasoning must surface: %+v", good.Scorers[0])
	}
	if bad.Pass || bad.Score != 0.2 {
		t.Fatalf("bad case: %+v", bad)
	}
	if res.Passed != 1 || res.Pass { // default threshold 1.0
		t.Fatalf("aggregate: %+v", res)
	}
}

func TestRunSet_JudgeErrorFailsClosed(t *testing.T) {
	tr := &fakeTurnRunner{
		replies: map[string]string{"p": "answer"},
		// resolver nil → judge model resolution errors.
	}
	set := &Set{Name: "s", Cases: []Case{{
		Name: "c", Prompt: "p", Model: "m",
		Scorers: []ScorerSpec{{LLMJudge: &JudgeSpec{Rubric: "r", Model: "judge-model"}}},
	}}}
	res, err := RunSet(context.Background(), tr, set, Options{RunID: "t4"})
	if err != nil {
		t.Fatal(err)
	}
	sr := res.Cases[0].Scorers[0]
	if sr.Pass || sr.Label != "judge:error" || sr.Score != 0 {
		t.Fatalf("judge failure must fail closed: %+v", sr)
	}
}

func TestRunSet_InvocationErrors(t *testing.T) {
	tr := &fakeTurnRunner{}
	if _, err := RunSet(context.Background(), tr, &Set{Name: "empty"}, Options{RunID: "x"}); err == nil {
		t.Fatal("empty set must error")
	}
	set := &Set{Name: "s", Cases: []Case{{Name: "c", Prompt: "p", Model: "m", Scorers: []ScorerSpec{{Contains: "x"}}}}}
	if _, err := RunSet(context.Background(), tr, set, Options{}); err == nil {
		t.Fatal("missing RunID must error")
	}
}

func TestRunJudge_ValidVerdictAndRetry(t *testing.T) {
	fake := fakellm.New()
	// First reply malformed, second valid — exercises the one corrective retry
	// around structuredoutput's no-retry gap.
	fake.Scenario("judge-retry", fakellm.Scenario{Steps: []fakellm.Step{
		fakellm.TextStep("I think it is fine, score high."),
		fakellm.TextStep(`{"pass": true, "score": 0.8}`),
	}})
	resolver := newFakeResolver(t, fake)

	v, err := RunJudge(context.Background(), resolver, "openai/gpt-5.2", "rubric", "prompt", "",
		"[[scenario:judge-retry]] the answer")
	if err != nil {
		t.Fatal(err)
	}
	if !v.Pass || v.Score != 0.8 {
		t.Fatalf("verdict: %+v", v)
	}
	if got := fake.Hits("judge-retry"); got != 2 {
		t.Fatalf("expected exactly one retry (2 hits), got %d", got)
	}
}

func TestRunJudge_GivesUpAfterOneRetry(t *testing.T) {
	fake := fakellm.New()
	fake.Scenario("judge-hopeless", fakellm.Scenario{Steps: []fakellm.Step{
		fakellm.TextStep("still not json"),
		fakellm.TextStep("nope"),
		fakellm.TextStep(`{"pass": true, "score": 1}`), // must never be reached
	}})
	resolver := newFakeResolver(t, fake)
	_, err := RunJudge(context.Background(), resolver, "openai/gpt-5.2", "r", "p", "", "[[scenario:judge-hopeless]] x")
	if err == nil {
		t.Fatal("want validation error after one retry")
	}
	if got := fake.Hits("judge-hopeless"); got != 2 {
		t.Fatalf("must stop after one retry, got %d hits", got)
	}
}

func TestRunJudge_SchemaRejectsOutOfRangeScore(t *testing.T) {
	fake := fakellm.New()
	fake.Scenario("judge-oob", fakellm.Scenario{Steps: []fakellm.Step{
		fakellm.TextStep(`{"pass": true, "score": 1.5}`),
		fakellm.TextStep(`{"pass": true, "score": 1.5}`),
	}})
	resolver := newFakeResolver(t, fake)
	if _, err := RunJudge(context.Background(), resolver, "openai/gpt-5.2", "r", "p", "", "[[scenario:judge-oob]] x"); err == nil {
		t.Fatal("score > 1 must fail schema validation")
	}
}

func TestRunJudge_NoModel(t *testing.T) {
	if _, err := RunJudge(context.Background(), &fakeTurnRunner{}, " ", "r", "p", "", "a"); err == nil {
		t.Fatal("empty judge slug must error")
	}
}
