package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/sched/handlers"
)

// schemaVendor is a minimal streamable-HTTP MCP server that lists fixed tools.
func schemaVendor(t *testing.T, tools []map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": *req.ID}
		switch req.Method {
		case "initialize":
			resp["result"] = map[string]any{"protocolVersion": "2025-06-18"}
		case "tools/list":
			resp["result"] = map[string]any{"tools": tools}
		default:
			resp["error"] = map[string]any{"code": -32601, "message": "method not found"}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func schemaVendorTools(t *testing.T) []map[string]any {
	t.Helper()
	var tools []map[string]any
	if err := json.Unmarshal([]byte(`[
	  {"name":"get_page","description":"d","inputSchema":{"type":"object","properties":{"slug":{"type":"string"}}}},
	  {"name":"update_page_data","description":"d","inputSchema":{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"range":{"type":"array","items":[{"type":"string"},{"type":"string"}],"additionalItems":false}}}},
	  {"name":"broken","description":"d","inputSchema":{"type":"object","properties":{"q":{"type":42}}}}
	]`), &tools); err != nil {
		t.Fatal(err)
	}
	return tools
}

// TestMCPTestVerb_ReportsSchemaIssues: `fleet mcp test` runs the model
// boundary's schema check on every listed tool and reports findings without
// failing the server — it is reachable and its other tools work.
func TestMCPTestVerb_ReportsSchemaIssues(t *testing.T) {
	srv := schemaVendor(t, schemaVendorTools(t))
	res := probeBundleServer("pages", config.MCPServerConfig{Type: "http", URL: srv.URL}, 10*time.Second, false)
	if !res.Connected || res.ToolCount != 3 {
		t.Fatalf("probe = %+v, want connected with 3 tools", res)
	}
	if len(res.SchemaIssues) != 2 ||
		res.SchemaIssues[0].Tool != "broken" || res.SchemaIssues[0].Status != agentcore.ToolSchemaInvalid ||
		res.SchemaIssues[1].Tool != "update_page_data" || res.SchemaIssues[1].Status != agentcore.ToolSchemaRewritten {
		t.Fatalf("schema issues = %+v, want broken=invalid, update_page_data=rewritten", res.SchemaIssues)
	}

	var text bytes.Buffer
	if code := emitMCPTestReport(&text, mcpTestReport{Passed: true, Results: []mcpTestResult{res}}, false); code != 0 {
		t.Fatalf("exit code = %d, want 0: schema findings are reported, not failed", code)
	}
	for _, want := range []string{
		"schema ✗ broken WITHHELD from the model — at /properties/q",
		"schema ! update_page_data translated (older JSON Schema draft) — /properties/range: positional items array rewritten to prefixItems",
	} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("text report missing %q:\n%s", want, text.String())
		}
	}

	var js bytes.Buffer
	emitMCPTestReport(&js, mcpTestReport{Passed: true, Results: []mcpTestResult{res}}, true)
	var parsed struct {
		Results []struct {
			SchemaIssues []agentcore.ToolSchemaIssue `json:"schema_issues"`
		} `json:"results"`
	}
	if err := json.Unmarshal(js.Bytes(), &parsed); err != nil || len(parsed.Results) != 1 || len(parsed.Results[0].SchemaIssues) != 2 {
		t.Fatalf("json report schema_issues = %+v (%v):\n%s", parsed, err, js.String())
	}
}

// TestMCPSchemaIssuesHandler: the admin read serves the runtime record with
// its start time, and an empty record as [] (not null) so `jq '.issues |
// length'` in fleet doctor reads 0.
func TestMCPSchemaIssuesHandler(t *testing.T) {
	rec := httptest.NewRecorder()
	mcpSchemaIssuesHandler(rec, httptest.NewRequest(http.MethodGet, "/admin/mcp-servers/schema-issues", nil))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("status %d, content-type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var body struct {
		Since  time.Time         `json:"since"`
		Issues []json.RawMessage `json:"issues"`
	}
	raw := rec.Body.String()
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	if body.Since.IsZero() {
		t.Errorf("since is zero: %s", raw)
	}
	if body.Issues == nil || strings.Contains(raw, `"issues":null`) {
		t.Errorf("issues must be a JSON array: %s", raw)
	}
}

// TestMCPSchemaIssuesRouteIsAdminGated: the route is mounted in the admin
// group of the real orchestrator mux: refused without the admin key, served
// with it.
func TestMCPSchemaIssuesRouteIsAdminGated(t *testing.T) {
	h := handlers.New(handlers.Config{AdminAPIKey: "test-admin-key"}, nil, nil)
	notes := handlers.NewNotesHandlers(nil, h)
	mux := buildOrchestratorMux(h, notes, reloadConfigHandler(nil), mcpReloadHandler(nil), nil)
	get := func(key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/admin/mcp-servers/schema-issues", nil)
		if key != "" {
			req.Header.Set("X-API-Key", key)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	if rec := get(""); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated GET = %d, want 401/403", rec.Code)
	}
	if rec := get("test-admin-key"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"issues":`) {
		t.Fatalf("admin GET = %d %q, want 200 with the record", rec.Code, rec.Body.String())
	}
}
