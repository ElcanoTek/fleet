package scheduledrun

import (
	"errors"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

// pagesRefreshRequirements is the production Pages refresh declaration shape:
// a server, a full-name required tool and a completion clause on it.
const pagesRefreshRequirements = "Refresh the page.\nEXECUTION REQUIREMENTS (JSON):\n" +
	`{"mcp_servers":["pages","fast_io"],"required_tools":["mcp_pages_update_page_data","mcp_fast_io_download"],` +
	`"completion":{"any_succeeded":["mcp_pages_record_refresh_check"]}}`

func TestRequirementsMissExplainedByTransientConnectFailureIsAConnectorOutage(t *testing.T) {
	req, err := parseExecutionRequirements(pagesRefreshRequirements)
	if err != nil {
		t.Fatal(err)
	}
	catalog := []mcp.ServerTool{{ServerName: "fast_io", Tool: mcp.Tool{Name: "download"}}}
	failures := []agentcore.MCPConnectFailure{{Server: "pages", Detail: "DNS lookup failed (no such host)", Transient: true}}
	err = req.checkToolsAgainst(catalog, nil, failures)
	if !errors.Is(err, agentcore.ErrConnectorUnavailable) {
		t.Fatalf("err = %v, want ErrConnectorUnavailable", err)
	}
	for _, want := range []string{
		"execution requirements: unavailable in the task's MCP/native tool roster: server pages, tool mcp_pages_update_page_data, completion tool mcp_pages_record_refresh_check",
		"server pages failed to connect this run (DNS lookup failed (no such host))",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestRequirementsMissNotExplainedByAConnectFailureStaysTerminal(t *testing.T) {
	req, err := parseExecutionRequirements(pagesRefreshRequirements)
	if err != nil {
		t.Fatal(err)
	}
	transientPages := agentcore.MCPConnectFailure{Server: "pages", Detail: "connection refused", Transient: true}
	for _, tc := range []struct {
		name     string
		catalog  []mcp.ServerTool
		failures []agentcore.MCPConnectFailure
	}{
		// fast_io was never configured or selected: no failure explains it.
		{"another server not configured", nil, []agentcore.MCPConnectFailure{transientPages}},
		// pages is down, but fast_io connected without the declared tool.
		{"a tool name no connected server provides", []mcp.ServerTool{{ServerName: "fast_io", Tool: mcp.Tool{Name: "upload"}}}, []agentcore.MCPConnectFailure{transientPages}},
		// pages refused the credential: an operator must fix it.
		{"a non-transient connect failure", []mcp.ServerTool{{ServerName: "fast_io", Tool: mcp.Tool{Name: "download"}}},
			[]agentcore.MCPConnectFailure{{Server: "pages", Detail: "HTTP 401 Unauthorized"}}},
		{"no connect failure at all", []mcp.ServerTool{{ServerName: "fast_io", Tool: mcp.Tool{Name: "download"}}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := req.checkToolsAgainst(tc.catalog, nil, tc.failures)
			if err == nil || errors.Is(err, agentcore.ErrConnectorUnavailable) {
				t.Fatalf("err = %v, want the terminal roster error", err)
			}
			if !strings.Contains(err.Error(), "check selected servers, tool permissions and connected accounts") {
				t.Errorf("terminal error lost its advice: %v", err)
			}
		})
	}
	// The non-transient failure is still named, so the operator sees why.
	err = req.checkToolsAgainst([]mcp.ServerTool{{ServerName: "fast_io", Tool: mcp.Tool{Name: "download"}}}, nil,
		[]agentcore.MCPConnectFailure{{Server: "pages", Detail: "HTTP 401 Unauthorized"}})
	if !strings.Contains(err.Error(), "server pages failed to connect this run (HTTP 401 Unauthorized)") {
		t.Errorf("terminal error does not name the failed server: %v", err)
	}
}

// A bare required tool name cannot be traced to the server that never
// connected; it is attributed to the run's transient outage.
func TestRequirementsBareToolNameAttributedToTheOutage(t *testing.T) {
	req, err := parseExecutionRequirements("x\nEXECUTION REQUIREMENTS (JSON):\n" + `{"mcp_servers":["pages"],"required_tools":["update_page_data"]}`)
	if err != nil {
		t.Fatal(err)
	}
	err = req.checkToolsAgainst(nil, nil, []agentcore.MCPConnectFailure{{Server: "pages", Detail: "JSON-RPC error -32000: temporarily unavailable", Transient: true}})
	if !errors.Is(err, agentcore.ErrConnectorUnavailable) {
		t.Fatalf("err = %v, want ErrConnectorUnavailable", err)
	}
}
