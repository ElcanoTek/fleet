package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// doRaw is do with a raw (possibly malformed) body.
func doRaw(t *testing.T, h http.Handler, method, path, body, user string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(gatewayContext(path, user), method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

type sidebarGet struct {
	Layout           json.RawMessage `json:"layout"`
	WorkspaceDefault json.RawMessage `json:"workspace_default"`
}

func decodeSidebar[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status %d (body %s)", w.Code, w.Body.String())
	}
	var out T
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	return out
}

const fullDefaults = `"sections":[{"id":"projects"},{"id":"chats"},{"id":"team"}]`

// TestSidebar_UserLayout drives /sidebar through the real routes: a fresh
// user has no layout and no workspace default; a PUT saves and answers the
// normalized layout (built-ins and sections filled in, a hidden home
// cleared); GET reads it back with the workspace default beside it; another
// user does not see it; bad input is a 400 that saves nothing; DELETE resets;
// an unsupported method is a 405 naming the allowed ones.
func TestSidebar_UserLayout(t *testing.T) {
	t.Parallel()

	s := memberFixture(t, "me@x.com", "other@x.com", "boss@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	h := s.Routes()

	got := decodeSidebar[sidebarGet](t, do(t, h, http.MethodGet, "/sidebar", nil, "me@x.com"))
	if string(got.Layout) != "null" || string(got.WorkspaceDefault) != "null" {
		t.Fatalf("fresh GET = %+v; want both null", got)
	}

	put := `{"version":1,"items":[{"id":"s1","kind":"chat","ref":"c-1","title":"  Board ","icon":"🚀","view":"dashboard"},{"id":"tasks","kind":"builtin","hidden":true}],"home":"tasks","compact":true}`
	want := `{"version":1,"items":[{"id":"s1","kind":"chat","ref":"c-1","title":"Board","icon":"🚀","view":"dashboard"},{"id":"tasks","kind":"builtin","hidden":true},{"id":"chat","kind":"builtin"},{"id":"patrols","kind":"builtin"}],` + fullDefaults + `,"home":"","compact":true}`
	saved := decodeSidebar[sidebarGet](t, doRaw(t, h, http.MethodPut, "/sidebar", put, "me@x.com"))
	if string(saved.Layout) != want {
		t.Errorf("PUT layout =\n %s\nwant\n %s", saved.Layout, want)
	}
	got = decodeSidebar[sidebarGet](t, do(t, h, http.MethodGet, "/sidebar", nil, "me@x.com"))
	if string(got.Layout) != want {
		t.Errorf("GET layout =\n %s\nwant\n %s", got.Layout, want)
	}
	if other := decodeSidebar[sidebarGet](t, do(t, h, http.MethodGet, "/sidebar", nil, "other@x.com")); string(other.Layout) != "null" {
		t.Errorf("other user's layout = %s; want null", other.Layout)
	}

	// The workspace default shows up beside the user's own.
	ws := `{"version":1,"items":[{"id":"x","kind":"page","ref":"help"}]}`
	if w := doRaw(t, h, http.MethodPut, "/admin/sidebar", ws, "boss@x.com"); w.Code != http.StatusOK {
		t.Fatalf("admin PUT = %d %s", w.Code, w.Body.String())
	}
	got = decodeSidebar[sidebarGet](t, do(t, h, http.MethodGet, "/sidebar", nil, "other@x.com"))
	if string(got.Layout) != "null" || !strings.Contains(string(got.WorkspaceDefault), `{"id":"x","kind":"page","ref":"help"}`) {
		t.Errorf("GET with a workspace default = %+v", got)
	}

	for name, body := range map[string]string{
		"bad json":       `{"version":`,
		"wrong version":  `{"version":2,"items":[]}`,
		"unknown kind":   `{"version":1,"items":[{"id":"a","kind":"link","ref":"x"}]}`,
		"builtin reused": `{"version":1,"items":[{"id":"chat","kind":"chat","ref":"c1"}]}`,
		"bad icon":       `{"version":1,"items":[{"id":"a","kind":"chat","ref":"c1","icon":"rocket"}]}`,
		"too large":      `{"version":1,"items":[{"id":"a","kind":"chat","ref":"c1","title":"` + strings.Repeat("x", 70<<10) + `"}]}`,
	} {
		w := doRaw(t, h, http.MethodPut, "/sidebar", body, "me@x.com")
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: PUT = %d %s; want 400", name, w.Code, w.Body.String())
		}
	}
	if w := doRaw(t, h, http.MethodPut, "/sidebar", `{"version":1,"items":[{"id":"a","kind":"link","ref":"x"}]}`, "me@x.com"); !strings.Contains(w.Body.String(), `unknown kind "link"`) {
		t.Errorf("400 body = %q; want the validation message", w.Body.String())
	}
	if got := decodeSidebar[sidebarGet](t, do(t, h, http.MethodGet, "/sidebar", nil, "me@x.com")); string(got.Layout) != want {
		t.Errorf("a refused PUT changed the layout: %s", got.Layout)
	}

	del := decodeSidebar[sidebarGet](t, do(t, h, http.MethodDelete, "/sidebar", nil, "me@x.com"))
	if string(del.Layout) != "null" {
		t.Errorf("DELETE = %s; want null", del.Layout)
	}
	if got := decodeSidebar[sidebarGet](t, do(t, h, http.MethodGet, "/sidebar", nil, "me@x.com")); string(got.Layout) != "null" {
		t.Errorf("GET after DELETE = %s; want null", got.Layout)
	}

	w := do(t, h, http.MethodPost, "/sidebar", nil, "me@x.com")
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, PUT, DELETE" {
		t.Errorf("POST = %d Allow=%q; want 405 GET, PUT, DELETE", w.Code, w.Header().Get("Allow"))
	}
}

type adminSidebarGet struct {
	Layout    json.RawMessage `json:"layout"`
	UpdatedBy string          `json:"updated_by"`
	UpdatedAt int64           `json:"updated_at"`
}

// TestSidebar_AdminWorkspaceDefault: /admin/sidebar is admin-only; an admin
// saves, reads (with author and time) and resets the workspace default.
func TestSidebar_AdminWorkspaceDefault(t *testing.T) {
	t.Parallel()

	s := memberFixture(t, "boss@x.com", "peon@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	h := s.Routes()

	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		if w := doRaw(t, h, m, "/admin/sidebar", `{"version":1}`, "peon@x.com"); w.Code != http.StatusForbidden {
			t.Errorf("%s as member = %d; want 403", m, w.Code)
		}
	}

	got := decodeSidebar[adminSidebarGet](t, do(t, h, http.MethodGet, "/admin/sidebar", nil, "boss@x.com"))
	if string(got.Layout) != "null" || got.UpdatedBy != "" || got.UpdatedAt != 0 {
		t.Fatalf("fresh GET = %+v", got)
	}
	put := `{"version":1,"sections":[{"id":"team","label":"Crew","hidden":true}],"home":"chat"}`
	want := `{"version":1,"items":[{"id":"chat","kind":"builtin"},{"id":"patrols","kind":"builtin"},{"id":"tasks","kind":"builtin"}],"sections":[{"id":"team","label":"Crew","hidden":true},{"id":"projects"},{"id":"chats"}],"home":"chat"}`
	saved := decodeSidebar[adminSidebarGet](t, doRaw(t, h, http.MethodPut, "/admin/sidebar", put, "boss@x.com"))
	if string(saved.Layout) != want || saved.UpdatedBy != "boss@x.com" || saved.UpdatedAt == 0 {
		t.Errorf("PUT = %+v (layout %s)", saved, saved.Layout)
	}
	got = decodeSidebar[adminSidebarGet](t, do(t, h, http.MethodGet, "/admin/sidebar", nil, "boss@x.com"))
	if string(got.Layout) != want || got.UpdatedBy != "boss@x.com" || got.UpdatedAt != saved.UpdatedAt {
		t.Errorf("GET = %+v (layout %s)", got, got.Layout)
	}
	if w := doRaw(t, h, http.MethodPut, "/admin/sidebar", `{"version":1,"sections":[{"id":"pinned"}]}`, "boss@x.com"); w.Code != http.StatusBadRequest {
		t.Errorf("bad section = %d; want 400", w.Code)
	}

	del := decodeSidebar[adminSidebarGet](t, do(t, h, http.MethodDelete, "/admin/sidebar", nil, "boss@x.com"))
	if string(del.Layout) != "null" || del.UpdatedBy != "" || del.UpdatedAt != 0 {
		t.Errorf("DELETE = %+v", del)
	}
	if got := decodeSidebar[adminSidebarGet](t, do(t, h, http.MethodGet, "/admin/sidebar", nil, "boss@x.com")); string(got.Layout) != "null" {
		t.Errorf("GET after DELETE = %s", got.Layout)
	}
	if w := do(t, h, http.MethodPatch, "/admin/sidebar", nil, "boss@x.com"); w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, PUT, DELETE" {
		t.Errorf("PATCH = %d Allow=%q", w.Code, w.Header().Get("Allow"))
	}
}

// TestSidebar_StaleRowReadsAsAbsent: a stored row that no longer passes the
// contract (a newer version, a removed icon) reads as null, never a 500.
func TestSidebar_StaleRowReadsAsAbsent(t *testing.T) {
	t.Parallel()

	s := memberFixture(t, "me@x.com", "boss@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	h := s.Routes()
	st := s.concreteStore(t)
	ctx := context.Background()
	if err := st.SetSidebarLayout(ctx, "me@x.com", []byte(`{"version":2,"items":[]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetWorkspaceSidebarLayout(ctx, []byte(`{"version":1,"items":[{"id":"a","kind":"chat","ref":"c","icon":"gone"}]}`), "boss@x.com"); err != nil {
		t.Fatal(err)
	}
	got := decodeSidebar[sidebarGet](t, do(t, h, http.MethodGet, "/sidebar", nil, "me@x.com"))
	if string(got.Layout) != "null" || string(got.WorkspaceDefault) != "null" {
		t.Errorf("GET with stale rows = %+v; want both null", got)
	}
	ws := decodeSidebar[adminSidebarGet](t, do(t, h, http.MethodGet, "/admin/sidebar", nil, "boss@x.com"))
	if string(ws.Layout) != "null" || ws.UpdatedBy != "" {
		t.Errorf("admin GET with a stale row = %+v; want null", ws)
	}
}

// TestSidebar_WorkspaceDefaultIsRedactedForMembers: an admin's pin to one of
// their own chats keeps its captured title on the admin read, but a member's
// GET /sidebar never carries it (the rail names a workspace pin only from the
// member's own lists, once they can open it), and a background task cannot
// be pinned in the workspace default at all.
func TestSidebar_WorkspaceDefaultIsRedactedForMembers(t *testing.T) {
	t.Parallel()

	s := memberFixture(t, "boss@x.com", "peon@x.com")
	setRole(t, s, "boss@x.com", "admin", "")
	h := s.Routes()

	put := `{"version":1,"items":[{"id":"s-1","kind":"chat","ref":"c1","title":"Board comp review","label":"Everyone's board"}],"sections":[]}`
	if w := doRaw(t, h, http.MethodPut, "/admin/sidebar", put, "boss@x.com"); w.Code != http.StatusOK {
		t.Fatalf("admin PUT = %d %s", w.Code, w.Body.String())
	}
	admin := decodeSidebar[adminSidebarGet](t, do(t, h, http.MethodGet, "/admin/sidebar", nil, "boss@x.com"))
	if !strings.Contains(string(admin.Layout), `"title":"Board comp review"`) {
		t.Errorf("admin read lost the title: %s", admin.Layout)
	}
	w := do(t, h, http.MethodGet, "/sidebar", nil, "peon@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("member GET = %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "Board comp review") {
		t.Errorf("member GET leaked the captured title: %s", body)
	}
	if !strings.Contains(body, `"ref":"c1"`) || !strings.Contains(body, `"label":"Everyone's board"`) {
		t.Errorf("member GET lost the pin or the admin's label: %s", body)
	}

	task := `{"version":1,"items":[{"id":"s-2","kind":"task","ref":"t1","title":"Payroll export"}],"sections":[]}`
	if w := doRaw(t, h, http.MethodPut, "/admin/sidebar", task, "boss@x.com"); w.Code != http.StatusBadRequest {
		t.Errorf("workspace task pin = %d; want 400", w.Code)
	}
	// A person may still pin their own task in their own layout.
	if w := doRaw(t, h, http.MethodPut, "/sidebar", task, "peon@x.com"); w.Code != http.StatusOK {
		t.Errorf("own task pin = %d %s", w.Code, w.Body.String())
	}
}
