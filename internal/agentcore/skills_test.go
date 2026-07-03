package agentcore

import (
	"strings"
	"testing"
)

// fakeSkillProposer records the last proposal and returns a fixed id.
type fakeSkillProposer struct {
	name, description, body, reason string
	calls                           int
	id                              string
}

func (f *fakeSkillProposer) Propose(name, description, body, reason string) (string, error) {
	f.calls++
	f.name, f.description, f.body, f.reason = name, description, body, reason
	return f.id, nil
}

// A well-formed propose_skill call is intercepted, routed through the
// SkillProposer, and reports the proposal id with a "not active yet" notice —
// mirrors checkNoteProposal.
func TestCheckSkillProposal_StagesPending(t *testing.T) {
	o := newOrchestrationState(nil, 0)
	fp := &fakeSkillProposer{id: "skill-123"}
	o.setSkillProposer(fp)

	raw := `{"name":"weekly-pacing","description":"compile the pacing report","body":"1. Pull.","reason":"user asked twice"}`
	blocked, msg := o.checkSkillProposal("propose_skill", raw)
	if !blocked {
		t.Fatal("expected propose_skill to be intercepted")
	}
	if fp.calls != 1 || fp.name != "weekly-pacing" || fp.body != "1. Pull." {
		t.Fatalf("proposer received wrong args: %+v", fp)
	}
	if !strings.Contains(msg, "skill-123") || !strings.Contains(msg, "SKILL_PROPOSED") {
		t.Fatalf("unexpected result message: %q", msg)
	}
	if !strings.Contains(msg, "NOT active") {
		t.Errorf("result should tell the agent the skill is not active yet: %q", msg)
	}

	// Other tools pass through; a missing proposer answers unavailable.
	if blocked, _ := o.checkSkillProposal("bash", `{}`); blocked {
		t.Fatal("non-propose_skill tool must not be intercepted")
	}
	bare := newOrchestrationState(nil, 0)
	blocked, msg = bare.checkSkillProposal("propose_skill", raw)
	if !blocked || !strings.Contains(msg, "SKILL_PROPOSAL_UNAVAILABLE") {
		t.Fatalf("unwired proposer should answer unavailable: %v %q", blocked, msg)
	}
}
