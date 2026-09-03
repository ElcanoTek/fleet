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
	if err := st.SetConversationTeamVisible(ctx, "alice@x.com", c.ID, true); err != nil {
		t.Fatalf("SetConversationTeamVisible: %v", err)
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
		ProjectID  string `json:"project_id"`
		Messages   []struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatalf("json: %v", err)
	}
	if snap.OwnerEmail != "alice@x.com" || snap.ProjectID != f.project.ID {
		t.Errorf("snapshot = %+v", snap)
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
	if err := f.st.SetConversationTeamVisible(f.ctx, "bob@x.com", bobChat.ID, true); err != nil {
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
