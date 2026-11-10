package playwrightrunner

import (
	"context"
	"strings"
	"testing"
)

// TestRunRefusesARedactedArgumentInAStep: a saved connector step's {arg} is
// substituted before the step is typed, so the placeholder check runs on the
// step as substituted. The run stops before any browser starts.
func TestRunRefusesARedactedArgumentInAStep(t *testing.T) {
	_, err := Run(context.Background(), RunRequest{
		URL:   "https://a.example.com/login",
		Steps: []Step{{Action: "fill", Selector: "#pw", Value: "{password}"}},
		Args:  map[string]any{"password": "[REDACTED]"},
	})
	if err == nil || !strings.Contains(err.Error(), `contains "[REDACTED]"`) {
		t.Errorf("Run err = %v, want the substituted placeholder refused", err)
	}
}
