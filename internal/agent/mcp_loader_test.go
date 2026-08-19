package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

func TestMCPLoadServers_HTTP_BindsRequestedServers(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		json.Unmarshal(body, &req)

		w.Header().Set("Content-Type", "application/json")

		if req.Method == "initialize" {
			resp := map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"protocolVersion": "2024-11-05",
					"capabilities":    map[string]any{},
				},
			}
			json.NewEncoder(w).Encode(resp)
			return
		}

		if req.Method == "tools/list" {
			resp := map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"tools": []any{},
				},
			}
			json.NewEncoder(w).Encode(resp)
			return
		}

		// Fallback
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  map[string]any{},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	a := &Agent{
		config: &config.Config{
			MCPServers: map[string]config.MCPServerConfig{
				"test_http_server": {
					Enabled: true,
					Type:    "http",
					URL:     ts.URL,
				},
			},
		},
		mcpClient:     mcp.NewClient(),
		loadedServers: map[string]bool{},
	}

	resp, err := a.loadMCPServers(context.Background(), []string{"test_http_server"}, "")
	if err != nil {
		t.Fatalf("loadMCPServers error: %v", err)
	}

	if !resp.StopTurn {
		t.Error("expected StopTurn to be true after loading a server")
	}

	txt := resp.Content
	if !strings.Contains(txt, "Loaded 1 server(s): test_http_server") {
		t.Errorf("unexpected response text: %q", txt)
	}

	if !a.loadedServers["test_http_server"] {
		t.Error("expected test_http_server to be marked as loaded")
	}
}

func TestMCPLoadServers_AlreadyLoaded(t *testing.T) {
	a := &Agent{
		config: &config.Config{
			MCPServers: map[string]config.MCPServerConfig{
				"test_http_server": {
					Enabled: true,
					Type:    "http",
					URL:     "http://localhost:9999", // should not be accessed
				},
			},
		},
		mcpClient:     mcp.NewClient(),
		loadedServers: map[string]bool{"test_http_server": true},
	}

	resp, err := a.loadMCPServers(context.Background(), []string{"test_http_server"}, "")
	if err != nil {
		t.Fatalf("loadMCPServers error: %v", err)
	}

	if resp.StopTurn {
		t.Error("expected StopTurn to be false when no new servers loaded")
	}

	txt := resp.Content
	if !strings.Contains(txt, "No new servers loaded.") {
		t.Errorf("unexpected response text: %q", txt)
	}
}

func TestMCPLoadServers_MissingConfigOrClient(t *testing.T) {
	a := &Agent{}
	resp, err := a.loadMCPServers(context.Background(), []string{"test"}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp.Content, "agent state unavailable") {
		t.Errorf("expected state unavailable, got %q", resp.Content)
	}

	a = &Agent{config: &config.Config{}}
	resp, err = a.loadMCPServers(context.Background(), []string{"test"}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(resp.Content, "no MCP client configured") {
		t.Errorf("expected no mcp client error, got %q", resp.Content)
	}
}

func TestMCPLoadServers_UnknownOrDisabledServer(t *testing.T) {
	a := &Agent{
		config: &config.Config{
			MCPServers: map[string]config.MCPServerConfig{
				"disabled_server": {Enabled: false},
			},
		},
		mcpClient:     mcp.NewClient(),
		loadedServers: map[string]bool{},
	}

	resp, err := a.loadMCPServers(context.Background(), []string{"unknown_server", "disabled_server"}, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	txt := resp.Content
	if !strings.Contains(txt, "No new servers loaded.") {
		t.Errorf("expected no servers loaded, got %q", txt)
	}
	if !strings.Contains(txt, "\"unknown_server\": unknown or disabled") {
		t.Errorf("expected unknown server error, got %q", txt)
	}
	if !strings.Contains(txt, "\"disabled_server\": unknown or disabled") {
		t.Errorf("expected disabled server error, got %q", txt)
	}
}
