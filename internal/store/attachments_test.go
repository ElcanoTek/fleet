package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mustTouch writes content to path and stamps its mtime to mtime.
func mustTouch(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func TestSweepAttachments_RemovesOldKeepsFresh(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	// Two flat files: one fresh, one stale.
	mustTouch(t, filepath.Join(dir, "fresh.csv"), "hello", now)
	mustTouch(t, filepath.Join(dir, "stale.csv"), "old", now.Add(-30*24*time.Hour))

	// One stale file inside a subdir — make sure recursion picks it up.
	sub := filepath.Join(dir, "by-sender")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustTouch(t, filepath.Join(sub, "report.pdf"), "x", now.Add(-30*24*time.Hour))

	removed, err := SweepAttachments(dir, 14*24*time.Hour)
	if err != nil {
		t.Fatalf("SweepAttachments: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed = %d, want 2", removed)
	}
	if _, err := os.Stat(filepath.Join(dir, "fresh.csv")); err != nil {
		t.Errorf("fresh.csv was deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "stale.csv")); err == nil {
		t.Error("stale.csv was not deleted")
	}
	if _, err := os.Stat(filepath.Join(sub, "report.pdf")); err == nil {
		t.Error("nested stale file was not deleted")
	}
	// Subdir should still exist (we don't prune empties).
	if _, err := os.Stat(sub); err != nil {
		t.Errorf("subdir was pruned: %v", err)
	}
}

func TestSweepAttachments_MissingDirIsNoOp(t *testing.T) {
	removed, err := SweepAttachments(filepath.Join(t.TempDir(), "does-not-exist"), 14*24*time.Hour)
	if err != nil {
		t.Fatalf("missing dir should not error, got: %v", err)
	}
	if removed != 0 {
		t.Errorf("removed = %d, want 0", removed)
	}
}

func TestSweepAttachments_NotADirIsError(t *testing.T) {
	dir := t.TempDir()
	notDir := filepath.Join(dir, "regular.file")
	mustTouch(t, notDir, "x", time.Now())

	_, err := SweepAttachments(notDir, 14*24*time.Hour)
	if err == nil {
		t.Fatal("expected error for non-directory path")
	}
}

func TestLooksLikeConversationID(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"valid v4", "ff638dee-ffcb-497d-ba91-e84fd6f94ae2", true},
		{"uppercase hex", "FF638DEE-FFCB-497D-BA91-E84FD6F94AE2", true},
		{"too short", "abc", false},
		{"missing dashes", "ff638deeffcb497dba91e84fd6f94ae2aaaa", false},
		{"non-hex char", "zz638dee-ffcb-497d-ba91-e84fd6f94ae2", false},
		{"extra tail", "ff638dee-ffcb-497d-ba91-e84fd6f94ae2x", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := looksLikeConversationID(c.in); got != c.want {
				t.Errorf("looksLikeConversationID(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestSweepOrphanWorkspaces(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	root := t.TempDir()

	// One live conversation → its dir must survive.
	if _, err := s.CreateUser(ctx, "w@x.com", "password123"); err != nil {
		t.Fatal(err)
	}
	live, err := s.CreateConversation(ctx, "w@x.com", "t", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	liveDir := filepath.Join(root, live.ID)
	if err := os.MkdirAll(liveDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(liveDir, "attach.csv"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}

	// One orphan UUID-shaped dir → must go.
	orphan := filepath.Join(root, "ff638dee-ffcb-497d-ba91-e84fd6f94ae2")
	if err := os.MkdirAll(orphan, 0o750); err != nil {
		t.Fatal(err)
	}

	// One non-UUID dir dropped by an operator → must be spared.
	operatorDir := filepath.Join(root, "operator-notes")
	if err := os.MkdirAll(operatorDir, 0o750); err != nil {
		t.Fatal(err)
	}

	removed, err := s.SweepOrphanWorkspaces(ctx, root)
	if err != nil {
		t.Fatalf("SweepOrphanWorkspaces: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed=%d, want 1", removed)
	}
	if _, err := os.Stat(liveDir); err != nil {
		t.Errorf("live dir should survive: %v", err)
	}
	if _, err := os.Stat(operatorDir); err != nil {
		t.Errorf("non-UUID dir should survive: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphan should be gone: err=%v", err)
	}
}

func TestSweepOrphanWorkspacesMissingRoot(t *testing.T) {
	s := newTestStore(t)
	n, err := s.SweepOrphanWorkspaces(context.Background(), filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("n=%d, want 0", n)
	}
}
