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

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
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

func (s *skillsFakeStore) ListEnabledForAllUserSkills(_ context.Context) ([]store.UserSkill, error) {
	var out []store.UserSkill
	for _, sk := range s.skills {
		if sk.Status == store.UserSkillStatusActive && sk.Visibility == store.UserSkillVisibilityWorkspace && sk.EnabledForAll {
			cp := sk
			cp.SharedBy = sk.UserEmail
			out = append(out, cp)
		}
	}
	return out, nil
}

func (s *skillsFakeStore) SetUserSkillEnabledForAll(_ context.Context, _, id string, on bool) (*store.UserSkill, error) {
	for i := range s.skills {
		if s.skills[i].ID != id {
			continue
		}
		if on && (s.skills[i].Status != store.UserSkillStatusActive || s.skills[i].Visibility != store.UserSkillVisibilityWorkspace) {
			return nil, store.ErrUserSkillInvalid
		}
		s.skills[i].EnabledForAll = on
		cp := s.skills[i]
		return &cp, nil
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
	t.Parallel()

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
	r = r.WithContext(asUser(r.Context(), "u@x.com"))
	w := httptest.NewRecorder()
	handler(w, r)
	return w
}

// The CRUD endpoints round-trip through the store seam with the right status
// codes: create → list → update (disable) → delete; invalid input 400s;
// unknown ids 404.
func TestUserSkillsEndpoints(t *testing.T) {
	t.Parallel()

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

	// GET /skills returns the caller's active user skills under user_skills so
	// the composer "/" autocomplete includes newly created personal skills.
	w = userSkillsReq(t, s, s.listSkills, http.MethodGet, "/skills", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"user_skills":[{"name":"deal-check"`) {
		t.Fatalf("GET /skills missing active user_skills: %d %s", w.Code, w.Body.String())
	}

	w = userSkillsReq(t, s, s.userSkillByID, http.MethodPut, "/user-skills/"+created.ID,
		`{"name":"deal-check","description":"verify a deal sheet","body":"1. Check.","status":"disabled"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "disabled") {
		t.Fatalf("update: %d %s", w.Code, w.Body.String())
	}

	// Disabled user skills are excluded from GET /skills user_skills.
	w = userSkillsReq(t, s, s.listSkills, http.MethodGet, "/skills", "")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"user_skills"`) {
		t.Fatalf("GET /skills should omit disabled user_skills: %d %s", w.Code, w.Body.String())
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

// Workspace-shared skills appear in GET /skills under shared_skills
// (source="workspace") and GET /skills/{name}, without auto-materializing into
// teammates' prompts until the teammate opts in ("Add to my skills"). Also
// verifies that materializeUserSkills cleans up stale skill directories when
// the user's effective roster becomes empty.
func TestWorkspaceSharedUserSkills(t *testing.T) {
	wsRoot := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", wsRoot)
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

	// GET /skills includes the workspace-shared skill under shared_skills (separate from bundle skills).
	w = userSkillsReq(t, s, s.listSkills, http.MethodGet, "/skills", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"shared_skills":[`) || !strings.Contains(w.Body.String(), `"opt-workbench"`) {
		t.Fatalf("GET /skills missing shared_skills: %d %s", w.Code, w.Body.String())
	}

	// GET /skills/opt-workbench returns its rendered SKILL.md content and raw body for 1-click install.
	w = userSkillsReq(t, s, s.skillByName, http.MethodGet, "/skills/opt-workbench", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "1. Open workbench.") || !strings.Contains(w.Body.String(), `"body":"1. Open workbench."`) {
		t.Fatalf("GET /skills/opt-workbench: %d %s", w.Code, w.Body.String())
	}

	// A teammate ("teammate@x.com") does NOT auto-materialize the shared skill without opting in.
	entries := s.materializeUserSkills(context.Background(), "teammate@x.com", "conv-teammate-1")
	if len(entries) != 0 {
		t.Fatalf("unconsented teammate materialize = %+v, want empty", entries)
	}

	// Author ("u@x.com") materializes their own skill, and deleting the last skill cleans up the stale directory.
	authorEntries := s.materializeUserSkills(context.Background(), "u@x.com", "conv-author-1")
	if len(authorEntries) != 1 || authorEntries[0].Name != "opt-workbench" {
		t.Fatalf("author materialize = %+v", authorEntries)
	}
	w = userSkillsReq(t, s, s.userSkillByID, http.MethodDelete, "/user-skills/"+created.ID, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", w.Code)
	}
	afterDelete := s.materializeUserSkills(context.Background(), "u@x.com", "conv-author-1")
	if len(afterDelete) != 0 {
		t.Fatalf("afterDelete = %+v, want empty", afterDelete)
	}
	stalePath := filepath.Join(wsRoot, "conv-author-1", "user-skills", "opt-workbench")
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Fatalf("stale skill directory %s was not removed after roster became empty: err=%v", stalePath, err)
	}
}

// A user who clones & edits a bundle skill (e.g. campaign-optimization-log)
// overrides that bundle skill's SKILL.md for their runs (and inGET /skills +
// GET /skills/{name} when shared with the workspace), while keeping bundled
// reference files under skills/<name>/ available.
func TestUserSkillOverridesBundleSkill(t *testing.T) {
	wsRoot := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", wsRoot)
	bundleDir := t.TempDir()
	writeTestSkill(t, bundleDir, "campaign-optimization-log", "bundle baseline description")
	fs := &skillsFakeStore{fakeChatStore: newFakeChatStore()}
	s := &Server{
		store:        fs,
		clientConfig: &clientconfig.Bundle{SkillsDir: bundleDir},
	}

	w := userSkillsReq(t, s, s.userSkillsCollection, http.MethodPost, "/user-skills",
		`{"name":"campaign-optimization-log","description":"updated trader optimization protocol","body":"1. Read Fast.io protocol.\n2. Open Workbench.","visibility":"workspace"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create override: %d %s", w.Code, w.Body.String())
	}

	entries := s.materializeUserSkills(context.Background(), "u@x.com", "conv-override-1")
	if len(entries) != 1 || entries[0].Name != "campaign-optimization-log" || entries[0].Path != "user-skills/campaign-optimization-log/SKILL.md" {
		t.Fatalf("override materialize = %+v", entries)
	}

	invoked := s.applySkillInvocation(context.Background(), "u@x.com", "msg", "/campaign-optimization-log review opts")
	if !strings.Contains(invoked, "user-skills/campaign-optimization-log/SKILL.md") {
		t.Fatalf("expected user-skill override path in invocation, got %q", invoked)
	}

	w = userSkillsReq(t, s, s.listSkills, http.MethodGet, "/skills", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"shared_by":"u@x.com"`) {
		t.Fatalf("GET /skills missing workspace override metadata: %d %s", w.Code, w.Body.String())
	}

	w = userSkillsReq(t, s, s.skillByName, http.MethodGet, "/skills/campaign-optimization-log", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "1. Read Fast.io protocol.") {
		t.Fatalf("GET /skills/campaign-optimization-log did not serve workspace override: %d %s", w.Code, w.Body.String())
	}
}

// A workspace-shared skill an admin turned on for everyone takes part in every
// user's chat turns from the admin's row, with no "Add to my skills" step:
// only an admin can switch it on, a teammate's own active skill of the same
// name still wins, an edit by the owner reaches everyone, and it stops the
// moment the owner is no longer an admin.
func TestUserSkillEnabledForAll(t *testing.T) {
	t.Setenv("FLEET_WORKSPACE_ROOT", t.TempDir())
	const admin, member, teammate = "admin@x.com", "member@x.com", "teammate@x.com"
	base := newFakeChatStore()
	base.users = map[string]*store.User{
		admin:    {Email: admin, Role: store.RoleAdmin},
		member:   {Email: member, Role: store.RoleMember},
		teammate: {Email: teammate, Role: store.RoleMember},
	}
	fs := &skillsFakeStore{fakeChatStore: base, skills: []store.UserSkill{
		{ID: "team-1", UserEmail: admin, Name: "deal-check", Description: "verify a deal sheet", Body: "Check the totals.", Status: store.UserSkillStatusActive, Visibility: store.UserSkillVisibilityWorkspace},
		{ID: "mem-1", UserEmail: member, Name: "member-tips", Description: "a member's shared skill", Body: "Tips.", Status: store.UserSkillStatusActive, Visibility: store.UserSkillVisibilityWorkspace},
		{ID: "priv-1", UserEmail: admin, Name: "private-notes", Description: "an admin's private skill", Body: "Notes.", Status: store.UserSkillStatusActive, Visibility: store.UserSkillVisibilityPrivate},
	}}
	s := &Server{store: fs}
	put := func(email, role, id, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPut, "/user-skills/"+id, strings.NewReader(body))
		r = r.WithContext(asUserRole(r.Context(), email, role))
		w := httptest.NewRecorder()
		s.userSkillByID(w, r)
		return w
	}
	names := func(entries []agent.UserSkillPromptEntry) string {
		var out []string
		for _, e := range entries {
			out = append(out, e.Name)
		}
		return strings.Join(out, ",")
	}

	// A member cannot switch it on, and the refused request writes nothing
	// (the body change it carried is not applied either).
	w := put(member, store.RoleMember, "mem-1", `{"name":"member-tips","description":"a member's shared skill","body":"Changed.","enabled_for_all":true}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("member enable for all = %d %s, want 403", w.Code, w.Body.String())
	}
	if fs.skills[1].EnabledForAll || fs.skills[1].Body != "Tips." {
		t.Fatalf("refused request changed the row: %+v", fs.skills[1])
	}
	if got := names(s.materializeUserSkills(context.Background(), teammate, "conv-all-1")); got != "" {
		t.Fatalf("teammate roster before anything is on = %q, want empty", got)
	}

	// A private skill cannot be turned on for everyone, even by an admin.
	w = put(admin, store.RoleAdmin, "priv-1", `{"name":"private-notes","description":"an admin's private skill","body":"Notes.","enabled_for_all":true}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("private skill enable for all = %d %s, want 400", w.Code, w.Body.String())
	}

	// The admin switches the shared skill on: the response says so, and the
	// teammate's next turn has it with no opt-in.
	w = put(admin, store.RoleAdmin, "team-1", `{"name":"deal-check","description":"verify a deal sheet","body":"Check the totals.","enabled_for_all":true}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"enabled_for_all":true`) {
		t.Fatalf("admin enable for all = %d %s", w.Code, w.Body.String())
	}
	entries := s.materializeUserSkills(context.Background(), teammate, "conv-all-1")
	if names(entries) != "deal-check" || entries[0].Path != "user-skills/deal-check/SKILL.md" {
		t.Fatalf("teammate roster = %+v, want deal-check", entries)
	}
	wsDir, _ := tools.EnsureWorkspaceDir("conv-all-1")
	md, err := os.ReadFile(filepath.Join(wsDir, "user-skills", "deal-check", "SKILL.md"))
	if err != nil || !strings.Contains(string(md), "Check the totals.") {
		t.Fatalf("materialized SKILL.md = %q, %v", md, err)
	}
	// The owner's own roster lists it once, not twice.
	if got := names(s.materializeUserSkills(context.Background(), admin, "conv-all-admin")); got != "deal-check,private-notes" {
		t.Fatalf("admin roster = %q", got)
	}
	// "/deal-check" works for the teammate, worded as a workspace skill.
	if got := s.applySkillInvocation(context.Background(), teammate, "/deal-check acme", "/deal-check acme"); !strings.Contains(got, "user-skills/deal-check/SKILL.md") || !strings.Contains(got, "the workspace skill") {
		t.Fatalf("teammate invocation = %q", got)
	}
	// GET /skills marks it, so the library shows it as on instead of offering "Add to my skills".
	r := httptest.NewRequest(http.MethodGet, "/skills", nil)
	r = r.WithContext(asUserRole(r.Context(), teammate, store.RoleMember))
	rec := httptest.NewRecorder()
	s.listSkills(rec, r)
	var listed skillsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	marked := map[string]bool{}
	for _, e := range listed.Shared {
		marked[e.Name] = e.EnabledForAll
	}
	if !marked["deal-check"] || marked["member-tips"] {
		t.Fatalf("shared_skills enabled_for_all = %v, want only deal-check", marked)
	}

	// The owner edits the skill without naming the flag: it stays on, and the
	// teammate's next turn gets the new text.
	w = put(admin, store.RoleAdmin, "team-1", `{"name":"deal-check","description":"verify a deal sheet","body":"Check the totals twice."}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"enabled_for_all":true`) {
		t.Fatalf("admin edit = %d %s", w.Code, w.Body.String())
	}
	s.materializeUserSkills(context.Background(), teammate, "conv-all-1")
	if md, _ := os.ReadFile(filepath.Join(wsDir, "user-skills", "deal-check", "SKILL.md")); !strings.Contains(string(md), "Check the totals twice.") {
		t.Fatalf("teammate did not get the owner's edit: %q", md)
	}

	// A teammate's own active skill of the same name wins over it.
	fs.skills = append(fs.skills, store.UserSkill{ID: "own-1", UserEmail: teammate, Name: "deal-check", Description: "my own version", Body: "Mine.", Status: store.UserSkillStatusActive, Visibility: store.UserSkillVisibilityPrivate})
	s.materializeUserSkills(context.Background(), teammate, "conv-all-1")
	if md, _ := os.ReadFile(filepath.Join(wsDir, "user-skills", "deal-check", "SKILL.md")); !strings.Contains(string(md), "Mine.") {
		t.Fatalf("teammate's own skill did not win: %q", md)
	}
	fs.skills = fs.skills[:len(fs.skills)-1]

	// The owner stops being an admin: the flag is still on the row, and the
	// skill is out of the teammate's turns and no longer marked in GET /skills.
	base.mu.Lock()
	base.users[admin] = &store.User{Email: admin, Role: store.RoleMember}
	base.mu.Unlock()
	if got := names(s.materializeUserSkills(context.Background(), teammate, "conv-all-1")); got != "" {
		t.Fatalf("teammate roster after the owner lost admin = %q, want empty", got)
	}
	if _, err := os.Stat(filepath.Join(wsDir, "user-skills", "deal-check")); !os.IsNotExist(err) {
		t.Fatalf("stale materialization left behind: %v", err)
	}
	// ...and as a member they can no longer switch it on, but can switch it off.
	if w = put(admin, store.RoleMember, "team-1", `{"name":"deal-check","description":"verify a deal sheet","body":"Check the totals twice.","enabled_for_all":true}`); w.Code != http.StatusForbidden {
		t.Fatalf("former admin enable for all = %d, want 403", w.Code)
	}
	if w = put(admin, store.RoleMember, "team-1", `{"name":"deal-check","description":"verify a deal sheet","body":"Check the totals twice.","enabled_for_all":false}`); w.Code != http.StatusOK || fs.skills[0].EnabledForAll {
		t.Fatalf("former admin switch off = %d %s (row %+v)", w.Code, w.Body.String(), fs.skills[0])
	}
}
