package scheduledrun

import (
	"context"
	"errors"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// yesVerifierModel is a fantasy.LanguageModel whose Generate always answers
// YES with a fixed usage — the llm exit-condition verifier's one-shot shape.
type yesVerifierModel struct{}

func (yesVerifierModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return &fantasy.Response{
		Content:      fantasy.ResponseContent{fantasy.TextContent{Text: "YES"}},
		FinishReason: fantasy.FinishReasonStop,
		Usage:        fantasy.Usage{InputTokens: 9, OutputTokens: 1},
	}, nil
}

func (yesVerifierModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	return nil, errors.New("stream not supported")
}

func (yesVerifierModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, errors.New("object generation not supported")
}

func (yesVerifierModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, errors.New("object streaming not supported")
}

func (yesVerifierModel) Provider() string { return "fake" }
func (yesVerifierModel) Model() string    { return "fake/loop-verifier" }

// TestEvalLLMExit_RecordsAuxUsage pins the #1118 visibility half of the loop
// verifier's documented accounting: its spend stays OFF the iteration cost /
// across-iteration MaxCostUSD ceiling (the #179 model), but the call is
// recorded in the worker session's labeled aux-usage ledger so it no longer
// vanishes.
func TestEvalLLMExit_RecordsAuxUsage(t *testing.T) {
	r := &Runner{}
	lc := &models.LoopConfig{ExitCondition: "llm", VerifierPrompt: "Did the worker finish?"}
	session := &models.LogSession{Cost: 1.25, PromptTokens: 1000}

	passed, result := r.evalLLMExit(context.Background(), lc, session, yesVerifierModel{})
	if !passed || result != "llm:YES" {
		t.Fatalf("evalLLMExit = (%v, %q), want (true, llm:YES)", passed, result)
	}
	if len(session.AuxUsage) != 1 {
		t.Fatalf("aux usage records = %d, want 1", len(session.AuxUsage))
	}
	rec := session.AuxUsage[0]
	if rec.Label != agentcore.AuxUsageLoopExitVerifier {
		t.Errorf("label = %q, want %q", rec.Label, agentcore.AuxUsageLoopExitVerifier)
	}
	if rec.Model != "fake/loop-verifier" || rec.PromptTokens != 9 || rec.CompletionTokens != 1 {
		t.Errorf("record = %+v, want model=fake/loop-verifier prompt=9 completion=1", rec)
	}
	// The iteration cost the loop's MaxCostUSD ceiling accumulates is the
	// session's headline Cost — the verifier record must not have moved it.
	if session.Cost != 1.25 || session.PromptTokens != 1000 {
		t.Errorf("verifier spend leaked into the headline session totals: cost=%f prompt=%d",
			session.Cost, session.PromptTokens)
	}
}

// TestConvertLogSession_CarriesAuxUsage pins the persistence seam between the
// run's in-memory ledger and the runner's submitted session (#1118): the
// verifier/reviewer append to agentcore.LogSession.AuxUsage during the run,
// and convertLogSession must carry those records into models.LogSession or
// they never reach the store.
func TestConvertLogSession_CarriesAuxUsage(t *testing.T) {
	ls := agent.NewLogSession()
	ls.AddAuxUsage(agentcore.AuxUsageRecord{
		Label: agentcore.AuxUsageEndOfRunVerifier, Model: "mock-model",
		PromptTokens: 12, CompletionTokens: 4, CostUSD: 0.002,
	})
	ls.AddAuxUsage(agentcore.AuxUsageRecord{
		Label: agentcore.AuxUsagePhoneAFriend, Model: "reviewer-model",
		PromptTokens: 30, CompletionTokens: 9, CostUSD: 0.01,
	})

	out := convertLogSession(nil, ls)
	if out == nil {
		t.Fatal("convertLogSession returned nil")
	}
	if len(out.AuxUsage) != 2 {
		t.Fatalf("converted aux usage records = %d, want 2", len(out.AuxUsage))
	}
	want := []models.AuxUsageRecord{
		{Label: agentcore.AuxUsageEndOfRunVerifier, Model: "mock-model", PromptTokens: 12, CompletionTokens: 4, CostUSD: 0.002},
		{Label: agentcore.AuxUsagePhoneAFriend, Model: "reviewer-model", PromptTokens: 30, CompletionTokens: 9, CostUSD: 0.01},
	}
	for i, w := range want {
		if out.AuxUsage[i] != w {
			t.Errorf("record %d = %+v, want %+v", i, out.AuxUsage[i], w)
		}
	}
}
