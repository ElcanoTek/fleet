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

// TestConversationPinOrder drives GET/PUT /conversations/pin-order through
// the /conversations/ dispatcher: a fresh user has no order, a PUT keeps only
// the caller's own pinned chats in the order given, the GET reads it back,
// and an unsupported method is a 405.
func TestConversationPinOrder(t *testing.T) {
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

	do := func(method, body string) (int, []string) {
		t.Helper()
		req := httptest.NewRequest(method, "/conversations/pin-order", strings.NewReader(body))
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, me))
		rec := httptest.NewRecorder()
		s.conversationByID(rec, req)
		var out struct {
			IDs []string `json:"conversation_ids"`
		}
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode %q: %v", rec.Body.String(), err)
			}
		}
		return rec.Code, out.IDs
	}

	if code, ids := do(http.MethodGet, ""); code != http.StatusOK || len(ids) != 0 || ids == nil {
		t.Fatalf("fresh GET = %d %v; want 200 and []", code, ids)
	}
	code, ids := do(http.MethodPut, `{"conversation_ids":["`+b.ID+`","`+theirs.ID+`","`+a.ID+`"]}`)
	if want := []string{b.ID, a.ID}; code != http.StatusOK || !reflect.DeepEqual(ids, want) {
		t.Fatalf("PUT = %d %v; want 200 %v", code, ids, want)
	}
	if code, ids := do(http.MethodGet, ""); code != http.StatusOK || !reflect.DeepEqual(ids, []string{b.ID, a.ID}) {
		t.Fatalf("GET after PUT = %d %v", code, ids)
	}
	if code, _ := do(http.MethodPut, `{"conversation_ids":`); code != http.StatusBadRequest {
		t.Errorf("bad json = %d, want 400", code)
	}
	if code, _ := do(http.MethodDelete, ""); code != http.StatusMethodNotAllowed {
		t.Errorf("DELETE = %d, want 405", code)
	}
}
