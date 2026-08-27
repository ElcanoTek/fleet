package agentcore

// Budget wind-down tests (#990): once spend crosses the wind-down fraction of
// a configured ceiling, every provider call gets a request-local wrap-up
// notice, with a one-shot fleet.budget_winddown event on the crossing. The
// notice must never fire on unlimited (zero) ceilings and must never enter
// the caller's message slice.

import (
	"context"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestCheckBudgetWindDown(t *testing.T) {
	orch := newOrchestrationState(nil, 0)
	// Unlimited ceilings never wind down.
	orch.CostUSD = 1e6
	if st := orch.checkBudgetWindDown(0.8); st.active {
		t.Fatal("zero ceilings must never wind down")
	}

	orch.setCeilings(10.0, 0)
	orch.CostUSD = 7.9
	if st := orch.checkBudgetWindDown(0.8); st.active {
		t.Fatalf("spend below fraction must not wind down (spent=%v)", orch.CostUSD)
	}
	orch.CostUSD = 8.0
	st := orch.checkBudgetWindDown(0.8)
	if !st.active || !st.first {
		t.Fatalf("crossing must be active+first, got %+v", st)
	}
	if st2 := orch.checkBudgetWindDown(0.8); !st2.active || st2.first {
		t.Fatalf("second check must be active but not first, got %+v", st2)
	}
	if notice := st.windDownNotice(); !strings.Contains(notice, "BUDGET WIND-DOWN") ||
		!strings.Contains(notice, "$8.00 of the $10.00 cost ceiling") {
		t.Fatalf("notice missing spend detail: %q", notice)
	}

	// Token ceiling path (uncached tokens: prompt - cached + completion).
	tok := newOrchestrationState(nil, 0)
	tok.setCeilings(0, 1000)
	tok.PromptTokens = 900
	tok.CachedTokens = 200
	tok.CompletionTokens = 100
	if st := tok.checkBudgetWindDown(0.8); !st.active || st.spentTokens != 800 {
		t.Fatalf("token wind-down expected at 800/1000, got %+v", st)
	}
}

func TestBudgetWindDownStepAppendsRequestLocalNotice(t *testing.T) {
	orch := newOrchestrationState(nil, 0)
	orch.setCeilings(10.0, 0)
	orch.CostUSD = 9.0

	step := budgetWindDownStep(EnvPrefix("FLEET"), orch, nil)
	in := []fantasy.Message{fantasy.NewUserMessage("do the work")}
	_, res, err := step(context.Background(), fantasy.PrepareStepFunctionOptions{Messages: in})
	if err != nil {
		t.Fatalf("step error: %v", err)
	}
	if len(res.Messages) != 2 || !strings.Contains(msgText(res.Messages[1]), "BUDGET WIND-DOWN") {
		t.Fatalf("notice not appended: %+v", res.Messages)
	}
	if len(in) != 1 {
		t.Fatalf("caller slice mutated: %d messages", len(in))
	}

	// Below the threshold the step leaves the request untouched.
	calm := newOrchestrationState(nil, 0)
	calm.setCeilings(10.0, 0)
	calm.CostUSD = 1.0
	step = budgetWindDownStep(EnvPrefix("FLEET"), calm, nil)
	_, res, err = step(context.Background(), fantasy.PrepareStepFunctionOptions{Messages: in})
	if err != nil {
		t.Fatalf("step error: %v", err)
	}
	if res.Messages != nil {
		t.Fatalf("below threshold must not modify messages: %+v", res.Messages)
	}
}
