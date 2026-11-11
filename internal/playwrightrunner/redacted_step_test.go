package playwrightrunner

import (
	"strings"
	"testing"
)

// TestValidateStepRefusesTheRedactionPlaceholder: a typed value copied from an
// earlier, redacted call would be typed as "[REDACTED]".
func TestValidateStepRefusesTheRedactionPlaceholder(t *testing.T) {
	for _, st := range []Step{
		{Action: "fill", Selector: "#pw", Value: "[REDACTED]"},
		{Action: "select", Selector: "#s", Value: "x [REDACTED] y"},
		{Action: "press", Selector: "#q", Value: "[REDACTED]"},
	} {
		if err := ValidateStep(st); err == nil || !strings.Contains(err.Error(), "{{PASSWORD}}") {
			t.Errorf("ValidateStep(%+v) = %v, want the placeholder refused", st, err)
		}
	}
	if err := ValidateStep(Step{Action: "fill", Selector: "#q", Value: "{{PASSWORD}}"}); err != nil {
		t.Errorf("a placeholder step was refused: %v", err)
	}
}
