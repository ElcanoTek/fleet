package store

import (
	"context"
	"testing"
)

// folderTestUser is the single owner used across the folder/label store tests.
const folderTestUser = "u@x.com"

// seedConv creates a conversation owned by folderTestUser and (optionally)
// assigns a folder + labels via BulkPatch, returning its id. Folder "" / nil
// labels leaves the defaults.
func seedConv(t *testing.T, s *Store, title, folder string, labels []string) string {
	t.Helper()
	ctx := context.Background()
	c, err := s.CreateConversation(ctx, folderTestUser, title, "victoria", "m", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if folder != "" || labels != nil {
		if _, err := s.BulkPatch(ctx, folderTestUser, []string{c.ID}, nil, &folder, labels); err != nil {
			t.Fatalf("BulkPatch: %v", err)
		}
	}
	return c.ID
}

// TestListFiltered_FolderAndLabels covers the #258 filter semantics: folder
// equality, single-label match, AND across multiple labels, folder+label
// combined, the explicit no-folder bucket, and the unfiltered baseline.
func TestListFiltered_FolderAndLabels(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const u = "u@x.com"

	work1 := seedConv(t, s, "w1", "Work", []string{"go", "urgent"})
	seedConv(t, s, "w2", "Work", []string{"go"})
	seedConv(t, s, "r1", "Research", []string{"urgent"})
	none := seedConv(t, s, "n1", "", nil)

	work := "Work"
	if got, err := s.ListFiltered(ctx, u, ListFilter{Folder: &work}); err != nil || len(got) != 2 {
		t.Fatalf("folder=Work: got %d (err %v), want 2", len(got), err)
	}
	if got, err := s.ListFiltered(ctx, u, ListFilter{Labels: []string{"go"}}); err != nil || len(got) != 2 {
		t.Fatalf("label=go: got %d (err %v), want 2", len(got), err)
	}
	// AND semantics: only work1 has both labels.
	got, err := s.ListFiltered(ctx, u, ListFilter{Labels: []string{"go", "urgent"}})
	if err != nil || len(got) != 1 || got[0].ID != work1 {
		t.Fatalf("label=go&urgent: got %d (err %v), want 1 (work1)", len(got), err)
	}
	// Folder + label combined.
	got, err = s.ListFiltered(ctx, u, ListFilter{Folder: &work, Labels: []string{"urgent"}})
	if err != nil || len(got) != 1 || got[0].ID != work1 {
		t.Fatalf("folder=Work&label=urgent: got %d (err %v), want 1 (work1)", len(got), err)
	}
	// Explicit no-folder bucket (folder="") returns only the unassigned conv.
	empty := ""
	got, err = s.ListFiltered(ctx, u, ListFilter{Folder: &empty})
	if err != nil || len(got) != 1 || got[0].ID != none {
		t.Fatalf("folder='': got %d (err %v), want 1 (none)", len(got), err)
	}
	// No filter → all four.
	if got, err := s.ListFiltered(ctx, u, ListFilter{}); err != nil || len(got) != 4 {
		t.Fatalf("no filter: got %d (err %v), want 4", len(got), err)
	}
}

// TestListFolders returns distinct non-empty folders with active-conversation
// counts, excluding the no-folder bucket (#258).
func TestListFolders(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const u = "u@x.com"

	seedConv(t, s, "w1", "Work", nil)
	seedConv(t, s, "w2", "Work", nil)
	seedConv(t, s, "r1", "Research", nil)
	seedConv(t, s, "n1", "", nil) // no folder — must not appear

	folders, err := s.ListFolders(ctx, u)
	if err != nil {
		t.Fatalf("ListFolders: %v", err)
	}
	// Ordered by name: Research, Work.
	want := map[string]int{"Work": 2, "Research": 1}
	if len(folders) != len(want) {
		t.Fatalf("ListFolders returned %d folders, want %d: %+v", len(folders), len(want), folders)
	}
	for _, fc := range folders {
		if want[fc.Name] != fc.Count {
			t.Errorf("folder %q count = %d, want %d", fc.Name, fc.Count, want[fc.Name])
		}
	}
	if folders[0].Name != "Research" || folders[1].Name != "Work" {
		t.Errorf("folders not name-ordered: %+v", folders)
	}
}

// TestRenameFolder moves every conversation from one folder name to another in a
// single update; the old name then has no conversations and the new one has them
// all (#258).
func TestRenameFolder(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const u = "u@x.com"

	seedConv(t, s, "w1", "Work", nil)
	seedConv(t, s, "w2", "Work", nil)
	seedConv(t, s, "other", "Research", nil)

	n, err := s.RenameFolder(ctx, u, "Work", "Client Work")
	if err != nil || n != 2 {
		t.Fatalf("RenameFolder: moved %d (err %v), want 2", n, err)
	}
	work := "Work"
	if got, _ := s.ListFiltered(ctx, u, ListFilter{Folder: &work}); len(got) != 0 {
		t.Errorf("old folder still has %d conversations, want 0", len(got))
	}
	client := "Client Work"
	if got, _ := s.ListFiltered(ctx, u, ListFilter{Folder: &client}); len(got) != 2 {
		t.Errorf("new folder has %d conversations, want 2", len(got))
	}
	// Renaming an unknown folder moves nothing (no error).
	if n, err := s.RenameFolder(ctx, u, "Nope", "X"); err != nil || n != 0 {
		t.Errorf("rename unknown folder: moved %d (err %v), want 0", n, err)
	}
}
