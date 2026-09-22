package clientconfig

import "testing"

// TestAgentPolicyCriticalToolAliases verifies the optional alias classes
// (#1604) parse from agent_policy.critical_tool_aliases and are carried
// through Bundle.AgentPolicy() as a defensive copy; a manifest without the key
// yields none.
func TestAgentPolicyCriticalToolAliases(t *testing.T) {
	dir := writeManifest(t, `
agent_policy:
  critical_tools:
    - update_page_data
    - update_page_data_upload
  critical_tool_aliases:
    update_page_data: [update_page_data_upload]
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := b.AgentPolicy()
	if got := p.CriticalToolAliases["update_page_data"]; len(got) != 1 || got[0] != "update_page_data_upload" {
		t.Fatalf("CriticalToolAliases = %v, want update_page_data: [update_page_data_upload]", p.CriticalToolAliases)
	}
	p.CriticalToolAliases["update_page_data"][0] = "mutated"
	if got := b.AgentPolicy().CriticalToolAliases["update_page_data"][0]; got != "update_page_data_upload" {
		t.Errorf("AgentPolicy() must return a copy; the bundle now says %q", got)
	}

	plain := writeManifest(t, `
agent_policy:
  critical_tools:
    - update_page_data
`)
	b, err = Load(plain)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := b.AgentPolicy().CriticalToolAliases; got != nil {
		t.Errorf("CriticalToolAliases = %v, want nil for a manifest without the key", got)
	}
}
