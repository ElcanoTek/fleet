package agentcore

import (
	"strings"
	"testing"
)

// TestScheduledPolicy_ProposeMemoryHonestBackstop locks in the #285 correctness
// fix: a scheduled run never wires a memoryProposer (user memories are
// interactive-only — scheduled tasks use the remember/recall task-memory tools
// instead), so a stray propose_memory call must be BLOCKED with an honest
// MEMORY_PROPOSAL_UNAVAILABLE message rather than falling through to the no-op
// tool body's misleading "Memory proposal created" success string.
func TestScheduledPolicy_ProposeMemoryHonestBackstop(t *testing.T) {
	p := NewScheduledPolicy(NewLogSession(), 50, 0, 0)
	blocked, msg := p.BeforeToolCall("propose_memory", "c1", `{"content":"remember this"}`)
	if !blocked {
		t.Fatal("scheduled propose_memory must be blocked (no human to confirm in unattended mode)")
	}
	if strings.Contains(msg, "Memory proposal created") {
		t.Fatalf("scheduled propose_memory must NOT report fake success, got %q", msg)
	}
	if !strings.Contains(msg, "MEMORY_PROPOSAL_UNAVAILABLE") {
		t.Fatalf("expected an honest UNAVAILABLE message, got %q", msg)
	}
}
