package httpapi

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDuTree_SumsRegularFilesRecursively(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), make([]byte, 100), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b.bin"), make([]byte, 50), 0o600); err != nil {
		t.Fatal(err)
	}

	got := duTree(context.Background(), dir)
	if got.Bytes != 150 {
		t.Errorf("Bytes = %d, want 150", got.Bytes)
	}
	if got.Files != 2 {
		t.Errorf("Files = %d, want 2", got.Files)
	}
}

func TestDuTree_MissingDirIsZeroNotError(t *testing.T) {
	t.Parallel()

	got := duTree(context.Background(), filepath.Join(t.TempDir(), "does-not-exist"))
	if got.Bytes != 0 || got.Files != 0 {
		t.Errorf("missing dir should account as empty, got %+v", got)
	}
}

func TestSweepTempUploads_RemovesOldKeepsFresh(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	stale := filepath.Join(dir, "old.bin")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(dir, "new.bin")
	if err := os.WriteFile(fresh, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	removed := sweepTempUploads(dir, 24*time.Hour)
	if removed != 1 {
		t.Errorf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale file must be gone")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh file must survive")
	}
}

func TestWorkspaceUsage_ExcludesNonConversationSubdirsFromLargestWorkspaces(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	convID := "7194bb84-4594-44f2-9c05-1eb35d5e21e1"
	for _, sub := range []struct {
		name string
		size int
	}{
		{"tasks", 500},
		{"mcp-runs", 400},
		{"shared", 200},
		{convID, 100},
	} {
		p := filepath.Join(root, sub.name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "f.bin"), make([]byte, sub.size), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	s := &Server{store: newFakeChatStore()}
	req := httptest.NewRequest("GET", "/admin/storage", nil)
	total, rows := s.workspaceUsage(req, root, 10)
	if total.Bytes != 1200 || total.Files != 4 {
		t.Fatalf("total = %+v, want Bytes=1200 Files=4", total)
	}
	if len(rows) != 1 || rows[0].ConversationID != convID {
		t.Fatalf("largest_workspaces must only include UUID conversation directories, got %+v", rows)
	}
}
