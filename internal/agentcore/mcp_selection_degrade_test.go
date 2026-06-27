package agentcore

import (
	"context"
	"testing"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// badCommand is a stdio command that cannot start, so AddStdioServer fails at
// initialize — simulating a flaky/broken MCP server.
const badCommand = "/nonexistent/fleet-mcp-test-binary-do-not-exist"

// TestBindMCPSelection_BestEffortSkipsFailure pins the graceful-degradation
// contract (#182): a best-effort server that fails to register is skipped and the
// loop continues — BindMCPSelection does NOT abort the run.
func TestBindMCPSelection_BestEffortSkipsFailure(t *testing.T) {
	client := mcp.NewClient()
	bases := map[string]MCPServerBase{
		"flaky":  {Command: badCommand}, // best-effort (Required=false)
		"flaky2": {Command: badCommand, Args: []string{"x"}},
	}
	selection := MCPSelection{{Server: "flaky"}, {Server: "flaky2"}}

	registered, err := BindMCPSelection(context.Background(), client, selection, bases)
	if err != nil {
		t.Fatalf("best-effort failures must NOT abort: got err %v", err)
	}
	if len(registered) != 0 {
		t.Errorf("no server should have registered, got %v", registered)
	}
}

// TestBindMCPSelection_RequiredFailureAborts pins that a Required server still
// fails the run when it cannot register.
func TestBindMCPSelection_RequiredFailureAborts(t *testing.T) {
	client := mcp.NewClient()
	bases := map[string]MCPServerBase{
		"loadbearing": {Command: badCommand, Required: true},
	}
	selection := MCPSelection{{Server: "loadbearing"}}

	if _, err := BindMCPSelection(context.Background(), client, selection, bases); err == nil {
		t.Fatal("a Required server failing to register must abort (return an error)")
	}
}

// TestBindMCPSelection_UnknownServerStillAborts pins that a config error (a
// selection naming a server absent from the catalog) remains fatal — graceful
// degradation covers runtime start failures, not misconfiguration.
func TestBindMCPSelection_UnknownServerStillAborts(t *testing.T) {
	client := mcp.NewClient()
	if _, err := BindMCPSelection(context.Background(), client, MCPSelection{{Server: "ghost"}}, map[string]MCPServerBase{}); err == nil {
		t.Fatal("an unknown/uncataloged server must remain a fatal config error")
	}
}
