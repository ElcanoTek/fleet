package clientconfig

import "testing"

// TestAgentPolicyEmailLastLists verifies agent_policy.email_last_tools and
// settleable_create_tools parse and are carried through Bundle.AgentPolicy()
// as defensive copies (Codex P1 on #1710: they were parsed but never copied,
// so every consumer saw nil); a manifest without the keys yields none.
func TestAgentPolicyEmailLastLists(t *testing.T) {
	dir := writeManifest(t, `
agent_policy:
  critical_tools: [execute_deal_from_prompt_inputs, update_deal]
  email_last_tools: [update_deal]
  settleable_create_tools: [execute_deal_from_prompt_inputs]
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := b.AgentPolicy()
	if len(p.EmailLastTools) != 1 || p.EmailLastTools[0] != "update_deal" {
		t.Fatalf("EmailLastTools = %v, want [update_deal]", p.EmailLastTools)
	}
	if len(p.SettleableCreateTools) != 1 || p.SettleableCreateTools[0] != "execute_deal_from_prompt_inputs" {
		t.Fatalf("SettleableCreateTools = %v, want [execute_deal_from_prompt_inputs]", p.SettleableCreateTools)
	}
	p.EmailLastTools[0], p.SettleableCreateTools[0] = "mutated", "mutated"
	again := b.AgentPolicy()
	if again.EmailLastTools[0] != "update_deal" || again.SettleableCreateTools[0] != "execute_deal_from_prompt_inputs" {
		t.Errorf("AgentPolicy() must return copies; the bundle now says %v / %v", again.EmailLastTools, again.SettleableCreateTools)
	}

	plain := writeManifest(t, `
agent_policy:
  critical_tools: [update_deal]
`)
	if b, err = Load(plain); err != nil {
		t.Fatalf("load: %v", err)
	}
	if p := b.AgentPolicy(); len(p.EmailLastTools) != 0 || len(p.SettleableCreateTools) != 0 {
		t.Errorf("want no email-last lists for a manifest without the keys, got %v / %v", p.EmailLastTools, p.SettleableCreateTools)
	}
}
