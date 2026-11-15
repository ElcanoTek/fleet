package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

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
	h.SetEmailTemplateCatalogProvider(func() ([]clientconfig.EmailTemplate, []string) {
		return clientconfig.ReadEmailTemplates(dir)
	})

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
	if len(items) != 1 || items[0].ID != "weekly" || items[0].PreviewHTML == "" {
		t.Fatalf("items = %+v", items)
	}
	if s := items[0].Sections[0]; !s.Required || s.Limits["max_bullets"] != 3 {
		t.Errorf("section = %+v", s)
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
	h.EmailTemplateLibrary(w, httptest.NewRequest(http.MethodGet, "/email-templates", nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("no principal = %d, want 403", w.Code)
	}
}

func TestEmailTemplatesShareThePromptLibraryAccessRule(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]bool{
		"/email-templates":   true,
		"/prompts":           true,
		"/prompts/export":    true,
		"/email-templates/x": false,
		"/tasks":             false,
	} {
		if got := isPromptLibraryPath(path); got != want {
			t.Errorf("isPromptLibraryPath(%q) = %v, want %v", path, got, want)
		}
	}
}
