package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCreateConversation_IdempotencyKey pins POST /conversations with
// client_request_id: a retry with the same key (its reply was lost) returns
// the conversation the first create made instead of a second one; another key,
// or none, makes a new one; a key never answers with a deleted chat; and keys
// are per user.
func TestCreateConversation_IdempotencyKey(t *testing.T) {
	s := serverFixture(t)
	const me = "idem@x.com"
	create := func(body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/conversations", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, me))
		rec := httptest.NewRecorder()
		s.listOrCreateConversations(rec, req)
		var out struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out.ID
	}
	count := func() int {
		t.Helper()
		list, err := s.concreteStore(t).List(context.Background(), me, false)
		if err != nil {
			t.Fatal(err)
		}
		return len(list)
	}

	code, first := create(`{"title":"Patrol","client_request_id":"k-1"}`)
	if code != http.StatusOK || first == "" {
		t.Fatalf("first create = %d %q", code, first)
	}
	code, again := create(`{"title":"Patrol","client_request_id":"k-1"}`)
	if code != http.StatusOK || again != first {
		t.Fatalf("retry with the same key = %d %q, want 200 %q", code, again, first)
	}
	if n := count(); n != 1 {
		t.Fatalf("conversations after a keyed retry = %d, want 1", n)
	}
	if _, other := create(`{"title":"Patrol","client_request_id":"k-2"}`); other == first {
		t.Fatal("a different key returned the same conversation")
	}
	if _, plain := create(`{"title":"Patrol"}`); plain == first {
		t.Fatal("no key returned the keyed conversation")
	}

	// The keyed chat is deleted (rolled back): the key never answers with the
	// deleted chat. Where deletes are soft the row keeps its ID and the key is
	// refused (409, "use a new key"); where they are hard the ID is free again
	// and the key makes a live chat.
	if err := s.store.Delete(context.Background(), me, first); err != nil {
		t.Fatal(err)
	}
	switch code, id := create(`{"title":"Patrol","client_request_id":"k-1"}`); code {
	case http.StatusConflict:
	case http.StatusOK:
		if conv, err := s.store.Get(context.Background(), me, id); err != nil || conv == nil {
			t.Fatalf("after the keyed chat was deleted, the key answered with %q, which is not a live chat", id)
		}
	default:
		t.Fatalf("after the keyed chat was deleted, the key answered %d", code)
	}

	// Another user's identical key is theirs alone.
	req := httptest.NewRequest(http.MethodPost, "/conversations", strings.NewReader(`{"title":"P","client_request_id":"k-2"}`))
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, "other@x.com"))
	rec := httptest.NewRecorder()
	s.listOrCreateConversations(rec, req)
	var theirs struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &theirs)
	if rec.Code != http.StatusOK || theirs.ID == "" {
		t.Fatalf("other user's create = %d %s", rec.Code, rec.Body.String())
	}
	if _, mine := create(`{"title":"Patrol","client_request_id":"k-2"}`); mine == theirs.ID {
		t.Fatal("the same key from two users gave the same chat")
	}

	if code, _ := create(`{"title":"x","client_request_id":"` + strings.Repeat("a", 129) + `"}`); code != http.StatusBadRequest {
		t.Errorf("over-long key = %d, want 400", code)
	}
}
