package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUnshareClauseIsSpelledOnce pins that every revocation UPDATE in this
// package splices unshareConversationSet instead of hand-writing the clause.
// A hand-written copy is how a new share column gets missed on one path,
// leaving a chat readable by an audience that was supposed to lose it (#1946).
// The marker is the last column of the clause, so it also catches a copy laid
// out across several lines.
func TestUnshareClauseIsSpelledOnce(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	const marker = "team_shared_with_id = NULL"
	if !strings.Contains(unshareConversationSet, marker) {
		t.Fatalf("unshareConversationSet no longer clears team_shared_with_id; update this test's marker")
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
			if strings.Contains(line, marker) && !strings.Contains(line, "const unshareConversationSet") {
				t.Errorf("%s:%d hand-writes the unshare clause; splice unshareConversationSet instead", f, i+1)
			}
		}
	}
}
