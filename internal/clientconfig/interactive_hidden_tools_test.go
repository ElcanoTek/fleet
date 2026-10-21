package clientconfig

import "testing"

// agent_policy.interactive_hidden_tools parses and is carried through
// Bundle.AgentPolicy() as a defensive copy; a manifest without it yields nil.
func TestAgentPolicyInteractiveHiddenTools(t *testing.T) {
	b, err := Load(writeManifest(t, `
agent_policy:
  interactive_hidden_tools: [ox_create_deal, ix_merge_deal_domains]
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := b.AgentPolicy()
	if len(p.InteractiveHiddenTools) != 2 || p.InteractiveHiddenTools[0] != "ox_create_deal" || p.InteractiveHiddenTools[1] != "ix_merge_deal_domains" {
		t.Fatalf("InteractiveHiddenTools = %v", p.InteractiveHiddenTools)
	}
	p.InteractiveHiddenTools[0] = "mutated"
	if again := b.AgentPolicy(); again.InteractiveHiddenTools[0] != "ox_create_deal" {
		t.Fatalf("AgentPolicy() must return a copy; the bundle now says %v", again.InteractiveHiddenTools)
	}
	if b, err = Load(writeManifest(t, "agent_policy:\n  critical_tools: [update_deal]\n")); err != nil {
		t.Fatalf("load: %v", err)
	}
	if p := b.AgentPolicy(); p.InteractiveHiddenTools != nil {
		t.Fatalf("absent key: InteractiveHiddenTools = %v, want nil", p.InteractiveHiddenTools)
	}
}
