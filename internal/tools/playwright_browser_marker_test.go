package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/playwrightrunner"
)

// TestPlaywrightBrowserRefusesRedactedAndUnresolvableArguments: a call that
// carries the redactor's placeholder anywhere is refused, and so is a $secret
// reference where there is no vault to resolve it (a scheduled run), which
// would otherwise be typed as written. No browser starts.
func TestPlaywrightBrowserRefusesRedactedAndUnresolvableArguments(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, p := range []PlaywrightBrowserParams{
		{Action: "run", URL: "https://a.example.com/", Steps: []playwrightrunner.Step{{Action: "fill", Selector: "#q", Value: "[REDACTED]"}}},
		{Action: "run", URL: "https://a.example.com/[REDACTED]"},
		{Action: "run", URL: "https://a.example.com/", PasswordRef: "[REDACTED]"},
	} {
		if _, err := RunPlaywrightBrowserForApproval(ctx, PlaywrightBrowserContext{}, p); !errors.Is(err, ErrRedactionMarkerArgument) {
			t.Errorf("%+v: err = %v, want ErrRedactionMarkerArgument", p, err)
		}
	}
	_, err := RunPlaywrightBrowserForApproval(ctx, PlaywrightBrowserContext{}, PlaywrightBrowserParams{Action: "run", URL: "https://a.example.com/", PasswordRef: "$secret:password"})
	if err == nil || !strings.Contains(err.Error(), "only be resolved in an interactive chat") {
		t.Errorf("a $secret reference with no vault: err = %v", err)
	}
}
