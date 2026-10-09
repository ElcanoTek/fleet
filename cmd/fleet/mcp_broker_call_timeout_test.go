package main

import (
	"context"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/mcpbroker"
)

// brokerSlowServerScript is a stdio MCP server whose "slow" tool answers only
// after 3 s, standing in for a long sequential deal_ids batch.
const brokerSlowServerScript = `
import json, sys, time
def send(obj):
    sys.stdout.write(json.dumps(obj) + "\n"); sys.stdout.flush()
for line in sys.stdin:
    if not line.strip():
        continue
    req = json.loads(line)
    rid, method = req.get("id"), req.get("method")
    if method == "initialize":
        send({"jsonrpc":"2.0","id":rid,"result":{"capabilities":{}}})
    elif method == "tools/list":
        send({"jsonrpc":"2.0","id":rid,"result":{"tools":[
            {"name":"slow","inputSchema":{"type":"object","properties":{}}}]}})
    elif method == "tools/call":
        time.sleep(3)
        send({"jsonrpc":"2.0","id":rid,"result":{"content":[{"type":"text","text":"late"}]}})
    elif rid is not None:
        send({"jsonrpc":"2.0","id":rid,"result":{}})
`

// TestBrokerScope_CallBudgetCrossesTheWire drives the production call path end
// to end — mcpbroker.Scope in the parent, the JSON pipe, brokerBackend's scope
// in the credential-owning child, the local broker, mcp.Server.callTool and a
// real stdio server — and pins that a per-call budget attached with
// mcp.WithCallTimeout reaches callTool. A context value does not cross the
// pipe on its own (the child rebuilds each call's context), so without the
// wire's callTimeoutMs the 300 ms budget is dropped and the call runs the
// server's full 3 s.
func TestBrokerScope_CallBudgetCrossesTheWire(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found")
	}
	client := mcp.NewClient()
	b := &brokerBackend{
		MCPBroker: agentcore.NewLocalMCPBroker(client, agentcore.DefaultRemediationHints),
		client:    client,
		bases: map[string]agentcore.MCPServerBase{
			"pacer": {Command: "python3", Args: []string{"-u", "-c", brokerSlowServerScript}},
		},
		enabled: map[string]bool{"pacer": true},
		scopes:  make(map[string]*brokerScope),
	}
	t.Cleanup(func() { _ = b.Close() })

	parentConn, childConn := net.Pipe()
	serveCtx, stopServe := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { _ = mcpbroker.NewServer(b).Serve(serveCtx, childConn); close(served) }()
	parent := mcpbroker.NewClient(parentConn)
	t.Cleanup(func() {
		_ = parent.Close()
		stopServe()
		<-served
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	scope, err := parent.OpenScope(ctx, mcpbroker.ScopeSpec{Selection: []mcpbroker.ScopeChoice{{Server: "pacer"}}})
	if err != nil {
		t.Fatalf("OpenScope: %v", err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })

	// No parent deadline at all: only the budget can stop this call.
	start := time.Now()
	text, _, err := scope.CallMCP(mcp.WithCallTimeout(context.Background(), 300*time.Millisecond), "pacer", "slow", nil)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("call returned %q after %v; the 300ms budget never reached mcp.Server.callTool", strings.TrimSpace(text), elapsed)
	}
	if elapsed >= 2500*time.Millisecond {
		t.Fatalf("call failed only after %v (%v); want it cut at the 300ms budget, well before the server's 3s answer", elapsed, err)
	}
}
