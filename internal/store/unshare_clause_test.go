package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// clearsShareColumn matches a literal clearing assignment to any of the four
// columns unshareConversationSet resets. A CASE expression (a conditional,
// deliberately partial write) does not match.
var clearsShareColumn = regexp.MustCompile(`\b(?:team_visible|multiplayer)\s*=\s*FALSE\b|\bteam_shared_with(?:_id)?\s*=\s*NULL\b`)

// allowedPartialClears lists the writes that clear one of those columns on
// purpose WITHOUT revoking the rest of the share, keyed by file and the
// column the line clears. Each needs a reason.
var allowedPartialClears = map[string]string{
	// Turning Multiplayer off clears only its own flag; team_visible and the
	// stamped audience are kept or cleared by CASE on project sharing.
	"multiplayer.go:multiplayer": "multiplayer toggle off is not a revocation",
}

// TestUnshareClauseIsSpelledOnce pins that every revocation UPDATE in this
// package splices unshareConversationSet instead of hand-writing the clause
// (#1946). A hand-written copy is how a share column gets missed on one path —
// leaving a chat readable by an audience that was supposed to lose it — so any
// literal clear of any of the four columns outside the constant fails here,
// including a copy that forgot a column.
func TestUnshareClauseIsSpelledOnce(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if got := clearsShareColumn.FindAllString(unshareConversationSet, -1); len(got) != 4 {
		t.Fatalf("unshareConversationSet clears %d of the 4 share columns (%q); update this test with it", len(got), got)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.Contains(line, "const unshareConversationSet") {
				continue
			}
			for _, m := range clearsShareColumn.FindAllString(line, -1) {
				column := strings.TrimSpace(strings.SplitN(m, "=", 2)[0])
				if _, ok := allowedPartialClears[f+":"+column]; ok {
					continue
				}
				t.Errorf("%s:%d hand-writes %q; splice unshareConversationSet instead (or, for a deliberate partial clear, add it to allowedPartialClears with a reason)", f, i+1, m)
			}
		}
	}
}
