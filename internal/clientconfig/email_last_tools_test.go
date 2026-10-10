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

// agent_policy.critical_tool_no_session_approval parses and is carried
// through Bundle.AgentPolicy() as a defensive copy; absent means none.
func TestAgentPolicyNoSessionApproval(t *testing.T) {
	dir := writeManifest(t, `
agent_policy:
  critical_tools: [create_deal, update_deal]
  critical_tool_no_session_approval: [create_deal]
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := b.AgentPolicy()
	if len(p.CriticalToolNoSessionApproval) != 1 || p.CriticalToolNoSessionApproval[0] != "create_deal" {
		t.Fatalf("CriticalToolNoSessionApproval = %v, want [create_deal]", p.CriticalToolNoSessionApproval)
	}
	p.CriticalToolNoSessionApproval[0] = "mutated"
	if again := b.AgentPolicy(); again.CriticalToolNoSessionApproval[0] != "create_deal" {
		t.Errorf("AgentPolicy() must return a copy; the bundle now says %v", again.CriticalToolNoSessionApproval)
	}
	plain := writeManifest(t, `
agent_policy:
  critical_tools: [update_deal]
`)
	if b, err = Load(plain); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := b.AgentPolicy().CriticalToolNoSessionApproval; len(got) != 0 {
		t.Errorf("want none for a manifest without the key, got %v", got)
	}
}

// agent_policy.critical_tool_card_describers parses and is copied.
func TestAgentPolicyCardDescribers(t *testing.T) {
	dir := writeManifest(t, `
agent_policy:
  critical_tools: [update_deal]
  parallel_safe_tools: [mcp_deals_describe_deal_update]
  critical_tool_card_describers:
    update_deal: describe_deal_update
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := b.AgentPolicy()
	if p.CriticalToolCardDescribers["update_deal"] != "describe_deal_update" {
		t.Fatalf("CriticalToolCardDescribers = %v", p.CriticalToolCardDescribers)
	}
	p.CriticalToolCardDescribers["update_deal"] = "mutated"
	if b.AgentPolicy().CriticalToolCardDescribers["update_deal"] != "describe_deal_update" {
		t.Error("AgentPolicy() must return a copy")
	}
}

// agent_policy.critical_tool_group_approval parses and is copied.
func TestAgentPolicyGroupApproval(t *testing.T) {
	dir := writeManifest(t, `
agent_policy:
  critical_tools: [execute_plan]
  critical_tool_group_approval: [execute_plan]
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := b.AgentPolicy()
	if len(p.CriticalToolGroupApproval) != 1 || p.CriticalToolGroupApproval[0] != "execute_plan" {
		t.Fatalf("CriticalToolGroupApproval = %v, want [execute_plan]", p.CriticalToolGroupApproval)
	}
	p.CriticalToolGroupApproval[0] = "mutated"
	if again := b.AgentPolicy(); again.CriticalToolGroupApproval[0] != "execute_plan" {
		t.Errorf("AgentPolicy() must return a copy; the bundle now says %v", again.CriticalToolGroupApproval)
	}
}
