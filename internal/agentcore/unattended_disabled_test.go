package agentcore

import (
	"strings"
	"testing"
)

// unattended_disabled_tools accepts only the built-ins it is meant to remove;
// a typo or a governance tool is reported, never silently installed.
func TestUnattendedDisabledTools(t *testing.T) {
	t.Cleanup(func() { ConfigureAgentPolicy(AgentPolicy{}) })
	p := AgentPolicy{UnattendedDisabledTools: []string{" create_task ", "web_fetch", "confirm_audit", "web_fetchh", ""}}
	problems := UnattendedDisabledToolProblems(p)
	if len(problems) != 3 {
		t.Fatalf("problems = %q, want 3 (confirm_audit, the typo, the blank)", problems)
	}
	joined := strings.Join(problems, "; ")
	for _, want := range []string{`"confirm_audit"`, `"web_fetchh"`, "blank"} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems %q do not mention %s", joined, want)
		}
	}
	ConfigureAgentPolicy(p)
	for name, want := range map[string]bool{"create_task": true, "web_fetch": true, "confirm_audit": false, "download_url": false} {
		if got := UnattendedToolDisabled(name); got != want {
			t.Errorf("UnattendedToolDisabled(%q) = %v, want %v", name, got, want)
		}
	}
	ConfigureAgentPolicy(AgentPolicy{})
	if UnattendedToolDisabled("create_task") {
		t.Error("an empty policy must disable nothing")
	}
}
