package store

import (
	"context"
	"testing"
)

// Workspace feature settings (migration 035). Load-bearing assertions: upsert
// semantics (a second Set replaces value + attribution), delete-is-reset (and
// idempotent), and the map read returns exactly the override rows.
func TestWorkspaceSettingsCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	got, err := s.WorkspaceSettings(ctx)
	if err != nil {
		t.Fatalf("list empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("fresh table should have no overrides, got %v", got)
	}

	if err := s.SetWorkspaceSetting(ctx, "pii_redaction_mode", "redact", "admin@x.com"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := s.SetWorkspaceSetting(ctx, "subagents_enabled", "true", "admin@x.com"); err != nil {
		t.Fatalf("set second: %v", err)
	}
	// Upsert: same key again replaces value and attribution.
	if err := s.SetWorkspaceSetting(ctx, "pii_redaction_mode", "block", "other@x.com"); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err = s.WorkspaceSettings(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 overrides, got %d: %v", len(got), got)
	}
	pii := got["pii_redaction_mode"]
	if pii.Value != "block" || pii.UpdatedBy != "other@x.com" || pii.UpdatedAt == 0 {
		t.Errorf("upserted row = %+v", pii)
	}

	if err := s.DeleteWorkspaceSetting(ctx, "pii_redaction_mode"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Deleting an absent key is a no-op, not an error.
	if err := s.DeleteWorkspaceSetting(ctx, "pii_redaction_mode"); err != nil {
		t.Fatalf("delete absent: %v", err)
	}
	got, err = s.WorkspaceSettings(ctx)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(got) != 1 || got["subagents_enabled"].Value != "true" {
		t.Errorf("after delete = %v, want only subagents_enabled", got)
	}
}
