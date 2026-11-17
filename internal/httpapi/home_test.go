package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/ElcanoTek/fleet/internal/store"
)

// decodeHome reads a /me/home answer, or the "home" field of GET /conversations.
func decodeHome(t *testing.T, body []byte) *store.HomeConversation {
	t.Helper()
	var out struct {
		Home *store.HomeConversation `json:"home"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode home: %v (%s)", err, body)
	}
	return out.Home
}

// /me/home end to end through the routed mux: none, set a teammate's shared
// chat, read it back (also from the list), refuse an unreadable chat with the
// same 404 every read gives, clear.
func TestMyHomeRoute(t *testing.T) {
	t.Parallel()

	f := newTeamHTTPFixture(t)
	h := f.srv.Routes()

	w := do(t, h, http.MethodGet, "/me/home", nil, "bob@x.com")
	if w.Code != http.StatusOK || decodeHome(t, w.Body.Bytes()) != nil {
		t.Fatalf("fresh GET /me/home = %d %s, want 200 with no home", w.Code, w.Body.String())
	}

	w = do(t, h, http.MethodPut, "/me/home", map[string]any{"conversation_id": f.chat.ID}, "bob@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("PUT /me/home = %d %s", w.Code, w.Body.String())
	}
	want := store.HomeConversation{ConversationID: f.chat.ID, Access: store.HomeAccessTeamView, Title: "Spread study", OwnerEmail: "alice@x.com"}
	if got := decodeHome(t, w.Body.Bytes()); got == nil || *got != want {
		t.Fatalf("PUT answer = %+v, want %+v", got, want)
	}

	w = do(t, h, http.MethodGet, "/conversations", nil, "bob@x.com")
	if got := decodeHome(t, w.Body.Bytes()); w.Code != http.StatusOK || got == nil || *got != want {
		t.Errorf("GET /conversations home = %+v (%d), want %+v", got, w.Code, want)
	}
	w = do(t, h, http.MethodGet, "/conversations?archived=true", nil, "bob@x.com")
	var archived map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &archived); err != nil {
		t.Fatal(err)
	}
	if _, ok := archived["home"]; ok {
		t.Error("the archived list must not carry home")
	}

	// Outside the team: the same 404 as any unknown chat.
	for _, id := range []string{f.chat.ID, "no-such-chat"} {
		w = do(t, h, http.MethodPut, "/me/home", map[string]any{"conversation_id": id}, "zoe@x.com")
		if w.Code != http.StatusNotFound {
			t.Errorf("outsider PUT %s = %d, want 404", id, w.Code)
		}
	}
	w = do(t, h, http.MethodPut, "/me/home", map[string]any{"conversation_id": " "}, "bob@x.com")
	if w.Code != http.StatusBadRequest {
		t.Errorf("blank conversation_id = %d, want 400", w.Code)
	}

	w = do(t, h, http.MethodDelete, "/me/home", nil, "bob@x.com")
	if w.Code != http.StatusOK || decodeHome(t, w.Body.Bytes()) != nil {
		t.Fatalf("DELETE /me/home = %d %s", w.Code, w.Body.String())
	}
	w = do(t, h, http.MethodGet, "/me/home", nil, "bob@x.com")
	if decodeHome(t, w.Body.Bytes()) != nil {
		t.Errorf("after DELETE = %s, want no home", w.Body.String())
	}
	w = do(t, h, http.MethodPost, "/me/home", nil, "bob@x.com")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /me/home = %d, want 405", w.Code)
	}
}

// A read-only viewer may read a Home but not choose one, like every other
// per-person setting behind rejectViewerWrites.
func TestMyHomeRouteViewerIsReadOnly(t *testing.T) {
	t.Parallel()

	f := newTeamHTTPFixture(t)
	f.srv.isMember = nil // the real store gate, which reads the role
	setRole(t, f.srv, "bob@x.com", store.RoleViewer, "quant")
	h := f.srv.Routes()
	if w := do(t, h, http.MethodGet, "/me/home", nil, "bob@x.com"); w.Code != http.StatusOK {
		t.Errorf("viewer GET = %d, want 200", w.Code)
	}
	if w := do(t, h, http.MethodPut, "/me/home", map[string]any{"conversation_id": f.chat.ID}, "bob@x.com"); w.Code != http.StatusForbidden {
		t.Errorf("viewer PUT = %d, want 403", w.Code)
	}
}
