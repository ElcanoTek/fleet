package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This suite is the union of chat's and cutlass's MCP client tests, ported
// verbatim against the merged client. There were no test-function-name
// collisions between the two suites, so every test is kept under its
// original name; the two halves are separated by the banner comments below.

// ===========================================================================
// chat suite — StdioTransport per-call framing, cancel poisoning, and the
// server-scoped (collision-proof) routing paths CallToolOn / CallToolPrefixed.
// ===========================================================================

// fakeServerScript is a minimal line-oriented JSON-RPC responder used to
// exercise StdioTransport against a real subprocess. Behavior is driven
// by the request's method name so each test controls the wire traffic:
//
//	echo            → respond with {"echo": <id>} immediately
//	notify_first    → emit an id-less notification line, then respond
//	stale_first     → emit a response with id 999999, then the real one
//	sleep_forever   → never respond (used for cancel/desync tests)
//	rpc_error       → respond with a JSON-RPC error whose message contains "EOF"
//	huge            → respond with a blob of params["bytes"] bytes (capped-read test)
const fakeServerScript = `
import json, sys
for line in sys.stdin:
    req = json.loads(line)
    rid, method = req.get("id"), req.get("method")
    def send(obj):
        sys.stdout.write(json.dumps(obj) + "\n")
        sys.stdout.flush()
    if method == "sleep_forever":
        continue
    if method == "huge":
        send({"jsonrpc": "2.0", "id": rid, "result": {"blob": "x" * req["params"]["bytes"]}})
        continue
    if method == "notify_first":
        send({"jsonrpc": "2.0", "method": "notifications/progress", "params": {"p": 1}})
    if method == "stale_first":
        send({"jsonrpc": "2.0", "id": 999999, "result": {"stale": True}})
    if method == "rpc_error":
        send({"jsonrpc": "2.0", "id": rid, "error": {"code": -32000, "message": "Unexpected EOF while parsing CSV"}})
        continue
    send({"jsonrpc": "2.0", "id": rid, "result": {"echo": rid}})
`

func newFakeTransport(t *testing.T) *StdioTransport {
	t.Helper()
	tr, err := NewStdioTransport("python3", []string{"-u", "-c", fakeServerScript}, nil)
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

func callEcho(t *testing.T, tr *StdioTransport, method string) (json.RawMessage, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return tr.Call(ctx, method, map[string]any{})
}

func TestStdioCall_RoundTrip(t *testing.T) {
	tr := newFakeTransport(t)
	res, err := callEcho(t, tr, "echo")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(string(res), `"echo": 1`) && !strings.Contains(string(res), `"echo":1`) {
		t.Fatalf("unexpected result: %s", res)
	}
}

func TestStdioCall_SkipsNotifications(t *testing.T) {
	tr := newFakeTransport(t)
	res, err := callEcho(t, tr, "notify_first")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if strings.Contains(string(res), "progress") {
		t.Fatalf("notification leaked as result: %s", res)
	}
	if !strings.Contains(string(res), "echo") {
		t.Fatalf("expected echo result, got: %s", res)
	}
}

func TestStdioCall_DiscardsMismatchedIDs(t *testing.T) {
	tr := newFakeTransport(t)
	res, err := callEcho(t, tr, "stale_first")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if strings.Contains(string(res), "stale") {
		t.Fatalf("stale response with wrong id returned to caller: %s", res)
	}
}

// TestStdioCall_OversizedResponseFailsLoud is the regression guard for the
// unbounded response read: a response line past stdioResponseCaptureCap must
// fail THIS call with an explicit over-cap error — never be buffered whole in
// host memory, never be silently truncated into a parseable-but-wrong result,
// and never kill the transport (the reader drains to the delimiter, so the
// stream stays framed and the next call works without a restart). Before the
// fix the whole line was buffered and returned as a successful result.
func TestStdioCall_OversizedResponseFailsLoud(t *testing.T) {
	tr := newFakeTransport(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// The blob alone exceeds the cap; the JSON envelope only adds to it.
	_, err := tr.Call(ctx, "huge", map[string]any{"bytes": stdioResponseCaptureCap + 1})
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("Call = %v, want an over-cap error", err)
	}
	if isTransportDeadError(err) {
		t.Errorf("over-cap error %q matches the dead-transport markers — a healthy subprocess would be restarted for nothing", err)
	}
	if isRequestNotDeliveredError(err) {
		t.Errorf("over-cap error %q reads as not-delivered — a retry would just fetch the same oversized response", err)
	}

	// The drain left the stream framed: the same transport must answer the
	// next call, with no restart in between.
	res, err := callEcho(t, tr, "echo")
	if err != nil {
		t.Fatalf("call after oversized response: %v — the drain broke the framing", err)
	}
	if !strings.Contains(string(res), "echo") {
		t.Fatalf("unexpected result after oversized response: %s", res)
	}
}

func TestStdioCall_CancelPoisonsTransport(t *testing.T) {
	tr := newFakeTransport(t)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := tr.Call(ctx, "sleep_forever", map[string]any{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error, got: %v", err)
	}

	// The next call must fail fast with the desync sentinel rather than
	// racing the orphaned reader for whatever the server writes next.
	if _, err := callEcho(t, tr, "echo"); !errors.Is(err, errTransportDesynced) {
		t.Fatalf("expected errTransportDesynced, got: %v", err)
	}
	if !isTransportDeadError(errTransportDesynced) {
		t.Fatal("desync sentinel must route through the dead-transport restart path")
	}
}

// TestStdioWrite_WedgedStdinHonorsCtx is the regression guard for the
// ctx-unaware stdin write (#1108): a subprocess that stops reading stdin
// leaves the pipe (64 KiB kernel buffer) full, and a request larger than the
// buffer blocks the writer forever. Before the fix t.stdin.Write ran bare
// under t.mu, so Call ignored its context — and because Server.callTool holds
// Server.mu for the whole call, one wedged connector cascaded into
// drainAndClose → Reload → reloadMu, permanently disabling hot-reload. The
// write must observe ctx, and the timed-out transport must be poisoned so the
// existing restart path recovers.
func TestStdioWrite_WedgedStdinHonorsCtx(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found, skipping stdio test")
	}
	old := stdioCloseGrace
	stdioCloseGrace = 200 * time.Millisecond
	defer func() { stdioCloseGrace = old }()

	// A child that never reads stdin: the pipe fills and every later write
	// byte blocks in the kernel.
	tr, err := NewStdioTransport("python3", []string{"-u", "-c", "import time\nwhile True: time.sleep(60)"}, nil)
	if err != nil {
		t.Fatalf("start non-reading child: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		// 1 MiB of arguments — model-authored args exceed the 64 KiB kernel
		// pipe buffer easily, so the write cannot complete.
		_, err := tr.Call(ctx, "tools/call", map[string]any{"blob": strings.Repeat("x", 1<<20)})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Call = %v, want a ctx deadline error", err)
		}
		if !isRequestNotDeliveredError(err) {
			t.Errorf("wedged-write error %q must read as not-delivered — the server never received a full request line", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Call ignored ctx while writing to a wedged subprocess — the write path is not context-aware")
	}

	// The transport is poisoned: the next call fails fast with the desync
	// sentinel so Server.callTool routes through the restart path.
	if _, err := tr.Call(context.Background(), "echo", map[string]any{}); !errors.Is(err, errTransportDesynced) {
		t.Fatalf("post-wedge call = %v, want errTransportDesynced", err)
	}
}

// TestStdioNotify_WedgedStdinHonorsCtx: Notify shares writeLocked with Call,
// but it previously discarded its ctx outright (`_ context.Context`) — pin
// the behavior change directly rather than only through Call.
func TestStdioNotify_WedgedStdinHonorsCtx(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found, skipping stdio test")
	}
	old := stdioCloseGrace
	stdioCloseGrace = 200 * time.Millisecond
	defer func() { stdioCloseGrace = old }()

	tr, err := NewStdioTransport("python3", []string{"-u", "-c", "import time\nwhile True: time.sleep(60)"}, nil)
	if err != nil {
		t.Fatalf("start non-reading child: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- tr.Notify(ctx, "notifications/progress", map[string]any{"blob": strings.Repeat("x", 1<<20)})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Notify = %v, want a ctx deadline error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Notify ignored ctx while writing to a wedged subprocess")
	}

	// Poisoned like a wedged Call: framing can't be trusted mid-write.
	if _, err := tr.Call(context.Background(), "echo", map[string]any{}); !errors.Is(err, errTransportDesynced) {
		t.Fatalf("post-wedge call = %v, want errTransportDesynced", err)
	}
}

func TestStdioClose_KillsHungChild(t *testing.T) {
	old := stdioCloseGrace
	stdioCloseGrace = 200 * time.Millisecond
	defer func() { stdioCloseGrace = old }()

	// A child that ignores stdin EOF and sleeps forever.
	tr, err := NewStdioTransport("python3", []string{"-u", "-c", "import time\nwhile True: time.sleep(60)"}, nil)
	if err != nil {
		t.Fatalf("start hung child: %v", err)
	}
	done := make(chan struct{})
	go func() {
		_ = tr.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return — hung child wedged it")
	}
}

func TestIsTransportDeadError_ExcludesRPCErrors(t *testing.T) {
	rpcErr := &RPCError{Code: -32000, Message: "Unexpected EOF while parsing CSV"}
	if isTransportDeadError(rpcErr) {
		t.Fatal("application-level RPC error mentioning EOF must not trigger a subprocess restart")
	}
	if !isTransportDeadError(errors.New("read |0: file already closed")) {
		t.Fatal("real transport error should be detected")
	}
}

func TestStdioCall_RPCErrorDoesNotPoison(t *testing.T) {
	tr := newFakeTransport(t)
	_, err := callEcho(t, tr, "rpc_error")
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("expected RPCError, got: %v", err)
	}
	// Transport must remain usable after an application error.
	if _, err := callEcho(t, tr, "echo"); err != nil {
		t.Fatalf("transport unusable after RPC error: %v", err)
	}
}

// fakeTransport lets the routing tests below install canned tool lists
// without real subprocesses.
type fakeTransport struct {
	calls []string
}

func (f *fakeTransport) Call(_ context.Context, method string, params interface{}) (json.RawMessage, error) {
	if method == "tools/call" {
		p := params.(map[string]interface{})
		f.calls = append(f.calls, p["name"].(string))
	}
	return json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`), nil
}

func (f *fakeTransport) Notify(_ context.Context, _ string, _ interface{}) error { return nil }

func (f *fakeTransport) Close() error { return nil }

func newRoutingClient() (*Client, map[string]*fakeTransport) {
	c := NewClient()
	transports := map[string]*fakeTransport{}
	add := func(name string, tools ...string) {
		ft := &fakeTransport{}
		transports[name] = ft
		srv := &Server{name: name, transport: ft}
		for _, tl := range tools {
			srv.tools = append(srv.tools, Tool{Name: tl})
		}
		c.servers[name] = srv
	}
	// sendgrid and mailbux deliberately overlap on send_email — the
	// production collision this routing exists to prevent.
	add("sendgrid", "send_email", "validate_email_content")
	add("mailbux", "send_email", "search_emails")
	add("email", "search_emails", "get_email")
	return c, transports
}

func TestCallToolOn_RoutesToNamedServer(t *testing.T) {
	c, transports := newRoutingClient()
	if _, err := c.CallToolOn(context.Background(), "mailbux", "send_email", nil); err != nil {
		t.Fatalf("CallToolOn: %v", err)
	}
	if len(transports["mailbux"].calls) != 1 || len(transports["sendgrid"].calls) != 0 {
		t.Fatalf("send_email routed to the wrong server: sendgrid=%v mailbux=%v",
			transports["sendgrid"].calls, transports["mailbux"].calls)
	}
	if _, err := c.CallToolOn(context.Background(), "nope", "send_email", nil); err == nil {
		t.Fatal("expected error for unknown server")
	}
}

func TestCallToolPrefixed_ResolvesServerFromFullName(t *testing.T) {
	c, transports := newRoutingClient()
	if _, err := c.CallToolPrefixed(context.Background(), "mcp_mailbux_send_email", nil); err != nil {
		t.Fatalf("CallToolPrefixed: %v", err)
	}
	if len(transports["mailbux"].calls) != 1 || len(transports["sendgrid"].calls) != 0 {
		t.Fatalf("prefixed routing hit the wrong server: sendgrid=%v mailbux=%v",
			transports["sendgrid"].calls, transports["mailbux"].calls)
	}
	if _, err := c.CallToolPrefixed(context.Background(), "mcp_unknown_send_email", nil); err == nil {
		t.Fatal("expected error for unknown prefixed server")
	}
	if _, err := c.CallToolPrefixed(context.Background(), "bash", nil); err == nil {
		t.Fatal("expected error for non-MCP name")
	}
}

// argsCaptureTransport records the exact params map of the last tools/call.
type argsCaptureTransport struct {
	lastParams map[string]interface{}
}

func (f *argsCaptureTransport) Call(_ context.Context, method string, params interface{}) (json.RawMessage, error) {
	if method == "tools/call" {
		f.lastParams, _ = params.(map[string]interface{})
	}
	return json.RawMessage(`{"content":[{"type":"text","text":"ok"}]}`), nil
}

func (f *argsCaptureTransport) Notify(_ context.Context, _ string, _ interface{}) error { return nil }

func (f *argsCaptureTransport) Close() error { return nil }

// A nil arguments map would marshal to "arguments": null on the wire, which
// strict MCP servers reject with -32602 (arguments must be an object when
// present). callTool must forward an empty object instead. Regression for the
// pages.list_templates failure where empty model args crossed the mcpbroker
// boundary (args,omitempty) as nil and the server refused the call.
func TestCallTool_NilArgumentsBecomeEmptyObject(t *testing.T) {
	ft := &argsCaptureTransport{}
	c := NewClient()
	c.servers["pages"] = &Server{
		name:      "pages",
		transport: ft,
		tools:     []Tool{{Name: "list_templates"}},
	}

	if _, err := c.CallToolOn(context.Background(), "pages", "list_templates", nil); err != nil {
		t.Fatalf("CallToolOn: %v", err)
	}
	args, ok := ft.lastParams["arguments"]
	if !ok {
		t.Fatal("tools/call params missing arguments")
	}
	m, ok := args.(map[string]interface{})
	if !ok || m == nil {
		t.Fatalf("arguments = %#v, want a non-nil empty object", args)
	}
	if len(m) != 0 {
		t.Fatalf("arguments = %#v, want empty", m)
	}
}

// ===========================================================================
// cutlass suite — Client integration over stdio/HTTP transports, SSE parsing,
// RPCError / JSONRPCID handling, the env-UTF8 contract, and the write-vs-read
// delivery distinction (isRequestNotDeliveredError).
// ===========================================================================

func TestStdioTransport(t *testing.T) {
	_, filename, _, _ := runtime.Caller(0)
	dir := filepath.Dir(filename)
	scriptPath := filepath.Join(dir, "testdata", "dummy_server.py")

	client := NewClient()
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Check if python3 is available
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found, skipping stdio test")
	}

	// Use python3 explicitly
	err := client.AddStdioServer(ctx, "dummy", "python3", []string{scriptPath}, nil, "")
	if err != nil {
		t.Fatalf("Failed to add stdio server: %v", err)
	}

	tools := client.GetAllTools()
	if len(tools) != 1 {
		t.Errorf("Expected 1 tool, got %d", len(tools))
	}

	if tools[0].Tool.Name != "echo" {
		t.Errorf("Expected tool name 'echo', got '%s'", tools[0].Tool.Name)
	}

	// Test CallTool
	result, err := client.CallTool(ctx, "echo", map[string]interface{}{"message": "hello"})
	if err != nil {
		t.Fatalf("Failed to call tool: %v", err)
	}

	if len(result.Content) == 0 {
		t.Errorf("Expected content in result")
	} else if result.Content[0].Text != "Echo: hello" {
		t.Errorf("Expected 'Echo: hello', got '%s'", result.Content[0].Text)
	}
}

// TestStdioServer_RelativeArgResolvesAgainstDir is the regression guard for the
// systemd-deploy bug where a bundle's relative `mcp/*.py` arg failed to launch
// because the subprocess inherited the fleet process cwd (/opt/fleet) instead of
// the bundle root (/opt/fleet/client). With dir set to the directory holding the
// script the relative arg resolves; with the wrong dir it does not.
func TestStdioServer_RelativeArgResolvesAgainstDir(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found, skipping stdio dir test")
	}
	_, filename, _, _ := runtime.Caller(0)
	pkgDir := filepath.Dir(filename)
	relArg := filepath.Join("testdata", "dummy_server.py") // relative to dir

	t.Run("correct dir launches the relative arg", func(t *testing.T) {
		client := NewClient()
		defer client.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := client.AddStdioServer(ctx, "rel", "python3", []string{relArg}, nil, pkgDir); err != nil {
			t.Fatalf("AddStdioServer with correct dir: %v", err)
		}
		tools := client.GetAllTools()
		if len(tools) != 1 || tools[0].Tool.Name != "echo" {
			t.Fatalf("expected the echo tool from the relative-arg server, got %+v", tools)
		}
	})

	t.Run("wrong dir cannot find the relative arg", func(t *testing.T) {
		client := NewClient()
		defer client.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// An empty temp dir does NOT contain testdata/dummy_server.py, so the
		// subprocess fails to start and initialization errors.
		if err := client.AddStdioServer(ctx, "rel", "python3", []string{relArg}, nil, t.TempDir()); err == nil {
			t.Fatal("expected AddStdioServer to fail when dir does not contain the relative script, but it succeeded")
		}
	})
}

func TestHTTPTransport(t *testing.T) {
	// Setup mock HTTP server
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		resp := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      req.ID,
		}

		switch req.Method {
		case "initialize":
			resp["result"] = map[string]interface{}{
				"protocolVersion": "2024-11-05",
			}
		case "tools/list":
			resp["result"] = map[string]interface{}{
				"tools": []Tool{
					{Name: "remote_echo", Description: "Remote Echo"},
				},
			}
		case "tools/call":
			resp["result"] = ToolResult{
				Content: []ContentBlock{
					{Type: "text", Text: "Remote Echo"},
				},
			}
		default:
			resp["error"] = map[string]interface{}{
				"code":    -32601,
				"message": "Method not found",
			}
		}

		json.NewEncoder(w).Encode(resp)
	})

	server := httptest.NewServer(handler)
	defer server.Close()

	client := NewClient()
	defer client.Close()

	ctx := context.Background()

	err := client.AddHTTPServer(ctx, "remote", server.URL)
	if err != nil {
		t.Fatalf("Failed to add HTTP server: %v", err)
	}

	tools := client.GetAllTools()
	if len(tools) != 1 {
		t.Errorf("Expected 1 tool, got %d", len(tools))
	}

	if tools[0].Tool.Name != "remote_echo" {
		t.Errorf("Expected tool name 'remote_echo', got '%s'", tools[0].Tool.Name)
	}

	result, err := client.CallTool(ctx, "remote_echo", nil)
	if err != nil {
		t.Fatalf("Failed to call tool: %v", err)
	}

	if result.Content[0].Text != "Remote Echo" {
		t.Errorf("Expected 'Remote Echo', got '%s'", result.Content[0].Text)
	}
}

func TestClient_Close(t *testing.T) {
	client := NewClient()
	if err := client.Close(); err != nil {
		t.Errorf("Close failed: %v", err)
	}
}

// blockingDeadTransport blocks Call until released, then fails with a
// dead-transport-shaped error — simulating a transport whose connection dies
// while Client.Close runs.
type blockingDeadTransport struct {
	entered chan struct{} // closed when Call is in flight (under Server.mu)
	release chan struct{} // Call returns its error once this closes
}

func (b *blockingDeadTransport) Call(context.Context, string, interface{}) (json.RawMessage, error) {
	close(b.entered)
	<-b.release
	return nil, fmt.Errorf("write |1: broken pipe")
}

func (b *blockingDeadTransport) Notify(context.Context, string, interface{}) error { return nil }

func (b *blockingDeadTransport) Close() error { return nil }

// TestClose_RetiresServers_NoPostCloseRespawn is the #1108 regression guard
// for the Close/restart race: an in-flight callTool whose transport died
// while Client.Close ran matched isTransportDeadError, and restartLocked
// respawned a credentialed stdio subprocess AFTER Close returned — with
// nothing left to ever close it. Close must retire each server under its own
// mutex, mirroring reload's drainAndClose: the drain waits out the racing
// call, closes whatever transport it left behind (a respawned one included),
// and `retired` refuses every later call.
func TestClose_RetiresServers_NoPostCloseRespawn(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found, skipping stdio respawn test")
	}
	bt := &blockingDeadTransport{entered: make(chan struct{}), release: make(chan struct{})}
	c := NewClient()
	srv := &Server{
		name:      "racy",
		transport: bt,
		// A real, restartable stdio command: without the retire fix the
		// dead-transport path respawns this as a live post-Close subprocess.
		stdioCommand: "python3",
		stdioArgs:    []string{"-u", "-c", fakeServerScript},
	}
	c.servers["racy"] = srv

	callDone := make(chan error, 1)
	go func() {
		_, err := srv.callTool(context.Background(), "echo", nil)
		callDone <- err
	}()
	<-bt.entered // the call is in flight, holding Server.mu

	closeDone := make(chan error, 1)
	go func() { closeDone <- c.Close() }()

	// Give Close time to reach the drain: on the fixed code it now blocks on
	// Server.mu; on the old code it has already returned without retiring
	// anything. Then let the in-flight call fail with its dead transport.
	time.Sleep(100 * time.Millisecond)
	close(bt.release)

	select {
	case <-closeDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return")
	}
	callErr := <-callDone

	// Whatever the racing call did, no live subprocess may survive Close.
	srv.mu.Lock()
	tr := srv.transport
	srv.mu.Unlock()
	if st, ok := tr.(*StdioTransport); ok {
		// ProcessState is only set once Wait has collected the child — i.e.
		// only if Close reaped whatever the racing call respawned.
		if st.cmd.ProcessState == nil {
			_ = st.Close() // don't leak the orphan out of the test
			t.Fatalf("credentialed subprocess survived Close (racing call returned: %v) — Close does not retire servers", callErr)
		}
	}

	// And the retired server refuses any later call instead of respawning.
	if _, err := srv.callTool(context.Background(), "echo", nil); err == nil {
		t.Fatal("post-Close call was accepted — server not retired")
	}
}

func TestCallToolNotFound(t *testing.T) {
	client := NewClient()
	_, err := client.CallTool(context.Background(), "nonexistent", nil)
	if err == nil {
		t.Error("Expected error for nonexistent tool")
	}
}

func TestHTTPTransportWithHeaders(t *testing.T) {
	// Setup mock HTTP server that validates headers
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Validate custom headers
		authHeader := r.Header.Get("Authorization")
		instanceHeader := r.Header.Get("X-Test-Instance")

		if authHeader != "Bearer test-token" {
			t.Errorf("Expected Authorization='Bearer test-token', got '%s'", authHeader)
		}
		if instanceHeader != "https://test.example.com" {
			t.Errorf("Expected X-Test-Instance='https://test.example.com', got '%s'", instanceHeader)
		}

		// Validate Accept header includes both application/json and text/event-stream
		acceptHeader := r.Header.Get("Accept")
		if !strings.Contains(acceptHeader, "application/json") {
			t.Errorf("Accept header should contain 'application/json', got '%s'", acceptHeader)
		}
		if !strings.Contains(acceptHeader, "text/event-stream") {
			t.Errorf("Accept header should contain 'text/event-stream', got '%s'", acceptHeader)
		}

		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		resp := map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      req.ID,
		}

		switch req.Method {
		case "initialize":
			resp["result"] = map[string]interface{}{
				"protocolVersion": "2024-11-05",
			}
		case "tools/list":
			resp["result"] = map[string]interface{}{
				"tools": []Tool{
					{Name: "remote_tool", Description: "Remote Tool"},
				},
			}
		default:
			resp["error"] = map[string]interface{}{
				"code":    -32601,
				"message": "Method not found",
			}
		}

		json.NewEncoder(w).Encode(resp)
	})

	server := httptest.NewServer(handler)
	defer server.Close()

	client := NewClient()
	defer client.Close()

	ctx := context.Background()

	// Add HTTP server with custom authentication headers
	headers := map[string]string{
		"Authorization":   "Bearer test-token",
		"X-Test-Instance": "https://test.example.com",
	}
	err := client.AddHTTPServerWithHeaders(ctx, "remote", server.URL, headers)
	if err != nil {
		t.Fatalf("Failed to add HTTP server with headers: %v", err)
	}

	tools := client.GetAllTools()
	if len(tools) != 1 {
		t.Errorf("Expected 1 tool, got %d", len(tools))
	}

	if tools[0].Tool.Name != "remote_tool" {
		t.Errorf("Expected tool name 'remote_tool', got '%s'", tools[0].Tool.Name)
	}
	if tools[0].ServerName != "remote" {
		t.Errorf("Expected server name 'remote', got '%s'", tools[0].ServerName)
	}
}

// TestHTTPTransportAcceptHeader tests that MCP requests include proper Accept headers
func TestHTTPTransportAcceptHeader(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Validate Content-Type
		contentType := r.Header.Get("Content-Type")
		if contentType != "application/json" {
			t.Errorf("Content-Type should be 'application/json', got '%s'", contentType)
		}

		// Validate Accept header includes both required types (some servers require this)
		acceptHeader := r.Header.Get("Accept")
		if !strings.Contains(acceptHeader, "application/json") {
			t.Errorf("Accept header must contain 'application/json', got '%s'", acceptHeader)
		}
		if !strings.Contains(acceptHeader, "text/event-stream") {
			t.Errorf("Accept header must contain 'text/event-stream', got '%s'", acceptHeader)
		}

		// Return success response
		var req struct {
			ID int `json:"id"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  map[string]interface{}{"protocolVersion": "2024-11-05"},
		})
	})

	server := httptest.NewServer(handler)
	defer server.Close()

	transport := NewHTTPTransport(server.URL)
	ctx := context.Background()

	_, err := transport.Call(ctx, "initialize", map[string]interface{}{})
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
}

// TestHTTPTransportSessionID tests that MCP session ID is captured and forwarded
func TestHTTPTransportSessionID(t *testing.T) {
	requestCount := 0
	expectedSessionID := "test-session-12345"

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++

		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		json.NewDecoder(r.Body).Decode(&req)

		if requestCount == 1 {
			// First request (initialize) - return session ID in response
			w.Header().Set("Mcp-Session-Id", expectedSessionID)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  map[string]interface{}{"protocolVersion": "2024-11-05"},
			})
		} else {
			// Subsequent requests - verify session ID is included
			sessionID := r.Header.Get("Mcp-Session-Id")
			if sessionID != expectedSessionID {
				t.Errorf("Request %d: Expected Mcp-Session-Id=%s, got %s", requestCount, expectedSessionID, sessionID)
			}

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result":  map[string]interface{}{"tools": []interface{}{}},
			})
		}
	})

	server := httptest.NewServer(handler)
	defer server.Close()

	transport := NewHTTPTransport(server.URL)
	ctx := context.Background()

	// First call (initialize) - should capture session ID
	_, err := transport.Call(ctx, "initialize", map[string]interface{}{})
	if err != nil {
		t.Fatalf("First call failed: %v", err)
	}

	// Second call (tools/list) - should include session ID
	_, err = transport.Call(ctx, "tools/list", map[string]interface{}{})
	if err != nil {
		t.Fatalf("Second call failed: %v", err)
	}

	if requestCount != 2 {
		t.Errorf("Expected 2 requests, got %d", requestCount)
	}
}

// TestCaptureSessionIDVariants tests that session ID is captured from various header names
func TestCaptureSessionIDVariants(t *testing.T) {
	tests := []struct {
		name       string
		headerName string
		headerVal  string
	}{
		{"standard case", "Mcp-Session-Id", "session-1"},
		{"uppercase", "MCP-SESSION-ID", "session-2"},
		{"lowercase", "mcp-session-id", "session-3"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transport := &HTTPTransport{}
			headers := http.Header{}
			headers.Set(tt.headerName, tt.headerVal)

			transport.captureSessionID(headers)

			if transport.sessionID != tt.headerVal {
				t.Errorf("Expected sessionID=%s, got %s", tt.headerVal, transport.sessionID)
			}
		})
	}
}

// TestHTTPTransportSSEResponse tests that SSE (Server-Sent Events) responses are parsed correctly
func TestHTTPTransportSSEResponse(t *testing.T) {
	tests := []struct {
		name        string
		sseResponse string
		wantResult  string
		wantErr     bool
		wantErrMsg  string
	}{
		{
			name:        "basic SSE with numeric ID",
			sseResponse: "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"protocolVersion\":\"2024-11-05\"}}\n\n",
			wantResult:  `{"protocolVersion":"2024-11-05"}`,
		},
		{
			name:        "SSE with string ID (string-ID variant)",
			sseResponse: "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":\"1\",\"result\":{\"name\":\"test\"}}\n\n",
			wantResult:  `{"name":"test"}`,
		},
		{
			name:        "SSE with data split across multiple events",
			sseResponse: "event: ping\ndata: {\"type\":\"ping\"}\n\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"status\":\"ok\"}}\n\n",
			wantResult:  `{"status":"ok"}`,
		},
		{
			name:        "SSE with error response",
			sseResponse: "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"error\":{\"code\":-32600,\"message\":\"Invalid Request\"}}\n\n",
			wantErr:     true,
			wantErrMsg:  "Invalid Request",
		},
		{
			name:        "SSE without trailing newline",
			sseResponse: "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"ok\":true}}",
			wantResult:  `{"ok":true}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(tt.sseResponse))
			})

			server := httptest.NewServer(handler)
			defer server.Close()

			transport := NewHTTPTransport(server.URL)
			ctx := context.Background()

			result, err := transport.Call(ctx, "initialize", map[string]interface{}{})

			if tt.wantErr {
				if err == nil {
					t.Fatal("Expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.wantErrMsg) {
					t.Errorf("Error = %q, want to contain %q", err.Error(), tt.wantErrMsg)
				}
				return
			}

			if err != nil {
				t.Fatalf("Call failed: %v", err)
			}

			// Normalize JSON for comparison
			var got, want interface{}
			json.Unmarshal(result, &got)
			json.Unmarshal([]byte(tt.wantResult), &want)

			gotBytes, _ := json.Marshal(got)
			wantBytes, _ := json.Marshal(want)

			if string(gotBytes) != string(wantBytes) {
				t.Errorf("Result = %s, want %s", string(result), tt.wantResult)
			}
		})
	}
}

// TestParseSSEResponse tests the SSE parser directly with various inputs
func TestParseSSEResponse(t *testing.T) {
	transport := &HTTPTransport{}

	tests := []struct {
		name       string
		input      string
		wantResult bool
		wantErr    bool
	}{
		{
			name:       "standard SSE format",
			input:      "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"test\":true}}\n\n",
			wantResult: true,
		},
		{
			// A numeric id echoed back as a string (the string-ID variant)
			// still matches the request id.
			name:       "data only (no event line)",
			input:      "data: {\"jsonrpc\":\"2.0\",\"id\":\"1\",\"result\":{}}\n\n",
			wantResult: true,
		},
		{
			name:       "multiple data lines concatenated",
			input:      "data: {\"jsonrpc\":\"2.0\",\ndata: \"id\":1,\"result\":{}}\n\n",
			wantResult: true,
		},
		{
			name:    "empty stream",
			input:   "",
			wantErr: true,
		},
		{
			name:    "no valid JSON-RPC",
			input:   "event: ping\ndata: hello\n\n",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := strings.NewReader(tt.input)
			result, err := transport.parseSSEResponse(reader, 1)

			if tt.wantErr {
				if err == nil {
					t.Error("Expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("parseSSEResponse failed: %v", err)
			}

			if tt.wantResult && len(result) == 0 {
				t.Error("Expected result, got empty")
			}
		})
	}
}

// TestRPCErrorUnmarshal tests the flexible error parsing for different error formats
func TestRPCErrorUnmarshal(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantCode    int
		wantMessage string
		wantContain string // partial match for error message
	}{
		{
			name:        "object format with code and message",
			input:       `{"code": 401, "message": "Unauthorized"}`,
			wantCode:    401,
			wantMessage: "Unauthorized",
		},
		{
			name:        "object format with negative code",
			input:       `{"code": -32601, "message": "Method not found"}`,
			wantCode:    -32601,
			wantMessage: "Method not found",
		},
		{
			name:        "string format",
			input:       `"Unauthorized"`,
			wantCode:    0,
			wantMessage: "Unauthorized",
		},
		{
			name:        "string format with spaces",
			input:       `"Invalid credentials provided"`,
			wantCode:    0,
			wantMessage: "Invalid credentials provided",
		},
		{
			name:        "empty object (fallback)",
			input:       `{}`,
			wantCode:    0,
			wantContain: "{}",
		},
		{
			name:        "object with only message",
			input:       `{"message": "Something went wrong"}`,
			wantCode:    0,
			wantMessage: "Something went wrong",
		},
		{
			name:        "object with only code",
			input:       `{"code": 500}`,
			wantCode:    500,
			wantMessage: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var rpcErr RPCError
			err := rpcErr.UnmarshalJSON([]byte(tt.input))
			if err != nil {
				t.Fatalf("UnmarshalJSON failed: %v", err)
			}

			if rpcErr.Code != tt.wantCode {
				t.Errorf("Code = %d, want %d", rpcErr.Code, tt.wantCode)
			}

			if tt.wantMessage != "" && rpcErr.Message != tt.wantMessage {
				t.Errorf("Message = %q, want %q", rpcErr.Message, tt.wantMessage)
			}

			if tt.wantContain != "" && !strings.Contains(rpcErr.Message, tt.wantContain) {
				t.Errorf("Message = %q, want to contain %q", rpcErr.Message, tt.wantContain)
			}

			// Verify Raw is preserved
			if rpcErr.Raw != tt.input {
				t.Errorf("Raw = %q, want %q", rpcErr.Raw, tt.input)
			}
		})
	}
}

// TestRPCErrorString tests the Error() method output format
func TestRPCErrorString(t *testing.T) {
	tests := []struct {
		name    string
		err     RPCError
		wantErr string
	}{
		{
			name:    "error with code",
			err:     RPCError{Code: 401, Message: "Unauthorized"},
			wantErr: "MCP error (401): Unauthorized",
		},
		{
			name:    "error without code",
			err:     RPCError{Code: 0, Message: "Unauthorized"},
			wantErr: "MCP error: Unauthorized",
		},
		{
			name:    "error with negative code",
			err:     RPCError{Code: -32601, Message: "Method not found"},
			wantErr: "MCP error (-32601): Method not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.err.Error()
			if got != tt.wantErr {
				t.Errorf("Error() = %q, want %q", got, tt.wantErr)
			}
		})
	}
}

// TestJSONRPCIDUnmarshal tests the JSONRPCID type handles both string and number IDs
func TestJSONRPCIDUnmarshal(t *testing.T) {
	tests := []struct {
		name          string
		input         string
		wantStringVal string
		wantIntVal    int
		wantIsString  bool
	}{
		{
			name:          "numeric ID",
			input:         `1`,
			wantStringVal: "1",
			wantIntVal:    1,
			wantIsString:  false,
		},
		{
			name:          "large numeric ID",
			input:         `12345`,
			wantStringVal: "12345",
			wantIntVal:    12345,
			wantIsString:  false,
		},
		{
			name:          "string numeric ID (string-ID variant)",
			input:         `"1"`,
			wantStringVal: "1",
			wantIntVal:    1,
			wantIsString:  true,
		},
		{
			name:          "string alpha ID",
			input:         `"abc123"`,
			wantStringVal: "abc123",
			wantIntVal:    0,
			wantIsString:  true,
		},
		{
			name:          "string UUID-like ID",
			input:         `"550e8400-e29b-41d4-a716-446655440000"`,
			wantStringVal: "550e8400-e29b-41d4-a716-446655440000",
			wantIntVal:    550, // Sscanf parses leading numeric portion
			wantIsString:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var id JSONRPCID
			err := id.UnmarshalJSON([]byte(tt.input))
			if err != nil {
				t.Fatalf("UnmarshalJSON failed: %v", err)
			}

			if id.StringValue != tt.wantStringVal {
				t.Errorf("StringValue = %q, want %q", id.StringValue, tt.wantStringVal)
			}

			if id.IntValue != tt.wantIntVal {
				t.Errorf("IntValue = %d, want %d", id.IntValue, tt.wantIntVal)
			}

			if id.IsString != tt.wantIsString {
				t.Errorf("IsString = %v, want %v", id.IsString, tt.wantIsString)
			}

			// Verify String() method
			if id.String() != tt.wantStringVal {
				t.Errorf("String() = %q, want %q", id.String(), tt.wantStringVal)
			}
		})
	}
}

// TestJSONRPCIDInResponse tests that responses with string IDs are correctly parsed
func TestJSONRPCIDInResponse(t *testing.T) {
	tests := []struct {
		name     string
		response string
		wantID   string
	}{
		{
			name:     "response with numeric ID",
			response: `{"jsonrpc":"2.0","id":1,"result":{}}`,
			wantID:   "1",
		},
		{
			name:     "response with string ID",
			response: `{"jsonrpc":"2.0","id":"1","result":{}}`,
			wantID:   "1",
		},
		{
			name:     "response with alpha string ID",
			response: `{"jsonrpc":"2.0","id":"request-abc","result":{}}`,
			wantID:   "request-abc",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var response struct {
				JSONRPC string          `json:"jsonrpc"`
				ID      JSONRPCID       `json:"id"`
				Result  json.RawMessage `json:"result"`
			}

			err := json.Unmarshal([]byte(tt.response), &response)
			if err != nil {
				t.Fatalf("Failed to unmarshal response: %v", err)
			}

			if response.ID.String() != tt.wantID {
				t.Errorf("ID = %q, want %q", response.ID.String(), tt.wantID)
			}
		})
	}
}

// TestStdioTransportEnvUTF8 verifies that stdio subprocess environment includes UTF-8 encoding variables
func TestStdioTransportEnvUTF8(t *testing.T) {
	// Create a transport with empty env to inspect cmd.Env
	transport, err := NewStdioTransport("echo", []string{"hello"}, nil)
	if err != nil {
		// echo will exit immediately, but we can still inspect the env that was set
		// Before start fails, cmd.Env is already populated
		t.Fatalf("Failed to create transport: %v", err)
	}
	defer transport.Close()

	env := transport.cmd.Env
	envMap := make(map[string]string)
	for _, e := range env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	// Verify UTF-8 encoding variables are present
	if val, ok := envMap["LANG"]; !ok || val != "C.UTF-8" {
		t.Errorf("Expected LANG=C.UTF-8, got %q (present=%v)", val, ok)
	}
	if val, ok := envMap["LC_ALL"]; !ok || val != "C.UTF-8" {
		t.Errorf("Expected LC_ALL=C.UTF-8, got %q (present=%v)", val, ok)
	}
	if val, ok := envMap["PYTHONIOENCODING"]; !ok || val != "utf-8" {
		t.Errorf("Expected PYTHONIOENCODING=utf-8, got %q (present=%v)", val, ok)
	}
}

// TestStdioTransportEnvUTF8WithCustomEnv verifies custom env vars don't remove UTF-8 settings
func TestStdioTransportEnvUTF8WithCustomEnv(t *testing.T) {
	customEnv := map[string]string{
		"PUBMATIC_API_KEY":      "test-key",
		"PUBMATIC_MCP_BASE_URL": "https://example.com",
	}

	transport, err := NewStdioTransport("echo", []string{"hello"}, customEnv)
	if err != nil {
		t.Fatalf("Failed to create transport: %v", err)
	}
	defer transport.Close()

	env := transport.cmd.Env
	envMap := make(map[string]string)
	for _, e := range env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	// Verify UTF-8 encoding variables are still present
	if val, ok := envMap["LANG"]; !ok || val != "C.UTF-8" {
		t.Errorf("Expected LANG=C.UTF-8, got %q (present=%v)", val, ok)
	}
	if val, ok := envMap["LC_ALL"]; !ok || val != "C.UTF-8" {
		t.Errorf("Expected LC_ALL=C.UTF-8, got %q (present=%v)", val, ok)
	}
	if val, ok := envMap["PYTHONIOENCODING"]; !ok || val != "utf-8" {
		t.Errorf("Expected PYTHONIOENCODING=utf-8, got %q (present=%v)", val, ok)
	}

	// Verify custom env vars are also present
	if val, ok := envMap["PUBMATIC_API_KEY"]; !ok || val != "test-key" {
		t.Errorf("Expected PUBMATIC_API_KEY=test-key, got %q (present=%v)", val, ok)
	}
	if val, ok := envMap["PUBMATIC_MCP_BASE_URL"]; !ok || val != "https://example.com" {
		t.Errorf("Expected PUBMATIC_MCP_BASE_URL=https://example.com, got %q (present=%v)", val, ok)
	}
}

// TestParseSSEResponseLargePayload verifies that SSE responses larger than the default
// 64KB bufio.Scanner limit are parsed correctly (regression test for token-too-long bug).
func TestParseSSEResponseLargePayload(t *testing.T) {
	transport := &HTTPTransport{}

	// Build a JSON-RPC response with a result field >64KB
	largeValue := strings.Repeat("x", 100*1024) // 100KB of data
	payload := `{"jsonrpc":"2.0","id":1,"result":{"data":"` + largeValue + `"}}`
	sseInput := "event: message\ndata: " + payload + "\n\n"

	result, err := transport.parseSSEResponse(strings.NewReader(sseInput), 1)
	if err != nil {
		t.Fatalf("parseSSEResponse failed on large payload: %v", err)
	}

	var parsed map[string]string
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("Failed to unmarshal result: %v", err)
	}

	if parsed["data"] != largeValue {
		t.Errorf("Expected %d bytes of data, got %d", len(largeValue), len(parsed["data"]))
	}
}

// TestHTTPParse_ResponseBoundedAtCap is the #1108 regression guard for the
// unbounded HTTP response read: both the JSON and the SSE parse paths must
// stop at httpResponseCaptureCap (the stdio ceiling) and fail that call with
// a clean over-cap error instead of buffering the body whole — before the
// fix parseJSONResponse decoded unbounded and parseSSEResponse capped only a
// single line, not total accumulation. The cap is lowered here so the test
// doesn't allocate 64 MiB.
func TestHTTPParse_ResponseBoundedAtCap(t *testing.T) {
	old := httpResponseCaptureCap
	httpResponseCaptureCap = 4 * 1024
	defer func() { httpResponseCaptureCap = old }()

	tr := &HTTPTransport{}

	t.Run("json over cap fails cleanly", func(t *testing.T) {
		big := `{"jsonrpc":"2.0","id":1,"result":{"data":"` + strings.Repeat("x", 8*1024) + `"}}`
		_, err := tr.parseJSONResponse(strings.NewReader(big), 1)
		if err == nil || !strings.Contains(err.Error(), "exceeded") {
			t.Fatalf("parseJSONResponse = %v, want an over-cap error", err)
		}
	})

	t.Run("json under cap parses", func(t *testing.T) {
		ok := `{"jsonrpc":"2.0","id":1,"result":{"data":"` + strings.Repeat("x", 1024) + `"}}`
		res, err := tr.parseJSONResponse(strings.NewReader(ok), 1)
		if err != nil {
			t.Fatalf("parseJSONResponse under cap: %v", err)
		}
		if !strings.Contains(string(res), "data") {
			t.Fatalf("unexpected result: %s", res)
		}
	})

	t.Run("sse over cap fails cleanly", func(t *testing.T) {
		// Each data line is well under the 10MB per-line scanner cap; the
		// TOTAL accumulation is what must trip the ceiling.
		var sb strings.Builder
		for sb.Len() < 8*1024 {
			sb.WriteString("data: " + strings.Repeat("y", 512) + "\n")
		}
		_, err := tr.parseSSEResponse(strings.NewReader(sb.String()), 1)
		if err == nil || !strings.Contains(err.Error(), "exceeded") {
			t.Fatalf("parseSSEResponse = %v, want an over-cap error", err)
		}
	})

	t.Run("sse under cap parses", func(t *testing.T) {
		in := "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"ok\":true}}\n\n"
		res, err := tr.parseSSEResponse(strings.NewReader(in), 1)
		if err != nil {
			t.Fatalf("parseSSEResponse under cap: %v", err)
		}
		if !strings.Contains(string(res), "ok") {
			t.Fatalf("unexpected result: %s", res)
		}
	})
}

// TestHTTPTransport_OversizedResponseIsPerCallError drives the cap end to end
// through HTTPTransport.Call — the same path remotemcp.probeServer and the
// per-run remote overlay use — and proves an oversized response fails only
// that call: the transport stays usable for the next, normal-sized one.
func TestHTTPTransport_OversizedResponseIsPerCallError(t *testing.T) {
	old := httpResponseCaptureCap
	httpResponseCaptureCap = 4 * 1024
	defer func() { httpResponseCaptureCap = old }()

	requests := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var req struct {
			ID int `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if requests == 1 {
			// First response: a single JSON body far past the cap, streamed
			// in chunks so the test server never builds a giant string.
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"data":"`, req.ID)
			chunk := strings.Repeat("z", 1024)
			for i := 0; i < 16; i++ {
				_, _ = io.WriteString(w, chunk)
			}
			_, _ = io.WriteString(w, `"}}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]any{"ok": true},
		})
	})
	server := httptest.NewServer(handler)
	defer server.Close()

	transport := NewHTTPTransport(server.URL)
	ctx := context.Background()

	if _, err := transport.Call(ctx, "tools/call", map[string]any{}); err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("oversized Call = %v, want an over-cap error", err)
	}
	res, err := transport.Call(ctx, "tools/call", map[string]any{})
	if err != nil {
		t.Fatalf("call after oversized response: %v — the cap must be a per-call error", err)
	}
	if !strings.Contains(string(res), "ok") {
		t.Fatalf("unexpected result after oversized response: %s", res)
	}
}

// TestHTTPTransport_RejectsMismatchedResponseID: the HTTP transport must
// verify the JSON-RPC response id against the request id the way stdio does
// (#1108) — a JSON response with a foreign id is rejected, and an SSE stream
// skips interleaved foreign-id responses until this call's answer arrives.
func TestHTTPTransport_RejectsMismatchedResponseID(t *testing.T) {
	t.Run("json wrong id rejected", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": 999999,
				"result": map[string]any{"stale": true},
			})
		})
		server := httptest.NewServer(handler)
		defer server.Close()

		transport := NewHTTPTransport(server.URL)
		_, err := transport.Call(context.Background(), "initialize", map[string]any{})
		if err == nil || !strings.Contains(err.Error(), "does not match request id") {
			t.Fatalf("Call = %v, want an id-mismatch error", err)
		}
	})

	t.Run("json id null with error keeps the server's diagnostic", func(t *testing.T) {
		// A server that cannot attribute the request answers id:null with
		// an error member — the call must still fail on the id check, but
		// the message has to carry the server's own complaint.
		handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"cannot attribute request"}}`))
		})
		server := httptest.NewServer(handler)
		defer server.Close()

		transport := NewHTTPTransport(server.URL)
		_, err := transport.Call(context.Background(), "initialize", map[string]any{})
		if err == nil || !strings.Contains(err.Error(), "does not match request id") {
			t.Fatalf("Call = %v, want an id-mismatch error", err)
		}
		if !strings.Contains(err.Error(), "cannot attribute request") {
			t.Fatalf("id-mismatch error hides the server's diagnostic: %v", err)
		}
	})

	t.Run("sse skips foreign-id response, returns matching one", func(t *testing.T) {
		tr := &HTTPTransport{}
		in := "data: {\"jsonrpc\":\"2.0\",\"id\":999999,\"result\":{\"stale\":true}}\n\n" +
			"data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"fresh\":true}}\n\n"
		res, err := tr.parseSSEResponse(strings.NewReader(in), 1)
		if err != nil {
			t.Fatalf("parseSSEResponse: %v", err)
		}
		if strings.Contains(string(res), "stale") || !strings.Contains(string(res), "fresh") {
			t.Fatalf("foreign-id SSE response misattributed to this call: %s", res)
		}
	})

	t.Run("sse with only a foreign-id response errors", func(t *testing.T) {
		tr := &HTTPTransport{}
		in := "data: {\"jsonrpc\":\"2.0\",\"id\":999999,\"error\":{\"code\":-32000,\"message\":\"someone else's failure\"}}\n\n"
		_, err := tr.parseSSEResponse(strings.NewReader(in), 1)
		if err == nil {
			t.Fatal("expected an error for a stream with no matching response")
		}
		var rpcErr *RPCError
		if errors.As(err, &rpcErr) {
			t.Fatalf("foreign-id RPC error attributed to this call: %v", err)
		}
	})
}

func TestIsTransportDeadError(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{fmt.Errorf("something random"), false},
		{fmt.Errorf("write |1: broken pipe"), true},
		{fmt.Errorf("failed to send request: EOF"), true},
		{fmt.Errorf("connection reset by peer"), true},
		{fmt.Errorf("os: process already finished"), true},
		{fmt.Errorf("file already closed"), true},
		{errors.New(errStdioTransportDead), true},
		// Wrapped stdlib identities — what the exec pipes actually produce —
		// must match via errors.Is even when the message alone wouldn't.
		{fmt.Errorf("read: %w", io.EOF), true},
		{fmt.Errorf("read: %w", io.ErrUnexpectedEOF), true},
		{fmt.Errorf("write: %w", syscall.EPIPE), true},
		{fmt.Errorf("write: %w", os.ErrClosed), true},
		// Full-word EOF forms from flattened chains still match.
		{fmt.Errorf("unexpected EOF"), true},
		// The old bare "eof" substring match misclassified these as a dead
		// transport and restarted a healthy subprocess (#1108): "EOF" glued
		// into a larger word, or the letters buried inside another word.
		{fmt.Errorf("EOFError: bad tool output"), false},
		{fmt.Errorf("whereof the parser complained"), false},
		{fmt.Errorf("stream ended: eof"), false}, // stdlib renders EOF uppercase; lowercase is app text
	}
	for _, tt := range tests {
		if got := isTransportDeadError(tt.err); got != tt.want {
			t.Errorf("isTransportDeadError(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

func TestIsRequestNotDeliveredError(t *testing.T) {
	tests := []struct {
		err  error
		want bool
	}{
		{nil, false},
		// Read-side death: request may have executed — NOT safe to retry.
		{fmt.Errorf("failed to read response: EOF"), false},
		{fmt.Errorf("connection reset by peer"), false},
		// Write-side death / poisoned transport: request never delivered — safe.
		{fmt.Errorf("%s: write |1: broken pipe", errStdioWriteFailed), true},
		{errors.New(errStdioTransportDead), true},
	}
	for _, tt := range tests {
		if got := isRequestNotDeliveredError(tt.err); got != tt.want {
			t.Errorf("isRequestNotDeliveredError(%v) = %v, want %v", tt.err, got, tt.want)
		}
	}
}

// TestStdioTransportSkipsNotificationsAndStaleResponses verifies the
// response-id matching in StdioTransport.Call: server notifications and
// responses to other request ids must be skipped, not returned as the
// answer to the current call.
func TestStdioTransportSkipsNotificationsAndStaleResponses(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found, skipping stdio test")
	}
	_, filename, _, _ := runtime.Caller(0)
	dir := filepath.Dir(filename)
	scriptPath := filepath.Join(dir, "testdata", "noisy_server.py")

	transport, err := NewStdioTransport("python3", []string{scriptPath}, nil)
	if err != nil {
		t.Fatalf("failed to start noisy server: %v", err)
	}
	defer transport.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := transport.Call(ctx, "echo", map[string]interface{}{"value": 42})
	if err != nil {
		t.Fatalf("call failed: %v", err)
	}
	var parsed struct {
		Echoed int `json:"echoed"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("unmarshal result %q: %v", string(result), err)
	}
	if parsed.Echoed != 42 {
		t.Fatalf("expected echoed=42, got %d (wrong response line attributed to call)", parsed.Echoed)
	}
}

// TestStdioTransportDeadAfterCancel verifies a cancelled call poisons the
// transport: the next call must fail fast with a transport-dead error (so
// Server.callTool restarts the process) instead of racing the abandoned
// reader goroutine on the shared bufio.Reader.
func TestStdioTransportDeadAfterCancel(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found, skipping stdio test")
	}
	_, filename, _, _ := runtime.Caller(0)
	dir := filepath.Dir(filename)
	scriptPath := filepath.Join(dir, "testdata", "slow_server.py")

	transport, err := NewStdioTransport("python3", []string{scriptPath}, nil)
	if err != nil {
		t.Fatalf("failed to start slow server: %v", err)
	}
	defer transport.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := transport.Call(ctx, "slow", nil); err == nil {
		t.Fatal("expected cancelled call to fail")
	}

	_, err = transport.Call(context.Background(), "slow", nil)
	if err == nil {
		t.Fatal("expected post-cancel call to fail fast")
	}
	if !isTransportDeadError(err) {
		t.Fatalf("post-cancel error must be transport-dead so the restart path fires, got: %v", err)
	}
	if !isRequestNotDeliveredError(err) {
		t.Fatalf("post-cancel error must be retry-safe (request never delivered), got: %v", err)
	}
}

// TestHTTPTransportErrorFormats tests that the HTTP transport handles different error formats
func TestHTTPTransportErrorFormats(t *testing.T) {
	tests := []struct {
		name           string
		errorResponse  interface{}
		wantErrContain string
	}{
		{
			name: "object error format",
			errorResponse: map[string]interface{}{
				"code":    401,
				"message": "Unauthorized",
			},
			wantErrContain: "(401): Unauthorized",
		},
		{
			name:           "string error format (string-ID variant)",
			errorResponse:  "Unauthorized",
			wantErrContain: "Unauthorized",
		},
		{
			name:           "null error (no error)",
			errorResponse:  nil,
			wantErrContain: "", // no error expected
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					ID     int    `json:"id"`
					Method string `json:"method"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}

				resp := map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req.ID,
				}

				if tt.errorResponse != nil {
					resp["error"] = tt.errorResponse
				} else {
					// Return success for null error test
					resp["result"] = map[string]interface{}{
						"protocolVersion": "2024-11-05",
					}
				}

				json.NewEncoder(w).Encode(resp)
			})

			server := httptest.NewServer(handler)
			defer server.Close()

			transport := NewHTTPTransport(server.URL)
			ctx := context.Background()

			result, err := transport.Call(ctx, "initialize", map[string]interface{}{})

			if tt.wantErrContain == "" {
				// Expect success
				if err != nil {
					t.Fatalf("Expected no error, got: %v", err)
				}
				if result == nil {
					t.Error("Expected result, got nil")
				}
			} else {
				// Expect error
				if err == nil {
					t.Fatal("Expected error, got nil")
				}
				if !strings.Contains(err.Error(), tt.wantErrContain) {
					t.Errorf("Error = %q, want to contain %q", err.Error(), tt.wantErrContain)
				}
			}
		})
	}
}
