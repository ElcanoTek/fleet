package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// skillsFakeStore adds an in-memory user_skills table to fakeChatStore so the
// materialization + invocation paths run without a database.
type skillsFakeStore struct {
	*fakeChatStore
	skills []store.UserSkill
}

func (s *skillsFakeStore) ListUserSkills(_ context.Context, _ string) ([]store.UserSkill, error) {
	return s.skills, nil
}

// Materialization mirrors the DB into the conversation workspace: active
// skills appear (and render valid SKILL.md), disabled ones don't, and stale
// dirs are cleaned up on the next turn.
func TestMaterializeUserSkills(t *testing.T) {
	t.Setenv("FLEET_WORKSPACE_ROOT", t.TempDir())
	fs := &skillsFakeStore{fakeChatStore: newFakeChatStore(), skills: []store.UserSkill{
		{ID: "1", Name: "deal-check", Description: "verify a deal sheet before it goes out", Body: "Check the totals.", Status: store.UserSkillStatusActive},
		{ID: "2", Name: "old-skill", Description: "a disabled skill that must not materialize", Body: "x", Status: store.UserSkillStatusDisabled},
	}}
	s := &Server{store: fs}

	entries := s.materializeUserSkills(context.Background(), "u@x.com", "conv-skills-1")
	if len(entries) != 1 || entries[0].Name != "deal-check" || entries[0].Path != "user-skills/deal-check/SKILL.md" {
		t.Fatalf("entries = %+v", entries)
	}
	wsDir, _ := tools.EnsureWorkspaceDir("conv-skills-1")
	md, err := os.ReadFile(filepath.Join(wsDir, "user-skills", "deal-check", "SKILL.md"))
	if err != nil {
		t.Fatalf("materialized file: %v", err)
	}
	if !strings.Contains(string(md), "name: deal-check") || !strings.Contains(string(md), "Check the totals.") {
		t.Errorf("SKILL.md content wrong:\n%s", md)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "user-skills", "old-skill")); !os.IsNotExist(err) {
		t.Error("disabled skill materialized")
	}

	// Disable it: the stale dir is removed on the next materialization.
	fs.skills[0].Status = store.UserSkillStatusDisabled
	if entries := s.materializeUserSkills(context.Background(), "u@x.com", "conv-skills-1"); len(entries) != 0 {
		t.Fatalf("disabled skill still in roster: %+v", entries)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "user-skills", "deal-check")); !os.IsNotExist(err) {
		t.Error("stale materialization not cleaned up")
	}
}

// "/name" invocation matches the caller's ACTIVE skills only, and points at
// the workspace path.
func TestMatchUserSkillInvocation(t *testing.T) {
	skills := []store.UserSkill{
		{Name: "deal-check", Status: store.UserSkillStatusActive},
		{Name: "sleepy", Status: store.UserSkillStatusDisabled},
	}
	if block := matchUserSkillInvocation("/deal-check the acme sheet", skills); !strings.Contains(block, "user-skills/deal-check/SKILL.md") {
		t.Errorf("no match: %q", block)
	}
	if block := matchUserSkillInvocation("/sleepy", skills); block != "" {
		t.Errorf("disabled skill matched: %q", block)
	}
	if block := matchUserSkillInvocation("/unknown", skills); block != "" {
		t.Errorf("unknown matched: %q", block)
	}
}
