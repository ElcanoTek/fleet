package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/store"
)

// pickerStatusServer serves an optional connector that is running (ix), one
// that started with no tools (tl), an always-on one (email), and a manifest
// connector whose credentials are not set (bq).
func pickerStatusServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("PICKER_IX_TOKEN", "x")
	t.Setenv("PICKER_TL_TOKEN", "x")
	t.Setenv("PICKER_EMAIL_TOKEN", "x")
	dir := t.TempDir()
	manifest := `
mcp_servers:
  - name: ix
    command: python3
    args: ["mcp/ix.py"]
    optional: true
    category: ssps
    enabled_env: [PICKER_IX_TOKEN]
  - name: tl
    command: python3
    args: ["mcp/tl.py"]
    optional: true
    category: ssps
    enabled_env: [PICKER_TL_TOKEN]
  - name: email
    command: python3
    args: ["mcp/email.py"]
    enabled_env: [PICKER_EMAIL_TOKEN]
  - name: bq
    command: python3
    args: ["mcp/bq.py"]
    optional: true
    display_name: "BigQuery Reporting"
    category: data-storage
    enabled_env: [PICKER_BQ_KEY_UNSET]
    tools: [bq_query, bq_list_tables]
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, err := clientconfig.Load(dir)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	return &Server{
		agent: &catalogEngine{
			catalog: []agent.OptionalServerInfo{
				{Name: "ix", ToolCount: 2, Tools: []string{"ix_create_deal", "ix_get_deal"}},
				{Name: "tl", ToolCount: 0, Tools: []string{}},
				{Name: agent.OptionalNativeImageGenName, ToolCount: 0, Tools: []string{}},
			},
			alwaysOn: []agent.AlwaysOnServerInfo{{Name: "email", ToolCount: 3, Available: true}},
		},
		store:        &prefsFakeStore{fakeChatStore: newFakeChatStore()},
		clientConfig: bundle,
	}
}

func TestMCPServerCatalogCarriesCategoryAndStatus(t *testing.T) {
	srv := pickerStatusServer(t)
	req := httptest.NewRequest(http.MethodGet, "/mcp-servers", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, "u@x.com"))
	w := httptest.NewRecorder()
	srv.listMCPServerCatalog(w, req)
	servers := decodeServerCatalog(t, w)

	for name, want := range map[string][2]string{
		"ix":    {"ssps", "available"},
		"tl":    {"ssps", "unavailable"},
		"email": {"misc", "available"},
		"bq":    {"data-storage", "needs_setup"},
		// The native image entry has no MCP tools by design.
		agent.OptionalNativeImageGenName: {"misc", "available"},
	} {
		e := catalogEntry(t, servers, name)
		if e["category"] != want[0] || e["status"] != want[1] {
			t.Errorf("%s: category=%v status=%v, want %v", name, e["category"], e["status"], want)
		}
	}
	bq := catalogEntry(t, servers, "bq")
	if bq["enabled"] != false || bq["tool_count"] != float64(2) || bq["display_name"] != "BigQuery Reporting" {
		t.Errorf("needs-setup row = %v", bq)
	}

	var body struct {
		Categories []clientconfig.ConnectorCategory `json:"categories"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Categories) == 0 || body.Categories[len(body.Categories)-1].ID != "misc" {
		t.Errorf("categories = %v, want an ordered list ending in misc", body.Categories)
	}
}

func TestConversationMCPServersListNeedsSetupWithoutOptingIn(t *testing.T) {
	srv := pickerStatusServer(t)
	req := httptest.NewRequest(http.MethodGet, "/conversations/c1/mcp-servers", nil)
	w := httptest.NewRecorder()
	conv := &store.Conversation{OptionalMCPServersEnabled: []string{"ix"}}
	srv.handleConversationMCPServersGet(w, req, "u@x.com", "c1", conv)
	servers := decodeServerCatalog(t, w)

	if ix := catalogEntry(t, servers, "ix"); ix["enabled"] != true || ix["category"] != "ssps" {
		t.Errorf("ix = %v", ix)
	}
	if bq := catalogEntry(t, servers, "bq"); bq["status"] != "needs_setup" || bq["enabled"] != false {
		t.Errorf("bq = %v", bq)
	}
	// Needs-setup connectors are never opt-in candidates.
	if srv.optionalServerWhitelist(context.Background(), "u@x.com")["bq"] {
		t.Error("needs-setup bq must not be in the opt-in whitelist")
	}
}

func TestMCPCatalogListsNeedsSetupConnectors(t *testing.T) {
	srv := pickerStatusServer(t)
	w := httptest.NewRecorder()
	srv.mcpCatalog(w, httptest.NewRequest(http.MethodGet, "/mcp-catalog", nil))
	var resp mcpCatalogResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range resp.Bundled {
		if e.Name == "bq" {
			found = true
			if !e.NeedsSetup || e.Category != "data-storage" || e.ToolCount != 2 {
				t.Errorf("bq directory entry = %+v", e)
			}
		}
		if e.Name == "ix" && (e.NeedsSetup || e.Category != "ssps") {
			t.Errorf("ix directory entry = %+v", e)
		}
	}
	if !found {
		t.Errorf("bq missing from directory: %+v", resp.Bundled)
	}
}

func TestRemoteConnectionCategoryFromDirectory(t *testing.T) {
	srv := &Server{clientConfig: &clientconfig.Bundle{RemoteMCPCatalog: []clientconfig.RemoteMCPCatalogEntry{
		{Name: "hubspot", URL: "https://mcp.hubspot.com/", Category: "crm-sales"},
		{Name: "odd", URL: "https://odd.example.com/mcp", Category: "ad-tech"},
		{Name: "eu", URL: "https://us.example.com/mcp", Category: "finance",
			URLVariants: []clientconfig.RemoteMCPURLVariant{{ID: "eu", URL: "https://EU.example.com:443/mcp"}}},
	}}}
	for url, want := range map[string]string{
		"https://mcp.hubspot.com/":     "crm-sales",
		"https://odd.example.com/mcp":  "misc",
		"https://eu.example.com/mcp":   "finance",
		"https://unknown.example.com/": "misc",
	} {
		if got := srv.remoteConnectionCategory(url); got != want {
			t.Errorf("remoteConnectionCategory(%q) = %q, want %q", url, got, want)
		}
	}
}

// metaEngine adds the Manager's reload-fresh connector metadata to the fake.
type metaEngine struct {
	*catalogEngine
	meta []clientconfig.ConnectorMeta
}

func (m metaEngine) ConnectorMeta() []clientconfig.ConnectorMeta { return m.meta }

// After an MCP reload the Manager's snapshot, not the boot bundle, decides
// categories and needs-setup rows.
func TestPickerPrefersReloadedConnectorMeta(t *testing.T) {
	srv := pickerStatusServer(t)
	srv.agent = metaEngine{catalogEngine: srv.agent.(*catalogEngine), meta: []clientconfig.ConnectorMeta{
		{Name: "ix", Category: "house", Configured: true, Optional: true},
		{Name: "tl", Category: "ssps", Configured: true, Optional: true},
		{Name: "email", Category: "misc", Configured: true},
		// bq is now configured, so it is no longer a needs-setup row.
		{Name: "bq", Category: "data-storage", Configured: true, Optional: true},
	}}
	req := httptest.NewRequest(http.MethodGet, "/mcp-servers", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, "u@x.com"))
	w := httptest.NewRecorder()
	srv.listMCPServerCatalog(w, req)
	servers := decodeServerCatalog(t, w)
	if ix := catalogEntry(t, servers, "ix"); ix["category"] != "house" {
		t.Errorf("ix category = %v, want the reloaded house", ix["category"])
	}
	for _, e := range servers {
		if e["name"] == "bq" {
			t.Errorf("bq is configured after the reload but still listed: %v", e)
		}
	}
}

// After a reload turns an always-on connector's credentials off, Settings lists
// it once, as needs-setup, not also as the boot-time always-on card.
func TestMCPCatalogAlwaysOnFollowsReloadedMeta(t *testing.T) {
	srv := pickerStatusServer(t)
	srv.agent = metaEngine{catalogEngine: srv.agent.(*catalogEngine), meta: []clientconfig.ConnectorMeta{
		{Name: "ix", Category: "ssps", Configured: true, Optional: true},
		{Name: "email", Category: "misc", Configured: false},
	}}
	w := httptest.NewRecorder()
	srv.mcpCatalog(w, httptest.NewRequest(http.MethodGet, "/mcp-catalog", nil))
	var resp mcpCatalogResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	var cards []mcpCatalogBundledEntry
	for _, e := range resp.Bundled {
		if e.Name == "email" {
			cards = append(cards, e)
		}
	}
	if len(cards) != 1 || !cards[0].NeedsSetup {
		t.Fatalf("email cards = %+v, want exactly one needs-setup card", cards)
	}
}

// A running connector is never also a needs-setup row, even when the metadata
// snapshot says it is unconfigured (as a broker parent's did before the pin).
func TestNeedsSetupSkipsListedConnectors(t *testing.T) {
	srv := pickerStatusServer(t)
	srv.agent = metaEngine{catalogEngine: srv.agent.(*catalogEngine), meta: []clientconfig.ConnectorMeta{
		{Name: "ix", Category: "ssps", Optional: true},
		{Name: "tl", Category: "ssps", Optional: true},
		{Name: "email", Category: "misc"},
		{Name: "bq", Category: "data-storage", Optional: true},
	}}
	req := httptest.NewRequest(http.MethodGet, "/mcp-servers", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, "u@x.com"))
	w := httptest.NewRecorder()
	srv.listMCPServerCatalog(w, req)
	count := map[string]int{}
	for _, e := range decodeServerCatalog(t, w) {
		count[e["name"].(string)]++
	}
	if count["ix"] != 1 || count["tl"] != 1 || count["bq"] != 1 {
		t.Errorf("picker rows per connector = %v, want ix, tl and bq once each", count)
	}

	w = httptest.NewRecorder()
	srv.mcpCatalog(w, httptest.NewRequest(http.MethodGet, "/mcp-catalog", nil))
	var resp mcpCatalogResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	dir := map[string]int{}
	for _, e := range resp.Bundled {
		dir[e.Name]++
	}
	if dir["ix"] != 1 || dir["tl"] != 1 {
		t.Errorf("directory rows per connector = %v, want ix and tl once each", dir)
	}
}
