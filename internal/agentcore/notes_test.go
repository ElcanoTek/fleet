package agentcore

import (
	"strings"
	"testing"
)

// fakeNoteProposer records the last proposal and returns a fixed id.
type fakeNoteProposer struct {
	slug, title, body, reason string
	calls                     int
	id                        string
	err                       error
}

func (f *fakeNoteProposer) Propose(slug, title, body, reason string) (string, error) {
	f.calls++
	f.slug, f.title, f.body, f.reason = slug, title, body, reason
	if f.err != nil {
		return "", f.err
	}
	return f.id, nil
}

// TestCheckNoteProposal_StagesPending verifies a well-formed propose_note call
// is intercepted (blocked=true), routed through the NoteProposer, and reports
// the proposal id with a "not live yet" notice. Mirrors checkMemoryProposal.
func TestCheckNoteProposal_StagesPending(t *testing.T) {
	o := newOrchestrationState(nil, 0)
	fp := &fakeNoteProposer{id: "prop-123"}
	o.setNoteProposer(fp)

	raw := `{"slug":"xandr-limits","title":"Xandr Limits","body":"# limits","reason":"learned a new cap"}`
	blocked, msg := o.checkNoteProposal("propose_note", raw)
	if !blocked {
		t.Fatal("expected propose_note to be intercepted (blocked=true)")
	}
	if fp.calls != 1 {
		t.Fatalf("expected proposer called once, got %d", fp.calls)
	}
	if fp.slug != "xandr-limits" || fp.title != "Xandr Limits" || fp.body != "# limits" || fp.reason != "learned a new cap" {
		t.Fatalf("proposer received wrong args: %+v", fp)
	}
	if !strings.Contains(msg, "prop-123") || !strings.Contains(msg, "NOTE_PROPOSED") {
		t.Fatalf("unexpected result message: %q", msg)
	}
	if !strings.Contains(msg, "NOT live") {
		t.Errorf("result should tell the agent the change is not live yet: %q", msg)
	}
}

// TestCheckNoteProposal_NotProposeNotePassesThrough verifies a non-propose_note
// tool name is NOT intercepted.
func TestCheckNoteProposal_NotProposeNotePassesThrough(t *testing.T) {
	o := newOrchestrationState(nil, 0)
	o.setNoteProposer(&fakeNoteProposer{id: "x"})
	if blocked, _ := o.checkNoteProposal("bash", `{}`); blocked {
		t.Fatal("non-propose_note tool must not be intercepted")
	}
}

// TestCheckNoteProposal_NilProposerGuard verifies the nil-proposer path returns
// an HONEST capability message (not the old "This is a bug") instead of panicking.
func TestCheckNoteProposal_NilProposerGuard(t *testing.T) {
	o := newOrchestrationState(nil, 0)
	blocked, msg := o.checkNoteProposal("propose_note", `{"slug":"s","title":"t","body":"b"}`)
	if !blocked {
		t.Fatal("expected blocked=true even with nil proposer")
	}
	if strings.Contains(msg, "This is a bug") {
		t.Fatalf("nil-proposer message must be honest capability text, not %q", msg)
	}
	if !strings.Contains(msg, "UNAVAILABLE") || !strings.Contains(msg, "Do NOT retry") {
		t.Fatalf("expected an honest UNAVAILABLE / do-not-retry message, got %q", msg)
	}
}

// TestCheckMemoryProposal_NilProposerGuard mirrors the note guard: nil memory
// proposer → blocked with honest UNAVAILABLE text, no panic, no "This is a bug".
func TestCheckMemoryProposal_NilProposerGuard(t *testing.T) {
	o := newOrchestrationState(nil, 0)
	blocked, msg := o.checkMemoryProposal("propose_memory", `{"content":"x"}`)
	if !blocked {
		t.Fatal("expected blocked=true even with nil proposer")
	}
	if strings.Contains(msg, "This is a bug") || !strings.Contains(msg, "UNAVAILABLE") {
		t.Fatalf("expected an honest UNAVAILABLE message, got %q", msg)
	}
}

// TestCheckNoteProposal_InvalidArgs verifies malformed JSON is reported, not
// forwarded to the proposer.
func TestCheckNoteProposal_InvalidArgs(t *testing.T) {
	o := newOrchestrationState(nil, 0)
	fp := &fakeNoteProposer{id: "x"}
	o.setNoteProposer(fp)
	blocked, msg := o.checkNoteProposal("propose_note", `{not json`)
	if !blocked {
		t.Fatal("expected blocked=true")
	}
	if fp.calls != 0 {
		t.Fatalf("proposer should not be called on invalid args, got %d calls", fp.calls)
	}
	if !strings.Contains(msg, "invalid arguments") {
		t.Fatalf("expected invalid-arguments message, got %q", msg)
	}
}

// TestScheduledPolicyInterceptsProposeNote verifies the propose_note interceptor
// is wired into the scheduled Policy's BeforeToolCall chain (BOTH modes), via
// SetNoteProposer.
func TestScheduledPolicyInterceptsProposeNote(t *testing.T) {
	p := NewScheduledPolicy(NewLogSession(), 50, 0, 0)
	fp := &fakeNoteProposer{id: "sp-1"}
	p.SetNoteProposer(fp)
	blocked, msg := p.BeforeToolCall("propose_note", "call-1", `{"slug":"s","title":"t","body":"b"}`)
	if !blocked {
		t.Fatal("scheduled policy must intercept propose_note")
	}
	if fp.calls != 1 {
		t.Fatalf("expected proposer called once, got %d", fp.calls)
	}
	if !strings.Contains(msg, "sp-1") {
		t.Fatalf("expected proposal id in message, got %q", msg)
	}
}

// TestInteractivePolicyInterceptsProposeNote verifies the same for the
// interactive Policy bundle (propose_note is wired in both modes; propose_memory
// stays interactive-only and is unaffected).
func TestInteractivePolicyInterceptsProposeNote(t *testing.T) {
	p := NewInteractivePolicy(0, 0, nil, nil)
	fp := &fakeNoteProposer{id: "ip-1"}
	p.SetNoteProposer(fp)
	blocked, msg := p.BeforeToolCall("propose_note", "call-1", `{"slug":"s","title":"t","body":"b"}`)
	if !blocked {
		t.Fatal("interactive policy must intercept propose_note")
	}
	if fp.calls != 1 {
		t.Fatalf("expected proposer called once, got %d", fp.calls)
	}
	if !strings.Contains(msg, "ip-1") {
		t.Fatalf("expected proposal id in message, got %q", msg)
	}
}
