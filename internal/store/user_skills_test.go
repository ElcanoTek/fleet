package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// User-skill CRUD: validation, per-user name uniqueness, ownership scoping,
// status transitions, and the rendered SKILL.md contract.
func TestUserSkills(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	seedAccount(t, s, "me@elcano.com", "other@elcano.com")
	ctx := context.Background()
	const me = "me@elcano.com"
	const other = "other@elcano.com"

	sk, err := s.CreateUserSkill(ctx, me, "deal-check", "verify a deal sheet before sending it out to a client", "1. Open the sheet.\n2. Check totals.")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sk.Status != UserSkillStatusActive {
		t.Errorf("status = %q", sk.Status)
	}

	for _, bad := range []struct{ name, desc, body string }{
		{"Bad Name", "d", "b"},
		{"ok-name", "", "b"},
		{"ok-name", "two\nlines", "b"},
		{"ok-name", "d", ""},
		{strings.Repeat("x", 70), "d", "b"},
	} {
		if _, err := s.CreateUserSkill(ctx, me, bad.name, bad.desc, bad.body); !errors.Is(err, ErrUserSkillInvalid) {
			t.Errorf("bad skill %+v accepted (err=%v)", bad, err)
		}
	}
	if _, err := s.CreateUserSkill(ctx, me, "deal-check", "duplicate name for the same user must be refused", "b"); !errors.Is(err, ErrUserSkillInvalid) {
		t.Errorf("duplicate name accepted: %v", err)
	}
	// A different user can reuse the name (per-user namespace). Cleaned up via
	// t.Cleanup — CI runs the suite twice (plain + race lane) against one
	// database, so a leaked row turns the second run's insert into a unique
	// violation.
	otherSk, err := s.CreateUserSkill(ctx, other, "deal-check", "same name, different user, different skill entirely", "b")
	if err != nil {
		t.Errorf("cross-user name reuse should be fine: %v", err)
	} else {
		t.Cleanup(func() { _ = s.DeleteUserSkill(context.Background(), other, otherSk.ID) })
	}

	// Ownership scoping.
	if _, err := s.GetUserSkill(ctx, other, sk.ID); !errors.Is(err, ErrUserSkillNotFound) {
		t.Errorf("foreign get should be not-found, got %v", err)
	}
	if err := s.DeleteUserSkill(ctx, other, sk.ID); !errors.Is(err, ErrUserSkillNotFound) {
		t.Errorf("foreign delete should be not-found, got %v", err)
	}

	// Update incl. disable.
	up, err := s.UpdateUserSkill(ctx, me, sk.ID, "deal-check-v2", "the second revision of the deal checking skill", "New body.", UserSkillStatusDisabled)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if up.Name != "deal-check-v2" || up.Status != UserSkillStatusDisabled {
		t.Errorf("update lost fields: %+v", up)
	}

	list, err := s.ListUserSkills(ctx, me)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %+v", err, list)
	}

	// Rendered markdown parses back under the bundle skill contract shape.
	md := RenderUserSkillMarkdown(up)
	if !strings.HasPrefix(md, "---\nname: deal-check-v2\ndescription: the second revision") || !strings.HasSuffix(md, "New body.\n") {
		t.Errorf("rendered SKILL.md wrong:\n%s", md)
	}

	if err := s.DeleteUserSkill(ctx, me, sk.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if list, _ := s.ListUserSkills(ctx, me); len(list) != 0 {
		t.Errorf("delete left rows: %+v", list)
	}
}

// Agent proposals stage inert and activate only on the owner's approval.
func TestUserSkillProposalFlow(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	ctx := context.Background()
	const me = "proposer@elcano.com"
	seedAccount(t, s, me) // a proposal is written only for an existing account

	prop, err := s.CreateUserSkillProposal(ctx, me, "weekly-pacing", "compile the weekly pacing report the way this user likes it", "1. Pull numbers.\n2. Format.")
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	t.Cleanup(func() { _ = s.DeleteUserSkill(context.Background(), me, prop.ID) })
	if prop.Status != UserSkillStatusProposed {
		t.Fatalf("status = %q, want proposed", prop.Status)
	}
	// Inert while proposed.
	active, err := s.ListActiveUserSkills(ctx, me)
	if err != nil || len(active) != 0 {
		t.Fatalf("proposed skill leaked into the active set: %v %+v", err, active)
	}
	// Approve = owner sets it active through the ordinary update path.
	up, err := s.UpdateUserSkill(ctx, me, prop.ID, prop.Name, prop.Description, prop.Body, UserSkillStatusActive)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if up.Status != UserSkillStatusActive {
		t.Errorf("status = %q", up.Status)
	}
	if active, _ := s.ListActiveUserSkills(ctx, me); len(active) != 1 {
		t.Errorf("approved skill missing from active set: %+v", active)
	}
	// Proposing an edit to an existing ACTIVE skill stages "<name>-proposed" so
	// the active skill stays live while the proposal awaits review.
	editedProp, err := s.CreateUserSkillProposal(ctx, me, "weekly-pacing", "updated weekly pacing workflow", "1. Pull numbers.\n2. Format.\n3. Verify.")
	if err != nil {
		t.Fatalf("propose edit to existing skill: %v", err)
	}
	t.Cleanup(func() { _ = s.DeleteUserSkill(context.Background(), me, editedProp.ID) })
	if editedProp.Name != "weekly-pacing-proposed" || editedProp.Status != UserSkillStatusProposed || !strings.Contains(editedProp.Body, "3. Verify.") {
		t.Errorf("edited proposal = %+v", editedProp)
	}
	if active, _ := s.ListActiveUserSkills(ctx, me); len(active) != 1 || active[0].Name != "weekly-pacing" {
		t.Errorf("original active skill must remain active while edit proposal is pending: %+v", active)
	}
	// Approving the staged "<name>-proposed" revision atomically updates the
	// base skill and removes the staging row in one transaction.
	merged, err := s.UpdateUserSkill(ctx, me, editedProp.ID, "weekly-pacing", editedProp.Description, editedProp.Body, UserSkillStatusActive)
	if err != nil {
		t.Fatalf("approve staged revision: %v", err)
	}
	if merged.ID != prop.ID || merged.Name != "weekly-pacing" || !strings.Contains(merged.Body, "3. Verify.") {
		t.Errorf("merged revision = %+v, want updated base skill %s", merged, prop.ID)
	}
	if all, _ := s.ListUserSkills(ctx, me); len(all) != 1 {
		t.Errorf("staging row was not removed on approval: %+v", all)
	}
	// Long skill names (56–64 chars) keep their full base name across staging
	// and approval without truncation.
	longName := strings.Repeat("a", 60)
	longSkill, err := s.CreateUserSkill(ctx, me, longName, "sixty char skill", "v1 body")
	if err != nil {
		t.Fatalf("create 60-char skill: %v", err)
	}
	t.Cleanup(func() { _ = s.DeleteUserSkill(context.Background(), me, longSkill.ID) })
	longProp, err := s.CreateUserSkillProposal(ctx, me, longName, "sixty char skill v2", "v2 body")
	if err != nil {
		t.Fatalf("propose edit to 60-char skill: %v", err)
	}
	t.Cleanup(func() { _ = s.DeleteUserSkill(context.Background(), me, longProp.ID) })
	if longProp.Name != longName+"-proposed" {
		t.Fatalf("longProp.Name = %q, want %q", longProp.Name, longName+"-proposed")
	}
	mergedLong, err := s.UpdateUserSkill(ctx, me, longProp.ID, longName, longProp.Description, longProp.Body, UserSkillStatusActive)
	if err != nil {
		t.Fatalf("approve 60-char revision: %v", err)
	}
	if mergedLong.ID != longSkill.ID || mergedLong.Name != longName || mergedLong.Body != "v2 body" {
		t.Errorf("mergedLong = %+v, want updated 60-char skill", mergedLong)
	}
	// Nothing can move it BACK to proposed through UpdateUserSkill.
	if _, err := s.UpdateUserSkill(ctx, me, prop.ID, prop.Name, prop.Description, prop.Body, UserSkillStatusProposed); !errors.Is(err, ErrUserSkillInvalid) {
		t.Errorf("re-proposing should be invalid, got %v", err)
	}
}

func TestUserSkillsWorkspaceSharing(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	seedAccount(t, s, "author@elcano.com")
	ctx := context.Background()
	const author = "author@elcano.com"
	const teammate = "teammate@elcano.com"

	shared, err := s.CreateUserSkillWithVisibility(
		ctx, author, "shared-opt-log", "shared optimization log workflow", "1. Load config.", UserSkillVisibilityWorkspace,
	)
	if err != nil {
		t.Fatalf("create shared: %v", err)
	}
	t.Cleanup(func() { _ = s.DeleteUserSkill(context.Background(), author, shared.ID) })

	if shared.Visibility != UserSkillVisibilityWorkspace {
		t.Fatalf("visibility = %q, want workspace", shared.Visibility)
	}
	wsList, err := s.ListWorkspaceSharedUserSkills(ctx)
	if err != nil {
		t.Fatalf("list workspace shared: %v", err)
	}
	found := false
	for _, sk := range wsList {
		if sk.ID == shared.ID && sk.SharedBy == author {
			found = true
		}
	}
	if !found {
		t.Fatalf("shared skill missing from ListWorkspaceSharedUserSkills: %+v", wsList)
	}

	// Teammate's ListActiveUserSkills does NOT auto-inject a teammate's shared
	// skill before explicit recipient activation ("Add to my skills").
	teammateActive, err := s.ListActiveUserSkills(ctx, teammate)
	if err != nil {
		t.Fatalf("teammate active: %v", err)
	}
	for _, sk := range teammateActive {
		if sk.Name == "shared-opt-log" {
			t.Fatalf("unconsented shared skill must not appear in teammate ListActiveUserSkills: %+v", teammateActive)
		}
	}

	// Unsharing removes it from workspace-shared list.
	unshared, err := s.UpdateUserSkillWithVisibility(
		ctx, author, shared.ID, shared.Name, shared.Description, shared.Body, UserSkillStatusActive, UserSkillVisibilityPrivate,
	)
	if err != nil {
		t.Fatalf("unshare: %v", err)
	}
	if unshared.Visibility != UserSkillVisibilityPrivate {
		t.Fatalf("visibility after unshare = %q", unshared.Visibility)
	}
}

// enabled_for_all: only an active workspace-shared skill can be switched on,
// the listing returns exactly those rows (across owners, newest first within a
// name), an ordinary edit keeps the flag, and it is cleared the moment the
// row stops being workspace-shared.
func TestUserSkillsEnabledForAll(t *testing.T) {
	t.Parallel()

	s := newTestStore(t)
	seedAccount(t, s, "admin@elcano.com", "second@elcano.com")
	ctx := context.Background()
	const admin = "admin@elcano.com"
	const second = "second@elcano.com"

	private, err := s.CreateUserSkill(ctx, admin, "private-notes", "an admin's private skill", "Notes.")
	if err != nil {
		t.Fatalf("create private: %v", err)
	}
	if _, err := s.SetUserSkillEnabledForAll(ctx, admin, private.ID, true); !errors.Is(err, ErrUserSkillInvalid) {
		t.Fatalf("private skill on for everyone: err = %v, want ErrUserSkillInvalid", err)
	}
	if _, err := s.SetUserSkillEnabledForAll(ctx, second, private.ID, true); !errors.Is(err, ErrUserSkillNotFound) {
		t.Fatalf("another user's skill: err = %v, want ErrUserSkillNotFound", err)
	}

	shared, err := s.CreateUserSkillWithVisibility(ctx, admin, "deal-check", "verify a deal sheet", "Check the totals.", UserSkillVisibilityWorkspace)
	if err != nil {
		t.Fatalf("create shared: %v", err)
	}
	if shared.EnabledForAll {
		t.Fatal("a new shared skill is on for everyone by default")
	}
	if got, _ := s.ListEnabledForAllUserSkills(ctx); len(got) != 0 {
		t.Fatalf("listing before anything is on = %+v", got)
	}
	on, err := s.SetUserSkillEnabledForAll(ctx, admin, shared.ID, true)
	if err != nil || !on.EnabledForAll {
		t.Fatalf("switch on: %+v, %v", on, err)
	}
	got, err := s.ListEnabledForAllUserSkills(ctx)
	if err != nil || len(got) != 1 || got[0].ID != shared.ID || got[0].SharedBy != admin {
		t.Fatalf("listing = %+v, %v", got, err)
	}
	// The shared catalog carries the flag too.
	if ws, _ := s.ListWorkspaceSharedUserSkills(ctx); len(ws) != 1 || !ws[0].EnabledForAll {
		t.Fatalf("workspace listing = %+v", ws)
	}

	// A second owner's skill of the same name is listed too; the catalog's
	// one-per-name pick prefers the one that is on for everyone even though
	// the other is newer.
	other, err := s.CreateUserSkillWithVisibility(ctx, second, "deal-check", "another deal check", "Other.", UserSkillVisibilityWorkspace)
	if err != nil {
		t.Fatalf("create second: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE user_skills SET updated_at = updated_at + 100 WHERE id = $1`, other.ID); err != nil {
		t.Fatal(err)
	}
	if ws, _ := s.ListWorkspaceSharedUserSkills(ctx); len(ws) != 1 || ws[0].ID != shared.ID {
		t.Fatalf("workspace listing with a newer plain share = %+v, want the one on for everyone", ws)
	}
	if _, err := s.SetUserSkillEnabledForAll(ctx, second, other.ID, true); err != nil {
		t.Fatalf("second switch on: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE user_skills SET updated_at = updated_at + 100 WHERE id = $1`, other.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListEnabledForAllUserSkills(ctx); len(got) != 2 || got[0].ID != other.ID || got[1].ID != shared.ID {
		t.Fatalf("listing with two owners = %+v, want the newer first", got)
	}
	if err := s.DeleteUserSkill(ctx, second, other.ID); err != nil {
		t.Fatal(err)
	}

	// An edit that does not touch visibility keeps it on.
	edited, err := s.UpdateUserSkill(ctx, admin, shared.ID, "deal-check", "verify a deal sheet", "Check the totals twice.", UserSkillStatusActive)
	if err != nil || !edited.EnabledForAll {
		t.Fatalf("edit: %+v, %v", edited, err)
	}
	// Disabled: the flag stays on the row, the listing drops it, and enabling resumes it.
	if _, err := s.UpdateUserSkill(ctx, admin, shared.ID, "deal-check", "verify a deal sheet", "Check the totals twice.", UserSkillStatusDisabled); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListEnabledForAllUserSkills(ctx); len(got) != 0 {
		t.Fatalf("a disabled skill is listed: %+v", got)
	}
	if _, err := s.UpdateUserSkill(ctx, admin, shared.ID, "deal-check", "verify a deal sheet", "Check the totals twice.", UserSkillStatusActive); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListEnabledForAllUserSkills(ctx); len(got) != 1 {
		t.Fatalf("re-enabled skill not listed: %+v", got)
	}

	// Made private: the flag is cleared, so sharing it again does not put it
	// back in everyone's turns by itself.
	unshared, err := s.UpdateUserSkillWithVisibility(ctx, admin, shared.ID, "deal-check", "verify a deal sheet", "Check the totals twice.", UserSkillStatusActive, UserSkillVisibilityPrivate)
	if err != nil || unshared.EnabledForAll {
		t.Fatalf("unshare: %+v, %v", unshared, err)
	}
	reshared, err := s.UpdateUserSkillWithVisibility(ctx, admin, shared.ID, "deal-check", "verify a deal sheet", "Check the totals twice.", UserSkillStatusActive, UserSkillVisibilityWorkspace)
	if err != nil || reshared.EnabledForAll {
		t.Fatalf("reshare: %+v, %v", reshared, err)
	}
	if got, _ := s.ListEnabledForAllUserSkills(ctx); len(got) != 0 {
		t.Fatalf("listing after unshare and reshare = %+v", got)
	}

	// Switching off needs no particular state.
	if off, err := s.SetUserSkillEnabledForAll(ctx, admin, private.ID, false); err != nil || off.EnabledForAll {
		t.Fatalf("switch off a private skill: %+v, %v", off, err)
	}
}
