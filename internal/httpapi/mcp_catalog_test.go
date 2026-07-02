package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
)

// The trust-labeled MCP directory (#538) must report both connector classes
// with their trust tags, and remote_mcp_enabled=false when the per-user OAuth
// flow is unconfigured — so the UI can render Connect vs. a disabled hint.
// DB-independent: the handler reads only in-memory catalog snapshots.
func TestMCPCatalogTrustLabeledClasses(t *testing.T) {
	s := &Server{
		clientConfig: &clientconfig.Bundle{
			RemoteMCPCatalog: []clientconfig.RemoteMCPCatalogEntry{
				{
					Name:        "github",
					DisplayName: "GitHub",
					Description: "GitHub's hosted MCP server.",
					URL:         "https://api.githubcopilot.com/mcp/",
					Vendor:      "GitHub, Inc.",
					DocsURL:     "https://docs.github.com/mcp",
				},
			},
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/mcp-catalog", nil)
	w := httptest.NewRecorder()
	s.mcpCatalog(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", w.Code)
	}
	var resp mcpCatalogResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if resp.RemoteMCPEnabled {
		t.Error("remote_mcp_enabled should be false with no OAuth service wired")
	}
	// s.agent is nil here (no bundled Optional servers); the list must be an
	// empty array, never null, so the UI can map over it unconditionally.
	if resp.Bundled == nil || len(resp.Bundled) != 0 {
		t.Errorf("bundled should be empty non-nil, got %#v", resp.Bundled)
	}
	if len(resp.ThirdParty) != 1 {
		t.Fatalf("want 1 third-party entry, got %d", len(resp.ThirdParty))
	}
	tp := resp.ThirdParty[0]
	if tp.Trust != "third_party" {
		t.Errorf("trust label %q, want third_party", tp.Trust)
	}
	if tp.Name != "github" || tp.URL != "https://api.githubcopilot.com/mcp/" || tp.Vendor != "GitHub, Inc." {
		t.Errorf("entry wrong: %+v", tp)
	}
}

// A non-GET is refused; a Server with no bundle and no agent still answers an
// empty, well-formed directory (the generic no-config boot must not 500).
func TestMCPCatalogMethodAndEmpty(t *testing.T) {
	s := &Server{}

	req := httptest.NewRequest(http.MethodPost, "/mcp-catalog", nil)
	w := httptest.NewRecorder()
	s.mcpCatalog(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status %d, want 405", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/mcp-catalog", nil)
	w = httptest.NewRecorder()
	s.mcpCatalog(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET status %d, want 200", w.Code)
	}
	var resp mcpCatalogResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if resp.Bundled == nil || resp.ThirdParty == nil {
		t.Errorf("lists must be non-nil empty arrays: %#v", resp)
	}
}
