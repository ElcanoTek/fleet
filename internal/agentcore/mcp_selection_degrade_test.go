package agentcore

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

	registered, err := BindMCPSelection(context.Background(), client, selection, bases, "")
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

	if _, err := BindMCPSelection(context.Background(), client, selection, bases, ""); err == nil {
		t.Fatal("a Required server failing to register must abort (return an error)")
	}
}

// TestBindMCPSelection_UnknownServerStillAborts pins that a config error (a
// selection naming a server absent from the catalog) remains fatal — graceful
// degradation covers runtime start failures, not misconfiguration.
func TestBindMCPSelection_UnknownServerStillAborts(t *testing.T) {
	client := mcp.NewClient()
	if _, err := BindMCPSelection(context.Background(), client, MCPSelection{{Server: "ghost"}}, map[string]MCPServerBase{}, ""); err == nil {
		t.Fatal("an unknown/uncataloged server must remain a fatal config error")
	}
}

// TestBindMCPSelectionReport_RecordsAndRetriesFailedServers: a best-effort
// HTTP server that cannot be reached is retried while the failure is
// transient, then skipped AND reported with a credential-free detail, and a
// refused credential is reported as not transient after a single attempt.
func TestBindMCPSelectionReport_RecordsAndRetriesFailedServers(t *testing.T) {
	saved := mcp.ConnectRetryDelays
	mcp.ConnectRetryDelays = []time.Duration{0, 0}
	t.Cleanup(func() { mcp.ConnectRetryDelays = saved })

	// A listener that is closed at once: connecting to it is refused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refusedURL := "http://" + ln.Addr().String() + "/mcp"
	_ = ln.Close()

	var unauthorizedHits atomic.Int32
	unauthorized := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		unauthorizedHits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer unauthorized.Close()

	client := mcp.NewClient()
	defer func() { _ = client.Close() }()
	bases := map[string]MCPServerBase{
		"pages":  {HTTPURL: refusedURL},
		"vendor": {HTTPURL: unauthorized.URL},
		"flaky":  {Command: badCommand},
	}
	registered, failed, err := BindMCPSelectionReport(mcp.WithConnectRetry(context.Background()), client,
		MCPSelection{{Server: "flaky"}, {Server: "pages"}, {Server: "vendor"}}, bases, "")
	if err != nil {
		t.Fatalf("best-effort failures must not abort: %v", err)
	}
	if len(registered) != 0 {
		t.Fatalf("registered = %v, want none", registered)
	}
	byServer := map[string]MCPConnectFailure{}
	for _, f := range failed {
		byServer[f.Server] = f
	}
	if f := byServer["pages"]; !f.Transient || f.Detail != "connection refused" {
		t.Errorf("pages failure = %+v, want transient connection refused", f)
	}
	if strings.Contains(byServer["pages"].Detail, ln.Addr().String()) {
		t.Error("the failure detail must not quote the server's address")
	}
	if f := byServer["vendor"]; f.Transient || f.Detail != "HTTP 401 Unauthorized" {
		t.Errorf("vendor failure = %+v, want a non-transient 401", f)
	}
	if got := unauthorizedHits.Load(); got != 1 {
		t.Errorf("a refused credential was attempted %d times, want 1", got)
	}
	if _, ok := byServer["flaky"]; !ok {
		t.Error("a stdio server that failed to start must be reported too")
	}
}
