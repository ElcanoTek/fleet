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
