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
	s := newTestStore(t)
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
	s := newTestStore(t)
	ctx := context.Background()
	const me = "proposer@elcano.com"

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
	// Nothing can move it BACK to proposed.
	if _, err := s.UpdateUserSkill(ctx, me, prop.ID, prop.Name, prop.Description, prop.Body, UserSkillStatusProposed); !errors.Is(err, ErrUserSkillInvalid) {
		t.Errorf("re-proposing should be invalid, got %v", err)
	}
}
