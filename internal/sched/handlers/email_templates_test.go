package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
)

func TestEmailTemplateLibraryListsBundleTemplates(t *testing.T) {
	t.Parallel()

	h, _ := setupTest(t)
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "previews"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"weekly.yaml":          "name: Weekly update\naudience: client\nsections:\n  - {id: summary, title: Summary, block: text, limits: {max_bullets: 3}}\n",
		"broken.yaml":          "name: [unclosed\n",
		"previews/weekly.html": "<!doctype html><p>Hi Team,</p>",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h.SetEmailTemplateCatalogProvider(
		func() ([]clientconfig.EmailTemplate, []string) { return clientconfig.ReadEmailTemplates(dir) },
		func(id string) (clientconfig.EmailTemplate, bool, []string) {
			return clientconfig.ReadEmailTemplate(dir, id)
		},
	)

	req := httptest.NewRequest(http.MethodGet, "/email-templates", nil)
	req.Header.Set("X-API-Key", "admin-key")
	w := httptest.NewRecorder()
	h.EmailTemplateLibrary(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /email-templates = %d: %s", w.Code, w.Body.String())
	}
	var items []clientconfig.EmailTemplate
	if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	// The broken file is logged and left out; it does not hide the good one.
	// The list carries no preview: the composer reads it only to learn whether
	// the library has anything in it.
	if len(items) != 1 || items[0].ID != "weekly" || !items[0].HasPreview || items[0].PreviewHTML != "" {
		t.Fatalf("items = %+v", items)
	}
	if strings.Contains(w.Body.String(), "preview_html") {
		t.Errorf("the list must not carry previews: %s", w.Body.String())
	}
	if s := items[0].Sections[0]; !s.Required || s.Limits["max_bullets"] != 3 {
		t.Errorf("section = %+v", s)
	}

	get := func(id string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/email-templates/"+id, nil)
		req.Header.Set("X-API-Key", "admin-key")
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("template_id", id)
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		w := httptest.NewRecorder()
		h.EmailTemplateLibraryItem(w, req)
		return w
	}
	w = get("weekly")
	var one clientconfig.EmailTemplate
	if err := json.Unmarshal(w.Body.Bytes(), &one); err != nil || w.Code != http.StatusOK {
		t.Fatalf("GET one = %d %v: %s", w.Code, err, w.Body.String())
	}
	if one.PreviewHTML != "<!doctype html><p>Hi Team,</p>" {
		t.Errorf("preview = %q", one.PreviewHTML)
	}
	for _, id := range []string{"broken", "nope", "..%2Fweekly"} {
		if w := get(id); w.Code != http.StatusNotFound {
			t.Errorf("GET %q = %d, want 404", id, w.Code)
		}
	}
}

func TestEmailTemplateLibraryEmptyAndForbidden(t *testing.T) {
	t.Parallel()

	h, _ := setupTest(t)
	req := httptest.NewRequest(http.MethodGet, "/email-templates", nil)
	req.Header.Set("X-API-Key", "admin-key")
	w := httptest.NewRecorder()
	h.EmailTemplateLibrary(w, req)
	// No provider: an empty JSON array, never null (the web client maps over it).
	if w.Code != http.StatusOK || w.Body.String() != "[]\n" {
		t.Fatalf("no catalog = %d %q", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	h.EmailTemplateLibraryItem(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("no catalog, one template = %d, want 404", w.Code)
	}

	for _, handler := range []http.HandlerFunc{h.EmailTemplateLibrary, h.EmailTemplateLibraryItem} {
		w = httptest.NewRecorder()
		handler(w, httptest.NewRequest(http.MethodGet, "/email-templates", nil))
		if w.Code != http.StatusForbidden {
			t.Fatalf("no principal = %d, want 403", w.Code)
		}
	}
}

func TestEmailTemplatesShareThePromptLibraryAccessRule(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]bool{
		"/email-templates":   true,
		"/prompts":           true,
		"/prompts/export":    true,
		"/email-templates/x": true,
		"/email-templatesx":  false,
		"/tasks":             false,
	} {
		if got := isPromptLibraryPath(path); got != want {
			t.Errorf("isPromptLibraryPath(%q) = %v, want %v", path, got, want)
		}
	}
}
