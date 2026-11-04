// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

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

	"github.com/ElcanoTek/fleet/internal/clientconfig"
)

const formPromptYAML = `name: Campaign page
fields:
  - {key: partner, label: Partner, type: select, required: true, options: [A, B], default: B}
  - {key: deals, label: Deals, type: textarea, advanced: true}
promptTemplate: |-
  Build the page.
  Partner: {partner}
  Deals: {deals}
`

// TestPromptLibraryItemFromGitCarriesForm pins the merge step without a
// database: a Git entry's form reaches the wire item unchanged, and a plain
// entry's JSON has neither form key (the web keys "is this a form?" off
// `fields`).
func TestPromptLibraryItemFromGitCarriesForm(t *testing.T) {
	floor := 1.0
	gp := clientconfig.Prompt{
		ID: "git:campaign.yaml", Name: "Campaign page", Content: formPromptYAML,
		Source: "git", Visibility: "workspace", ReadOnly: true, Path: "prompts/campaign.yaml",
		Fields: []clientconfig.PromptField{
			{Key: "partner", Label: "Partner", Type: "select", Required: true, Options: []string{"A", "B"}, Default: "B"},
			{Key: "count", Label: "Count", Type: "number", Min: &floor, Default: 3.0},
		},
		PromptTemplate: "Partner: {partner} {count}",
	}
	item := promptLibraryItemFromGit(gp)
	if !item.ReadOnly || item.Source != "git" || item.Content != formPromptYAML {
		t.Fatalf("git item = %+v", item)
	}
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Fields []struct {
			Key      string   `json:"key"`
			Type     string   `json:"type"`
			Required bool     `json:"required"`
			Options  []string `json:"options"`
			Default  any      `json:"default"`
			Min      *float64 `json:"min"`
		} `json:"fields"`
		PromptTemplate string `json:"prompt_template"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.PromptTemplate != gp.PromptTemplate || len(wire.Fields) != 2 {
		t.Fatalf("wire = %s", raw)
	}
	if f := wire.Fields[0]; f.Key != "partner" || f.Type != "select" || !f.Required || f.Default != "B" || len(f.Options) != 2 {
		t.Errorf("select field on the wire = %+v", f)
	}
	if f := wire.Fields[1]; f.Min == nil || *f.Min != 1 || f.Default != 3.0 {
		t.Errorf("number field on the wire = %+v", f)
	}

	plain := promptLibraryItemFromGit(clientconfig.Prompt{ID: "git:plain.md", Name: "Plain", Content: "# Plain"})
	raw, err = json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"fields"`) || strings.Contains(string(raw), `"prompt_template"`) {
		t.Errorf("plain git item carries form keys: %s", raw)
	}
}

// TestPromptLibraryCollectionServesGitForms drives GET /prompts end to end
// over a real bundle prompts/ directory: a valid form carries fields and
// prompt_template, while a plain prompt and an INVALID form are both served as
// plain entries with their raw content — a broken form never hides the prompt.
func TestPromptLibraryCollectionServesGitForms(t *testing.T) {
	h, _ := setupTest(t)
	dir := t.TempDir()
	for name, body := range map[string]string{
		"campaign.yaml": formPromptYAML,
		"plain.yaml":    "name: Plain\ngoal: Just text.\n",
		"broken.yaml":   "name: Broken\nfields: [{key: a, label: A, type: radio}]\npromptTemplate: \"{a}\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h.SetPromptCatalogProvider(func() ([]clientconfig.Prompt, []string) { return clientconfig.ReadPrompts(dir) })

	req := httptest.NewRequest(http.MethodGet, "/prompts", nil)
	req.Header.Set("X-API-Key", "admin-key")
	w := httptest.NewRecorder()
	h.PromptLibraryCollection(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /prompts = %d: %s", w.Code, w.Body.String())
	}
	var items []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	byID := map[string]map[string]any{}
	for _, it := range items {
		byID[it["id"].(string)] = it
	}

	form := byID["git:campaign.yaml"]
	if form == nil {
		t.Fatalf("form prompt missing from %s", w.Body.String())
	}
	if form["prompt_template"] != "Build the page.\nPartner: {partner}\nDeals: {deals}" {
		t.Errorf("prompt_template = %#v", form["prompt_template"])
	}
	if fields, _ := form["fields"].([]any); len(fields) != 2 {
		t.Errorf("fields = %#v", form["fields"])
	}
	if form["content"] != formPromptYAML {
		t.Errorf("content must stay the raw file, got %#v", form["content"])
	}

	for _, id := range []string{"git:plain.yaml", "git:broken.yaml"} {
		it := byID[id]
		if it == nil {
			t.Fatalf("%s missing from %s", id, w.Body.String())
		}
		if _, ok := it["fields"]; ok {
			t.Errorf("%s served as a form: %#v", id, it)
		}
		if _, ok := it["prompt_template"]; ok {
			t.Errorf("%s carries prompt_template: %#v", id, it)
		}
	}
}

func TestPromptLibraryHeaderTrustAuthorizesChatMembers(t *testing.T) {
	h, _ := setupTest(t)
	h.config.SharedToken = "shared-secret"
	h.SetChatMemberRoleProvider(func(_ context.Context, email string) (string, error) {
		switch email {
		case "chatmember@example.com":
			return "member", nil
		case "chatviewer@example.com":
			return "viewer", nil
		default:
			return "", nil
		}
	})

	promptsAuthed := h.AdminOrUserAuthMiddleware(http.HandlerFunc(h.PromptLibraryCollection))
	meAuthed := h.AdminOrUserAuthMiddleware(http.HandlerFunc(h.GetCurrentUser))

	// A Chat contributor with ops_role="none" (not in sched.users) can list and create prompts.
	getReq := httptest.NewRequest(http.MethodGet, "/prompts", nil)
	getReq.Header.Set("X-Orchestrator-Server-Token", "shared-secret")
	getReq.Header.Set("X-User-Email", "chatmember@example.com")
	getRec := httptest.NewRecorder()
	promptsAuthed.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("chat member GET /prompts = %d: %s", getRec.Code, getRec.Body.String())
	}

	postReq := httptest.NewRequest(http.MethodPost, "/prompts", strings.NewReader(`{"name":"My Chat Prompt","content":"Run check","visibility":"private"}`))
	postReq.Header.Set("X-Orchestrator-Server-Token", "shared-secret")
	postReq.Header.Set("X-User-Email", "chatmember@example.com")
	postRec := httptest.NewRecorder()
	promptsAuthed.ServeHTTP(postRec, postReq)
	if postRec.Code != http.StatusCreated {
		t.Fatalf("chat member POST /prompts = %d: %s", postRec.Code, postRec.Body.String())
	}

	// A Chat viewer can read prompts (200) but cannot create them (403).
	viewerGet := httptest.NewRequest(http.MethodGet, "/prompts", nil)
	viewerGet.Header.Set("X-Orchestrator-Server-Token", "shared-secret")
	viewerGet.Header.Set("X-User-Email", "chatviewer@example.com")
	viewerGetRec := httptest.NewRecorder()
	promptsAuthed.ServeHTTP(viewerGetRec, viewerGet)
	if viewerGetRec.Code != http.StatusOK {
		t.Fatalf("chat viewer GET /prompts = %d: %s", viewerGetRec.Code, viewerGetRec.Body.String())
	}

	viewerPost := httptest.NewRequest(http.MethodPost, "/prompts", strings.NewReader(`{"name":"Nope","content":"No","visibility":"private"}`))
	viewerPost.Header.Set("X-Orchestrator-Server-Token", "shared-secret")
	viewerPost.Header.Set("X-User-Email", "chatviewer@example.com")
	viewerPostRec := httptest.NewRecorder()
	promptsAuthed.ServeHTTP(viewerPostRec, viewerPost)
	if viewerPostRec.Code != http.StatusForbidden {
		t.Fatalf("chat viewer POST /prompts = %d, want 403", viewerPostRec.Code)
	}

	// Operations Center routes (/api/me) remain 403 not_a_member for chat-only users.
	meReq := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	meReq.Header.Set("X-Orchestrator-Server-Token", "shared-secret")
	meReq.Header.Set("X-User-Email", "chatmember@example.com")
	meRec := httptest.NewRecorder()
	meAuthed.ServeHTTP(meRec, meReq)
	if meRec.Code != http.StatusForbidden {
		t.Fatalf("chat-only member GET /api/me = %d, want 403 not_a_member", meRec.Code)
	}
}
