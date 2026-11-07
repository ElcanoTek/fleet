package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// TestConversationOrder drives GET/PUT /conversations/order through the
// /conversations/ dispatcher: a fresh user has no orders, a PUT keeps only the
// caller's own chats in the scope in the order given, the GET reads it back
// by scope, and a bad scope, bad JSON or an unsupported method is refused.
func TestConversationOrder(t *testing.T) {
	t.Parallel()

	s := serverFixture(t)
	ctx := context.Background()
	const me = "rail@x.com"
	a := seedConv(t, s, me)
	b := seedConv(t, s, me)
	theirs := seedConv(t, s, "someone@x.com")
	for _, id := range []string{a.ID, b.ID} {
		if err := s.store.SetPinned(ctx, me, id, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.store.SetPinned(ctx, "someone@x.com", theirs.ID, true); err != nil {
		t.Fatal(err)
	}

	do := func(method, body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(method, "/conversations/order", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, me))
		rec := httptest.NewRecorder()
		s.conversationByID(rec, req)
		return rec.Code, rec.Body.String()
	}
	orders := func() map[string][]string {
		t.Helper()
		code, body := do(http.MethodGet, "")
		if code != http.StatusOK {
			t.Fatalf("GET = %d %s", code, body)
		}
		var out struct {
			Orders map[string][]string `json:"orders"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		return out.Orders
	}

	if got := orders(); len(got) != 0 {
		t.Fatalf("fresh orders = %v; want none", got)
	}
	code, body := do(http.MethodPut, `{"scope":"pinned","conversation_ids":["`+b.ID+`","`+theirs.ID+`","`+a.ID+`"]}`)
	if code != http.StatusOK || !strings.Contains(body, `"conversation_ids":["`+b.ID+`","`+a.ID+`"]`) {
		t.Fatalf("PUT = %d %s", code, body)
	}
	if got := orders(); !reflect.DeepEqual(got["pinned"], []string{b.ID, a.ID}) {
		t.Fatalf("GET after PUT = %v", got)
	}
	if code, _ := do(http.MethodPut, `{"scope":"everything","conversation_ids":[]}`); code != http.StatusBadRequest {
		t.Errorf("bad scope = %d, want 400", code)
	}
	if code, _ := do(http.MethodPut, `{"scope":`); code != http.StatusBadRequest {
		t.Errorf("bad json = %d, want 400", code)
	}
	if code, _ := do(http.MethodDelete, ""); code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE = %d, want 405", code)
	}
}
