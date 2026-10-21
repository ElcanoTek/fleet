package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/store"
)

// hiddenPickerServer is pickerStatusServer with tools a bundle hides from
// chat: one of ix's two, the whole of an optional ox connector, one of the
// always-on email connector's three, and both of the needs-setup bq's.
func hiddenPickerServer(t *testing.T) *Server {
	t.Helper()
	srv := pickerStatusServer(t)
	eng := srv.agent.(*catalogEngine)
	eng.catalog = append(eng.catalog, agent.OptionalServerInfo{Name: "ox", ToolCount: 1, Tools: []string{"ox_create_deal"}})
	eng.alwaysOn = []agent.AlwaysOnServerInfo{{
		Name: "email", ToolCount: 3, Available: true,
		Tools: []string{"email_draft", "email_search", "email_send"},
	}}
	return srv
}

func hideFromChat(t *testing.T, members ...string) {
	t.Helper()
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{InteractiveHiddenTools: members})
}

func pickerRowNames(servers []map[string]any) []string {
	names := make([]string, 0, len(servers))
	for _, s := range servers {
		names = append(names, s["name"].(string))
	}
	return names
}

func toolList(row map[string]any) []string {
	tools := row["tools"].([]any)
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.(string))
	}
	return out
}

// checkHiddenPicker is what both chat picker routes must show when
// ix_create_deal, ox_create_deal, email_send and bq's tools are hidden.
func checkHiddenPicker(t *testing.T, servers []map[string]any) {
	t.Helper()
	ix := catalogEntry(t, servers, "ix")
	if got := toolList(ix); !slices.Equal(got, []string{"ix_get_deal"}) || ix["tool_count"] != float64(1) {
		t.Errorf("ix row = tools %v, count %v; want only ix_get_deal", got, ix["tool_count"])
	}
	// An always-on row carries a count, never names; the hidden one is not
	// counted and not sent.
	email := catalogEntry(t, servers, "email")
	if email["tool_count"] != float64(2) || len(toolList(email)) != 0 {
		t.Errorf("email row = %v; want 2 tools, no names", email)
	}
	// A connector with nothing left for chat is not offered at all, whether
	// it is running (ox) or waiting for setup (bq).
	for _, name := range []string{"ox", "bq"} {
		if slices.Contains(pickerRowNames(servers), name) {
			t.Errorf("%s is listed, but every tool it has is hidden from chat", name)
		}
	}
	// Nothing else changes: a connector with no tools by design stays.
	for _, name := range []string{"tl", agent.OptionalNativeImageGenName} {
		catalogEntry(t, servers, name)
	}
	for _, row := range servers {
		for _, tool := range toolList(row) {
			if slices.Contains([]string{"ix_create_deal", "ox_create_deal", "email_send", "bq_query", "bq_list_tables"}, tool) {
				t.Errorf("%s still lists %s", row["name"], tool)
			}
		}
	}
}

func TestChatToolsPicker_StartupCatalogOmitsInteractiveHiddenTools(t *testing.T) {
	hideFromChat(t, "ix_create_deal", "ox_create_deal", "email_send", "bq_query", "bq_list_tables")
	srv := hiddenPickerServer(t)
	req := httptest.NewRequest(http.MethodGet, "/mcp-servers", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, "u@x.com"))
	w := httptest.NewRecorder()
	srv.listMCPServerCatalog(w, req)
	checkHiddenPicker(t, decodeServerCatalog(t, w))
}

func TestChatToolsPicker_ConversationCatalogOmitsInteractiveHiddenTools(t *testing.T) {
	hideFromChat(t, "ix_create_deal", "ox_create_deal", "email_send", "bq_query", "bq_list_tables")
	srv := hiddenPickerServer(t)
	w := httptest.NewRecorder()
	// ox was opted in before the bundle hid its only tool: it is still not
	// offered, and the opt-in reaches no tool (the run gate drops it).
	conv := &store.Conversation{OptionalMCPServersEnabled: []string{"ix", "ox"}}
	srv.handleConversationMCPServersGet(w, httptest.NewRequest(http.MethodGet, "/conversations/c1/mcp-servers", nil), "u@x.com", "c1", conv)
	servers := decodeServerCatalog(t, w)
	checkHiddenPicker(t, servers)
	if ix := catalogEntry(t, servers, "ix"); ix["enabled"] != true {
		t.Errorf("ix lost its opt-in: %v", ix)
	}
}

// With no interactive_hidden_tools the picker is exactly as before.
func TestChatToolsPicker_NoHiddenListChangesNothing(t *testing.T) {
	hideFromChat(t)
	srv := hiddenPickerServer(t)
	req := httptest.NewRequest(http.MethodGet, "/mcp-servers", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, "u@x.com"))
	w := httptest.NewRecorder()
	srv.listMCPServerCatalog(w, req)
	servers := decodeServerCatalog(t, w)
	if got := toolList(catalogEntry(t, servers, "ix")); !slices.Equal(got, []string{"ix_create_deal", "ix_get_deal"}) {
		t.Errorf("ix tools = %v", got)
	}
	if catalogEntry(t, servers, "email")["tool_count"] != float64(3) {
		t.Error("email count changed")
	}
	catalogEntry(t, servers, "ox")
	catalogEntry(t, servers, "bq")
}

// The Connections directory (/mcp-catalog) describes connectors for every
// run, scheduled ones included, so it still lists a chat-hidden tool's count.
func TestMCPCatalogDirectoryKeepsChatHiddenTools(t *testing.T) {
	hideFromChat(t, "ix_create_deal")
	srv := hiddenPickerServer(t)
	w := httptest.NewRecorder()
	srv.mcpCatalog(w, httptest.NewRequest(http.MethodGet, "/mcp-catalog", nil))
	var resp mcpCatalogResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, e := range resp.Bundled {
		if e.Name == "ix" && e.ToolCount != 2 {
			t.Errorf("directory ix = %+v; scheduled runs still use both tools", e)
		}
	}
}
