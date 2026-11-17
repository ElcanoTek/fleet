package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// jsonEqual compares two JSON documents by value (JSONB re-orders keys).
func jsonEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("stored layout is not JSON: %v (%s)", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("layout = %s, want %s", got, want)
	}
}

// TestSidebarLayout_User pins the per-user layout row: absent until saved
// (ErrSidebarLayoutNotFound), keyed by the lower-cased email, replaced by a
// second save, kept apart from other users, and gone after a delete (which is
// idempotent).
func TestSidebarLayout_User(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()
	const me = "Rail@Elcanotek.com"

	if _, err := s.SidebarLayout(ctx, me); !errors.Is(err, ErrSidebarLayoutNotFound) {
		t.Fatalf("fresh layout err = %v, want ErrSidebarLayoutNotFound", err)
	}
	first := `{"version":1,"items":[{"id":"chat","kind":"builtin"}],"sections":[],"home":""}`
	if err := s.SetSidebarLayout(ctx, me, []byte(first)); err != nil {
		t.Fatal(err)
	}
	got, err := s.SidebarLayout(ctx, "rail@elcanotek.com")
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, got, first)

	second := `{"version":1,"items":[],"sections":[],"home":"","compact":true}`
	if err := s.SetSidebarLayout(ctx, "rail@elcanotek.com", []byte(second)); err != nil {
		t.Fatal(err)
	}
	got, err = s.SidebarLayout(ctx, me)
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, got, second)
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM chat.sidebar_layouts`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows = %d, %v; want one row for the lower-cased email", n, err)
	}

	if _, err := s.SidebarLayout(ctx, "someone@elcanotek.com"); !errors.Is(err, ErrSidebarLayoutNotFound) {
		t.Errorf("other user's layout err = %v, want not found", err)
	}
	if err := s.SetSidebarLayout(ctx, "", []byte(first)); err == nil {
		t.Error("saved a layout for an empty email")
	}

	if err := s.DeleteSidebarLayout(ctx, me); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SidebarLayout(ctx, me); !errors.Is(err, ErrSidebarLayoutNotFound) {
		t.Errorf("after delete err = %v, want not found", err)
	}
	if err := s.DeleteSidebarLayout(ctx, me); err != nil {
		t.Errorf("second delete = %v, want nil", err)
	}
}

// TestSidebarLayout_Workspace pins the singleton workspace default: absent
// until saved, replaced (never duplicated) by a second save with its author
// and time, and removed by a reset.
func TestSidebarLayout_Workspace(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.WorkspaceSidebarLayout(ctx); !errors.Is(err, ErrSidebarLayoutNotFound) {
		t.Fatalf("fresh workspace layout err = %v, want not found", err)
	}
	first := `{"version":1,"items":[],"sections":[],"home":""}`
	saved, err := s.SetWorkspaceSidebarLayout(ctx, []byte(first), "Admin@X.com")
	if err != nil {
		t.Fatal(err)
	}
	if saved.UpdatedBy != "admin@x.com" || saved.UpdatedAt == 0 {
		t.Errorf("saved = %+v", saved)
	}
	second := `{"version":1,"items":[],"sections":[],"home":"","compact":true}`
	if _, err := s.SetWorkspaceSidebarLayout(ctx, []byte(second), "other@x.com"); err != nil {
		t.Fatal(err)
	}
	got, err := s.WorkspaceSidebarLayout(ctx)
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, got.Layout, second)
	if got.UpdatedBy != "other@x.com" || got.UpdatedAt < saved.UpdatedAt {
		t.Errorf("row = %+v", got)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM chat.workspace_sidebar_layout`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows = %d, %v; want the singleton", n, err)
	}

	if err := s.DeleteWorkspaceSidebarLayout(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WorkspaceSidebarLayout(ctx); !errors.Is(err, ErrSidebarLayoutNotFound) {
		t.Errorf("after reset err = %v, want not found", err)
	}
	if err := s.DeleteWorkspaceSidebarLayout(ctx); err != nil {
		t.Errorf("second reset = %v, want nil", err)
	}
}

// TestSidebarLayout_DeleteUserRemovesIt: deleting the account deletes its
// saved layout, and leaves the workspace default alone.
func TestSidebarLayout_DeleteUserRemovesIt(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()
	const me = "leaver@elcanotek.com"
	if _, err := s.CreateUser(ctx, me, "correct-horse-battery"); err != nil {
		t.Fatal(err)
	}
	layout := []byte(`{"version":1,"items":[],"sections":[],"home":""}`)
	if err := s.SetSidebarLayout(ctx, me, layout); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetWorkspaceSidebarLayout(ctx, layout, me); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser(ctx, "Leaver@Elcanotek.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SidebarLayout(ctx, me); !errors.Is(err, ErrSidebarLayoutNotFound) {
		t.Errorf("layout after DeleteUser err = %v, want not found", err)
	}
	if _, err := s.WorkspaceSidebarLayout(ctx); err != nil {
		t.Errorf("workspace default after DeleteUser: %v", err)
	}
}
