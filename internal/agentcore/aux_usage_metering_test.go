package agentcore

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/tools"
)

// Auxiliary model-call metering (#1118) regression guards. The invariant under
// test: every model call made on behalf of a run — not just the main loop's
// steps — lands in the SAME accounting checkCeilings and Result.Usage read, or
// (for the documented host-side extras) in an explicit labeled ledger. These
// tests cover the run-loop side: the compaction-summarizer seam and the
// context-carried recorder for model-calling native tools.

// manyMessagesInput seeds a run with enough history that a force-compaction
// actually compacts (forceCompactMessageHistory needs more than head + the
// 20-message tail).
type manyMessagesInput struct{ n int }

func (m manyMessagesInput) Prompt(_ context.Context) (string, []fantasy.Message, string, error) {
	msgs := make([]fantasy.Message, 0, m.n)
	for i := 0; i < m.n; i++ {
		msgs = append(msgs, fantasy.NewUserMessage("filler turn"))
	}
	return "sys", msgs, "aux-usage", nil
}

// TestCompactionSummarizerSpend_FlowsIntoRunUsage drives a real
// context-too-large force-compaction through Run and proves the driver-side
// summarizer's model call is metered into the run's Result.Usage via
// CompactionSummarizeInput.RecordUsage — the #1118 leak: before the seam
// existed the summarizer fired with no RecordUsage at all, exactly when the
// run was already huge. It also pins that the ceiling probe is wired.
func TestCompactionSummarizerSpend_FlowsIntoRunUsage(t *testing.T) {
	// First provider call rejects the prompt as too large (non-retryable in
	// fantasy: no status code, no retry header), forcing one compaction; the
	// re-driven round then completes with 50 in / 10 out.
	calls := 0
	model := &namedMockModel{name: "aux1118-compaction"}
	model.streamFunc = func(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
		calls++
		if calls == 1 {
			return nil, &fantasy.ProviderError{ContextTooLargeErr: true, Message: "prompt too large"}
		}
		return streamStop()(nil, call)
	}

	summarized := false
	res, err := Run(context.Background(), ModeInteractive, RunConfig{EnvPrefix: CanonicalEnvPrefix}, Deps{
		Input:    manyMessagesInput{n: 30},
		Observer: &captureObserver{},
		Policy:   NewInteractivePolicy(0, 0, nil, nil),
		Executor: &stubExecutor{},
		Model:    model,
		CompactionSummarizer: func(_ context.Context, in CompactionSummarizeInput) fantasy.Message {
			if in.RecordUsage == nil {
				t.Fatal("CompactionSummarizeInput.RecordUsage was not wired by Run")
			}
			if in.OverCeiling == nil {
				t.Fatal("CompactionSummarizeInput.OverCeiling was not wired by Run")
			}
			if in.OverCeiling() {
				t.Fatal("run with no ceilings must not report over-ceiling")
			}
			summarized = true
			// Simulate the summarizer's own metered model call.
			in.RecordUsage(fantasy.Usage{InputTokens: 7, OutputTokens: 3}, fantasy.ProviderMetadata{})
			return fantasy.NewUserMessage(compactionSummaryPrefix + "] condensed")
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !summarized {
		t.Fatal("force-compaction never invoked the summarizer")
	}
	// Main pass (50/10) + the summarizer's call (7/3) must BOTH be counted.
	if res.Usage.PromptTokens != 57 || res.Usage.CompletionTokens != 13 {
		t.Fatalf("summarizer usage not folded into run accounting: got prompt=%d completion=%d, want 57/13",
			res.Usage.PromptTokens, res.Usage.CompletionTokens)
	}
}

// TestCompactionSummarizeInput_CapabilitiesBindToRunState pins the engine-side
// contract: the seam input's RecordUsage feeds the bound orchestration state
// and OverCeiling reflects its ceilings; an engine with no bound state (built
// directly, as tests do) hands out the zero-capability input.
func TestCompactionSummarizeInput_CapabilitiesBindToRunState(t *testing.T) {
	e := newMockEngine(t, &mockModel{})

	// Unbound: no metering, no ceiling information — pre-#1118 behavior.
	if in := e.compactionSummarizeInput(nil); in.RecordUsage != nil || in.OverCeiling != nil {
		t.Fatal("unbound engine must hand out a zero-capability CompactionSummarizeInput")
	}

	orch := newOrchestrationState(e.logSession, 50)
	orch.setCeilings(0.05, 0)
	e.bindRunUsage(orch)
	in := e.compactionSummarizeInput(nil)
	if in.RecordUsage == nil || in.OverCeiling == nil {
		t.Fatal("bound engine must wire both capabilities")
	}
	if in.OverCeiling() {
		t.Fatal("fresh run must not be over-ceiling")
	}
	in.RecordUsage(fantasy.Usage{InputTokens: 11, OutputTokens: 4}, fantasy.ProviderMetadata{})
	if got := usageSnapshot(orch); got.PromptTokens != 11 || got.CompletionTokens != 4 {
		t.Fatalf("RecordUsage did not feed the run state: got prompt=%d completion=%d", got.PromptTokens, got.CompletionTokens)
	}
	// Push the run past its cost ceiling; the probe must flip so a driver
	// summarizer degrades to truncation instead of buying another call.
	orch.mu.Lock()
	orch.CostUSD = 0.06
	orch.mu.Unlock()
	if !in.OverCeiling() {
		t.Fatal("OverCeiling must report true once the run's cost ceiling is met")
	}
}

// TestUpdateAuxUsage_DoesNotClobberLastStepSignals pins the aux/main split in
// the accounting seam: an auxiliary call accumulates into the cumulative
// ceiling/billing totals but must NOT overwrite LastStepInputTokens /
// LastStepPromptTokens — those are the MAIN loop's per-call input size, read
// by checkContextPressure and the chat context meter. Without the split, a
// Stop during a proactive compaction reported the summarizer's (small) prompt
// as the conversation's window fill.
func TestUpdateAuxUsage_DoesNotClobberLastStepSignals(t *testing.T) {
	session := NewLogSession()
	orch := newOrchestrationState(session, 0)

	// A main-loop step establishes the per-call input-size signals.
	orch.updateUsage("main-model", fantasy.Usage{InputTokens: 100, OutputTokens: 20}, fantasy.ProviderMetadata{})
	// An aux call (summarizer / metadata tool) accumulates totals only.
	orch.updateAuxUsage("aux-model", fantasy.Usage{InputTokens: 7, OutputTokens: 3}, fantasy.ProviderMetadata{})

	got := usageSnapshot(orch)
	if got.PromptTokens != 107 || got.CompletionTokens != 23 {
		t.Fatalf("aux usage not accumulated into totals: prompt=%d completion=%d, want 107/23",
			got.PromptTokens, got.CompletionTokens)
	}
	if got.LastStepInputTokens != 100 {
		t.Fatalf("LastStepInputTokens = %d, want 100 (aux call must not overwrite the main loop's per-call signal)",
			got.LastStepInputTokens)
	}
	if session.LastStepPromptTokens != 100 {
		t.Fatalf("logSession.LastStepPromptTokens = %d, want 100 (compaction trigger reads this)",
			session.LastStepPromptTokens)
	}
	if session.PromptTokens != 107 || session.CompletionTokens != 23 {
		t.Fatalf("logSession totals = %d/%d, want 107/23", session.PromptTokens, session.CompletionTokens)
	}
}

// generateOnlyModel is a fantasy.LanguageModel whose Generate returns a fixed
// text + usage — the shape of the metadata tools' one-shot call.
type generateOnlyModel struct {
	mockModel
	text  string
	usage fantasy.Usage
	slug  string
}

func (m *generateOnlyModel) Generate(_ context.Context, _ fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{
		Content:      fantasy.ResponseContent{fantasy.TextContent{Text: m.text}},
		FinishReason: fantasy.FinishReasonStop,
		Usage:        m.usage,
	}, nil
}

func (m *generateOnlyModel) Model() string { return m.slug }

// staticResolver satisfies tools.ModelResolver with a fixed model.
type staticResolver struct{ model fantasy.LanguageModel }

func (r staticResolver) Resolve(context.Context, string) (fantasy.LanguageModel, error) {
	return r.model, nil
}

// TestMetadataToolModelCallSpend_FlowsIntoRunUsage is the #1118 acceptance
// guard for the model-invocable git-metadata tools: a suggest_branch_name call
// made DURING a governed run makes its own Generate call, and that call's
// tokens must land in the run's Result.Usage (the same counters the ceilings
// read) via the context-carried tools.UsageRecorder Run installs. Before the
// fix this test failed with 100/20 — the metadata model's 20/7 vanished.
func TestMetadataToolModelCallSpend_FlowsIntoRunUsage(t *testing.T) {
	metadataModel := &generateOnlyModel{
		text:  "feat/add-oauth-login",
		usage: fantasy.Usage{InputTokens: 20, OutputTokens: 7},
		slug:  "fake/metadata",
	}

	// Round shape: step 1 calls suggest_branch_name, step 2 finishes with text.
	// Each main step reports 50/10.
	calls := 0
	model := &namedMockModel{name: "aux1118-metadata"}
	model.streamFunc = func(_ context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
		calls++
		step := calls
		return func(yield func(fantasy.StreamPart) bool) {
			if step == 1 {
				if !yield(fantasy.StreamPart{
					Type: fantasy.StreamPartTypeToolCall, ID: "mt-1",
					ToolCallName:  "suggest_branch_name",
					ToolCallInput: `{"context":"adds OAuth2 login for the web app"}`,
				}) {
					return
				}
				yield(fantasy.StreamPart{
					Type:         fantasy.StreamPartTypeFinish,
					FinishReason: fantasy.FinishReasonToolCalls,
					Usage:        fantasy.Usage{InputTokens: 50, OutputTokens: 10},
				})
				return
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, Delta: "done"}) {
				return
			}
			yield(fantasy.StreamPart{
				Type:         fantasy.StreamPartTypeFinish,
				FinishReason: fantasy.FinishReasonStop,
				Usage:        fantasy.Usage{InputTokens: 50, OutputTokens: 10},
			})
		}, nil
	}

	res, err := Run(context.Background(), ModeInteractive, RunConfig{
		EnvPrefix:   CanonicalEnvPrefix,
		NativeTools: []fantasy.AgentTool{tools.NewSuggestBranchNameTool(staticResolver{model: metadataModel}, "fake/metadata")},
	}, Deps{
		Input:    stubInput{system: "sys", user: "name my branch", label: "t"},
		Observer: &captureObserver{},
		Policy:   NewInteractivePolicy(0, 0, nil, nil),
		Executor: &stubExecutor{},
		Model:    model,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(res.FinalText, "done") {
		t.Fatalf("unexpected final text %q", res.FinalText)
	}
	// Two main steps (2×50/10) + the metadata model's call (20/7).
	if res.Usage.PromptTokens != 120 || res.Usage.CompletionTokens != 27 {
		t.Fatalf("metadata-tool spend not folded into run accounting: got prompt=%d completion=%d, want 120/27",
			res.Usage.PromptTokens, res.Usage.CompletionTokens)
	}
}
