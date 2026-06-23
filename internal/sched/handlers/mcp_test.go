// Copyright (c) 2025 ElcanoTek
// All rights reserved. This is a private repository.

package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetMCPServers_EmptyWhenNoProvider(t *testing.T) {
	h := &Handlers{} // nil mcpCatalog provider
	rr := httptest.NewRecorder()
	h.GetMCPServers(rr, httptest.NewRequest(http.MethodGet, "/mcp-servers", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var body struct {
		Servers []MCPServerCatalogEntry `json:"servers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// A nil provider must still yield a well-formed empty array (not null), so
	// the picker renders "no connectors" rather than choking on a 404/null.
	if body.Servers == nil {
		t.Fatal("servers is null; want []")
	}
	if len(body.Servers) != 0 {
		t.Fatalf("servers = %+v, want empty", body.Servers)
	}
}

func TestGetMCPServersAndAccounts_FromProvider(t *testing.T) {
	h := &Handlers{}
	h.SetMCPCatalogProvider(func() []MCPServerCatalogEntry {
		return []MCPServerCatalogEntry{
			{Name: "xandr", DisplayName: "Xandr", Description: "DSP", ToolCount: 7, Enabled: false, Accounts: []string{"client_a", "client_b"}},
			{Name: "sendgrid", Description: "email", ToolCount: 2, Enabled: true, Accounts: nil},
		}
	})

	// /mcp-servers carries the catalog verbatim.
	rr := httptest.NewRecorder()
	h.GetMCPServers(rr, httptest.NewRequest(http.MethodGet, "/mcp-servers", nil))
	var srv struct {
		Servers []MCPServerCatalogEntry `json:"servers"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &srv); err != nil {
		t.Fatalf("decode servers: %v", err)
	}
	if len(srv.Servers) != 2 || srv.Servers[0].Name != "xandr" || srv.Servers[0].ToolCount != 7 {
		t.Fatalf("unexpected servers: %+v", srv.Servers)
	}

	// /mcp-accounts flattens the seats; sendgrid (no accounts) contributes none.
	rr = httptest.NewRecorder()
	h.GetMCPAccounts(rr, httptest.NewRequest(http.MethodGet, "/mcp-accounts", nil))
	var acc struct {
		Accounts []MCPAccountEntry `json:"accounts"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &acc); err != nil {
		t.Fatalf("decode accounts: %v", err)
	}
	want := map[string]bool{"xandr/client_a": true, "xandr/client_b": true}
	if len(acc.Accounts) != 2 {
		t.Fatalf("accounts = %+v, want exactly the 2 xandr seats", acc.Accounts)
	}
	for _, a := range acc.Accounts {
		if !want[a.Server+"/"+a.Account] {
			t.Fatalf("unexpected seat %s/%s", a.Server, a.Account)
		}
	}
}
