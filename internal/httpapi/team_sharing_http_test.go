package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

// teamHTTPFixture wires two teammates, an outsider, and a team-shared project
// with one chat its owner shared with the team — the world the ADR-0057
// endpoints are about.
type teamHTTPFixture struct {
	srv     *Server
	st      *store.Store
	ctx     context.Context
	project *store.Project
	chat    *store.Conversation
}

func newTeamHTTPFixture(t *testing.T) teamHTTPFixture {
	t.Helper()
	srv := serverFixture(t)
	st := srv.store.(*store.Store)
	ctx := context.Background()
	for _, u := range []struct{ email, team string }{
		{"alice@x.com", "quant"}, {"bob@x.com", "quant"}, {"zoe@x.com", "ops"},
	} {
		if _, err := st.CreateUser(ctx, u.email, "pw-123456"); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		team := u.team
		if _, err := st.SetUserRoleTeam(ctx, u.email, nil, &team); err != nil {
			t.Fatalf("SetUserRoleTeam: %v", err)
		}
	}
	p, err := st.CreateProject(ctx, &store.Project{
		OwnerEmail: "alice@x.com", Name: "Quant", TeamID: "quant",
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	c, err := st.CreateConversation(ctx, "alice@x.com", "Spread study", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if err := st.SetConversationProject(ctx, "alice@x.com", c.ID, p.ID); err != nil {
		t.Fatalf("SetConversationProject: %v", err)
	}
	if _, err := st.AppendHistory(ctx, c.ID, []agent.HistoryEntry{
		{Role: "user", Type: "text", Content: json.RawMessage(`{"text":"what is the spread?"}`)},
		{Role: "assistant", Type: "text", Content: json.RawMessage(`{"text":"about 12 bps"}`)},
	}); err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}
	if stored, err := st.SetConversationTeamVisible(ctx, "alice@x.com", c.ID, true); err != nil || !stored {
		t.Fatalf("SetConversationTeamVisible = (%v, %v), want (true, nil)", stored, err)
	}
	return teamHTTPFixture{srv: srv, st: st, ctx: ctx, project: p, chat: c}
}

// convSub issues METHOD /conversations/{id}/{sub} as user.
func convSub(t *testing.T, srv *Server, method, user, convID, sub, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRecorder()
	req := httptest.NewRequest(method, "/conversations/"+convID+"/"+sub, strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, user))
	srv.conversationByID(r, req)
	return r
}

// projectSub issues METHOD /projects/{id}/{sub} as user.
func projectSub(t *testing.T, srv *Server, method, user, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(method, "/projects/"+path, strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, user))
	srv.projectByID(w, req)
	return w
}

// GET /conversations/{id}/team-view is the one conversation route a non-owner
// may reach, and only through both gates.
func TestConversationTeamView(t *testing.T) {
	f := newTeamHTTPFixture(t)

	w := convSub(t, f.srv, "GET", "bob@x.com", f.chat.ID, "team-view", "")
	if w.Code != 200 {
		t.Fatalf("teammate team-view: status %d body %s", w.Code, w.Body.String())
	}
	var snap struct {
		OwnerEmail string `json:"owner_email"`
		TeamID     string `json:"team_id"`
		Messages   []struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatalf("json: %v", err)
	}
	if snap.OwnerEmail != "alice@x.com" || snap.TeamID != "quant" {
		t.Errorf("snapshot = %+v", snap)
	}
	// The owner's working state is read for Branch but never sent: the fork's
	// project and settings are decided server-side from the parent row.
	for _, k := range []string{"persona", "model", "project_id", "lockdown"} {
		if strings.Contains(w.Body.String(), `"`+k+`"`) {
			t.Errorf("snapshot leaks the owner's %s", k)
		}
	}
	if len(snap.Messages) != 2 || snap.Messages[0].ID == 0 {
		t.Errorf("messages = %+v, want two text entries with ids", snap.Messages)
	}

	// Another team is 404 — indistinguishable from "no such chat", so team
	// membership is never probeable from here.
	if w := convSub(t, f.srv, "GET", "zoe@x.com", f.chat.ID, "team-view", ""); w.Code != 404 {
		t.Errorf("other team: status %d, want 404", w.Code)
	}
	// So is a chat nobody shared.
	priv, err := f.st.CreateConversation(f.ctx, "alice@x.com", "Private", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if w := convSub(t, f.srv, "GET", "bob@x.com", priv.ID, "team-view", ""); w.Code != 404 {
		t.Errorf("unshared chat: status %d, want 404", w.Code)
	}
}

// A teammate branches a shared chat into their own; the fork lands in the same
// project and belongs to them.
func TestBranchTeamSharedConversationOverHTTP(t *testing.T) {
	f := newTeamHTTPFixture(t)
	msgs, err := f.st.LoadHistory(f.ctx, f.chat.ID)
	if err != nil || len(msgs) == 0 {
		t.Fatalf("LoadHistory: %v", err)
	}
	point := msgs[len(msgs)-1].ID

	body, _ := json.Marshal(map[string]any{"branch_point_message_id": point, "title": "Bob's fork"})
	w := convSub(t, f.srv, "POST", "bob@x.com", f.chat.ID, "branch", string(body))
	if w.Code != 201 {
		t.Fatalf("branch: status %d body %s", w.Code, w.Body.String())
	}
	var out store.Conversation
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("json: %v", err)
	}
	if out.UserEmail != "bob@x.com" || out.ProjectID != f.project.ID {
		t.Errorf("branch = owner %q project %q", out.UserEmail, out.ProjectID)
	}

	// A non-teammate gets the same 404 the read gives.
	if w := convSub(t, f.srv, "POST", "zoe@x.com", f.chat.ID, "branch", string(body)); w.Code != 404 {
		t.Errorf("outsider branch: status %d, want 404", w.Code)
	}
}

// The project home's Team section and the delete-impact counts.
func TestProjectTeamConversationsAndImpact(t *testing.T) {
	f := newTeamHTTPFixture(t)
	if _, err := f.st.CreateProjectMemory(f.ctx, f.project.ID, "bob@x.com", "quote in bps", "fact"); err != nil {
		t.Fatal(err)
	}

	w := projectSub(t, f.srv, "GET", "bob@x.com", f.project.ID+"/team-conversations", "")
	if w.Code != 200 {
		t.Fatalf("team-conversations: status %d body %s", w.Code, w.Body.String())
	}
	var list struct {
		Conversations []struct {
			ID         string `json:"id"`
			UserEmail  string `json:"user_email"`
			ShareToken string `json:"share_token"`
		} `json:"conversations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("json: %v", err)
	}
	if len(list.Conversations) != 1 || list.Conversations[0].ID != f.chat.ID {
		t.Fatalf("team conversations = %+v", list.Conversations)
	}
	if list.Conversations[0].ShareToken != "" {
		t.Error("a teammate listing must not carry the owner's public share token")
	}

	// A non-member 404s on the whole subresource tree.
	if w := projectSub(t, f.srv, "GET", "zoe@x.com", f.project.ID+"/team-conversations", ""); w.Code != 404 {
		t.Errorf("non-member: status %d, want 404", w.Code)
	}

	w = projectSub(t, f.srv, "GET", "alice@x.com", f.project.ID+"/impact", "")
	if w.Code != 200 {
		t.Fatalf("impact: status %d body %s", w.Code, w.Body.String())
	}
	var impact store.ProjectImpact
	if err := json.Unmarshal(w.Body.Bytes(), &impact); err != nil {
		t.Fatalf("json: %v", err)
	}
	if impact.Memories != 1 || impact.Chats != 1 || impact.Members != 1 || impact.TeamSharedChats != 1 {
		t.Errorf("impact = %+v", impact)
	}
}

// Team learnings: any member writes, but changing an existing entry is its
// author's or the project owner's — a shared store where anybody can rewrite
// anybody is not a record a team can trust.
func TestProjectMemoryPermissions(t *testing.T) {
	f := newTeamHTTPFixture(t)

	// Bob (a member, not the owner) writes one.
	w := projectSub(t, f.srv, "POST", "bob@x.com", f.project.ID+"/memories", `{"content":"quote spreads in bps"}`)
	if w.Code != 200 {
		t.Fatalf("member write: status %d body %s", w.Code, w.Body.String())
	}
	var m store.Memory
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("json: %v", err)
	}
	if m.UserEmail != "bob@x.com" {
		t.Errorf("provenance = %q, want the writer", m.UserEmail)
	}

	// Its author may retire it (the default remove — record kept).
	w = projectSub(t, f.srv, "PATCH", "bob@x.com", f.project.ID+"/memories/"+m.ID, `{"retired":true}`)
	if w.Code != 200 {
		t.Fatalf("author retire: status %d body %s", w.Code, w.Body.String())
	}
	var retired store.Memory
	if err := json.Unmarshal(w.Body.Bytes(), &retired); err != nil {
		t.Fatal(err)
	}
	if !retired.Retired() || retired.UserEmail != "bob@x.com" {
		t.Errorf("retired = %v author = %q", retired.Retired(), retired.UserEmail)
	}

	// The project OWNER may manage anyone's entry.
	w = projectSub(t, f.srv, "PATCH", "alice@x.com", f.project.ID+"/memories/"+m.ID, `{"retired":false}`)
	if w.Code != 200 {
		t.Fatalf("owner restore: status %d body %s", w.Code, w.Body.String())
	}

	// A third member who is neither may not.
	if _, err := f.st.CreateUser(f.ctx, "carl@x.com", "pw-123456"); err != nil {
		t.Fatal(err)
	}
	team := "quant"
	if _, err := f.st.SetUserRoleTeam(f.ctx, "carl@x.com", nil, &team); err != nil {
		t.Fatal(err)
	}
	w = projectSub(t, f.srv, "PATCH", "carl@x.com", f.project.ID+"/memories/"+m.ID, `{"content":"nonsense"}`)
	if w.Code != 403 {
		t.Errorf("third member patch: status %d, want 403", w.Code)
	}
	w = projectSub(t, f.srv, "DELETE", "carl@x.com", f.project.ID+"/memories/"+m.ID, "")
	if w.Code != 403 {
		t.Errorf("third member delete: status %d, want 403", w.Code)
	}

	// An id that is not this project's is 404, not 403 — it names nothing here.
	w = projectSub(t, f.srv, "PATCH", "alice@x.com", f.project.ID+"/memories/does-not-exist", `{"retired":true}`)
	if w.Code != 404 {
		t.Errorf("unknown memory: status %d, want 404", w.Code)
	}
}

// Promotion (D5) and the destination choice on the approval card (D1), over
// the endpoints the UI actually calls.
func TestPromoteAndAcceptIntoProject(t *testing.T) {
	f := newTeamHTTPFixture(t)

	personal, err := f.st.CreateMemory(f.ctx, "bob@x.com", "the desk closes at 16:00", "manual", "fact")
	if err != nil {
		t.Fatal(err)
	}
	w := projectSub(t, f.srv, "POST", "bob@x.com", f.project.ID+"/memories",
		`{"from_memory_id":"`+personal.ID+`"}`)
	if w.Code != 200 {
		t.Fatalf("promote: status %d body %s", w.Code, w.Body.String())
	}
	remaining, err := f.st.ListMemories(f.ctx, "bob@x.com")
	if err != nil {
		t.Fatal(err)
	}
	for _, pm := range remaining {
		if pm.ID == personal.ID {
			t.Error("promotion must MOVE the memory, not copy it")
		}
	}

	// A proposal accepted with a project_id lands in team learnings.
	prop, err := f.st.CreateMemoryProposal(f.ctx, "bob@x.com", "", store.MemoryProposalParams{
		Content: "settlement is T+1", Kind: "fact",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/memories/"+prop.ID+"/accept",
		strings.NewReader(`{"project_id":"`+f.project.ID+`"}`))
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, "bob@x.com"))
	rec := httptest.NewRecorder()
	f.srv.memoryByID(rec, req)
	if rec.Code != 200 {
		t.Fatalf("accept into project: status %d body %s", rec.Code, rec.Body.String())
	}
	shared, err := f.st.ListProjectMemories(f.ctx, f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(shared) != 2 {
		t.Fatalf("project memories = %d, want 2", len(shared))
	}

	// A project the caller is not a member of is 404 — accepting somewhere you
	// cannot see is not a thing.
	other, err := f.st.CreateProject(f.ctx, &store.Project{OwnerEmail: "zoe@x.com", Name: "Theirs"})
	if err != nil {
		t.Fatal(err)
	}
	prop2, err := f.st.CreateMemoryProposal(f.ctx, "bob@x.com", "", store.MemoryProposalParams{
		Content: "nope", Kind: "fact",
	})
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest("POST", "/memories/"+prop2.ID+"/accept",
		strings.NewReader(`{"project_id":"`+other.ID+`"}`))
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, "bob@x.com"))
	rec = httptest.NewRecorder()
	f.srv.memoryByID(rec, req)
	if rec.Code != 404 {
		t.Errorf("accept into a foreign project: status %d, want 404", rec.Code)
	}
}

// Sources lists FILES. Every conversation workspace carries the bundle-mount
// symlinks (personas, protocols, shared, skills, system_prompts) pointing
// outside the workspace root; they used to surface as tiny "files" whose
// download correctly tripped the traversal guard and dumped raw error text
// into a tab.
func TestProjectFilesExcludesBundleSymlinks(t *testing.T) {
	srv := serverFixture(t)
	st := srv.store.(*store.Store)
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)

	proj, err := st.CreateProject(ctx, &store.Project{OwnerEmail: "owner@x.com", Name: "Home"})
	if err != nil {
		t.Fatal(err)
	}
	conv, err := st.CreateConversation(ctx, "owner@x.com", "analysis", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetConversationProject(ctx, "owner@x.com", conv.ID, proj.ID); err != nil {
		t.Fatal(err)
	}

	ws := filepath.Join(root, conv.ID)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "report.csv"), []byte("a,b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The bundle mounts: a symlink to a directory and one to a file, both
	// outside the workspace root.
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "personas"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "chat.md"), []byte("prompt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "personas"), filepath.Join(ws, "personas")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "chat.md"), filepath.Join(ws, "system_prompts")); err != nil {
		t.Fatal(err)
	}

	w := getProjectSub(t, srv, "owner@x.com", proj.ID, "files")
	if w.Code != 200 {
		t.Fatalf("files: status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Files []struct {
			Name string `json:"name"`
		} `json:"files"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Files) != 1 || resp.Files[0].Name != "report.csv" {
		t.Errorf("files = %+v, want only the real file", resp.Files)
	}
}

// GET /me/team carries what LEAVING would cost, so the confirm can state it
// before acting instead of reporting it afterwards.
func TestMyTeamReportsLeaveImpact(t *testing.T) {
	f := newTeamHTTPFixture(t)
	// A team-shared project bob does NOT own, plus his own shared chat.
	bobChat, err := f.st.CreateConversation(f.ctx, "bob@x.com", "Bob's", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetConversationProject(f.ctx, "bob@x.com", bobChat.ID, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.SetConversationTeamVisible(f.ctx, "bob@x.com", bobChat.ID, true); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/me/team", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, "bob@x.com"))
	w := httptest.NewRecorder()
	f.srv.handleMyTeam(w, req)
	if w.Code != 200 {
		t.Fatalf("me/team: status %d body %s", w.Code, w.Body.String())
	}
	var out struct {
		TeamID         string `json:"team_id"`
		SharedProjects int    `json:"shared_projects"`
		SharedChats    int    `json:"shared_chats"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.TeamID != "quant" || out.SharedProjects != 1 || out.SharedChats != 1 {
		t.Errorf("me/team = %+v, want quant / 1 project / 1 chat", out)
	}
}

// Ownership transfer over HTTP (ADR-0057). Two callers may do it — the owner,
// and an ADMIN, because the case it exists for is an owner who has left and
// cannot click anything. Everyone else gets the same 404 a non-member gets.
func TestProjectTransferOwnership(t *testing.T) {
	f := newTeamHTTPFixture(t)
	body := `{"to_email":"bob@x.com"}`

	// A member who is not the owner and not an admin: 404, and no transfer.
	if w := projectSub(t, f.srv, "POST", "bob@x.com", f.project.ID+"/transfer", body); w.Code != 404 {
		t.Errorf("member transfer: status %d, want 404", w.Code)
	}
	// Someone outside the team entirely: also 404 — the route leaks nothing
	// about which projects exist.
	if w := projectSub(t, f.srv, "POST", "zoe@x.com", f.project.ID+"/transfer", body); w.Code != 404 {
		t.Errorf("outsider transfer: status %d, want 404", w.Code)
	}
	if p, _ := f.st.GetProject(f.ctx, f.project.ID); p.OwnerEmail != "alice@x.com" {
		t.Fatalf("owner changed on a refused transfer: %q", p.OwnerEmail)
	}

	// The owner hands it over.
	w := projectSub(t, f.srv, "POST", "alice@x.com", f.project.ID+"/transfer", body)
	if w.Code != 200 {
		t.Fatalf("owner transfer: status %d body %s", w.Code, w.Body.String())
	}
	var out store.Project
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.OwnerEmail != "bob@x.com" || out.TeamID != "quant" {
		t.Errorf("transferred project = %+v", out)
	}

	// Handing it outside the team is refused with the reason, not a 404 —
	// the caller is authorized, the target is wrong.
	w = projectSub(t, f.srv, "POST", "bob@x.com", f.project.ID+"/transfer", `{"to_email":"zoe@x.com"}`)
	if w.Code != 400 {
		t.Errorf("cross-team transfer: status %d body %s", w.Code, w.Body.String())
	}
}

// The admin path: a departed owner cannot act, so an admin must be able to.
func TestProjectTransferByAdmin(t *testing.T) {
	f := newTeamHTTPFixture(t)
	// serverFixture's config has no ADMIN_EMAILS, so the admin arrives the
	// other way the middleware supplies: the request's resolved role.
	req := httptest.NewRequest("POST", "/projects/"+f.project.ID+"/transfer",
		strings.NewReader(`{"to_email":"bob@x.com"}`))
	ctx := context.WithValue(req.Context(), ctxKeyUser, "zoe@x.com")
	ctx = context.WithValue(ctx, ctxKeyRole, store.RoleAdmin)
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()
	f.srv.projectByID(w, req)
	if w.Code != 200 {
		t.Fatalf("admin transfer: status %d body %s", w.Code, w.Body.String())
	}
	if p, _ := f.st.GetProject(f.ctx, f.project.ID); p.OwnerEmail != "bob@x.com" {
		t.Errorf("owner after admin transfer = %q", p.OwnerEmail)
	}
}

// GET /projects/{id}/members backs the transfer picker — and only the people
// who can transfer may read it.
func TestProjectMembersEndpoint(t *testing.T) {
	f := newTeamHTTPFixture(t)
	w := projectSub(t, f.srv, "GET", "alice@x.com", f.project.ID+"/members", "")
	if w.Code != 200 {
		t.Fatalf("members: status %d body %s", w.Code, w.Body.String())
	}
	var out struct {
		Members []string `json:"members"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Members) != 2 {
		t.Errorf("members = %v, want alice + bob", out.Members)
	}
	// A plain MEMBER is refused: this enumerates the whole team, including
	// people who have never shared anything, which no other project surface
	// gives them — and the only caller that needs it is the transfer picker,
	// which only the owner and admins can use.
	if w := projectSub(t, f.srv, "GET", "bob@x.com", f.project.ID+"/members", ""); w.Code != 404 {
		t.Errorf("plain member: status %d, want 404", w.Code)
	}
	// Membership-gated like every other project subresource.
	if w := projectSub(t, f.srv, "GET", "zoe@x.com", f.project.ID+"/members", ""); w.Code != 404 {
		t.Errorf("non-member: status %d, want 404", w.Code)
	}
}

// Deleting an account that still owns a team-shared project is refused with a
// 409 that NAMES the projects — the admin's next step is a transfer, not a
// support ticket. Before this the delete succeeded and took the team's project
// and every learning in it.
func TestAdminUserDeleteRefusesOwnedSharedProjects(t *testing.T) {
	f := newTeamHTTPFixture(t)
	if _, err := f.st.CreateProjectMemory(f.ctx, f.project.ID, "bob@x.com", "a learning", "fact"); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("DELETE", "/admin/users/alice%40x.com", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, "root@x.com"))
	w := httptest.NewRecorder()
	f.srv.handleAdminUserDelete(w, req, "alice@x.com")
	if w.Code != 409 {
		t.Fatalf("delete owner of a shared project: status %d body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Quant") {
		t.Errorf("the 409 must name the project to transfer: %s", w.Body.String())
	}
	// The body is JSON and carries the project ID alongside the name. Names
	// alone made the refusal a dead end: it told the admin to transfer the
	// project and gave them no route to the control that does it, and the id
	// cannot be recovered client-side (GET /projects is scoped to the caller's
	// own and team-visible projects; an admin is usually neither).
	var refusal struct {
		Error    string `json:"error"`
		Projects []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"owns_shared_projects"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("409 body is not JSON: %v (%s)", err, w.Body.String())
	}
	if !strings.Contains(refusal.Error, "transfer them to another member first") {
		t.Errorf("error prose = %q, want the next step spelled out", refusal.Error)
	}
	if len(refusal.Projects) != 1 {
		t.Fatalf("owns_shared_projects = %+v, want exactly one", refusal.Projects)
	}
	if refusal.Projects[0].ID != f.project.ID || refusal.Projects[0].Name != "Quant" {
		t.Errorf("owns_shared_projects[0] = %+v, want {%s Quant}",
			refusal.Projects[0], f.project.ID)
	}
	if u, err := f.st.GetUser(f.ctx, "alice@x.com"); err != nil || u == nil {
		t.Error("the refused delete must leave the account intact")
	}

	// After a transfer the delete goes through, and the team keeps everything.
	if _, err := f.st.TransferProjectOwnership(f.ctx, f.project.ID, "bob@x.com"); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	f.srv.handleAdminUserDelete(w, req, "alice@x.com")
	if w.Code != 200 && w.Code != 204 {
		t.Fatalf("delete after transfer: status %d body %s", w.Code, w.Body.String())
	}
	if p, _ := f.st.GetProject(f.ctx, f.project.ID); p == nil {
		t.Error("the team's project must survive the owner's account deletion")
	}
	if mems, _ := f.st.ListProjectMemories(f.ctx, f.project.ID); len(mems) != 1 {
		t.Error("the team's learnings must survive it too")
	}
}

// A project already shared with a team KEEPS that team through any later edit.
//
// Red/green: PATCH resolved `team_shared: true` into the caller's CURRENT
// team every time, and the Projects modal sends the flag on every save. So an
// owner moved from `quant` to `ops` who then renamed the project handed `ops`
// every team learning `quant` had written, and locked `quant` out — no dialog,
// no trace. Same defect migration 054 fixed for conversations, still live for
// the project.
func TestPatchDoesNotRepointAnAlreadySharedProject(t *testing.T) {
	f := newTeamHTTPFixture(t)
	st := f.srv.store.(*store.Store)

	// An admin moves the owner out of the team the project is shared with.
	ops := "ops"
	if _, err := st.SetUserRoleTeam(f.ctx, "alice@x.com", nil, &ops); err != nil {
		t.Fatal(err)
	}

	// An ordinary edit that happens to re-send the flag, as the modal does.
	w := projectSub(t, f.srv, "PATCH", "alice@x.com", f.project.ID,
		`{"name":"Quant renamed","team_shared":true}`)
	if w.Code != 200 {
		t.Fatalf("patch: status %d body %s", w.Code, w.Body.String())
	}
	var out struct {
		Name   string `json:"name"`
		TeamID string `json:"team_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Name != "Quant renamed" {
		t.Errorf("name = %q, want the rename to apply", out.Name)
	}
	if out.TeamID != "quant" {
		t.Errorf("team = %q, want quant — a rename must not hand the project to another team", out.TeamID)
	}

	// Unsharing then re-sharing IS the deliberate way to re-point it.
	if w := projectSub(t, f.srv, "PATCH", "alice@x.com", f.project.ID, `{"team_shared":false}`); w.Code != 200 {
		t.Fatalf("unshare: status %d body %s", w.Code, w.Body.String())
	}
	w = projectSub(t, f.srv, "PATCH", "alice@x.com", f.project.ID, `{"team_shared":true}`)
	if w.Code != 200 {
		t.Fatalf("re-share: status %d body %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.TeamID != "ops" {
		t.Errorf("team after a deliberate re-share = %q, want ops", out.TeamID)
	}
}

// The share endpoint reports the state it STORED, never the state it was asked
// for — and refuses, rather than silently doing nothing, when the chat has no
// audience or no home (ADR-0057).
func TestShareWithTeamReportsWhatItStored(t *testing.T) {
	f := newTeamHTTPFixture(t)
	st := f.srv.store.(*store.Store)

	// A chat with a home: 200 and team_visible:true.
	w := convSub(t, f.srv, "POST", "alice@x.com", f.chat.ID, "share-with-team", `{"visible":true}`)
	if w.Code != 200 {
		t.Fatalf("share: status %d body %s", w.Code, w.Body.String())
	}
	var out struct {
		TeamVisible bool `json:"team_visible"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.TeamVisible {
		t.Error("a share that took effect must report team_visible:true")
	}

	// The owner leaves the team. Sharing now has no audience to name: a 409
	// with the reason, not a 200 the UI would badge as shared.
	if _, err := st.SetOwnTeam(f.ctx, "alice@x.com", "", false); err != nil {
		t.Fatal(err)
	}
	c2, err := st.CreateConversation(f.ctx, "alice@x.com", "Another", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetConversationProject(f.ctx, "alice@x.com", c2.ID, f.project.ID); err != nil {
		t.Fatal(err)
	}
	w = convSub(t, f.srv, "POST", "alice@x.com", c2.ID, "share-with-team", `{"visible":true}`)
	if w.Code != 409 {
		t.Fatalf("share with no team: status %d body %s, want 409", w.Code, w.Body.String())
	}
	if got, _ := st.Get(f.ctx, "alice@x.com", c2.ID); got.TeamVisible {
		t.Error("a refused share must leave the flag FALSE")
	}
}

// Unticking "Share with my team" over HTTP moves a teammate's chats into their
// own unfiled chats — and the confirm's counts are on the impact read that
// precedes it.
//
// Red/green: the PATCH unshared the chats and left them FILED in a project the
// teammate could no longer see, and the rail lists chats through the projects
// the viewer can see — so their own conversations disappeared from Projects,
// Temporary and Archived alike, with nothing deleted and nothing said.
func TestUntickingTeamSharingUnfilesTeammateChats(t *testing.T) {
	f := newTeamHTTPFixture(t)
	// Bob's own chat, filed in Alice's team-shared project (a branch of hers is
	// how this normally happens) and shared back with the team.
	bobs, err := f.st.CreateConversation(f.ctx, "bob@x.com", "Bob's branch", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetConversationProject(f.ctx, "bob@x.com", bobs.ID, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if stored, err := f.st.SetConversationTeamVisible(f.ctx, "bob@x.com", bobs.ID, true); err != nil || !stored {
		t.Fatalf("SetConversationTeamVisible = (%v, %v), want (true, nil)", stored, err)
	}

	// The confirm's numbers, by their JSON names — this is the contract the
	// dialog's copy ("{N} chats from teammates will move to their unfiled
	// chats.") is rendered from.
	w := projectSub(t, f.srv, "GET", "alice@x.com", f.project.ID+"/impact", "")
	if w.Code != 200 {
		t.Fatalf("impact: status %d body %s", w.Code, w.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("json: %v", err)
	}
	for field, want := range map[string]float64{
		"chats_from_teammates": 1,
		"teammates_with_chats": 1,
	} {
		got, ok := raw[field]
		if !ok {
			t.Fatalf("impact response is missing %q: %s", field, w.Body.String())
		}
		if got != want {
			t.Errorf("%s = %v, want %v", field, got, want)
		}
	}

	// The untick itself.
	w = projectSub(t, f.srv, "PATCH", "alice@x.com", f.project.ID, `{"team_shared":false}`)
	if w.Code != 200 {
		t.Fatalf("untick: status %d body %s", w.Code, w.Body.String())
	}

	// Bob's chat is his again: unfiled, unshared, still there, still listed.
	got, err := f.st.Get(f.ctx, "bob@x.com", bobs.ID)
	if err != nil || got == nil {
		t.Fatalf("bob's chat must still exist (err=%v)", err)
	}
	if got.ProjectID != "" || got.TeamVisible {
		t.Errorf("bob's chat = project %q / team_visible %v, want unfiled and unshared",
			got.ProjectID, got.TeamVisible)
	}
	list, err := f.st.List(f.ctx, "bob@x.com", false)
	if err != nil {
		t.Fatal(err)
	}
	var listed bool
	for _, c := range list {
		if c.ID == bobs.ID {
			listed = true
		}
	}
	if !listed {
		t.Error("bob's chat is missing from his own listing — the vanishing bug is back")
	}
	// Alice keeps her own chat where it was; she still owns the project.
	mine, err := f.st.Get(f.ctx, "alice@x.com", f.chat.ID)
	if err != nil || mine == nil {
		t.Fatalf("alice's chat: %v", err)
	}
	if mine.ProjectID != f.project.ID || mine.TeamVisible {
		t.Errorf("alice's chat = project %q / team_visible %v, want still filed and unshared",
			mine.ProjectID, mine.TeamVisible)
	}
}
