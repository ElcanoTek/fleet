package scheduledrun

import (
	"context"
	"errors"
	"testing"

	"charm.land/fantasy"
)

// A configured FLEET_VERIFIER_MODEL enables verification; a slug that does not
// resolve must fall back to the run's own model, never to nil — nil would turn
// the verifier off on a run with no fallback model.
func TestVerifierModelFor(t *testing.T) {
	runModel := yesVerifierModel{}
	dedicated := &yesVerifierModel{}
	ok := func(context.Context, string) (fantasy.LanguageModel, error) { return dedicated, nil }
	fail := func(context.Context, string) (fantasy.LanguageModel, error) { return nil, errors.New("unknown slug") }
	ctx := context.Background()

	if got := verifierModelFor(ctx, "", "t1", runModel, ok); got != nil {
		t.Fatalf("unset slug = %v, want nil (the agent then verifies with the run's model)", got)
	}
	if got := verifierModelFor(ctx, "  ", "t1", runModel, ok); got != nil {
		t.Fatalf("blank slug = %v, want nil", got)
	}
	if got := verifierModelFor(ctx, "acme/judge", "t1", runModel, ok); got != fantasy.LanguageModel(dedicated) {
		t.Fatalf("resolved slug = %v, want the dedicated model", got)
	}
	if got := verifierModelFor(ctx, "acme/missing", "t1", runModel, fail); got != fantasy.LanguageModel(runModel) {
		t.Fatalf("unresolved slug = %v, want the run's own model", got)
	}
}
