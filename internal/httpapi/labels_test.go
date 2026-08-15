package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// labelTestUser is the single owner used across the label httpapi tests.
const labelTestUser = "alice@x.com"

// listCount issues GET /conversations<query> as labelTestUser and returns how
// many conversations came back.
func listCount(t *testing.T, h http.Handler, query string) int {
	t.Helper()
	rec := do(t, h, http.MethodGet, "/conversations"+query, nil, labelTestUser)
	if rec.Code != http.StatusOK {
		t.Fatalf("list %q: got %d (%s)", query, rec.Code, rec.Body.String())
	}
	var resp struct {
		Conversations []map[string]any `json:"conversations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	return len(resp.Conversations)
}

// TestLabels_EndToEnd drives the #258 surface through the mux: assign labels via
// bulk PATCH, then filter GET /conversations by ?label= with AND semantics.
func TestLabels_EndToEnd(t *testing.T) {
	s := serverFixture(t)
	st := s.concreteStore(t)
	ctx := context.Background()
	h := s.Routes()
	const u = labelTestUser

	mk := func(title string) string {
		c, err := st.CreateConversation(ctx, u, title, "victoria", "m", false)
		if err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
		return c.ID
	}
	w1, w2, r1 := mk("w1"), mk("w2"), mk("r1")
	mk("none") // stays unlabeled

	patch := func(id string, labels []string) {
		body := map[string]any{
			"conversation_ids": []string{id},
			"changes":          map[string]any{"labels": labels},
		}
		if rec := do(t, h, http.MethodPatch, "/conversations/bulk", body, u); rec.Code != http.StatusOK {
			t.Fatalf("patch %s: got %d (%s)", id, rec.Code, rec.Body.String())
		}
	}
	patch(w1, []string{"go", "urgent"})
	patch(w2, []string{"go"})
	patch(r1, []string{"urgent"})

	if n := listCount(t, h, "?label=go"); n != 2 {
		t.Errorf("?label=go: got %d, want 2", n)
	}
	if n := listCount(t, h, "?label=go&label=urgent"); n != 1 {
		t.Errorf("?label=go&label=urgent (AND): got %d, want 1", n)
	}
	if n := listCount(t, h, ""); n != 4 {
		t.Errorf("no filter: got %d, want 4", n)
	}
}

// TestBulkPatch_LabelValidation: the HTTP layer bounds label inputs (#258)
// before any store write.
func TestBulkPatch_LabelValidation(t *testing.T) {
	s := serverFixture(t)
	st := s.concreteStore(t)
	ctx := context.Background()
	h := s.Routes()
	const u = labelTestUser
	c, err := st.CreateConversation(ctx, u, "c", "victoria", "m", false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	code := func(changes map[string]any) int {
		body := map[string]any{"conversation_ids": []string{c.ID}, "changes": changes}
		return do(t, h, http.MethodPatch, "/conversations/bulk", body, u).Code
	}
	many := make([]string, 11)
	for i := range many {
		many[i] = "l"
	}
	cases := []struct {
		name    string
		changes map[string]any
	}{
		{"too many labels", map[string]any{"labels": many}},
		{"label too long", map[string]any{"labels": []string{strings.Repeat("x", 33)}}},
		{"empty label", map[string]any{"labels": []string{"   "}}},
	}
	for _, tc := range cases {
		if got := code(tc.changes); got != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", tc.name, got)
		}
	}
	// A valid patch (trimmed, within bounds) still succeeds and is normalized.
	if got := code(map[string]any{"labels": []string{" go "}}); got != http.StatusOK {
		t.Fatalf("valid patch: got %d, want 200", got)
	}
	conv, _ := st.Get(ctx, u, c.ID)
	if len(conv.Labels) != 1 || conv.Labels[0] != "go" {
		t.Errorf("normalization failed: labels=%v", conv.Labels)
	}
}
