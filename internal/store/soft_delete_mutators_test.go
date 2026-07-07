package store

import (
	"context"
	"errors"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// TestConversationMutators_SoftDeletedNotMutable pins the #596 contract: under
// FLEET_CONVERSATION_SOFT_DELETE a tombstoned conversation is uniformly
// immutable. Every conversation setter carries the same deleted_at IS NULL
// guard SetShareToken/SetThinkingConfig always had, so each one no-ops (0 rows
// affected → "not found" / ErrTitleLocked) on a soft-deleted row while still
// working on a live one.
func TestConversationMutators_SoftDeletedNotMutable(t *testing.T) {
	s := newTestStore(t)
	s.SetSoftDelete(true)
	ctx := context.Background()
	const owner = "alice@x.com"

	live, err := s.CreateConversation(ctx, owner, "live", "victoria", "m-1", false)
	if err != nil {
		t.Fatalf("CreateConversation(live): %v", err)
	}
	dead, err := s.CreateConversation(ctx, owner, "dead", "victoria", "m-1", false)
	if err != nil {
		t.Fatalf("CreateConversation(dead): %v", err)
	}
	if err := s.Delete(ctx, owner, dead.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	seconds := 90
	mutators := []struct {
		name string
		call func(convID string) error
	}{
		{"SetModel", func(id string) error { return s.SetModel(ctx, owner, id, "m-2") }},
		{"UpdateTitle", func(id string) error { return s.UpdateTitle(ctx, owner, id, "auto") }},
		{"RenameTitle", func(id string) error { return s.RenameTitle(ctx, owner, id, "manual") }},
		{"SetPinned", func(id string) error { return s.SetPinned(ctx, owner, id, true) }},
		{"SetArchived", func(id string) error { return s.SetArchived(ctx, owner, id, true) }},
		{"SetOptionalMCPServers", func(id string) error { return s.SetOptionalMCPServers(ctx, owner, id, []string{"gamma"}) }},
		{"SetApprovalTimeout", func(id string) error { return s.SetApprovalTimeout(ctx, owner, id, &seconds) }},
	}
	for _, m := range mutators {
		if err := m.call(live.ID); err != nil {
			t.Errorf("%s(live) = %v, want success", m.name, err)
		}
		err := m.call(dead.ID)
		switch m.name {
		case "UpdateTitle":
			// The auto-titler's 0-rows signal is ErrTitleLocked (a benign skip);
			// live got its title above BEFORE RenameTitle locked it.
			if !errors.Is(err, ErrTitleLocked) {
				t.Errorf("UpdateTitle(soft-deleted) = %v, want ErrTitleLocked", err)
			}
		default:
			if err == nil || err.Error() != "conversation not found" {
				t.Errorf("%s(soft-deleted) = %v, want \"conversation not found\"", m.name, err)
			}
		}
	}

	// The tombstoned row really is untouched.
	var title, model string
	var pinned bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT title, model, pinned FROM conversations WHERE id = $1`, dead.ID,
	).Scan(&title, &model, &pinned); err != nil {
		t.Fatalf("read tombstone: %v", err)
	}
	if title != "dead" || model != "m-1" || pinned {
		t.Errorf("soft-deleted row mutated: title=%q model=%q pinned=%v", title, model, pinned)
	}
}

// TestAdminStats_ExcludesSoftDeleted pins the #579 contract: AdminStats
// filters deleted_at IS NULL like every other conversation read, so a
// tombstoned conversation inflates neither the per-user counts nor
// last_activity (Delete bumps updated_at, so an unfiltered MAX would report
// the deletion itself as recent activity).
func TestAdminStats_ExcludesSoftDeleted(t *testing.T) {
	s := newTestStore(t)
	s.SetSoftDelete(true)
	ctx := context.Background()
	const owner = "alice@x.com"

	kept, err := s.CreateConversation(ctx, owner, "kept", "victoria", "m-1", false)
	if err != nil {
		t.Fatalf("CreateConversation(kept): %v", err)
	}
	// Give the surviving conversation a KNOWN old activity timestamp so the
	// last_activity assertion can't be masked by same-second writes.
	const keptActivity = int64(1000)
	if _, err := s.db.ExecContext(ctx,
		`UPDATE conversations SET updated_at = $1 WHERE id = $2`, keptActivity, kept.ID); err != nil {
		t.Fatalf("age kept conversation: %v", err)
	}

	// A pinned conversation with fresh history, then soft-deleted: it must not
	// count, not pin-count, and not surface as activity.
	gone, err := s.CreateConversation(ctx, owner, "gone", "victoria", "m-1", false)
	if err != nil {
		t.Fatalf("CreateConversation(gone): %v", err)
	}
	if err := s.SetPinned(ctx, owner, gone.ID, true); err != nil {
		t.Fatalf("SetPinned: %v", err)
	}
	if _, err := s.AppendHistory(ctx, gone.ID, []agent.HistoryEntry{
		{Role: "user", Type: "text", Content: []byte(`{"text":"hi"}`)},
	}); err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}
	if err := s.Delete(ctx, owner, gone.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	rows, err := s.AdminStats(ctx)
	if err != nil {
		t.Fatalf("AdminStats: %v", err)
	}
	var row *AdminRow
	for i := range rows {
		if rows[i].Email == owner {
			row = &rows[i]
		}
	}
	if row == nil {
		t.Fatalf("AdminStats has no row for %s: %+v", owner, rows)
	}
	if row.ConversationCount != 1 {
		t.Errorf("ConversationCount = %d, want 1 (soft-deleted row must not count)", row.ConversationCount)
	}
	if row.PinnedCount != 0 {
		t.Errorf("PinnedCount = %d, want 0 (the pinned conversation is tombstoned)", row.PinnedCount)
	}
	if row.LastActivity != keptActivity {
		t.Errorf("LastActivity = %d, want %d (a deletion must not read as recent activity)", row.LastActivity, keptActivity)
	}
}
