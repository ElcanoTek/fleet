package admincli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestRenderDLQTable — the dead-letter listing goes through the shared
// renderer: a header naming the columns, then one aligned row per task with
// the reason collapsed to one line. It used to print headerless tab-joined
// lines with ad-hoc "attempts=" / "tags=" prefixes.
func TestRenderDLQTable(t *testing.T) {
	when := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	reason := "boom\nsecond line"
	id := uuid.MustParse("cccccccc-1111-2222-3333-444444444444")
	tasks := []*models.Task{
		{ID: id, DeadLetteredAt: &when, DeadLetterAttempts: 3, DeadLetterReason: &reason, Tags: []string{"a", "b"}},
		{ID: uuid.New()}, // never stamped: placeholders, not a panic
	}
	var buf bytes.Buffer
	if err := renderDLQTable(&buf, tasks); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want header + 2 rows, got:\n%s", out)
	}
	for _, want := range []string{"ID", "DEAD_LETTERED_AT", "ATTEMPTS", "TAGS", "REASON",
		id.String(), "2026-08-01T12:00:00Z", "3", "a,b", "boom second line", "?", "-"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should contain %q; got:\n%s", want, out)
		}
	}
	// The reason must be collapsed onto its row: the embedded newline is what
	// used to break the column alignment for every row after it. (Matching on
	// "second line\n" would be wrong — the row itself legitimately ends in a
	// newline once the reason is collapsed.)
	if strings.Contains(out, "attempts=") || strings.Contains(out, reason) {
		t.Errorf("legacy prefixes / multi-line reason leaked:\n%s", out)
	}
}

// TestSchedDLQListRejectsBadLimit — the list family shares one --limit rule
// (>= 1, page with --offset); the old "<=0 = all" special case is gone. The
// checks run before any DB is opened, so they are testable without one.
func TestSchedDLQListRejectsBadLimit(t *testing.T) {
	if code := schedDLQList([]string{"--limit", "0"}); code != 1 {
		t.Errorf("--limit 0: exit %d, want 1", code)
	}
	if code := schedDLQList([]string{"--limit", "-5"}); code != 1 {
		t.Errorf("--limit -5: exit %d, want 1", code)
	}
	if code := schedDLQList([]string{"--offset", "-1"}); code != 1 {
		t.Errorf("--offset -1: exit %d, want 1", code)
	}
	if code := schedDLQList([]string{"stray"}); code != 1 {
		t.Errorf("stray positional: exit %d, want 1", code)
	}
}
