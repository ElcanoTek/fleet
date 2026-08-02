package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/mcpbroker"
)

const brokerScopeServerScript = `
import json, os, sys
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
            {"name":"identity","description":"test identity",
             "inputSchema":{"type":"object","properties":{}}}]}})
    elif method == "tools/call":
        text = "|".join([
            os.environ.get("TEST_SCOPE_TOKEN", "<unset>"),
            os.environ.get("TEST_TASK_ID", "<unset>"),
            os.environ.get("TEST_WORKSPACE", "<unset>"),
        ])
        send({"jsonrpc":"2.0","id":rid,"result":{"content":[{"type":"text","text":text}]}})
    elif rid is not None:
        send({"jsonrpc":"2.0","id":rid,"result":{}})
`

func newBrokerBackendForScopeTest(t *testing.T) *brokerBackend {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found")
	}
	client := mcp.NewClient()
	return &brokerBackend{
		MCPBroker: agentcore.NewLocalMCPBroker(client, agentcore.DefaultRemediationHints),
		client:    client,
		bases: map[string]agentcore.MCPServerBase{
			"acct": {
				Command: "python3",
				Args:    []string{"-u", "-c", brokerScopeServerScript},
				BaseEnv: map[string]string{
					"TEST_SCOPE_TOKEN": "default-seat",
					"TEST_TASK_ID":     "${FLEET_TASK_ID}",
					"TEST_WORKSPACE":   "${FLEET_WORKSPACE}",
				},
			},
		},
		scopes: make(map[string]*brokerScope),
	}
}

func TestBrokerBackend_ScopeOwnsAccountTaskAndWorkspace(t *testing.T) {
	b := newBrokerBackendForScopeTest(t)
	t.Cleanup(func() { _ = b.Close() })
	t.Setenv("TEST_SCOPE_TOKEN_CLIENT_A", "account-a-token")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	id, tools, err := b.OpenScope(ctx, mcpbroker.ScopeSpec{
		Selection: []mcpbroker.ScopeChoice{{Server: "acct", Account: "client_a"}},
		TaskID:    "task-7",
		Workspace: "/tmp/fleet-scope-workspace",
	})
	if err != nil {
		t.Fatalf("OpenScope: %v", err)
	}
	if id == "" {
		t.Fatal("OpenScope returned an empty ID")
	}
	if len(tools) != 1 || tools[0].Server != "acct_client_a" || tools[0].Tool != "identity" {
		t.Fatalf("tools = %+v, want account-variant catalog", tools)
	}

	text, isErr, err := b.CallMCPInScope(ctx, id, "acct_client_a", "identity", nil)
	if err != nil || isErr {
		t.Fatalf("CallMCPInScope = (%q, %v, %v)", text, isErr, err)
	}
	if strings.TrimSpace(text) != "account-a-token|task-7|/tmp/fleet-scope-workspace" {
		t.Fatalf("identity = %q, want account/task/workspace scoped values", text)
	}

	if err := b.CloseScope(ctx, id); err != nil {
		t.Fatalf("CloseScope: %v", err)
	}
	if err := b.CloseScope(ctx, id); err != nil {
		t.Fatalf("idempotent CloseScope: %v", err)
	}
	_, _, err = b.CallMCPInScope(ctx, id, "acct_client_a", "identity", nil)
	if err == nil || !strings.Contains(err.Error(), "unknown scope") {
		t.Fatalf("call after close = %v, want unknown-scope error", err)
	}
}

func TestBrokerBackend_OpenScopeRefusesUnknownServer(t *testing.T) {
	b := newBrokerBackendForScopeTest(t)
	t.Cleanup(func() { _ = b.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, _, err := b.OpenScope(ctx, mcpbroker.ScopeSpec{
		Selection: []mcpbroker.ScopeChoice{{Server: "missing"}},
	})
	if err == nil {
		t.Fatal("OpenScope accepted an unknown server")
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.scopes) != 0 {
		t.Fatalf("failed open leaked %d scope(s)", len(b.scopes))
	}
}

func TestBrokerBackend_CloseReapsOutstandingScopes(t *testing.T) {
	b := newBrokerBackendForScopeTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	id, _, err := b.OpenScope(ctx, mcpbroker.ScopeSpec{
		Selection: []mcpbroker.ScopeChoice{{Server: "acct"}},
	})
	if err != nil {
		t.Fatalf("OpenScope: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	b.mu.RLock()
	remaining := len(b.scopes)
	b.mu.RUnlock()
	if remaining != 0 {
		t.Fatalf("Close left %d scope(s)", remaining)
	}
	if _, _, err := b.CallMCPInScope(ctx, id, "acct", "identity", nil); err == nil {
		t.Fatal("closed backend retained an outstanding scope")
	}
}

func TestBrokerBackend_ReloadRefreshesFutureScopesAndPreservesActiveScope(t *testing.T) {
	b := newBrokerBackendForScopeTest(t)
	t.Cleanup(func() { _ = b.Close() })
	t.Setenv("TEST_SCOPE_TOKEN_BLUE", "reloaded-account-token")
	b.reloadConfig = func() (*config.Config, error) {
		return &config.Config{MCPServers: map[string]config.MCPServerConfig{
			"acct": {
				Type:    "stdio",
				Command: "python3",
				Args:    []string{"-u", "-c", brokerScopeServerScript},
				Env: map[string]string{
					"TEST_SCOPE_TOKEN": "reloaded-seat",
					"TEST_TASK_ID":     "${FLEET_TASK_ID}",
					"TEST_WORKSPACE":   "${FLEET_WORKSPACE}",
				},
				Enabled:     true,
				AccountVars: []string{"TEST_SCOPE_TOKEN"},
			},
		}}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	oldID, _, err := b.OpenScope(ctx, mcpbroker.ScopeSpec{Selection: []mcpbroker.ScopeChoice{{Server: "acct"}}})
	if err != nil {
		t.Fatalf("OpenScope before reload: %v", err)
	}
	result, err := b.Reload(ctx)
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if len(result.Summary.Added) != 1 || result.Summary.Added[0] != "acct" {
		t.Fatalf("reload summary = %+v, want acct added", result.Summary)
	}
	if len(result.Tools) != 1 || result.Tools[0].Server != "acct" || result.Tools[0].Tool != "identity" {
		t.Fatalf("reload tools = %+v, want refreshed acct.identity", result.Tools)
	}
	if len(result.Accounts["acct"]) != 1 || result.Accounts["acct"][0] != "blue" {
		t.Fatalf("reload accounts = %+v, want acct:[blue]", result.Accounts)
	}

	oldText, _, err := b.CallMCPInScope(ctx, oldID, "acct", "identity", nil)
	if err != nil {
		t.Fatalf("old scope call: %v", err)
	}
	if strings.TrimSpace(oldText) != "default-seat|<unset>|<unset>" {
		t.Fatalf("old scope changed across reload: %q", oldText)
	}

	newID, _, err := b.OpenScope(ctx, mcpbroker.ScopeSpec{Selection: []mcpbroker.ScopeChoice{{Server: "acct"}}})
	if err != nil {
		t.Fatalf("OpenScope after reload: %v", err)
	}
	newText, _, err := b.CallMCPInScope(ctx, newID, "acct", "identity", nil)
	if err != nil {
		t.Fatalf("new scope call: %v", err)
	}
	if strings.TrimSpace(newText) != "reloaded-seat|<unset>|<unset>" {
		t.Fatalf("new scope did not use reloaded bases: %q", newText)
	}
}
