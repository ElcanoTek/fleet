// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestGetMCPServers_MergesRemoteForUser locks in #466: the orchestrator picker
// must surface the caller's per-user remote (hosted) MCP servers (#443) after
// the bundle catalog, keyed by the authenticated principal's email — so a server
// connected in chat shows up in the task form too. A principal with no user (an
// admin API key) sees only the bundle catalog.
func TestGetMCPServers_MergesRemoteForUser(t *testing.T) {
	h := &Handlers{}
	h.SetMCPCatalogProvider(func() []MCPServerCatalogEntry {
		return []MCPServerCatalogEntry{{Name: "xandr", Description: "DSP", Enabled: false}}
	})
	var gotEmail string
	h.SetRemoteMCPServersProvider(func(_ context.Context, email string) []MCPServerCatalogEntry {
		gotEmail = email
		if email != "alice@example.com" {
			return nil
		}
		return []MCPServerCatalogEntry{{Name: "my-notion", Description: "connected", Enabled: true, Remote: true}}
	})

	decode := func(rr *httptest.ResponseRecorder) []MCPServerCatalogEntry {
		t.Helper()
		var body struct {
			Servers []MCPServerCatalogEntry `json:"servers"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body.Servers
	}

	// 1. A resolved user → bundle catalog + that user's remote servers.
	req := httptest.NewRequest(http.MethodGet, "/mcp-servers", nil)
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, &models.User{Username: "alice@example.com"}))
	rr := httptest.NewRecorder()
	h.GetMCPServers(rr, req)
	servers := decode(rr)
	if gotEmail != "alice@example.com" {
		t.Fatalf("remote provider got email %q, want alice@example.com", gotEmail)
	}
	if len(servers) != 2 || servers[0].Name != "xandr" || servers[1].Name != "my-notion" || !servers[1].Remote {
		t.Fatalf("expected [xandr, my-notion(remote)], got %+v", servers)
	}

	// 2. No user in context (admin API key) → bundle catalog only, no remote.
	rr = httptest.NewRecorder()
	h.GetMCPServers(rr, httptest.NewRequest(http.MethodGet, "/mcp-servers", nil))
	servers = decode(rr)
	if len(servers) != 1 || servers[0].Name != "xandr" {
		t.Fatalf("admin (no user) should see only the bundle catalog, got %+v", servers)
	}

	// 3. A user with no connected remote servers → bundle catalog unchanged.
	req = httptest.NewRequest(http.MethodGet, "/mcp-servers", nil)
	req = req.WithContext(context.WithValue(req.Context(), userContextKey, &models.User{Username: "bob@example.com"}))
	rr = httptest.NewRecorder()
	h.GetMCPServers(rr, req)
	if servers := decode(rr); len(servers) != 1 || servers[0].Name != "xandr" {
		t.Fatalf("user with no remote servers should see only the bundle catalog, got %+v", servers)
	}
}
