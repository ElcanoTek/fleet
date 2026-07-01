package agent

import (
	"encoding/json"
	"strings"
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

// TestKeepRecentTranscript verifies truncation keeps the TAIL (recent turns),
// which is where the refined result lives — not the exploratory opening.
func TestKeepRecentTranscript(t *testing.T) {
	if got := keepRecentTranscript("short", 100); got != "short" {
		t.Errorf("under-limit input should pass through, got %q", got)
	}
	long := strings.Repeat("a", 50) + "FINAL_RESULT"
	got := keepRecentTranscript(long, 20)
	if !strings.HasSuffix(got, "FINAL_RESULT") {
		t.Errorf("truncation must keep the recent tail; got %q", got)
	}
	if !strings.Contains(got, "earlier turns omitted") {
		t.Errorf("truncation should mark dropped earlier turns; got %q", got)
	}
}
