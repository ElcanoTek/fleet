package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
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
	if _, err := s.SetSidebarLayout(ctx, me, []byte(first), 0); err != nil {
		t.Fatal(err)
	}
	got, err := s.SidebarLayout(ctx, "rail@elcanotek.com")
	if err != nil {
		t.Fatal(err)
	}
	jsonEqual(t, got, first)

	second := `{"version":1,"items":[],"sections":[],"home":"","compact":true}`
	if _, err := s.SetSidebarLayout(ctx, "rail@elcanotek.com", []byte(second), 0); err != nil {
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
	if _, err := s.SetSidebarLayout(ctx, "", []byte(first), 0); err == nil {
		t.Error("saved a layout for an empty email")
	}

	if _, err := s.DeleteSidebarLayout(ctx, me, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SidebarLayout(ctx, me); !errors.Is(err, ErrSidebarLayoutNotFound) {
		t.Errorf("after delete err = %v, want not found", err)
	}
	if _, err := s.DeleteSidebarLayout(ctx, me, 0); err != nil {
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
	if _, err := s.SetSidebarLayout(ctx, me, layout, 0); err != nil {
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

// TestSidebarLayout_RevOrdersSaves: numbered saves apply only when newer than
// the stored one, so a request that overlaps a later one (a save still in
// flight when the page unloads and sends the next) can never win; a reset is
// ordered the same way and an older save cannot undo it.
func TestSidebarLayout_RevOrdersSaves(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()
	const me = "order@elcanotek.com"
	older := `{"version":1,"items":[],"sections":[],"home":"older"}`
	newer := `{"version":1,"items":[],"sections":[],"home":"newer"}`

	read := func() string {
		t.Helper()
		raw, err := s.SidebarLayout(ctx, me)
		if errors.Is(err, ErrSidebarLayoutNotFound) {
			return "<none>"
		}
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	apply := func(layout string, rev int64) bool {
		t.Helper()
		ok, err := s.SetSidebarLayout(ctx, me, []byte(layout), rev)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}

	if !apply(newer, 20) {
		t.Fatal("first save not applied")
	}
	if apply(older, 10) {
		t.Error("an older save was applied over a newer one")
	}
	if !strings.Contains(read(), `"newer"`) {
		t.Errorf("after a late older save, layout = %s; want the newer one", read())
	}
	if apply(older, 20) {
		t.Error("an equal rev was applied")
	}

	// A numbered reset wins over an older save that lands after it.
	ok, err := s.DeleteSidebarLayout(ctx, me, 30)
	if err != nil || !ok {
		t.Fatalf("reset = %v, %v", ok, err)
	}
	if got := read(); got != "<none>" {
		t.Errorf("after reset, layout = %s; want none", got)
	}
	// The layout and its revision come from one read: a reset is no layout
	// with the reset's revision.
	if raw, rev, err := s.SidebarLayoutWithRev(ctx, me); err != nil || raw != nil || rev != 30 {
		t.Errorf("SidebarLayoutWithRev after reset = %s, %d, %v; want nil, 30", raw, rev, err)
	}
	if apply(older, 25) {
		t.Error("an older save brought the layout back after a reset")
	}
	if got := read(); got != "<none>" {
		t.Errorf("after a late save, layout = %s; want none", got)
	}
	// A newer save after the reset applies.
	if !apply(newer, 40) || !strings.Contains(read(), `"newer"`) {
		t.Errorf("a save after the reset was not applied: %s", read())
	}
	if raw, rev, err := s.SidebarLayoutWithRev(ctx, me); err != nil || !strings.Contains(string(raw), `"newer"`) || rev != 40 {
		t.Errorf("SidebarLayoutWithRev = %s, %d, %v; want the newer layout at 40", raw, rev, err)
	}
	if raw, rev, err := s.SidebarLayoutWithRev(ctx, "nobody@elcanotek.com"); err != nil || raw != nil || rev != 0 {
		t.Errorf("SidebarLayoutWithRev(no row) = %s, %d, %v", raw, rev, err)
	}
	// Unnumbered (rev 0) always applies, as before.
	if !apply(older, 0) || !strings.Contains(read(), `"older"`) {
		t.Errorf("an unnumbered save was not applied: %s", read())
	}
}
