package httpapi

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/store"
)

// postProject issues POST /conversations/{id}/project as user with the given
// body and returns the recorder.
func postProject(t *testing.T, srv *Server, user, convID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/conversations/"+convID+"/project", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, user))
	w := httptest.NewRecorder()
	srv.conversationByID(w, req)
	return w
}

// A conversation can be re-filed into a project the caller is a member of,
// and unfiled again with an empty project_id (#509 follow-up). Before the
// re-file endpoint, project_id was set-once at creation, so the sidebar's
// drag-a-chat-into-a-project flow had nothing to call.
func TestConversationProjectRefileAndUnfile(t *testing.T) {
	srv := serverFixture(t)
	st := srv.store.(*store.Store)
	ctx := context.Background()
	const user = "u@x.com"

	proj, err := st.CreateProject(ctx, &store.Project{OwnerEmail: user, Name: "Growth"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	conv, err := st.CreateConversation(ctx, user, "t", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	if w := postProject(t, srv, user, conv.ID, `{"project_id":"`+proj.ID+`"}`); w.Code != 204 {
		t.Fatalf("re-file: status %d body %s", w.Code, w.Body.String())
	}
	got, err := st.Get(ctx, user, conv.ID)
	if err != nil || got == nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ProjectID != proj.ID {
		t.Errorf("ProjectID = %q, want %q", got.ProjectID, proj.ID)
	}

	if w := postProject(t, srv, user, conv.ID, `{"project_id":""}`); w.Code != 204 {
		t.Fatalf("unfile: status %d body %s", w.Code, w.Body.String())
	}
	got, err = st.Get(ctx, user, conv.ID)
	if err != nil || got == nil {
		t.Fatalf("Get after unfile: %v", err)
	}
	if got.ProjectID != "" {
		t.Errorf("ProjectID after unfile = %q, want empty", got.ProjectID)
	}
}

// A project the caller is not a member of must be unreachable — 404 (not
// 403), mirroring projectForMember so project ids don't leak membership
// state. The conversation stays unfiled.
func TestConversationProjectRefileNonMember(t *testing.T) {
	srv := serverFixture(t)
	st := srv.store.(*store.Store)
	ctx := context.Background()
	const owner = "owner@x.com"
	const caller = "caller@x.com"

	proj, err := st.CreateProject(ctx, &store.Project{OwnerEmail: owner, Name: "Private"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	conv, err := st.CreateConversation(ctx, caller, "t", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	if w := postProject(t, srv, caller, conv.ID, `{"project_id":"`+proj.ID+`"}`); w.Code != 404 {
		t.Fatalf("non-member re-file: status %d, want 404 (body %s)", w.Code, w.Body.String())
	}
	got, err := st.Get(ctx, caller, conv.ID)
	if err != nil || got == nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ProjectID != "" {
		t.Errorf("ProjectID = %q, want empty — non-member assignment must not stick", got.ProjectID)
	}
}

// Re-filing someone else's conversation must fail even when the caller is a
// member of the target project: the store's owner-scoped UPDATE is the
// enforcement, and the victim's conversation stays untouched.
func TestConversationProjectRefileForeignConversation(t *testing.T) {
	srv := serverFixture(t)
	st := srv.store.(*store.Store)
	ctx := context.Background()
	const victim = "victim@x.com"
	const attacker = "attacker@x.com"

	proj, err := st.CreateProject(ctx, &store.Project{OwnerEmail: attacker, Name: "Mine"})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	conv, err := st.CreateConversation(ctx, victim, "t", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}

	if w := postProject(t, srv, attacker, conv.ID, `{"project_id":"`+proj.ID+`"}`); w.Code == 204 {
		t.Fatalf("foreign conversation re-file succeeded; want an error status")
	}
	got, err := st.Get(ctx, victim, conv.ID)
	if err != nil || got == nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ProjectID != "" {
		t.Errorf("victim ProjectID = %q, want empty", got.ProjectID)
	}
}
