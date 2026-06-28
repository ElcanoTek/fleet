package store

import (
	"context"
	"testing"
	"time"
)

// backdate sets a conversation's updated_at to d in the past (helper for the
// archive/sweep tests, which all key off updated_at).
func backdate(ctx context.Context, t *testing.T, s *Store, id string, d time.Duration) {
	t.Helper()
	when := time.Now().Add(-d).Unix()
	if _, err := s.db.ExecContext(ctx, `UPDATE conversations SET updated_at = $1 WHERE id = $2`, when, id); err != nil {
		t.Fatalf("backdate: %v", err)
	}
}

// TestSetArchived_HidesFromListAndUnpins pins a conversation, archives it, and
// asserts it leaves the active list, joins the archived list, and is unpinned
// (#282: pinned and archived are mutually exclusive "keep" states).
func TestSetArchived_HidesFromListAndUnpins(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	c, _ := s.CreateConversation(ctx, "u@x.com", "to archive", "victoria", "", false)
	if err := s.SetPinned(ctx, "u@x.com", c.ID, true); err != nil {
		t.Fatalf("SetPinned: %v", err)
	}

	if err := s.SetArchived(ctx, "u@x.com", c.ID, true); err != nil {
		t.Fatalf("SetArchived: %v", err)
	}

	active, _ := s.List(ctx, "u@x.com", false)
	if len(active) != 0 {
		t.Errorf("archived conversation still in active list: %d", len(active))
	}
	archived, _ := s.List(ctx, "u@x.com", true)
	if len(archived) != 1 || archived[0].ID != c.ID {
		t.Fatalf("archived list: got %+v", archived)
	}
	if archived[0].ArchivedAt == nil {
		t.Error("archived conversation should have a non-nil ArchivedAt")
	}
	if archived[0].Pinned {
		t.Error("archiving should have cleared the pin")
	}

	// Unarchive restores it to the active list with a nil ArchivedAt.
	if err := s.SetArchived(ctx, "u@x.com", c.ID, false); err != nil {
		t.Fatalf("SetArchived(false): %v", err)
	}
	active, _ = s.List(ctx, "u@x.com", false)
	if len(active) != 1 || active[0].ArchivedAt != nil {
		t.Errorf("unarchive should restore to active with nil ArchivedAt: %+v", active)
	}
}

// TestSetArchived_WrongUser is a no-op-and-error on a foreign conversation.
func TestSetArchived_WrongUser(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	c, _ := s.CreateConversation(ctx, "owner@x.com", "", "victoria", "", false)

	if err := s.SetArchived(ctx, "intruder@x.com", c.ID, true); err == nil {
		t.Error("archiving another user's conversation should error")
	}
	got, _ := s.Get(ctx, "owner@x.com", c.ID)
	if got == nil || got.ArchivedAt != nil {
		t.Error("foreign archive must not have touched the row")
	}
}

// TestAutoArchiveOlderThan files away stale unpinned conversations while leaving
// pinned, recent, and already-archived ones untouched; a non-positive duration
// is a no-op.
func TestAutoArchiveOlderThan(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	stale, _ := s.CreateConversation(ctx, "u@x.com", "stale", "victoria", "", false)
	pinnedStale, _ := s.CreateConversation(ctx, "u@x.com", "pinned-stale", "victoria", "", false)
	recent, _ := s.CreateConversation(ctx, "u@x.com", "recent", "victoria", "", false)

	_ = s.SetPinned(ctx, "u@x.com", pinnedStale.ID, true)
	// Backdate AFTER the pin toggle (SetPinned bumps updated_at).
	backdate(ctx, t, s, stale.ID, 40*24*time.Hour)
	backdate(ctx, t, s, pinnedStale.ID, 40*24*time.Hour)
	// recent keeps its fresh updated_at.

	n, err := s.AutoArchiveOlderThan(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatalf("AutoArchiveOlderThan: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly the stale unpinned one archived, got %d", n)
	}

	if got, _ := s.Get(ctx, "u@x.com", stale.ID); got == nil || got.ArchivedAt == nil {
		t.Error("stale unpinned conversation should have been archived")
	}
	if got, _ := s.Get(ctx, "u@x.com", pinnedStale.ID); got == nil || got.ArchivedAt != nil {
		t.Error("pinned conversation must never be auto-archived")
	}
	if got, _ := s.Get(ctx, "u@x.com", recent.ID); got == nil || got.ArchivedAt != nil {
		t.Error("recent conversation must not be auto-archived")
	}

	// Idempotent: a second pass archives nothing new (already-archived excluded).
	if n, _ := s.AutoArchiveOlderThan(ctx, 30*24*time.Hour); n != 0 {
		t.Errorf("second pass should archive nothing, got %d", n)
	}
	// Non-positive duration disables the feature.
	if n, err := s.AutoArchiveOlderThan(ctx, 0); err != nil || n != 0 {
		t.Errorf("duration<=0 must be a no-op, got (%d, %v)", n, err)
	}
}

// TestSweep_ArchivedExemptFromTTL proves an archived conversation is NOT
// TTL-hard-deleted even when long-stale — archive is a "keep" state like pin.
func TestSweep_ArchivedExemptFromTTL(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	c, _ := s.CreateConversation(ctx, "u@x.com", "", "victoria", "", false)
	if err := s.SetArchived(ctx, "u@x.com", c.ID, true); err != nil {
		t.Fatalf("SetArchived: %v", err)
	}
	// Backdate AFTER archiving (SetArchived bumps updated_at).
	backdate(ctx, t, s, c.ID, 30*24*time.Hour)

	expired, _, err := s.SweepExpired(ctx, 14*24*time.Hour, 100)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if expired != 0 {
		t.Errorf("archived conversation was TTL-swept: %d", expired)
	}
	if got, _ := s.Get(ctx, "u@x.com", c.ID); got == nil {
		t.Error("archived conversation must survive the TTL sweep")
	}
}

// TestSweep_ArchivedExemptFromCap proves archived conversations neither count
// toward the unpinned cap nor are targeted by the cap eviction. The active
// count must EXCEED the cap so the eviction loop actually runs (4 active > cap
// 3) — exercising the `archived_at IS NULL` filter in the OFFSET-delete; if the
// archived rows leaked into either the count or the delete, the assertions below
// would catch it.
func TestSweep_ArchivedExemptFromCap(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// 4 active unpinned, oldest→newest by deterministic updated_at.
	active := make([]string, 4)
	for i := 0; i < 4; i++ {
		c, err := s.CreateConversation(ctx, "u@x.com", "active", "victoria", "", false)
		if err != nil {
			t.Fatalf("create active[%d]: %v", i, err)
		}
		active[i] = c.ID
	}
	base := time.Now().Unix()
	for i, id := range active {
		if _, err := s.db.ExecContext(ctx, `UPDATE conversations SET updated_at = $1 WHERE id = $2`, base-int64(10-i), id); err != nil {
			t.Fatalf("set updated_at: %v", err)
		}
	}
	// 3 archived (which must be invisible to the cap entirely).
	archivedIDs := make([]string, 3)
	for i := 0; i < 3; i++ {
		c, _ := s.CreateConversation(ctx, "u@x.com", "archived", "victoria", "", false)
		if err := s.SetArchived(ctx, "u@x.com", c.ID, true); err != nil {
			t.Fatalf("SetArchived: %v", err)
		}
		archivedIDs[i] = c.ID
	}

	// Cap 3: of the 4 active, the oldest (active[0]) is evicted; archived ignored.
	_, evicted, err := s.SweepExpired(ctx, 14*24*time.Hour, 3)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if evicted != 1 {
		t.Fatalf("expected exactly 1 active evicted (4 active - cap 3), got %d", evicted)
	}
	if got, _ := s.Get(ctx, "u@x.com", active[0]); got != nil {
		t.Error("the oldest active conversation should have been cap-evicted")
	}
	for i := 1; i < 4; i++ {
		if got, _ := s.Get(ctx, "u@x.com", active[i]); got == nil {
			t.Errorf("active[%d] (within cap) should have survived", i)
		}
	}
	// Every archived conversation survives and stays archived.
	for i, id := range archivedIDs {
		if got, _ := s.Get(ctx, "u@x.com", id); got == nil || got.ArchivedAt == nil {
			t.Errorf("archived[%d] must survive the cap, still archived", i)
		}
	}
}

// TestDeleteAllUnpinned_SkipsArchived proves the bulk "delete all unpinned"
// action leaves archived conversations alone (#282): the user can't see them
// when triggering it, so silently destroying them would be surprising.
func TestDeleteAllUnpinned_SkipsArchived(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	plain, _ := s.CreateConversation(ctx, "u@x.com", "plain unpinned", "victoria", "", false)
	arch, _ := s.CreateConversation(ctx, "u@x.com", "archived", "victoria", "", false)
	if err := s.SetArchived(ctx, "u@x.com", arch.ID, true); err != nil {
		t.Fatalf("SetArchived: %v", err)
	}

	n, err := s.DeleteAllUnpinned(ctx, "u@x.com")
	if err != nil {
		t.Fatalf("DeleteAllUnpinned: %v", err)
	}
	if n != 1 {
		t.Errorf("only the plain unpinned conversation should be deleted, got %d", n)
	}
	if got, _ := s.Get(ctx, "u@x.com", plain.ID); got != nil {
		t.Error("plain unpinned conversation should have been deleted")
	}
	if got, _ := s.Get(ctx, "u@x.com", arch.ID); got == nil {
		t.Error("archived conversation must survive delete-all-unpinned")
	}
}
