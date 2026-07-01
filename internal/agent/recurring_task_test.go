package agent

import (
	"encoding/json"
	"testing"

	"github.com/ElcanoTek/fleet/internal/structuredoutput"
)

// TestRecurringTaskSchema locks the #455 synthesizer's output contract: the
// schema compiles, a conforming object validates, and a non-conforming one is
// rejected.
func TestRecurringTaskSchema(t *testing.T) {
	raw := json.RawMessage(recurringTaskSchema)
	if err := structuredoutput.ValidateSchema(raw); err != nil {
		t.Fatalf("recurringTaskSchema does not compile: %v", err)
	}

	ok := `{"name":"Daily failed-task report","prompt":"Report scheduled tasks that failed in the last 24h.","cron":"0 9 * * *","rationale":"a daily ops check"}`
	if _, err := structuredoutput.ValidateOutput(ok, raw); err != nil {
		t.Errorf("conforming proposal should validate: %v", err)
	}

	// Missing the required prompt.
	bad := `{"name":"x","cron":"0 9 * * *","rationale":"y"}`
	if _, err := structuredoutput.ValidateOutput(bad, raw); err == nil {
		t.Error("a proposal missing `prompt` must be rejected")
	}
}
