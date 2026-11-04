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
// or none, makes a new one; and a key whose conversation was deleted (rolled
// back) makes a fresh one rather than pointing at a missing chat.
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
		list, err := s.store.List(context.Background(), me, false)
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

	// The keyed chat is rolled back: the same key now makes a fresh one.
	if err := s.store.Delete(context.Background(), me, first); err != nil {
		t.Fatal(err)
	}
	if _, fresh := create(`{"title":"Patrol","client_request_id":"k-1"}`); fresh == "" || fresh == first {
		t.Fatalf("after the keyed chat was deleted, the key returned %q (want a new chat)", fresh)
	}

	if code, _ := create(`{"title":"x","client_request_id":"` + strings.Repeat("a", 129) + `"}`); code != http.StatusBadRequest {
		t.Errorf("over-long key = %d, want 400", code)
	}
}
