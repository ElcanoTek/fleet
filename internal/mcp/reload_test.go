package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// mcpHTTPTestServer starts an in-process MCP-over-HTTP server advertising a
// single tool named toolName. Enough of the JSON-RPC lifecycle to satisfy
// Server.initialize (initialize + tools/list) and CallToolOn (tools/call).
func mcpHTTPTestServer(t *testing.T, toolName string) *httptest.Server {
	t.Helper()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "initialize":
			resp["result"] = map[string]any{"protocolVersion": "2024-11-05"}
		case "tools/list":
			resp["result"] = map[string]any{"tools": []Tool{{Name: toolName, Description: toolName}}}
		case "tools/call":
			resp["result"] = ToolResult{Content: []ContentBlock{{Type: "text", Text: toolName}}}
		default:
			resp["result"] = map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func serverNames(c *Client) map[string]bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]bool, len(c.servers))
	for n := range c.servers {
		out[n] = true
	}
	return out
}

// TestReload_AddRemoveRestartUnchanged exercises the four diff outcomes.
func TestReload_AddRemoveRestartUnchanged(t *testing.T) {
	ctx := context.Background()
	srvA := mcpHTTPTestServer(t, "tool_a")
	srvB := mcpHTTPTestServer(t, "tool_b")
	srvC := mcpHTTPTestServer(t, "tool_c")
	srvA2 := mcpHTTPTestServer(t, "tool_a2")

	c := NewClient()
	t.Cleanup(func() { _ = c.Close() })
	if err := c.AddHTTPServer(ctx, "A", srvA.URL); err != nil {
		t.Fatalf("add A: %v", err)
	}
	if err := c.AddHTTPServer(ctx, "B", srvB.URL); err != nil {
		t.Fatalf("add B: %v", err)
	}

	// Keep A unchanged, drop B, add C.
	sum, err := c.Reload(ctx, []ServerDef{
		{Name: "A", URL: srvA.URL},
		{Name: "C", URL: srvC.URL},
	})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !eqStrings(sum.Added, []string{"C"}) {
		t.Errorf("Added=%v want [C]", sum.Added)
	}
	if !eqStrings(sum.Removed, []string{"B"}) {
		t.Errorf("Removed=%v want [B]", sum.Removed)
	}
	if !eqStrings(sum.Unchanged, []string{"A"}) {
		t.Errorf("Unchanged=%v want [A]", sum.Unchanged)
	}
	if len(sum.Restarted) != 0 {
		t.Errorf("Restarted=%v want []", sum.Restarted)
	}
	if names := serverNames(c); !names["A"] || !names["C"] || names["B"] {
		t.Errorf("live servers after reload = %v; want A,C and not B", names)
	}

	// Restart A by pointing it at a different URL.
	sum, err = c.Reload(ctx, []ServerDef{
		{Name: "A", URL: srvA2.URL},
		{Name: "C", URL: srvC.URL},
	})
	if err != nil {
		t.Fatalf("reload restart: %v", err)
	}
	if !eqStrings(sum.Restarted, []string{"A"}) {
		t.Errorf("Restarted=%v want [A]", sum.Restarted)
	}
	if !eqStrings(sum.Unchanged, []string{"C"}) {
		t.Errorf("Unchanged=%v want [C]", sum.Unchanged)
	}
	// A now advertises tool_a2.
	var haveA2 bool
	for _, st := range c.GetAllTools() {
		if st.ServerName == "A" && st.Tool.Name == "tool_a2" {
			haveA2 = true
		}
	}
	if !haveA2 {
		t.Errorf("restarted server A should advertise tool_a2; tools=%+v", c.GetAllTools())
	}
}

// TestReload_PreservesSyntheticHTTPToolServer proves the synthetic inline-http
// tools server is never removed by a reload that doesn't mention it.
func TestReload_PreservesSyntheticHTTPToolServer(t *testing.T) {
	ctx := context.Background()
	c := NewClient()
	t.Cleanup(func() { _ = c.Close() })
	c.AddHTTPTools([]HTTPToolSpec{{Name: "inline_ping", URL: "https://example.test/ping", Method: "GET"}})
	if !c.HasServer(HTTPToolServerName) {
		t.Fatal("precondition: synthetic server should exist")
	}
	if _, err := c.Reload(ctx, nil); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !c.HasServer(HTTPToolServerName) {
		t.Error("reload with empty set must not remove the synthetic inline-http server")
	}
}

// TestReload_BuildFailureLeavesRegistryUnchanged verifies the all-or-nothing
// swap: if a new server fails to initialize, the live registry is untouched.
func TestReload_BuildFailureLeavesRegistryUnchanged(t *testing.T) {
	ctx := context.Background()
	srvA := mcpHTTPTestServer(t, "tool_a")
	c := NewClient()
	t.Cleanup(func() { _ = c.Close() })
	if err := c.AddHTTPServer(ctx, "A", srvA.URL); err != nil {
		t.Fatalf("add A: %v", err)
	}
	// B has neither Command nor URL → buildServer fails.
	_, err := c.Reload(ctx, []ServerDef{
		{Name: "A", URL: srvA.URL},
		{Name: "B"},
	})
	if err == nil {
		t.Fatal("expected reload to fail on an invalid server def")
	}
	if names := serverNames(c); !names["A"] || names["B"] {
		t.Errorf("registry after failed reload = %v; want unchanged (A only)", names)
	}
}

// TestReload_RetiredServerRefusesCall verifies that a caller holding a *Server
// captured (as CallToolOn does) just before a reload REMOVES it cannot call the
// now-closed transport: callTool refuses a retired server. This is what stops a
// removed stdio server from being resurrected (a new orphaned subprocess) via
// the dead-transport restart path.
func TestReload_RetiredServerRefusesCall(t *testing.T) {
	ctx := context.Background()
	srv := mcpHTTPTestServer(t, "tool_x")
	c := NewClient()
	t.Cleanup(func() { _ = c.Close() })
	if err := c.AddHTTPServer(ctx, "X", srv.URL); err != nil {
		t.Fatalf("add X: %v", err)
	}
	// Capture the *Server the way CallToolOn does, before the reload.
	c.mu.RLock()
	old := c.servers["X"]
	c.mu.RUnlock()

	if _, err := c.Reload(ctx, nil); err != nil { // removes X
		t.Fatalf("reload: %v", err)
	}
	if _, err := old.callTool(ctx, "tool_x", nil); err == nil {
		t.Error("callTool on a retired server must refuse, got nil error")
	}
}

// TestReload_CompletesWhileStdinWedged verifies the ctx-aware stdin write
// (#1108) actually breaks the wedge cascade: an in-flight callTool blocked
// writing to a subprocess that stopped reading stdin holds Server.mu,
// drainAndClose waits on that mutex, and Reload waits on the drain while
// holding reloadMu — before the fix, one wedged connector disabled hot-reload
// permanently. Now the in-flight call fails when its ctx expires, the drain
// proceeds, and Reload completes with the wedged server removed and retired.
func TestReload_CompletesWhileStdinWedged(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found, skipping stdio test")
	}
	old := stdioCloseGrace
	stdioCloseGrace = 200 * time.Millisecond
	defer func() { stdioCloseGrace = old }()

	// A child that never reads stdin: a request past the 64 KiB kernel pipe
	// buffer blocks the writer.
	tr, err := NewStdioTransport("python3", []string{"-u", "-c", "import time\nwhile True: time.sleep(60)"}, nil)
	if err != nil {
		t.Fatalf("start non-reading child: %v", err)
	}
	srv := &Server{
		name:         "wedged",
		transport:    tr,
		def:          ServerDef{Name: "wedged", Command: "python3"},
		stdioCommand: "python3",
	}
	c := NewClient()
	c.servers["wedged"] = srv
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	callDone := make(chan error, 1)
	go func() {
		_, err := srv.callTool(ctx, "anything", map[string]interface{}{"blob": strings.Repeat("x", 1<<20)})
		callDone <- err
	}()

	// Let the call enter the blocked write (holding Server.mu) first.
	time.Sleep(50 * time.Millisecond)

	reloadDone := make(chan error, 1)
	go func() {
		sum, rerr := c.Reload(context.Background(), nil) // removes "wedged"
		if rerr == nil && !eqStrings(sum.Removed, []string{"wedged"}) {
			rerr = fmt.Errorf("Removed=%v want [wedged]", sum.Removed)
		}
		reloadDone <- rerr
	}()

	select {
	case err := <-reloadDone:
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Reload wedged behind a blocked stdin write — the cascade is not broken")
	}
	if err := <-callDone; err == nil {
		t.Fatal("wedged call unexpectedly succeeded")
	}
	// The drained server is retired: a caller still holding the pointer is
	// refused instead of respawning the subprocess.
	if _, err := srv.callTool(context.Background(), "anything", nil); err == nil {
		t.Fatal("retired wedged server accepted a call")
	}
}

// TestReload_Concurrent hammers CallToolOn / GetAllTools / CallToolPrefixed from
// many goroutines while Reload repeatedly flips the server set. Run under -race,
// it exercises the Client.mu / Server.mu / map-swap interplay for data races and
// deadlocks.
func TestReload_Concurrent(t *testing.T) {
	ctx := context.Background()
	srv1 := mcpHTTPTestServer(t, "t1")
	srv2 := mcpHTTPTestServer(t, "t2")
	srv3 := mcpHTTPTestServer(t, "t3")

	c := NewClient()
	t.Cleanup(func() { _ = c.Close() })
	if err := c.AddHTTPServer(ctx, "s1", srv1.URL); err != nil {
		t.Fatalf("add s1: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// Readers: concurrent tool calls + catalog reads.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = c.CallToolOn(ctx, "s1", "t1", nil)
				_ = c.GetAllTools()
				_, _ = c.CallToolPrefixed(ctx, "mcp_s2_t2", nil)
			}
		}()
	}

	// Reloader: flip between three server-set shapes.
	sets := [][]ServerDef{
		{{Name: "s1", URL: srv1.URL}, {Name: "s2", URL: srv2.URL}},
		{{Name: "s1", URL: srv1.URL}, {Name: "s3", URL: srv3.URL}},
		{{Name: "s2", URL: srv2.URL}, {Name: "s3", URL: srv3.URL}},
	}
	for i := 0; i < 30; i++ {
		if _, err := c.Reload(ctx, sets[i%len(sets)]); err != nil {
			t.Errorf("reload %d: %v", i, err)
			break
		}
	}
	close(stop)
	wg.Wait()
}
