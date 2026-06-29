package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// createNConvs provisions n conversations for userEmail and returns their IDs.
func createNConvs(t *testing.T, s *Store, userEmail string, n int) []string {
	t.Helper()
	ctx := context.Background()
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		c, err := s.CreateConversation(ctx, userEmail, "", "victoria", "", false)
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		ids = append(ids, c.ID)
	}
	return ids
}

// TestDeleteByIDs_TargetedHardDelete proves the happy path: exactly the supplied
// IDs are hard-deleted and the returned count matches.
func TestDeleteByIDs_TargetedHardDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ids := createNConvs(t, s, "u@x.com", 3)

	n, err := s.DeleteByIDs(ctx, "u@x.com", ids)
	if err != nil {
		t.Fatalf("DeleteByIDs: %v", err)
	}
	if n != 3 {
		t.Errorf("deleted = %d, want 3", n)
	}
	// None survive.
	for _, id := range ids {
		if g, _ := s.Get(ctx, "u@x.com", id); g != nil {
			t.Errorf("conversation %s still present after delete", id)
		}
	}
}

// TestDeleteByIDs_ForeignIDRejected is the table-driven ownership path: any
// ID the caller doesn't own (foreign user's row, or a non-existent ID) aborts
// the whole request with ErrForeignConversation and leaves every row intact.
func TestDeleteByIDs_ForeignIDRejected(t *testing.T) {
	cases := []struct {
		name string
		ids  func(ownerIDs, otherIDs []string) []string
	}{
		{
			name: "one foreign user's ID",
			ids:  func(owner, other []string) []string { return []string{owner[0], other[0]} },
		},
		{
			name: "one non-existent ID mixed in",
			ids: func(owner, _ []string) []string {
				return []string{owner[0], owner[1], "00000000-0000-0000-0000-000000000000"}
			},
		},
		{
			name: "all foreign",
			ids:  func(_, other []string) []string { return other },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			ctx := context.Background()
			ownerIDs := createNConvs(t, s, "owner@x.com", 2)
			otherIDs := createNConvs(t, s, "intruder@x.com", 2)

			req := tc.ids(ownerIDs, otherIDs)
			n, err := s.DeleteByIDs(ctx, "owner@x.com", req)
			if !errors.Is(err, ErrForeignConversation) {
				t.Fatalf("err = %v, want ErrForeignConversation", err)
			}
			if n != 0 {
				t.Errorf("deleted = %d, want 0 (no-op on rejection)", n)
			}
			// Every owner row must still be present — a foreign ID never
			// produces a partial delete.
			for _, id := range ownerIDs {
				if g, _ := s.Get(ctx, "owner@x.com", id); g == nil {
					t.Errorf("owner conversation %s deleted by a rejected request", id)
				}
			}
		})
	}
}

// TestDeleteByIDs_SoftDelete proves the soft-delete path: deleted rows are
// tombstoned (deleted_at set) rather than removed, and are hidden from List
// and Get while still present in the DB.
func TestDeleteByIDs_SoftDelete(t *testing.T) {
	s := newTestStore(t)
	s.SetSoftDelete(true)
	ctx := context.Background()
	ids := createNConvs(t, s, "u@x.com", 2)

	n, err := s.DeleteByIDs(ctx, "u@x.com", ids)
	if err != nil {
		t.Fatalf("DeleteByIDs: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted = %d, want 2", n)
	}

	// Hidden from List.
	list, err := s.List(ctx, "u@x.com", false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("List returned %d conversations, want 0 (soft-deleted hidden)", len(list))
	}
	// Hidden from Get.
	for _, id := range ids {
		if g, _ := s.Get(ctx, "u@x.com", id); g != nil {
			t.Errorf("Get returned soft-deleted conversation %s", id)
		}
	}
	// Still in the DB with a tombstone set.
	var deletedAt *time.Time
	for _, id := range ids {
		if err := s.db.QueryRowContext(ctx,
			`SELECT deleted_at FROM conversations WHERE id = $1`, id).Scan(&deletedAt); err != nil {
			t.Fatalf("query deleted_at: %v", err)
		}
		if deletedAt == nil {
			t.Errorf("conversation %s has NULL deleted_at (want tombstoned)", id)
		}
	}
}

// TestBulkPatch_AdditiveFields proves each changes field is written when
// supplied, and that the mutation lands on every supplied ID in one round-trip.
func TestBulkPatch_AdditiveFields(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ids := createNConvs(t, s, "u@x.com", 3)

	pin := true
	folder := "Archive"
	labels := []string{"done", "review"}
	n, err := s.BulkPatch(ctx, "u@x.com", ids, &pin, &folder, labels)
	if err != nil {
		t.Fatalf("BulkPatch: %v", err)
	}
	if n != 3 {
		t.Errorf("updated = %d, want 3", n)
	}
	for _, id := range ids {
		g, _ := s.Get(ctx, "u@x.com", id)
		if g == nil {
			t.Fatalf("Get %s: nil", id)
		}
		if !g.Pinned || g.Folder != "Archive" || len(g.Labels) != 2 {
			t.Errorf("conversation %s = %+v", id, g)
		}
	}
}

// TestBulkPatch_NilFieldsUntouched proves a nil pointer leaves the stored
// value alone (additive semantics) — only supplied fields are written.
func TestBulkPatch_NilFieldsUntouched(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ids := createNConvs(t, s, "u@x.com", 1)

	// First patch: set folder + labels.
	folder := "Old work"
	labels := []string{"keep"}
	if _, err := s.BulkPatch(ctx, "u@x.com", ids, nil, &folder, labels); err != nil {
		t.Fatalf("BulkPatch set: %v", err)
	}
	// Second patch: only flip pinned; folder + labels must survive.
	pin := true
	if _, err := s.BulkPatch(ctx, "u@x.com", ids, &pin, nil, nil); err != nil {
		t.Fatalf("BulkPatch pin: %v", err)
	}
	g, _ := s.Get(ctx, "u@x.com", ids[0])
	if g == nil {
		t.Fatal("Get: nil")
	}
	if !g.Pinned || g.Folder != "Old work" || len(g.Labels) != 1 || g.Labels[0] != "keep" {
		t.Errorf("nil-field untouched: %+v", g)
	}
}

// TestBulkPatch_ForeignIDRejected proves the ownership pre-check: a foreign
// ID aborts the transaction so no row is mutated.
func TestBulkPatch_ForeignIDRejected(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ownerIDs := createNConvs(t, s, "owner@x.com", 1)
	_ = createNConvs(t, s, "intruder@x.com", 1)

	// Seed an intruder conversation so we can grab its ID directly.
	intruderConv, _ := s.CreateConversation(ctx, "intruder@x.com", "", "victoria", "", false)

	req := []string{ownerIDs[0], intruderConv.ID}
	pin := true
	n, err := s.BulkPatch(ctx, "owner@x.com", req, &pin, nil, nil)
	if !errors.Is(err, ErrForeignConversation) {
		t.Fatalf("err = %v, want ErrForeignConversation", err)
	}
	if n != 0 {
		t.Errorf("updated = %d, want 0 (transaction rolled back)", n)
	}
	// Owner row must be untouched (pinned still false).
	g, _ := s.Get(ctx, "owner@x.com", ownerIDs[0])
	if g == nil || g.Pinned {
		t.Errorf("owner row mutated by rejected BulkPatch: %+v", g)
	}
}

// TestDeleteAllMatching_FolderLabel proves filter-based bulk delete respects the
// optional folder/label params and leaves non-matching rows alone.
func TestDeleteAllMatching_FolderLabel(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	mk := func(folder string, labels []string) string {
		c, _ := s.CreateConversation(ctx, "u@x.com", "", "victoria", "", false)
		_, _ = s.db.ExecContext(ctx,
			`UPDATE conversations SET folder = $1, labels = $2 WHERE id = $3`,
			folder, labels, c.ID)
		return c.ID
	}
	keep := mk("", nil)
	inFolder := mk("Old work", nil)
	tagged := mk("", []string{"archived"})
	taggedInFolder := mk("Old work", []string{"archived"})

	// Delete everything in the "Old work" folder tagged "archived".
	n, err := s.DeleteAllMatching(ctx, "u@x.com", "Old work", "archived")
	if err != nil {
		t.Fatalf("DeleteAllMatching: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted = %d, want 1 (only the tagged-in-folder row)", n)
	}
	// The other three survive.
	for _, id := range []string{keep, inFolder, tagged} {
		if g, _ := s.Get(ctx, "u@x.com", id); g == nil {
			t.Errorf("non-matching conversation %s was deleted", id)
		}
	}
	// The matched row is gone.
	if g, _ := s.Get(ctx, "u@x.com", taggedInFolder); g != nil {
		t.Errorf("matched conversation %s survived", taggedInFolder)
	}
}

// TestSweepExpired_SoftDeletePurge proves the two-phase soft-delete contract:
// TTL sweep tombstones (not hard-deletes), and the 30-day purge step
// permanently removes rows whose deleted_at fell out of window.
func TestSweepExpired_SoftDeletePurge(t *testing.T) {
	s := newTestStore(t)
	s.SetSoftDelete(true)
	ctx := context.Background()

	// A fresh conversation that's past TTL → should be tombstoned, not purged.
	fresh, _ := s.CreateConversation(ctx, "u@x.com", "", "victoria", "", false)
	backdate := time.Now().Add(-30 * 24 * time.Hour).Unix()
	_, _ = s.db.ExecContext(ctx, `UPDATE conversations SET updated_at = $1 WHERE id = $2`, backdate, fresh.ID)

	// A conversation tombstoned >30 days ago → should be permanently purged.
	old, _ := s.CreateConversation(ctx, "u@x.com", "", "victoria", "", false)
	_, _ = s.db.ExecContext(ctx,
		`UPDATE conversations SET deleted_at = NOW() - INTERVAL '31 days' WHERE id = $1`, old.ID)

	expired, _, err := s.SweepExpired(ctx, 14*24*time.Hour, 100)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	// expired counts both the tombstoned fresh row (1) and the purged old row (1).
	if expired != 2 {
		t.Errorf("expired = %d, want 2 (1 tombstoned + 1 purged)", expired)
	}

	// The fresh row is now tombstoned (still in DB, hidden from reads).
	var freshTomb *time.Time
	_ = s.db.QueryRowContext(ctx, `SELECT deleted_at FROM conversations WHERE id = $1`, fresh.ID).Scan(&freshTomb)
	if freshTomb == nil {
		t.Error("fresh conversation was not tombstoned by the TTL sweep")
	}
	// The old row is permanently gone.
	var n int
	_ = s.db.QueryRowContext(ctx, `SELECT count(*) FROM conversations WHERE id = $1`, old.ID).Scan(&n)
	if n != 0 {
		t.Error("soft-deleted conversation older than 30 days was not purged")
	}
}
