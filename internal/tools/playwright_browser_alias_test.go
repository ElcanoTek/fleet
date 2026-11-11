package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/playwrightrunner"
)

// TestPlaywrightBrowserAcceptsItsAliases: every alias reaches its action (and
// that action's own validation) instead of "unsupported playwright_browser
// action". No browser is started: each call fails on its arguments first.
func TestPlaywrightBrowserAcceptsItsAliases(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := map[string]string{
		"probe": "URL must use https://", "navigate": "URL must use https://", "browse": "URL must use https://", "RUN": "URL must use https://",
		"copilot": "only available in interactive user conversations", "live": "only available in interactive user conversations", "open_session": "only available in interactive user conversations",
		"step": "browser session not found", "run_steps": "browser session not found", "act": "browser session not found",
		"close": "browser session not found", "end": "browser session not found",
		"": "URL must use https://", "snapshot": "browser session not found",
	}
	for action, want := range cases {
		_, err := RunPlaywrightBrowserForApproval(ctx, PlaywrightBrowserContext{}, PlaywrightBrowserParams{
			Action: action, URL: "http://a.example.com/", SessionID: "no-such-session",
			Steps: []playwrightrunner.Step{{Action: "click", Selector: "#go"}},
		})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("action %q: err = %v, want it to reach the action (%q)", action, err, want)
		}
	}
	if _, err := RunPlaywrightBrowserForApproval(ctx, PlaywrightBrowserContext{}, PlaywrightBrowserParams{Action: "teleport"}); err == nil || !strings.Contains(err.Error(), "unsupported playwright_browser action") {
		t.Errorf("an unknown action: err = %v, want unsupported", err)
	}
}
