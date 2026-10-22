package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	nextID int
}

func (s *skillsFakeStore) ListUserSkills(_ context.Context, email string) ([]store.UserSkill, error) {
	if email == "" {
		return s.skills, nil
	}
	var out []store.UserSkill
	for _, sk := range s.skills {
		if sk.UserEmail == "" || sk.UserEmail == email {
			out = append(out, sk)
		}
	}
	return out, nil
}

func (s *skillsFakeStore) ListWorkspaceSharedUserSkills(_ context.Context) ([]store.UserSkill, error) {
	var out []store.UserSkill
	for _, sk := range s.skills {
		if sk.Status == store.UserSkillStatusActive && sk.Visibility == store.UserSkillVisibilityWorkspace {
			cp := sk
			cp.SharedBy = sk.UserEmail
			out = append(out, cp)
		}
	}
	return out, nil
}

func (s *skillsFakeStore) CreateUserSkill(ctx context.Context, email, name, description, body string) (*store.UserSkill, error) {
	return s.CreateUserSkillWithVisibility(ctx, email, name, description, body, store.UserSkillVisibilityPrivate)
}

func (s *skillsFakeStore) CreateUserSkillWithVisibility(_ context.Context, email, name, description, body, visibility string) (*store.UserSkill, error) {
	if name == "" || (visibility != store.UserSkillVisibilityPrivate && visibility != store.UserSkillVisibilityWorkspace) {
		return nil, store.ErrUserSkillInvalid
	}
	s.nextID++
	sk := store.UserSkill{
		ID: string(rune('a' + s.nextID)), UserEmail: email, Name: name, Description: description,
		Body: body, Status: store.UserSkillStatusActive, Visibility: visibility,
	}
	s.skills = append(s.skills, sk)
	return &sk, nil
}

func (s *skillsFakeStore) UpdateUserSkill(ctx context.Context, email, id, name, description, body, status string) (*store.UserSkill, error) {
	return s.UpdateUserSkillWithVisibility(ctx, email, id, name, description, body, status, "")
}

func (s *skillsFakeStore) UpdateUserSkillWithVisibility(_ context.Context, _, id, name, description, body, status, visibility string) (*store.UserSkill, error) {
	if visibility != "" && visibility != store.UserSkillVisibilityPrivate && visibility != store.UserSkillVisibilityWorkspace {
		return nil, store.ErrUserSkillInvalid
	}
	for i := range s.skills {
		if s.skills[i].ID == id {
			s.skills[i].Name, s.skills[i].Description, s.skills[i].Body, s.skills[i].Status = name, description, body, status
			if visibility != "" {
				s.skills[i].Visibility = visibility
			}
			cp := s.skills[i]
			return &cp, nil
		}
	}
	return nil, store.ErrUserSkillNotFound
}

func (s *skillsFakeStore) DeleteUserSkill(_ context.Context, _, id string) error {
	for i := range s.skills {
		if s.skills[i].ID == id {
			s.skills = append(s.skills[:i], s.skills[i+1:]...)
			return nil
		}
	}
	return store.ErrUserSkillNotFound
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

func userSkillsReq(t *testing.T, _ *Server, handler http.HandlerFunc, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r = r.WithContext(context.WithValue(r.Context(), ctxKeyUser, "u@x.com"))
	w := httptest.NewRecorder()
	handler(w, r)
	return w
}

// The CRUD endpoints round-trip through the store seam with the right status
// codes: create → list → update (disable) → delete; invalid input 400s;
// unknown ids 404.
func TestUserSkillsEndpoints(t *testing.T) {
	fs := &skillsFakeStore{fakeChatStore: newFakeChatStore()}
	s := &Server{store: fs}

	w := userSkillsReq(t, s, s.userSkillsCollection, http.MethodPost, "/user-skills",
		`{"name":"deal-check","description":"verify a deal sheet","body":"1. Check."}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var created store.UserSkill
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	if w := userSkillsReq(t, s, s.userSkillsCollection, http.MethodPost, "/user-skills",
		`{"name":"","description":"d","body":"b"}`); w.Code != http.StatusBadRequest {
		t.Errorf("invalid create: %d", w.Code)
	}

	w = userSkillsReq(t, s, s.userSkillsCollection, http.MethodGet, "/user-skills", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "deal-check") {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}

	w = userSkillsReq(t, s, s.userSkillByID, http.MethodPut, "/user-skills/"+created.ID,
		`{"name":"deal-check","description":"verify a deal sheet","body":"1. Check.","status":"disabled"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "disabled") {
		t.Fatalf("update: %d %s", w.Code, w.Body.String())
	}

	if w := userSkillsReq(t, s, s.userSkillByID, http.MethodDelete, "/user-skills/ghost", ""); w.Code != http.StatusNotFound {
		t.Errorf("delete ghost: %d", w.Code)
	}
	if w := userSkillsReq(t, s, s.userSkillByID, http.MethodDelete, "/user-skills/"+created.ID, ""); w.Code != http.StatusNoContent {
		t.Errorf("delete: %d", w.Code)
	}
	if len(fs.skills) != 0 {
		t.Errorf("delete left rows: %+v", fs.skills)
	}
}

// Workspace-shared skills appear in GET /skills (source="workspace"),
// GET /skills/{name}, materialize into teammates' workspaces, and can be
// invoked via /<name> by teammates.
func TestWorkspaceSharedUserSkills(t *testing.T) {
	t.Setenv("FLEET_WORKSPACE_ROOT", t.TempDir())
	fs := &skillsFakeStore{fakeChatStore: newFakeChatStore()}
	s := &Server{store: fs}

	w := userSkillsReq(t, s, s.userSkillsCollection, http.MethodPost, "/user-skills",
		`{"name":"opt-workbench","description":"review campaign optimizations","body":"1. Open workbench.","visibility":"workspace"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create shared: %d %s", w.Code, w.Body.String())
	}
	var created store.UserSkill
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Visibility != store.UserSkillVisibilityWorkspace {
		t.Fatalf("visibility = %q, want workspace", created.Visibility)
	}

	// GET /skills includes the workspace-shared skill.
	w = userSkillsReq(t, s, s.listSkills, http.MethodGet, "/skills", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"source":"workspace"`) || !strings.Contains(w.Body.String(), `"opt-workbench"`) {
		t.Fatalf("GET /skills missing shared skill: %d %s", w.Code, w.Body.String())
	}

	// GET /skills/opt-workbench returns its rendered SKILL.md content.
	w = userSkillsReq(t, s, s.skillByName, http.MethodGet, "/skills/opt-workbench", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "1. Open workbench.") {
		t.Fatalf("GET /skills/opt-workbench: %d %s", w.Code, w.Body.String())
	}

	// A teammate ("teammate@x.com") materializes and can invoke /opt-workbench.
	entries := s.materializeUserSkills(context.Background(), "teammate@x.com", "conv-teammate-1")
	if len(entries) != 1 || entries[0].Name != "opt-workbench" {
		t.Fatalf("teammate materialize = %+v", entries)
	}
	invoked := s.applySkillInvocation(context.Background(), "teammate@x.com", "/opt-workbench ELC00166", "/opt-workbench ELC00166")
	if !strings.Contains(invoked, "user-skills/opt-workbench/SKILL.md") {
		t.Fatalf("teammate invocation did not match: %q", invoked)
	}
}
