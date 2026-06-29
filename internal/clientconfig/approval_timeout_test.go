package clientconfig

import "testing"

// TestAgentPolicyCriticalToolTimeouts verifies the optional per-tool approval
// timeout map (#225) parses from agent_policy.critical_tool_timeouts and is
// carried through Bundle.AgentPolicy(), while the long-standing critical_tools
// string list keeps working unchanged.
func TestAgentPolicyCriticalToolTimeouts(t *testing.T) {
	dir := writeManifest(t, `
agent_policy:
  critical_tools:
    - create_deal
  critical_tool_timeouts:
    send_email: 600
    bash: 60
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := b.AgentPolicy()
	// The existing string list still parses.
	if len(p.CriticalToolSuffixes) == 0 || p.CriticalToolSuffixes[0] != "create_deal" {
		t.Errorf("CriticalToolSuffixes = %v, want [create_deal]", p.CriticalToolSuffixes)
	}
	// The new per-tool timeout map is carried.
	if got := p.CriticalToolTimeouts["send_email"]; got != 600 {
		t.Errorf("CriticalToolTimeouts[send_email] = %d, want 600", got)
	}
	if got := p.CriticalToolTimeouts["bash"]; got != 60 {
		t.Errorf("CriticalToolTimeouts[bash] = %d, want 60", got)
	}
}

// TestAgentPolicyWithoutTimeouts is the backward-compatibility guard: a manifest
// that uses critical_tools as a plain string list and declares no
// critical_tool_timeouts still loads, with a nil/empty timeout map (#225).
func TestAgentPolicyWithoutTimeouts(t *testing.T) {
	dir := writeManifest(t, `
agent_policy:
  critical_tools:
    - send_email
    - create_deal
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := b.AgentPolicy()
	if len(p.CriticalToolTimeouts) != 0 {
		t.Errorf("CriticalToolTimeouts = %v, want empty", p.CriticalToolTimeouts)
	}
	if len(p.CriticalToolSuffixes) != 2 {
		t.Errorf("CriticalToolSuffixes = %v, want 2 entries", p.CriticalToolSuffixes)
	}
}
