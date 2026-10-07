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

// TestSweepOrphanWorkspacesReadOnlySubdir — an orphan whose tree holds a
// directory without the owner-write bit (an agent's `chmod a-w`, an archive
// unpacked with read-only modes) must still be removed. A plain RemoveAll
// fails there, and the sweep used to drop that error, so the dir — 4.8 GB of
// it on one production box — was retried and silently kept forever.
func TestSweepOrphanWorkspacesReadOnlySubdir(t *testing.T) {
	s := newTestStore(t)
	root := t.TempDir()
	orphan := filepath.Join(root, "0a3c2f6e-5b1d-4c8e-9f7a-2d4b6e8a0c1f")
	locked := filepath.Join(orphan, "nissan", "Nissan")
	if err := os.MkdirAll(locked, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "report.csv"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o550); err != nil {
		t.Fatal(err)
	}
	// If the sweep regresses, put the bit back so t.TempDir can clean up.
	t.Cleanup(func() { _ = os.Chmod(locked, 0o750) })
	if os.RemoveAll(orphan) == nil {
		t.Skip("filesystem lets a plain RemoveAll through a read-only dir (running as root?) — nothing to prove")
	}

	removed, err := s.SweepOrphanWorkspaces(context.Background(), root)
	if err != nil {
		t.Fatalf("SweepOrphanWorkspaces: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed=%d, want 1", removed)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("orphan with a read-only subdir should be gone: err=%v", err)
	}
}

// TestSweepOrphanWorkspacesUnreadableOrphan — the orphan dir ITSELF without
// owner read (`chmod 000 .` / `chmod 0111 .` by the sandbox) cannot be opened
// to walk, so its mode is repaired through the parent before the walk.
func TestSweepOrphanWorkspacesUnreadableOrphan(t *testing.T) {
	for _, mode := range []os.FileMode{0o000, 0o111} {
		t.Run(mode.String(), func(t *testing.T) {
			s := newTestStore(t)
			root := t.TempDir()
			orphan := filepath.Join(root, "1b4d3e7f-6c2e-4d9f-8a0b-3e5c7f9b1d2a")
			if err := os.MkdirAll(filepath.Join(orphan, "sub"), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(orphan, "sub", "f.csv"), []byte("x"), 0o640); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(orphan, mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(orphan, 0o750) })

			removed, err := s.SweepOrphanWorkspaces(context.Background(), root)
			if err != nil {
				t.Fatalf("SweepOrphanWorkspaces: %v", err)
			}
			if removed != 1 {
				t.Errorf("removed=%d, want 1", removed)
			}
			if _, err := os.Lstat(orphan); !os.IsNotExist(err) {
				t.Errorf("unreadable orphan should be gone: err=%v", err)
			}
		})
	}
}

// TestRemoveWorkspaceTreeSymlinkCannotEscape — a sandbox that swaps an orphan
// for a symlink to a directory OUTSIDE the workspace must not get that
// directory chmodded or deleted: every operation goes through an os.Root at the
// workspace root, which refuses to resolve an escaping name.
func TestRemoveWorkspaceTreeSymlinkCannotEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "locked")
	if err := os.MkdirAll(victim, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "keep.txt"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(victim, 0o550); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(victim, 0o750) })
	const name = "2c5e4f8a-7d3f-4ea0-9b1c-4f6d8a0c2e3b"
	if err := os.Symlink(outside, filepath.Join(root, name)); err != nil {
		t.Fatal(err)
	}

	_ = removeWorkspaceTree(root, name)

	info, err := os.Stat(victim)
	if err != nil {
		t.Fatalf("directory outside the workspace was removed: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o550 {
		t.Errorf("directory outside the workspace was chmodded: mode %o, want 550", got)
	}
	if _, err := os.Stat(filepath.Join(victim, "keep.txt")); err != nil {
		t.Errorf("file outside the workspace was removed: %v", err)
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

// ── admin storage management ─────────────────────────────────────────────

// backdateConversation shifts a conversation's updated_at to `when` (unix
// seconds) so cutoff-based queries can be exercised without sleeping.
func backdateConversation(t *testing.T, s *Store, id string, when int64) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`UPDATE conversations SET updated_at = $1 WHERE id = $2`, when, id); err != nil {
		t.Fatalf("backdate %s: %v", id, err)
	}
}

func TestDeleteUnpinnedOlderThan_SparesProtectedRows(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	old := time.Now().Add(-90 * 24 * time.Hour).Unix()

	stale, err := s.CreateConversation(ctx, "u@x.com", "stale", "victoria", "", false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	backdateConversation(t, s, stale.ID, old)

	pinned, err := s.CreateConversation(ctx, "u@x.com", "pinned", "victoria", "", false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetPinned(ctx, "u@x.com", pinned.ID, true); err != nil {
		t.Fatalf("pin: %v", err)
	}
	backdateConversation(t, s, pinned.ID, old)

	fresh, err := s.CreateConversation(ctx, "u@x.com", "fresh", "victoria", "", false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	n, err := s.DeleteUnpinnedOlderThan(ctx, time.Now().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("DeleteUnpinnedOlderThan: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted = %d, want 1 (only the stale unpinned conv)", n)
	}
	if c, _ := s.Get(ctx, "u@x.com", stale.ID); c != nil {
		t.Error("stale unpinned conversation must be gone")
	}
	if c, _ := s.Get(ctx, "u@x.com", pinned.ID); c == nil {
		t.Error("pinned conversation must survive")
	}
	if c, _ := s.Get(ctx, "u@x.com", fresh.ID); c == nil {
		t.Error("fresh conversation must survive")
	}
}

func TestStorageConversationStats_CountsReclaimable(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	old := time.Now().Add(-90 * 24 * time.Hour).Unix()

	stale, err := s.CreateConversation(ctx, "u@x.com", "stale", "victoria", "", false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	backdateConversation(t, s, stale.ID, old)

	pinned, err := s.CreateConversation(ctx, "u@x.com", "pinned-old", "victoria", "", false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.SetPinned(ctx, "u@x.com", pinned.ID, true); err != nil {
		t.Fatalf("pin: %v", err)
	}
	backdateConversation(t, s, pinned.ID, old)

	if _, err := s.CreateConversation(ctx, "u@x.com", "fresh", "victoria", "", false); err != nil {
		t.Fatalf("create: %v", err)
	}

	stats, err := s.StorageConversationStats(ctx, time.Now().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatalf("StorageConversationStats: %v", err)
	}
	if stats.Total != 3 {
		t.Errorf("Total = %d, want 3", stats.Total)
	}
	if stats.Pinned != 1 {
		t.Errorf("Pinned = %d, want 1", stats.Pinned)
	}
	if stats.Protected != 1 {
		t.Errorf("Protected = %d, want 1", stats.Protected)
	}
	if stats.ReclaimableAtCutoff != 1 {
		t.Errorf("ReclaimableAtCutoff = %d, want 1 (only the stale unpinned conv)", stats.ReclaimableAtCutoff)
	}
}

func TestConversationStorageMetaByIDs(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	c, err := s.CreateConversation(ctx, "u@x.com", "big analysis", "victoria", "", false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	meta, err := s.ConversationStorageMetaByIDs(ctx, []string{c.ID, "00000000-0000-0000-0000-000000000000"})
	if err != nil {
		t.Fatalf("ConversationStorageMetaByIDs: %v", err)
	}
	if len(meta) != 1 {
		t.Fatalf("len(meta) = %d, want 1 (unknown id absent)", len(meta))
	}
	m := meta[c.ID]
	if m.Title != "big analysis" || m.UserEmail != "u@x.com" || m.Pinned {
		t.Errorf("meta mismatch: %+v", m)
	}
}
