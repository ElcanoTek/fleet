package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/structuredoutput"
)

// TestPatrolDraftSchema pins the output contract: all three fields required,
// nothing else allowed.
func TestPatrolDraftSchema(t *testing.T) {
	valid := `{"title":"Morning sweep","prompt":"**Mission**","summary":"Checks deals."}`
	if _, err := structuredoutput.ValidateOutput(valid, json.RawMessage(patrolDraftSchema)); err != nil {
		t.Fatalf("a conforming draft was rejected: %v", err)
	}
	for name, bad := range map[string]string{
		"missing prompt": `{"title":"t","summary":"s"}`,
		"extra field":    `{"title":"t","prompt":"p","summary":"s","cron":"0 9 * * *"}`,
	} {
		if _, err := structuredoutput.ValidateOutput(bad, json.RawMessage(patrolDraftSchema)); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

// TestParsePatrolDraft normalizes the model's output: trimmed fields, a
// fallback title, and an empty prompt refused rather than launched.
func TestParsePatrolDraft(t *testing.T) {
	d, err := parsePatrolDraft(`{"title":"  ","prompt":"  Do the thing  ","summary":" s "}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if d.Title != "My patrol" || d.Prompt != "Do the thing" || d.Summary != "s" {
		t.Errorf("draft = %+v", d)
	}
	if _, err := parsePatrolDraft(`{"title":"t","prompt":"   ","summary":"s"}`); err == nil {
		t.Error("an empty prompt should be refused")
	}
	if _, err := parsePatrolDraft(`not json`); err == nil {
		t.Error("non-JSON output should be refused")
	}
	long, err := parsePatrolDraft(`{"title":"` + strings.Repeat("é", 100) + `","prompt":"p","summary":"s"}`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(long.Title) > 80 || !strings.HasPrefix(long.Title, "é") {
		t.Errorf("a long title should be cut at a rune boundary within 80 bytes, got %q", long.Title)
	}
}

// TestPatrolDraftUserMessage renders the wizard's answers: blanks dropped,
// lists bounded, the autonomy mode normalized, and an empty connector list
// said out loud so the model does not invent one.
func TestPatrolDraftUserMessage(t *testing.T) {
	goals := []string{"Flag misconfigured deals", "  "}
	for i := 0; i < 20; i++ {
		goals = append(goals, "extra goal")
	}
	msg := patrolDraftUserMessage(PatrolDraftInput{
		Goals:    goals,
		Notes:    "Focus on CTV",
		Memories: []string{"Never email clients without asking", ""},
		Cadence:  "Weekdays at 08:00 (America/New_York)",
		Mode:     "YOLO",
		Persona:  "user:twin",
	})
	for _, want := range []string{
		"- Flag misconfigured deals\n",
		"Focus on CTV",
		"(none — the assistant can only use its built-in tools)",
		"- Never email clients without asking\n",
		"Schedule: Weekdays at 08:00 (America/New_York)",
		"Autonomy mode: suggest",
		"Runs as persona: user:twin",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
	if n := strings.Count(msg, "- extra goal"); n != patrolDraftMaxGoals-1 {
		t.Errorf("goals should be capped at %d, got %d extra", patrolDraftMaxGoals, n)
	}

	withConn := patrolDraftUserMessage(PatrolDraftInput{
		Goals:      []string{"Scan my inbox"},
		Connectors: []PatrolDraftConnector{{Name: "gmail", Description: "Read and search mail"}, {Name: " "}},
		Mode:       "act",
	})
	if !strings.Contains(withConn, "- gmail — Read and search mail\n") || strings.Contains(withConn, "(none") {
		t.Errorf("connector list rendered wrong:\n%s", withConn)
	}
	if !strings.Contains(withConn, "Autonomy mode: act") {
		t.Errorf("act mode should survive normalization:\n%s", withConn)
	}

	// Memories: the same cap as a turn's injection (50), not fewer.
	mems := make([]string, 0, 60)
	for i := 0; i < 60; i++ {
		mems = append(mems, "rule")
	}
	many := patrolDraftUserMessage(PatrolDraftInput{Goals: []string{"g"}, Memories: mems})
	if n := strings.Count(many, "- rule\n"); n != 50 {
		t.Errorf("memories should be capped at 50 (the injection cap), got %d", n)
	}
}
