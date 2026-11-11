package tools

import (
	"context"
	"errors"
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
	for _, p := range []PlaywrightBrowserParams{
		{Action: "run", URL: "https://a.example.com/", PasswordRef: "$secret:password"},
		{Action: "run", URL: "https://a.example.com/$secret:password"},
		{Action: "run", URL: "https://a.example.com/", Steps: []playwrightrunner.Step{{Action: "goto", Value: "https://a.example.com/$secret:password"}}},
	} {
		_, err := RunPlaywrightBrowserForApproval(ctx, PlaywrightBrowserContext{}, p)
		if !errors.Is(err, ErrNoVaultForSecretReference) {
			t.Errorf("%+v: err = %v, want ErrNoVaultForSecretReference", p, err)
		}
	}
}
