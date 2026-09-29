package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/store"
)

// TestDriftFromCatalog pins the F13 comparison: only a row whose name is a
// directory entry is compared, only the fields that differ are reported,
// each with the directory's current value, and an entry that cannot be
// compared (a tenant placeholder) reports nothing.
func TestDriftFromCatalog(t *testing.T) {
	catalog := []clientconfig.RemoteMCPCatalogEntry{
		{Name: "moved", URL: "https://mcp.moved.example.com/mcp", Auth: "oauth"},
		{Name: "same", URL: "https://MCP.Same.example.com:443/mcp", Auth: "oauth"},
		{Name: "retyped", URL: "https://mcp.retyped.example.com/mcp", Auth: "api_key", APIKeyHeader: "X-Api-Key"},
		{Name: "rehomed-key", URL: "https://mcp.rehomed.example.com/mcp", Auth: "api_key", APIKeyQuery: "api_key"},
		{Name: "bearer-now", URL: "https://mcp.bearer.example.com/mcp", Auth: "api_key"},
		{Name: "tenant", URL: "https://{workspace}.example.com/mcp", Auth: "tenant"},
		{Name: "default-auth", URL: "https://mcp.default.example.com/mcp"},
		{Name: "composio", URL: "https://backend.composio.dev/v3/mcp/{SERVER_ID}/mcp?user_id={USER_ID}", Auth: "api_key", APIKeyHeader: "x-api-key"},
		{Name: "scrapfly", URL: "https://mcp.scrapfly.io/{project}/mcp", Auth: "open"},
		{Name: " padded ", URL: " https://mcp.padded.example.com/v2 ", Auth: "api_key", APIKeyHeader: " X-Api-Key "},
	}
	row := func(name, url, auth, header, query string) store.RemoteMCPServer {
		return store.RemoteMCPServer{Name: name, URL: url, AuthKind: auth, APIKeyHeader: header, APIKeyQuery: query}
	}
	cases := []struct {
		name string
		row  store.RemoteMCPServer
		want *catalogDrift
	}{
		{"the endpoint moved", row("moved", "https://old.moved.example.com/mcp", "oauth", "", ""), &catalogDrift{URL: "https://mcp.moved.example.com/mcp"}},
		{"a canonical row matches a loosely spelled entry", row("same", "https://mcp.same.example.com/mcp", "oauth", "", ""), nil},
		{"the auth kind changed", row("retyped", "https://mcp.retyped.example.com/mcp", "oauth", "", ""), &catalogDrift{Auth: "api_key"}},
		{"Composio: a placeholder entry still compares auth (tenant login → api key), never its URL", row("composio", "https://backend.composio.dev/v3/mcp/abc?user_id=u1", "oauth", "", ""), &catalogDrift{Auth: "api_key"}},
		{"a placeholder entry whose auth matches reports nothing", row("scrapfly", "https://mcp.scrapfly.io/p1/mcp", "open", "", ""), nil},
		{"a matching api_key row with the same header, any case", row("retyped", "https://mcp.retyped.example.com/mcp", "api_key", "x-api-key", ""), nil},
		{"a query-parameter name is case-sensitive", row("rehomed-key", "https://mcp.rehomed.example.com/mcp", "api_key", "", "API_KEY"), &catalogDrift{KeySentAs: "the api_key query parameter"}},
		{"the key moved from a header to the query", row("rehomed-key", "https://mcp.rehomed.example.com/mcp", "api_key", "X-Key", ""), &catalogDrift{KeySentAs: "the api_key query parameter"}},
		{"the key moved from a header to bearer", row("bearer-now", "https://mcp.bearer.example.com/mcp", "api_key", "X-Key", ""), &catalogDrift{KeySentAs: "the Authorization: Bearer header"}},
		{"URL and auth both differ", row("retyped", "https://old.retyped.example.com/mcp", "open", "", ""), &catalogDrift{URL: "https://mcp.retyped.example.com/mcp", Auth: "api_key"}},
		{"a tenant entry is not compared", row("tenant", "https://acme.example.com/mcp", "oauth", "", ""), nil},
		{"a pre-column row is an OAuth row", row("default-auth", "https://mcp.default.example.com/mcp", "", "", ""), nil},
		{"a padded entry matches its trimmed row and compares trimmed fields", row("padded", "https://mcp.padded.example.com/v1", "api_key", "X-Api-Key", ""), &catalogDrift{URL: "https://mcp.padded.example.com/v2"}},
		{"a hand-typed server is not a directory entry", row("mine", "https://mcp.mine.example.com", "oauth", "", ""), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := driftFromCatalog(tc.row, catalog)
			switch {
			case got == nil && tc.want == nil:
			case got == nil || tc.want == nil || *got != *tc.want:
				t.Fatalf("drift = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestRemoteMCPServersListReportsCatalogDrift is the wire: a saved row whose
// directory entry moved carries catalog_drift; the same row seen by a user
// it is shared with does not.
func TestRemoteMCPServersListReportsCatalogDrift(t *testing.T) {
	srv, _, row, user := remoteMCPFixture(t)
	srv.clientConfig = &clientconfig.Bundle{RemoteMCPCatalog: []clientconfig.RemoteMCPCatalogEntry{
		{Name: row.Name, URL: "https://mcp.github.example.com/v2", Auth: "oauth"},
	}}
	req := httptest.NewRequest("GET", "/remote-mcp-servers", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, user))
	w := httptest.NewRecorder()
	srv.remoteMCPServers(w, req)
	if w.Code != 200 {
		t.Fatalf("GET: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Servers []struct {
			ID           string        `json:"id"`
			CatalogDrift *catalogDrift `json:"catalog_drift"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Servers) != 1 || resp.Servers[0].ID != row.ID {
		t.Fatalf("servers = %+v", resp.Servers)
	}
	if d := resp.Servers[0].CatalogDrift; d == nil || d.URL != "https://mcp.github.example.com/v2" || d.Auth != "" || d.KeySentAs != "" {
		t.Fatalf("catalog_drift = %+v", d)
	}

	// Shared with someone else: their list shows the row under
	// shared_with_me without the field — only the owner can re-add it.
	const grantee = "grantee@example.com"
	if err := srv.remoteMCP.ShareServer(context.Background(), user, row.ID, grantee); err != nil {
		t.Fatalf("ShareServer: %v", err)
	}
	greq := httptest.NewRequest("GET", "/remote-mcp-servers", nil)
	greq = greq.WithContext(context.WithValue(greq.Context(), ctxKeyUser, grantee))
	gw := httptest.NewRecorder()
	srv.remoteMCPServers(gw, greq)
	var shared struct {
		SharedWithMe []map[string]json.RawMessage `json:"shared_with_me"`
	}
	if err := json.Unmarshal(gw.Body.Bytes(), &shared); err != nil {
		t.Fatalf("decode grantee list: %v", err)
	}
	if len(shared.SharedWithMe) != 1 {
		t.Fatalf("grantee sees %d shared rows: %s", len(shared.SharedWithMe), gw.Body.String())
	}
	if _, present := shared.SharedWithMe[0]["catalog_drift"]; present {
		t.Fatalf("catalog_drift present on a shared row: %s", gw.Body.String())
	}

	// The same row with the entry corrected back to what it has: no drift,
	// and the field is absent from the wire rather than null.
	srv.clientConfig = &clientconfig.Bundle{RemoteMCPCatalog: []clientconfig.RemoteMCPCatalogEntry{
		{Name: row.Name, URL: row.URL, Auth: "oauth"},
	}}
	w = httptest.NewRecorder()
	srv.remoteMCPServers(w, req)
	var raw struct {
		Servers []map[string]json.RawMessage `json:"servers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := raw.Servers[0]["catalog_drift"]; present {
		t.Fatalf("catalog_drift present on a matching row: %s", w.Body.String())
	}
}
