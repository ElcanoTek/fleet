package store

// Load-bearing assertions: shared-file CRUD round-trips every column, listing
// is folder-then-name ordered, the (folder, name) unique constraint surfaces
// as ErrSharedFileExists on both create and rename, delete returns the row it
// removed, and the byte total sums the manifest.

import (
	"context"
	"errors"
	"testing"
)

func TestSharedFilesCRUD(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	created, err := st.CreateSharedFile(ctx, SharedFile{
		ID: "tok1", Name: "b.csv", Folder: "hist", Description: "2019 data",
		SizeBytes: 42, ContentType: "text/csv", SHA256: "deadbeef", UploadedBy: "admin@x",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.CreatedAt == 0 || created.UpdatedAt == 0 {
		t.Fatalf("timestamps not stamped: %+v", created)
	}
	if _, err := st.CreateSharedFile(ctx, SharedFile{ID: "tok2", Name: "a.csv"}); err != nil {
		t.Fatalf("create second: %v", err)
	}

	// Duplicate staged path → the typed conflict.
	if _, err := st.CreateSharedFile(ctx, SharedFile{ID: "tok3", Name: "b.csv", Folder: "hist"}); !errors.Is(err, ErrSharedFileExists) {
		t.Fatalf("duplicate create err = %v, want ErrSharedFileExists", err)
	}

	got, err := st.GetSharedFile(ctx, "tok1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got != created {
		t.Fatalf("get = %+v, want %+v", got, created)
	}
	if _, err := st.GetSharedFile(ctx, "nope"); !errors.Is(err, ErrSharedFileNotFound) {
		t.Fatalf("get missing err = %v", err)
	}

	// Ordering: folder "" sorts before "hist".
	list, err := st.ListSharedFiles(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %v, %v", list, err)
	}
	if list[0].ID != "tok2" || list[1].ID != "tok1" {
		t.Fatalf("list order = %s, %s", list[0].ID, list[1].ID)
	}

	// Rename/move/describe.
	upd, err := st.UpdateSharedFileMeta(ctx, "tok1", "renamed.csv", "", "fresh")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Name != "renamed.csv" || upd.Folder != "" || upd.Description != "fresh" {
		t.Fatalf("update result = %+v", upd)
	}
	if _, err := st.UpdateSharedFileMeta(ctx, "tok2", "renamed.csv", "", ""); !errors.Is(err, ErrSharedFileExists) {
		t.Fatalf("rename onto occupied path err = %v", err)
	}
	if _, err := st.UpdateSharedFileMeta(ctx, "nope", "x", "", ""); !errors.Is(err, ErrSharedFileNotFound) {
		t.Fatalf("update missing err = %v", err)
	}

	total, err := st.TotalSharedFileBytes(ctx)
	if err != nil || total != 42 {
		t.Fatalf("total = %d, %v (want 42)", total, err)
	}

	deleted, err := st.DeleteSharedFile(ctx, "tok1")
	if err != nil || deleted.Name != "renamed.csv" {
		t.Fatalf("delete = %+v, %v", deleted, err)
	}
	if _, err := st.DeleteSharedFile(ctx, "tok1"); !errors.Is(err, ErrSharedFileNotFound) {
		t.Fatalf("double delete err = %v", err)
	}
}

// TestSharedFilePathNamespaceConflicts pins the guard on the collision UNIQUE
// (folder, name) cannot see: a root-level file named "q3" and a folder named
// "q3" are different (folder, name) pairs, but "shared/q3" cannot be both a
// file and a directory. Admitting both left the staged-tree reconciler failing
// on every pass forever, with whichever row reached the sandbox flapping by map
// iteration order — so the conflict has to be refused at write time, while
// there is still a request to return 409 on.
func TestSharedFilePathNamespaceConflicts(t *testing.T) {
	ctx := context.Background()

	t.Run("folder blocks a root file of the same name", func(t *testing.T) {
		st := newTestStore(t)
		if _, err := st.CreateSharedFile(ctx, SharedFile{ID: "a", Name: "x.csv", Folder: "q3"}); err != nil {
			t.Fatalf("create in folder: %v", err)
		}
		_, err := st.CreateSharedFile(ctx, SharedFile{ID: "b", Name: "q3"})
		if !errors.Is(err, ErrSharedFileNameIsFolder) {
			t.Fatalf("create root file named after a folder = %v, want ErrSharedFileNameIsFolder", err)
		}
		// The refused insert must leave nothing behind.
		if list, err := st.ListSharedFiles(ctx); err != nil || len(list) != 1 {
			t.Fatalf("list after refusal = %v, %v; want the one original row", list, err)
		}
	})

	t.Run("root file blocks a folder of the same name", func(t *testing.T) {
		st := newTestStore(t)
		if _, err := st.CreateSharedFile(ctx, SharedFile{ID: "a", Name: "q3"}); err != nil {
			t.Fatalf("create root file: %v", err)
		}
		_, err := st.CreateSharedFile(ctx, SharedFile{ID: "b", Name: "x.csv", Folder: "q3"})
		if !errors.Is(err, ErrSharedFileNameIsFolder) {
			t.Fatalf("create into a folder named after a root file = %v, want ErrSharedFileNameIsFolder", err)
		}
	})

	t.Run("rename into the conflict is refused both ways", func(t *testing.T) {
		st := newTestStore(t)
		if _, err := st.CreateSharedFile(ctx, SharedFile{ID: "nested", Name: "x.csv", Folder: "q3"}); err != nil {
			t.Fatalf("create nested: %v", err)
		}
		if _, err := st.CreateSharedFile(ctx, SharedFile{ID: "root", Name: "other.csv"}); err != nil {
			t.Fatalf("create root: %v", err)
		}
		// Renaming the root file to the folder's name.
		if _, err := st.UpdateSharedFileMeta(ctx, "root", "q3", "", ""); !errors.Is(err, ErrSharedFileNameIsFolder) {
			t.Fatalf("rename onto a folder name = %v, want ErrSharedFileNameIsFolder", err)
		}
		// And the reverse: moving the nested row out, then a root file in.
		if _, err := st.UpdateSharedFileMeta(ctx, "root", "q3x", "", ""); err != nil {
			t.Fatalf("unrelated rename must still work: %v", err)
		}
		if _, err := st.UpdateSharedFileMeta(ctx, "nested", "y.csv", "q3x", ""); !errors.Is(err, ErrSharedFileNameIsFolder) {
			t.Fatalf("move into a folder named after a root file = %v, want ErrSharedFileNameIsFolder", err)
		}
	})

	t.Run("a row does not conflict with itself", func(t *testing.T) {
		st := newTestStore(t)
		if _, err := st.CreateSharedFile(ctx, SharedFile{ID: "a", Name: "q3", Folder: "q3"}); err != nil {
			t.Fatalf("create: %v", err)
		}
		// Same (folder, name), new description: the guard must not read the row
		// it is updating as its own blocker.
		if _, err := st.UpdateSharedFileMeta(ctx, "a", "q3", "q3", "note"); err != nil {
			t.Fatalf("self-update refused: %v", err)
		}
	})

	t.Run("unrelated names and a missing id are unaffected", func(t *testing.T) {
		st := newTestStore(t)
		for _, f := range []SharedFile{
			{ID: "a", Name: "q3.csv"},           // "q3.csv" != folder "q3"
			{ID: "b", Name: "q3", Folder: "q4"}, // a file named after a DIFFERENT folder's sibling
			{ID: "c", Name: "q4.csv"},
		} {
			if _, err := st.CreateSharedFile(ctx, f); err != nil {
				t.Fatalf("create %s must be allowed: %v", f.ID, err)
			}
		}
		// 404 must still beat 409: an unknown id is not-found, not a conflict.
		if _, err := st.UpdateSharedFileMeta(ctx, "missing", "z.csv", "", ""); !errors.Is(err, ErrSharedFileNotFound) {
			t.Fatalf("update unknown id = %v, want ErrSharedFileNotFound", err)
		}
	})
}
