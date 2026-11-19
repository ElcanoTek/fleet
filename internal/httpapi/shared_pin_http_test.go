package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/store"
)

// tooManyPinsStore refuses every pin on somebody else's chat as past the cap.
type tooManyPinsStore struct{ chatStore }

func (tooManyPinsStore) SetSharedPin(context.Context, string, string, bool) error {
	return store.ErrTooManySharedPins
}

// A pin past the per-person cap is a 409 with the reason, not a 404 or a 500.
func TestConversationPin_PastTheCapIsAConflict(t *testing.T) {
	t.Parallel()

	f := newTeamHTTPFixture(t)
	f.srv.store = tooManyPinsStore{f.srv.store}
	w := convSub(t, f.srv, http.MethodPost, "bob@x.com", f.chat.ID, "pin", `{"pinned":true}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "too many pinned chats") {
		t.Errorf("pin past the cap = %d %q, want 409 naming the reason", w.Code, w.Body.String())
	}
}

// POST /conversations/{id}/pin on a teammate's shared chat pins it for the
// caller only: their shared listing reports it pinned, the owner's chat is
// not pinned, another team gets the 404 a wrong ID gets, and the pinned
// order accepts the chat.
func TestConversationPin_TeammatesSharedChat(t *testing.T) {
	t.Parallel()

	f := newTeamHTTPFixture(t)
	sharedPinned := func(user string) map[string]bool {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/conversations?scope=shared", nil)
		req = req.WithContext(asUser(req.Context(), user))
		f.srv.listOrCreateConversations(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET ?scope=shared as %s = %d %s", user, w.Code, w.Body.String())
		}
		var out struct {
			Conversations []struct {
				ID     string `json:"id"`
				Pinned bool   `json:"pinned"`
			} `json:"conversations"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, c := range out.Conversations {
			got[c.ID] = c.Pinned
		}
		return got
	}

	if got := sharedPinned("bob@x.com"); len(got) != 1 || got[f.chat.ID] {
		t.Fatalf("before pinning, bob's shared list = %v; want the chat, unpinned", got)
	}
	if w := convSub(t, f.srv, http.MethodPost, "bob@x.com", f.chat.ID, "pin", `{"pinned":true}`); w.Code != http.StatusNoContent {
		t.Fatalf("teammate pin = %d %s, want 204", w.Code, w.Body.String())
	}
	if got := sharedPinned("bob@x.com"); !got[f.chat.ID] {
		t.Errorf("after pinning, bob's shared list = %v; want the chat pinned", got)
	}
	owned, err := f.st.Get(f.ctx, "alice@x.com", f.chat.ID)
	if err != nil {
		t.Fatal(err)
	}
	if owned.Pinned {
		t.Error("a teammate's pin pinned the chat for its owner")
	}

	// Another team cannot pin it, and learns nothing from trying.
	missing := convSub(t, f.srv, http.MethodPost, "zoe@x.com", "no-such-chat", "pin", `{"pinned":true}`)
	other := convSub(t, f.srv, http.MethodPost, "zoe@x.com", f.chat.ID, "pin", `{"pinned":true}`)
	if other.Code != http.StatusNotFound || missing.Code != other.Code || missing.Body.String() != other.Body.String() {
		t.Errorf("outsider pin = %d %q, unknown ID = %d %q; want the same 404",
			other.Code, other.Body.String(), missing.Code, missing.Body.String())
	}

	// The pinned order keeps the teammate's chat bob pinned.
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/conversations/order",
		strings.NewReader(`{"scope":"pinned","conversation_ids":["`+f.chat.ID+`"]}`))
	req = req.WithContext(asUser(req.Context(), "bob@x.com"))
	f.srv.conversationByID(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"conversation_ids":["`+f.chat.ID+`"]`) {
		t.Errorf("PUT order = %d %s; want the shared pin kept", w.Code, w.Body.String())
	}

	if w := convSub(t, f.srv, http.MethodPost, "bob@x.com", f.chat.ID, "pin", `{"pinned":false}`); w.Code != http.StatusNoContent {
		t.Fatalf("teammate unpin = %d %s, want 204", w.Code, w.Body.String())
	}
	if got := sharedPinned("bob@x.com"); got[f.chat.ID] {
		t.Errorf("after unpinning, bob's shared list = %v", got)
	}

	// The owner's own pin still works, and still does not show for bob.
	if w := convSub(t, f.srv, http.MethodPost, "alice@x.com", f.chat.ID, "pin", `{"pinned":true}`); w.Code != http.StatusNoContent {
		t.Fatalf("owner pin = %d %s, want 204", w.Code, w.Body.String())
	}
	if got := sharedPinned("bob@x.com"); got[f.chat.ID] {
		t.Errorf("the owner's pin shows in a teammate's shared list: %v", got)
	}
}
