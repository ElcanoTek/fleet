package httpapi

import (
	"context"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/store"
)

// TestApprovedConnectorStudioAliasesRun: an approved connector_studio call
// reaches the handler with the action as the model wrote it. An alias the
// approval gate staged ("browser_probe", "live_view", ...) must run that
// action, not fail as unsupported after the person approved it. No browser
// is started: the http:// url fails the action's own check first.
func TestApprovedConnectorStudioAliasesRun(t *testing.T) {
	t.Parallel()
	s := &Server{}
	for _, action := range []string{"browser_probe", "inspect_browser", "playwright_probe", "Probe_Browser",
		"start_session", "copilot_session", "live_view", "live_browser", "open_live_browser"} {
		args := `{"action":"` + action + `","url":"http://a.example.com/","steps":[{"action":"click","selector":"#go"}]}`
		_, err := s.runStagedConnectorStudio(context.Background(), &store.Approval{ToolName: "connector_studio", UserEmail: "alice@example.com", ArgsJSON: args})
		if err == nil || !strings.Contains(err.Error(), "URL must use https://") {
			t.Errorf("approved %q: err = %v, want the action's own URL check", action, err)
		}
	}
	if _, err := s.runStagedConnectorStudio(context.Background(), &store.Approval{ArgsJSON: `{"action":"teleport"}`}); err == nil || !strings.Contains(err.Error(), `unsupported action "teleport"`) {
		t.Errorf("an unknown action: err = %v, want unsupported", err)
	}
}
