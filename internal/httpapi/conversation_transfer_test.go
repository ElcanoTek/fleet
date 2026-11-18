package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// POST /conversations/{id}/transfer: the owner (or an admin) hands a chat to
// another person. The store test pins what moves; this pins who may ask, what
// each refusal answers, and the reconciliation that follows.
func TestConversationTransferRoute(t *testing.T) {
	t.Setenv("FLEET_WORKSPACE_ROOT", t.TempDir())

	st := newScheduleFakeStore()
	st.users["gone@x.com"] = &store.User{Email: "gone@x.com", Enabled: false}
	srv := newDefaultChatServer(t, &fakeEngine{}, st)
	defer srv.StopBackground()
	h := srv.Routes()
	ctx := context.Background()

	const owner, next, mate = "jeanne@elcanotek.com", "client@reklaim.com", "mate@elcanotek.com"
	conv, err := st.CreateConversation(ctx, owner, "Opti workbench", "victoria", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if _, err := st.SetConversationMultiplayer(ctx, owner, conv.ID, true, []string{mate}, nil); err != nil {
		t.Fatalf("SetConversationMultiplayer: %v", err)
	}
	skills := filepath.Join(tools.WorkspaceDirForConversation(conv.ID), userSkillsRoot, "mine")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatalf("mkdir skills: %v", err)
	}

	post := func(reqCtx context.Context, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/conversations/"+conv.ID+"/transfer", bytes.NewBufferString(body))
		req = req.WithContext(reqCtx)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// A participant is not the owner: the same 404 as a chat that is not there.
	if rec := post(asUser(ctx, mate), `{"to_email":"`+next+`"}`); rec.Code != http.StatusNotFound {
		t.Errorf("participant transfer = %d, want 404", rec.Code)
	}
	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"to themselves":    {`{"to_email":"` + owner + `"}`, http.StatusBadRequest},
		"no target":        {`{}`, http.StatusBadRequest},
		"disabled account": {`{"to_email":"gone@x.com"}`, http.StatusBadRequest},
	} {
		if rec := post(asUser(ctx, owner), tc.body); rec.Code != tc.want {
			t.Errorf("%s = %d, want %d: %s", name, rec.Code, tc.want, rec.Body.String())
		}
	}
	if c, _ := st.Get(ctx, owner, conv.ID); c == nil {
		t.Fatal("a refused transfer moved the chat")
	}

	rec := post(asUser(ctx, owner), `{"to_email":"`+next+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner transfer = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		OwnerEmail    string `json:"owner_email"`
		PreviousOwner string `json:"previous_owner"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.OwnerEmail != next || out.PreviousOwner != owner {
		t.Errorf("response = %s (err %v)", rec.Body.String(), err)
	}
	if c, _ := st.Get(ctx, next, conv.ID); c == nil || c.Multiplayer || c.TeamVisible {
		t.Errorf("the chat is not the new owner's private chat: %+v", c)
	}
	if _, err := os.Stat(filepath.Dir(skills)); !os.IsNotExist(err) {
		t.Errorf("the previous owner's skills are still in the workspace (err %v)", err)
	}
	// The previous owner no longer owns it, so asking again is a 404.
	if rec := post(asUser(ctx, owner), `{"to_email":"`+mate+`"}`); rec.Code != http.StatusNotFound {
		t.Errorf("previous owner's second transfer = %d, want 404", rec.Code)
	}
	// An admin may move any chat.
	if rec := post(asUserRole(ctx, "boss@elcanotek.com", store.RoleAdmin), `{"to_email":"`+mate+`"}`); rec.Code != http.StatusOK {
		t.Errorf("admin transfer = %d: %s", rec.Code, rec.Body.String())
	}
	if c, _ := st.Get(ctx, mate, conv.ID); c == nil {
		t.Error("the admin's transfer did not move the chat")
	}
}
