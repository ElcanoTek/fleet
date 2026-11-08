package agent

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// The written fleet-session.json is the redacted copy of the run's session.
// It must keep every field — the correlation ids above all (#2052) — and only
// scrub the text, so a field added later cannot be dropped silently.
func TestRedactedSessionKeepsCorrelationIDs(t *testing.T) {
	secret := "sk-ant-" + strings.Repeat("C", 32)
	s := agentcore.NewLogSession()
	s.Title = "run " + secret
	s.SetCorrelation("0af7651916cd43dd8448eb211c80319c", "run-7f3c", "turn-42")
	s.OutputJSON = `{"note":"` + secret + `"}`
	s.RunOutcome = "blocked"
	s.RunOutcomeDetail = "blocked by " + secret

	data, err := marshalLogSession(redactLogSession(s))
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	for _, want := range []string{`"trace_id": "0af7651916cd43dd8448eb211c80319c"`, `"run_id": "run-7f3c"`, `"turn_id": "turn-42"`, `"run_outcome": "blocked"`} {
		if !strings.Contains(out, want) {
			t.Errorf("redacted session lost %s:\n%s", want, out)
		}
	}
	if strings.Contains(out, secret) {
		t.Errorf("redacted session leaked the secret:\n%s", out)
	}
}
