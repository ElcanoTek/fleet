package admincli

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// schemaIssuesServer stands in for the orchestrator's admin read, asserting
// the CLI calls the right route with the admin key.
func schemaIssuesServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/admin/mcp-servers/schema-issues" {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		if r.Header.Get("X-API-Key") != "k" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestMCPSchemaIssuesPrintsFindings(t *testing.T) {
	srv := schemaIssuesServer(t, `{"since":"2026-09-30T17:24:01Z","issues":[
	  {"server":"pages","tool":"broken","status":"invalid","detail":"at /properties/q: bad type"},
	  {"server":"pages","tool":"update_page_data","status":"rewritten","detail":"/properties/range: positional items array rewritten to prefixItems"}]}`)
	var code int
	out := captureStdout(t, func() { code = cmdMCP([]string{"schema-issues", "--server", srv.URL, "--admin-key", "k"}) })
	if code != 0 {
		t.Fatalf("exit %d, want 0 (findings are data, not a CLI failure); output:\n%s", code, out)
	}
	for _, want := range []string{
		"MCP tool schema issues recorded since 2026-09-30T17:24:01Z:",
		"✗ invalid   mcp_pages_broken (WITHHELD from the model; the connector must fix its schema)",
		"at /properties/q: bad type",
		"! rewritten mcp_pages_update_page_data (translated for the model; the connector should update its schema)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestMCPSchemaIssuesEmptyAndJSON(t *testing.T) {
	const body = `{"since":"2026-09-30T17:24:01Z","issues":[]}`
	srv := schemaIssuesServer(t, body)
	out := captureStdout(t, func() { cmdMCP([]string{"schema-issues", "--server", srv.URL, "--admin-key", "k"}) })
	if !strings.Contains(out, "No MCP tool schema issues recorded since 2026-09-30T17:24:01Z") {
		t.Errorf("empty record output = %q", out)
	}
	out = captureStdout(t, func() { cmdMCP([]string{"schema-issues", "--json", "--server", srv.URL, "--admin-key", "k"}) })
	if strings.TrimSpace(out) != body {
		t.Errorf("--json output = %q, want the raw record %q", out, body)
	}
}

func TestMCPSchemaIssuesErrors(t *testing.T) {
	srv := schemaIssuesServer(t, `{}`)
	if code := cmdMCP([]string{"schema-issues", "--server", srv.URL, "--admin-key", "wrong"}); code == 0 {
		t.Error("a refused admin key must exit non-zero")
	}
	t.Setenv("ADMIN_API_KEY", "")
	t.Setenv("FLEET_ENV_FILE", t.TempDir()+"/absent.env")
	if code := cmdMCP([]string{"schema-issues", "--server", srv.URL}); code == 0 {
		t.Error("no admin key must exit non-zero")
	}
}
