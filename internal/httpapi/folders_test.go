package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/store"
)

// folderTestUser is the single owner used across the folder/label httpapi tests.
const folderTestUser = "alice@x.com"

// listCount issues GET /conversations<query> as folderTestUser and returns how
// many conversations came back.
func listCount(t *testing.T, h http.Handler, query string) int {
	t.Helper()
	rec := do(t, h, http.MethodGet, "/conversations"+query, nil, folderTestUser)
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

// TestFolders_EndToEnd drives the #258 surface through the mux: assign folders +
// labels via bulk PATCH, filter GET /conversations by ?folder= / ?label= (AND),
// enumerate GET /folders, and rename via POST /folders/rename.
func TestFolders_EndToEnd(t *testing.T) {
	s := serverFixture(t)
	st := s.concreteStore(t)
	ctx := context.Background()
	h := s.Routes()
	const u = "alice@x.com"

	mk := func(title string) string {
		c, err := st.CreateConversation(ctx, u, title, "victoria", "m", false)
		if err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
		return c.ID
	}
	w1, w2, r1 := mk("w1"), mk("w2"), mk("r1")
	mk("none") // stays in the no-folder bucket

	patch := func(id, folder string, labels []string) {
		body := map[string]any{
			"conversation_ids": []string{id},
			"changes":          map[string]any{"folder": folder, "labels": labels},
		}
		if rec := do(t, h, http.MethodPatch, "/conversations/bulk", body, u); rec.Code != http.StatusOK {
			t.Fatalf("patch %s: got %d (%s)", id, rec.Code, rec.Body.String())
		}
	}
	patch(w1, "Work", []string{"go", "urgent"})
	patch(w2, "Work", []string{"go"})
	patch(r1, "Research", []string{"urgent"})

	if n := listCount(t, h, "?folder=Work"); n != 2 {
		t.Errorf("?folder=Work: got %d, want 2", n)
	}
	if n := listCount(t, h, "?label=go"); n != 2 {
		t.Errorf("?label=go: got %d, want 2", n)
	}
	if n := listCount(t, h, "?label=go&label=urgent"); n != 1 {
		t.Errorf("?label=go&label=urgent (AND): got %d, want 1", n)
	}
	if n := listCount(t, h, ""); n != 4 {
		t.Errorf("no filter: got %d, want 4", n)
	}

	// GET /folders → Research:1, Work:2 (no-folder bucket excluded).
	rec := do(t, h, http.MethodGet, "/folders", nil, u)
	if rec.Code != http.StatusOK {
		t.Fatalf("/folders: got %d (%s)", rec.Code, rec.Body.String())
	}
	var fresp struct {
		Folders []store.FolderCount `json:"folders"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &fresp); err != nil {
		t.Fatalf("decode folders: %v", err)
	}
	counts := map[string]int{}
	for _, fc := range fresp.Folders {
		counts[fc.Name] = fc.Count
	}
	if counts["Work"] != 2 || counts["Research"] != 1 || len(fresp.Folders) != 2 {
		t.Errorf("/folders = %+v, want Work:2 Research:1", fresp.Folders)
	}

	// Rename Work → Client; its conversations follow.
	if rec := do(t, h, http.MethodPost, "/folders/rename", map[string]string{"from": "Work", "to": "Client"}, u); rec.Code != http.StatusOK {
		t.Fatalf("rename: got %d (%s)", rec.Code, rec.Body.String())
	}
	if n := listCount(t, h, "?folder=Client"); n != 2 {
		t.Errorf("after rename ?folder=Client: got %d, want 2", n)
	}
	if n := listCount(t, h, "?folder=Work"); n != 0 {
		t.Errorf("after rename ?folder=Work: got %d, want 0", n)
	}
}

// TestBulkPatch_FolderLabelValidation: the HTTP layer bounds folder/label inputs
// (#258) before any store write.
func TestBulkPatch_FolderLabelValidation(t *testing.T) {
	s := serverFixture(t)
	st := s.concreteStore(t)
	ctx := context.Background()
	h := s.Routes()
	const u = "alice@x.com"
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
		{"folder too long", map[string]any{"folder": strings.Repeat("x", 65)}},
	}
	for _, tc := range cases {
		if got := code(tc.changes); got != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", tc.name, got)
		}
	}
	// A valid patch (trimmed, within bounds) still succeeds and is normalized.
	if got := code(map[string]any{"folder": "  Work  ", "labels": []string{" go "}}); got != http.StatusOK {
		t.Fatalf("valid patch: got %d, want 200", got)
	}
	conv, _ := st.Get(ctx, u, c.ID)
	if conv.Folder != "Work" || len(conv.Labels) != 1 || conv.Labels[0] != "go" {
		t.Errorf("normalization failed: folder=%q labels=%v", conv.Folder, conv.Labels)
	}
}
