package agentcore

import (
	"errors"
	"testing"
)

type stubMemoryProposer struct{ err error }

func (s stubMemoryProposer) Propose(_, _ string) (string, error) { return "mem-1", s.err }

type stubNoteProposer struct{ err error }

func (s stubNoteProposer) Propose(_, _, _, _ string) (string, error) { return "note-1", s.err }

type failingStageSink struct{}

func (failingStageSink) Stage(_, _, _ string) (string, error) { return "", errors.New("down") }
func (failingStageSink) StageSuggestion(_ string) (string, string, error) {
	return "", "", errors.New("down")
}

// Each staging gate's success text is recognized by IsStagedResult, and its
// failure texts are not: the chat escalation signals read the first as a card
// shown to the user and the second as a failed call. Driving the real gates
// keeps the prefixes and the texts from drifting apart.
func TestIsStagedResult_MatchesTheGatesOwnText(t *testing.T) {
	t.Parallel()

	staged := newOrchestrationState(nil)
	staged.setMemoryProposer(stubMemoryProposer{})
	staged.setNoteProposer(stubNoteProposer{})
	staged.setSkillProposer(&fakeSkillProposer{id: "skill-1"})
	staged.setApprovalSink(&stubApprovalSink{returnID: "preview-1"})

	failed := newOrchestrationState(nil)
	failed.setMemoryProposer(stubMemoryProposer{err: errors.New("down")})
	failed.setNoteProposer(stubNoteProposer{err: errors.New("down")})
	failed.setApprovalSink(failingStageSink{})

	unwired := newOrchestrationState(nil)

	type gate func(o *orchestrationState) (bool, string)
	memory := func(o *orchestrationState) (bool, string) {
		return o.checkMemoryProposal("propose_memory", `{"content":"x","kind":"project"}`)
	}
	note := func(o *orchestrationState) (bool, string) {
		return o.checkNoteProposal("propose_note", `{"slug":"s","title":"t","body":"b","reason":"r"}`)
	}
	skill := func(o *orchestrationState) (bool, string) {
		return o.checkSkillProposal("propose_skill", `{"name":"n","description":"d","body":"b","reason":"r"}`)
	}
	preview := func(o *orchestrationState) (bool, string) {
		return o.checkPreviewEmailSafety(toolNamePreviewEmail, "call-1", `{"to":["a@example.com"],"subject":"s","body":"b"}`)
	}
	badMemory := func(o *orchestrationState) (bool, string) {
		return o.checkMemoryProposal("propose_memory", `{`)
	}

	for _, tc := range []struct {
		name   string
		o      *orchestrationState
		g      gate
		staged bool
	}{
		{"memory proposed", staged, memory, true},
		{"note proposed", staged, note, true},
		{"skill proposed", staged, skill, true},
		{"preview displayed", staged, preview, true},
		{"memory stage failed", failed, memory, false},
		{"memory invalid arguments", staged, badMemory, false},
		{"note stage failed", failed, note, false},
		{"preview failed", failed, preview, false},
		{"memory unavailable", unwired, memory, false},
		{"skill unavailable", unwired, skill, false},
		{"preview unavailable", unwired, preview, false},
	} {
		blocked, msg := tc.g(tc.o)
		if !blocked {
			t.Fatalf("%s: gate did not answer the call", tc.name)
		}
		if got := IsStagedResult(msg); got != tc.staged {
			t.Errorf("%s: IsStagedResult(%q) = %v, want %v", tc.name, msg, got, tc.staged)
		}
	}
}
