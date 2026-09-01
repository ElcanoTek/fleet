package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

func alwaysOnCatalogServer() *Server {
	return &Server{
		agent: &catalogEngine{
			catalog: []agent.OptionalServerInfo{{
				Name: "gamma", Description: "Optional decks", ToolCount: 1, Tools: []string{"create_deck"},
			}},
			alwaysOn: []agent.AlwaysOnServerInfo{
				{Name: "email", Description: "Inbound reports", ToolCount: 10, Available: true},
				{Name: "broken", Description: "Expected connector", Available: false},
			},
		},
		store: &prefsFakeStore{fakeChatStore: newFakeChatStore()},
	}
}

func decodeServerCatalog(t *testing.T, w *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var response struct {
		Servers []map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return response.Servers
}

func catalogEntry(t *testing.T, servers []map[string]any, name string) map[string]any {
	t.Helper()
	for _, entry := range servers {
		if entry["name"] == name {
			return entry
		}
	}
	t.Fatalf("server %q missing from catalog: %v", name, servers)
	return nil
}

func TestMCPServerCatalogIncludesLiveAlwaysOnStatus(t *testing.T) {
	srv := alwaysOnCatalogServer()
	req := httptest.NewRequest(http.MethodGet, "/mcp-servers", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, "u@x.com"))
	w := httptest.NewRecorder()

	srv.listMCPServerCatalog(w, req)
	servers := decodeServerCatalog(t, w)

	email := catalogEntry(t, servers, "email")
	if email["always_on"] != true || email["enabled"] != true || email["tool_count"] != float64(10) {
		t.Errorf("available always-on row = %v", email)
	}
	broken := catalogEntry(t, servers, "broken")
	if broken["always_on"] != true || broken["enabled"] != false {
		t.Errorf("unavailable always-on row = %v", broken)
	}
}

func TestConversationMCPServerCatalogKeepsAlwaysOnSeparateFromOptIns(t *testing.T) {
	srv := alwaysOnCatalogServer()
	req := httptest.NewRequest(http.MethodGet, "/conversations/c1/mcp-servers", nil)
	w := httptest.NewRecorder()
	conv := &store.Conversation{OptionalMCPServersEnabled: []string{"gamma"}}

	srv.handleConversationMCPServersGet(w, req, "u@x.com", "c1", conv)
	servers := decodeServerCatalog(t, w)

	if email := catalogEntry(t, servers, "email"); email["always_on"] != true || email["enabled"] != true {
		t.Errorf("always-on row = %v", email)
	}
	if gamma := catalogEntry(t, servers, "gamma"); gamma["always_on"] != nil || gamma["enabled"] != true {
		t.Errorf("optional row = %v", gamma)
	}
}
