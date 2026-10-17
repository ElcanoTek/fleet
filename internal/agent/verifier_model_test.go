package agent

import (
	"context"
	"errors"
	"testing"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openrouter"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// namedVerifier is a scripted verifier with its own model slug, so a test can
// tell which model judged.
type namedVerifier struct {
	scriptedVerifier
	name     string
	provider string
}

func (m *namedVerifier) Model() string { return m.name }

func (m *namedVerifier) Provider() string {
	if m.provider == "" {
		return "mock"
	}
	return m.provider
}

func TestEndOfRunVerifierModel_Selection(t *testing.T) {
	main := &namedVerifier{name: "main"}
	fallback := &namedVerifier{name: "fallback"}
	dedicated := &namedVerifier{name: "dedicated"}
	for _, tc := range []struct {
		name      string
		a         *Agent
		want      string
		wantRetry string
	}{
		{"main model judges, fallback retries", &Agent{model: main, fallbackModel: fallback}, "main", "fallback"},
		{"dedicated verifier wins", &Agent{model: main, fallbackModel: fallback, verifierModel: dedicated}, "dedicated", "fallback"},
		{"no fallback: retry on the judge", &Agent{model: main}, "main", "main"},
		{"fallback same route as judge: retry on the judge", &Agent{model: main, fallbackModel: &namedVerifier{name: "main"}}, "main", "main"},
		{"agent without a model falls back", &Agent{fallbackModel: fallback}, "fallback", "fallback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.endOfRunVerifierModel().Model(); got != tc.want {
				t.Errorf("verifier model = %q, want %q", got, tc.want)
			}
			if got := tc.a.endOfRunVerifierRetryModel().Model(); got != tc.wantRetry {
				t.Errorf("retry model = %q, want %q", got, tc.wantRetry)
			}
		})
	}
	if (&Agent{}).endOfRunVerifierModel() != nil {
		t.Error("an agent with no models must have no verifier")
	}
}

// Same-slug failover through another provider (fallback_providers) is a
// different route, so the verifier's retry takes it rather than re-asking the
// route that just failed.
func TestEndOfRunVerifierRetryModel_SameSlugOtherProvider(t *testing.T) {
	judge := &namedVerifier{name: "anthropic/claude-opus-5.5", provider: "openrouter"}
	sameSlugOtherProvider := &namedVerifier{name: "anthropic/claude-opus-5.5", provider: "bedrock-direct"}
	a := &Agent{model: judge, fallbackModel: sameSlugOtherProvider}
	if got := a.endOfRunVerifierRetryModel(); got != fantasy.LanguageModel(sameSlugOtherProvider) {
		t.Fatalf("retry model = %v (provider %s), want the other-provider fallback", got.Model(), got.Provider())
	}
}

// The verifier's one-off call carries the judging model's routing policy, like
// the summarizers, so the verdict is not served by an endpoint the run itself
// excludes.
func TestRunEndOfRunVerifier_CarriesRoutingPolicy(t *testing.T) {
	model := &optionsCapturingModel{itMockModel: itMockModel{generateText: `{"missing_actions": []}`}, slug: floorSlug}
	a := &Agent{model: model, fallbackModel: model, logSession: NewLogSession()}
	if _, err := a.runEndOfRunVerifier(context.Background(), "send the report", "sent", nil); err != nil {
		t.Fatalf("runEndOfRunVerifier: %v", err)
	}
	model.omu.Lock()
	defer model.omu.Unlock()
	if len(model.opts) != 1 {
		t.Fatalf("verifier made %d calls, want 1", len(model.opts))
	}
	or, ok := model.opts[0][openrouter.Name].(*openrouter.ProviderOptions)
	if !ok || or.Provider == nil {
		t.Fatalf("verifier call options = %#v, want an OpenRouter provider policy", model.opts[0])
	}
}

// Enablement is unchanged from when the fallback model judged: a run with
// neither a fallback nor a dedicated verifier is not verified, even though it
// has a model of its own.
func TestEndOfRunVerifierEnabled(t *testing.T) {
	main := &namedVerifier{name: "main"}
	for _, tc := range []struct {
		name string
		a    *Agent
		want bool
	}{
		{"fallback configured", &Agent{model: main, fallbackModel: &namedVerifier{name: "fallback"}}, true},
		{"dedicated verifier configured", &Agent{model: main, verifierModel: &namedVerifier{name: "dedicated"}}, true},
		{"neither: not verified", &Agent{model: main}, false},
	} {
		if got := tc.a.endOfRunVerifierEnabled(); got != tc.want {
			t.Errorf("%s: enabled = %t, want %t", tc.name, got, tc.want)
		}
	}
}

// The verifier judges with the run's own model, not the fallback, and its
// aux-usage record names the model that judged.
func TestRunEndOfRunVerifier_UsesMainModelAndRecordsIt(t *testing.T) {
	main := &namedVerifier{scriptedVerifier: scriptedVerifier{replies: []string{`{"missing_actions": []}`}}, name: "main"}
	fallback := &namedVerifier{scriptedVerifier: scriptedVerifier{replies: []string{"ERR"}}, name: "fallback"}
	a := &Agent{model: main, fallbackModel: fallback, logSession: NewLogSession()}
	missing, err := a.runEndOfRunVerifier(context.Background(), "send the report", "sent", nil)
	if err != nil || len(missing) != 0 {
		t.Fatalf("runEndOfRunVerifier = (%v, %v), want clean", missing, err)
	}
	if main.calls != 1 || fallback.calls != 0 {
		t.Fatalf("calls main=%d fallback=%d, want 1/0", main.calls, fallback.calls)
	}
	recs := a.logSession.SnapshotAuxUsage()
	if len(recs) != 1 || recs[0].Model != "main" {
		t.Fatalf("aux records = %+v, want one record for model main", recs)
	}
}

// In a scheduled run, a verifier call with no verdict on the judging model is
// retried once on the fallback model, which then decides.
func TestScheduledVerifier_RetriesOnFallbackModel(t *testing.T) {
	withFastVerifierRetry(t)
	judge := &namedVerifier{scriptedVerifier: scriptedVerifier{replies: []string{"ERR"}}, name: "judge"}
	fallback := &namedVerifier{scriptedVerifier: scriptedVerifier{replies: []string{`{"missing_actions":["send the report"]}`}}, name: "fallback"}
	scriptedRunFallback = fallback
	t.Cleanup(func() { scriptedRunFallback = nil })
	_, _, _, err := scriptedRun(t, []struct{ tool, input string }{
		{"confirm_audit", publishAudit},
		{"mcp_pages_update_page_data", `{"slug":"x","data":{}}`},
	}, judge, nil, &pagesBroker{calls: map[string]int{}}, nil)
	if judge.calls == 0 || fallback.calls == 0 {
		t.Fatalf("calls judge=%d fallback=%d: the judge must be asked first and the fallback must retry", judge.calls, fallback.calls)
	}
	// The fallback's verdict decided: it reported a missing action, so the run
	// cannot finish as verified (a fail-open would need two outages).
	if !errors.Is(err, agentcore.ErrCompletionUnverified) {
		t.Fatalf("run error = %v, want ErrCompletionUnverified from the fallback's verdict", err)
	}
}

var _ fantasy.LanguageModel = (*namedVerifier)(nil)
