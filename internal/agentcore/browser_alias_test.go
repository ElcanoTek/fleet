package agentcore

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/tools"
)

// TestBrowserGateAndExecutorsAgreeOnActions ties the interactive approval
// gate (classifyRiskyBrowser) to the actions each browser tool actually runs
// (tools.PlaywrightBrowserActions / ConnectorStudioActions), in both
// directions and in any case:
//
//   - every action the tool runs browser steps for is staged when its steps
//     type or click, so no spelling of it runs a fill without a card;
//   - every action the gate stages is one the tool runs, so an approved card
//     does not fail as "unsupported action" after the person approved it.
func TestBrowserGateAndExecutorsAgreeOnActions(t *testing.T) {
	stepActions := map[string]map[string]bool{
		toolNamePlaywrightBrowser: {"run": true, "start_session": true, "steps": true},
		toolNameConnectorStudio:   {tools.ConnectorStudioActionProbeBrowser: true, tools.ConnectorStudioActionStartCopilotSession: true},
	}
	accepted := map[string]map[string]string{
		toolNamePlaywrightBrowser: tools.PlaywrightBrowserActions(),
		toolNameConnectorStudio:   tools.ConnectorStudioActions(),
	}
	staged := func(tool, action string) bool {
		ok, _ := classifyRiskyBrowser(tool, `{"action":"`+action+`","steps":[{"action":"fill","selector":"#q","value":"x"}]}`)
		return ok
	}
	for tool, actions := range accepted {
		stagedCount := 0
		for name, canonical := range actions {
			for _, spelled := range []string{name, " " + name + " ", strings.ToUpper(name)} {
				switch got := staged(tool, spelled); {
				case stepActions[tool][canonical] && !got:
					t.Errorf("%s: action %q runs browser steps (%s) but a fill under it is not staged for approval", tool, spelled, canonical)
				case got && !stepActions[tool][canonical]:
					t.Errorf("%s: the gate stages action %q, which does not run browser steps (%s)", tool, spelled, canonical)
				case got:
					stagedCount++
				}
			}
		}
		if stagedCount == 0 {
			t.Errorf("%s: the gate staged nothing; the test is not exercising it", tool)
		}
	}
	// The other direction for names the tools do not accept: the gate must
	// not stage what would fail after approval.
	for tool, actions := range accepted {
		for _, name := range []string{"teleport", "do", "go", "probe_http_browser"} {
			if _, known := actions[name]; !known && staged(tool, name) {
				t.Errorf("%s: the gate stages %q, which the tool does not accept", tool, name)
			}
		}
	}
}
