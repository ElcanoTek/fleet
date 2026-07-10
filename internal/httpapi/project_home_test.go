package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ElcanoTek/fleet/internal/store"
)

// getProjectSub issues GET /projects/{id}/{sub} as user and returns the recorder.
func getProjectSub(t *testing.T, srv *Server, user, projectID, sub string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/projects/"+projectID+"/"+sub, nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, user))
	w := httptest.NewRecorder()
	srv.projectByID(w, req)
	return w
}

// The project home's chat list and Sources panel are scoped to the CALLER'S
// OWN conversations — a team-shared project must never enumerate another
// member's chats or files (#237's conversations-stay-private rule).
func TestProjectHome_ConversationsAndFilesAreCallerScoped(t *testing.T) {
	srv := serverFixture(t)
	st := srv.store.(*store.Store)
	ctx := context.Background()
	const owner = "owner@x.com"
	const other = "other@x.com"

	// Workspace root under the test's temp dir so the walk sees only what
	// this test creates (and cleanup is automatic).
	t.Setenv("FLEET_WORKSPACE_ROOT", t.TempDir())

	proj, err := st.CreateProject(ctx, &store.Project{OwnerEmail: owner, Name: "Home"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	mine, err := st.CreateConversation(ctx, owner, "my analysis", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if err := st.SetConversationProject(ctx, owner, mine.ID, proj.ID); err != nil {
		t.Fatalf("SetConversationProject: %v", err)
	}
	theirs, err := st.CreateConversation(ctx, other, "their analysis", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation (other): %v", err)
	}
	if _, err := st.CreateProject(ctx, &store.Project{OwnerEmail: other, Name: "decoy"}); err != nil {
		t.Fatalf("decoy project: %v", err)
	}
	// File the foreign conversation into the SAME project id directly (the
	// HTTP layer would 404 a non-member, but the store call stands in for a
	// team member's own filing).
	if err := st.SetConversationProject(ctx, other, theirs.ID, proj.ID); err != nil {
		t.Fatalf("SetConversationProject (other): %v", err)
	}

	// One workspace file per conversation.
	for _, conv := range []struct{ id, name string }{
		{mine.ID, "report.csv"},
		{theirs.ID, "secret.txt"},
	} {
		dir := filepath.Join(os.Getenv("FLEET_WORKSPACE_ROOT"), conv.id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir workspace: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, conv.name), []byte("x"), 0o644); err != nil {
			t.Fatalf("write file: %v", err)
		}
	}

	// Chat list: only the caller's conversation.
	w := getProjectSub(t, srv, owner, proj.ID, "conversations")
	if w.Code != 200 {
		t.Fatalf("conversations: status %d body %s", w.Code, w.Body.String())
	}
	var convResp struct {
		Conversations []struct {
			ID string `json:"id"`
		} `json:"conversations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &convResp); err != nil {
		t.Fatalf("conversations json: %v", err)
	}
	if len(convResp.Conversations) != 1 || convResp.Conversations[0].ID != mine.ID {
		t.Errorf("conversations = %+v, want only %s", convResp.Conversations, mine.ID)
	}

	// Sources: only the caller's file, with the fields the UI needs.
	w = getProjectSub(t, srv, owner, proj.ID, "files")
	if w.Code != 200 {
		t.Fatalf("files: status %d body %s", w.Code, w.Body.String())
	}
	var fileResp struct {
		Files []struct {
			ConversationID string `json:"conversation_id"`
			Path           string `json:"path"`
			Name           string `json:"name"`
			Size           int64  `json:"size"`
		} `json:"files"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &fileResp); err != nil {
		t.Fatalf("files json: %v", err)
	}
	if len(fileResp.Files) != 1 {
		t.Fatalf("files = %+v, want exactly the caller's one", fileResp.Files)
	}
	f := fileResp.Files[0]
	if f.ConversationID != mine.ID || f.Name != "report.csv" || f.Path != "report.csv" || f.Size != 1 {
		t.Errorf("file = %+v, want report.csv in %s", f, mine.ID)
	}

	// Non-member: the whole subresource tree 404s (projectForMember).
	if w := getProjectSub(t, srv, "stranger@x.com", proj.ID, "files"); w.Code != 404 {
		t.Errorf("non-member files: status %d, want 404", w.Code)
	}
}
