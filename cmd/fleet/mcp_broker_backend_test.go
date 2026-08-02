package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
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

	id, tools, _, err := b.OpenScope(ctx, mcpbroker.ScopeSpec{
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

	_, _, _, err := b.OpenScope(ctx, mcpbroker.ScopeSpec{
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

func TestBrokerBackend_RemoteScopeFailsWhenDisabled(t *testing.T) {
	b := newBrokerBackendForScopeTest(t)
	t.Cleanup(func() { _ = b.Close() })
	_, _, _, err := b.OpenScope(context.Background(), mcpbroker.ScopeSpec{
		Remote: &mcpbroker.RemoteScopeSpec{UserEmail: "user@example.com"},
	})
	if err == nil || err.Error() != "remote MCP OAuth is not configured in broker" {
		t.Fatalf("OpenScope error = %v, want explicit disabled error", err)
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.scopes) != 0 {
		t.Fatalf("disabled remote open leaked %d scope(s)", len(b.scopes))
	}
}

type brokerRemoteResolver struct {
	conns    []agent.RemoteMCPConn
	listErr  error
	token    string
	tokenErr error
	asked    []string
}

func (r *brokerRemoteResolver) ConnectedServersForUser(context.Context, string) ([]agent.RemoteMCPConn, error) {
	return append([]agent.RemoteMCPConn(nil), r.conns...), r.listErr
}

func (r *brokerRemoteResolver) AcquireTokenByID(_ context.Context, _, id string) (string, error) {
	r.asked = append(r.asked, id)
	return r.token, r.tokenErr
}

func (*brokerRemoteResolver) SafeHTTPClient() *http.Client { return http.DefaultClient }

func TestBrokerBackend_RemoteScopeOwnsCredentialedClient(t *testing.T) {
	const token = "test-remote-bearer"
	var sawBearer atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") == "Bearer "+token {
			sawBearer.Store(true)
		}
		var rpc struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(req.Body).Decode(&rpc); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if len(rpc.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := map[string]any{}
		switch rpc.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "echo", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "remote-ok"}}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result})
	}))
	t.Cleanup(srv.Close)

	b := newBrokerBackendForScopeTest(t)
	b.remoteMCP = &brokerRemoteResolver{
		conns: []agent.RemoteMCPConn{{ID: "remote-id", Name: "remote", URL: srv.URL}},
		token: token,
	}
	t.Cleanup(func() { _ = b.Close() })

	id, tools, skipped, err := b.OpenScope(context.Background(), mcpbroker.ScopeSpec{
		Remote: &mcpbroker.RemoteScopeSpec{UserEmail: "user@example.com"},
	})
	if err != nil {
		t.Fatalf("OpenScope: %v", err)
	}
	if len(tools) != 1 || tools[0].Server != "remote" || tools[0].Tool != "echo" {
		t.Fatalf("tools = %+v, want remote.echo", tools)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v", skipped)
	}
	publicMetadata, err := json.Marshal(struct {
		Tools   []mcpbroker.ToolDescriptor
		Skipped []string
	}{Tools: tools, Skipped: skipped})
	if err != nil {
		t.Fatalf("marshal public metadata: %v", err)
	}
	if strings.Contains(string(publicMetadata), token) {
		t.Fatal("remote bearer reached public scope metadata")
	}
	text, isErr, err := b.CallMCPInScope(context.Background(), id, "remote", "echo", nil)
	if err != nil || isErr || strings.TrimSpace(text) != "remote-ok" {
		t.Fatalf("CallMCPInScope = (%q, %v, %v)", text, isErr, err)
	}
	if !sawBearer.Load() {
		t.Fatal("child-owned remote client did not attach its bearer")
	}
	if err := b.CloseScope(context.Background(), id); err != nil {
		t.Fatalf("CloseScope: %v", err)
	}
}

func TestBrokerBackend_RemoteScopePreservesFilterAndSkippedSemantics(t *testing.T) {
	b := newBrokerBackendForScopeTest(t)
	resolver := &brokerRemoteResolver{
		conns:    []agent.RemoteMCPConn{{ID: "dead-id", Name: "dead", URL: "https://dead.example.com"}},
		tokenErr: errors.New("needs reauth"),
	}
	b.remoteMCP = resolver
	t.Cleanup(func() { _ = b.Close() })

	// Interactive explicit-none: no connection or token attempt, but the scope
	// still opens so its lifecycle is identical to a populated remote overlay.
	id, tools, skipped, err := b.OpenScope(context.Background(), mcpbroker.ScopeSpec{
		Remote: &mcpbroker.RemoteScopeSpec{UserEmail: "user@example.com", FilterEnabled: true},
	})
	if err != nil || len(tools) != 0 || len(skipped) != 0 || len(resolver.asked) != 0 {
		t.Fatalf("filtered empty scope = (tools=%v skipped=%v asked=%v err=%v)", tools, skipped, resolver.asked, err)
	}
	if err := b.CloseScope(context.Background(), id); err != nil {
		t.Fatalf("CloseScope: %v", err)
	}

	// Scheduled all-connected: the unavailable token is attempted and its public
	// server name is surfaced, while the underlying error remains child-side.
	_, tools, skipped, err = b.OpenScope(context.Background(), mcpbroker.ScopeSpec{
		Remote: &mcpbroker.RemoteScopeSpec{UserEmail: "user@example.com"},
	})
	if err != nil || len(tools) != 0 || len(skipped) != 1 || skipped[0] != "dead" {
		t.Fatalf("all-connected scope = (tools=%v skipped=%v err=%v)", tools, skipped, err)
	}
	if len(resolver.asked) != 1 || resolver.asked[0] != "dead-id" {
		t.Fatalf("token attempts = %v, want [dead-id]", resolver.asked)
	}
}

func TestBrokerBackend_RemoteScopeErrorIsValueFree(t *testing.T) {
	b := newBrokerBackendForScopeTest(t)
	const sensitiveDetail = "database-detail-must-not-cross-broker"
	b.remoteMCP = &brokerRemoteResolver{listErr: errors.New(sensitiveDetail)}
	t.Cleanup(func() { _ = b.Close() })

	_, _, _, err := b.OpenScope(context.Background(), mcpbroker.ScopeSpec{
		Remote: &mcpbroker.RemoteScopeSpec{UserEmail: "user@example.com"},
	})
	if err == nil || err.Error() != "remote MCP scope unavailable" {
		t.Fatalf("OpenScope error = %v, want value-free remote error", err)
	}
	if strings.Contains(err.Error(), sensitiveDetail) {
		t.Fatal("resolver detail crossed the broker boundary")
	}
}

func TestOpenMCPBrokerRemoteStore(t *testing.T) {
	if st, resolver, err := openMCPBrokerRemoteStore(&config.Config{}); err != nil || st != nil || resolver != nil {
		t.Fatalf("disabled store = (%v, %v, %v), want nils", st, resolver, err)
	}
	t.Run("refuses shared chat and sched database before open", func(t *testing.T) {
		const sameDSN = "postgres://db.example.com/shared"
		t.Setenv("FLEET_CHAT_DATABASE_URL", sameDSN)
		t.Setenv("FLEET_SCHED_DATABASE_URL", sameDSN)
		st, resolver, err := openMCPBrokerRemoteStore(&config.Config{
			PublicBaseURL:         "https://fleet.example.com",
			MCPOAuthEncryptionKey: []byte("0123456789abcdef0123456789abcdef"),
		})
		if err == nil || st != nil || resolver != nil {
			t.Fatalf("shared database check = (%v, %v, %v), want refusal", st, resolver, err)
		}
	})

	dsn := strings.TrimSpace(os.Getenv("FLEET_TEST_DATABASE_URL"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("CHAT_TEST_DATABASE_URL"))
	}
	if dsn == "" {
		t.Skip("FLEET_TEST_DATABASE_URL/CHAT_TEST_DATABASE_URL not set")
	}
	pool := config.DBPoolConfig{
		MaxOpenConns:    2,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
		ConnectTimeout:  5 * time.Second,
	}
	st, resolver, err := openMCPBrokerRemoteStore(&config.Config{
		DatabaseURL:           dsn,
		ChatDBPool:            pool,
		PublicBaseURL:         "https://fleet.example.com",
		MCPOAuthEncryptionKey: []byte("0123456789abcdef0123456789abcdef"),
	})
	if err != nil {
		t.Fatalf("openMCPBrokerRemoteStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if resolver == nil || !st.RemoteMCPEncryptionEnabled() {
		t.Fatal("enabled broker store did not install the remote resolver and cipher")
	}
	if got := st.PoolStats().MaxOpenConnections; got != pool.MaxOpenConns {
		t.Fatalf("broker store max connections = %d, want %d", got, pool.MaxOpenConns)
	}
}

func TestMCPBrokerRemoteConfigured(t *testing.T) {
	for _, prefix := range []string{"FLEET_", "CHAT_", "CUTLASS_"} {
		t.Setenv(prefix+"MCP_OAUTH_ENCRYPTION_KEY", "")
		t.Setenv(prefix+"PUBLIC_BASE_URL", "")
	}
	if mcpBrokerRemoteConfigured() {
		t.Fatal("empty remote environment reported configured")
	}
	t.Setenv("FLEET_MCP_OAUTH_ENCRYPTION_KEY", "configured-placeholder")
	if mcpBrokerRemoteConfigured() {
		t.Fatal("encryption key without public base URL reported configured")
	}
	// Config's canonical/legacy lookup is per field, so a canonical key and
	// legacy base URL are a valid inherited combination too.
	t.Setenv("CHAT_PUBLIC_BASE_URL", "https://fleet.example.com")
	if !mcpBrokerRemoteConfigured() {
		t.Fatal("complete canonical/legacy remote environment reported disabled")
	}
}

func TestBrokerBackend_CloseReapsOutstandingScopes(t *testing.T) {
	b := newBrokerBackendForScopeTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	id, _, _, err := b.OpenScope(ctx, mcpbroker.ScopeSpec{
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

	oldID, _, _, err := b.OpenScope(ctx, mcpbroker.ScopeSpec{Selection: []mcpbroker.ScopeChoice{{Server: "acct"}}})
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
	if len(result.Servers) != 1 || result.Servers[0].Name != "acct" || len(result.Servers[0].AccountVars) != 1 ||
		result.Servers[0].AccountVars[0] != "TEST_SCOPE_TOKEN" || !result.Servers[0].UsesWorkspace {
		t.Fatalf("reload servers = %+v, want public acct metadata", result.Servers)
	}

	oldText, _, err := b.CallMCPInScope(ctx, oldID, "acct", "identity", nil)
	if err != nil {
		t.Fatalf("old scope call: %v", err)
	}
	if strings.TrimSpace(oldText) != "default-seat|<unset>|<unset>" {
		t.Fatalf("old scope changed across reload: %q", oldText)
	}

	newID, _, _, err := b.OpenScope(ctx, mcpbroker.ScopeSpec{Selection: []mcpbroker.ScopeChoice{{Server: "acct"}}})
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
