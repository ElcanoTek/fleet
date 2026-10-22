package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// stopServerScript is a minimal stdio MCP server whose one tool works through
// "items": it reports progress after the first, then reads its stdin for a
// notifications/cancelled naming its own request id, and answers NORMALLY
// with what it saw, as a cooperative server does. A call without the
// cooperative-cancel mark answers at once.
const stopServerScript = `
import json, sys
def send(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()
for line in sys.stdin:
    req = json.loads(line)
    rid, method = req.get("id"), req.get("method")
    if method == "initialize":
        send({"jsonrpc": "2.0", "id": rid, "result": {"protocolVersion": "2024-11-05", "capabilities": {}, "serverInfo": {"name": "s", "version": "1"}}})
    elif method == "tools/list":
        send({"jsonrpc": "2.0", "id": rid, "result": {"tools": [{"name": "run_items", "inputSchema": {"type": "object"}}]}})
    elif method == "tools/call":
        meta = req["params"].get("_meta") or {}
        out = {"cooperative": meta.get("fleet/cooperative_cancel") is True}
        if out["cooperative"]:
            send({"jsonrpc": "2.0", "method": "notifications/progress", "params": {"progressToken": meta.get("progressToken"), "progress": 1, "total": 3}})
            while True:
                note = json.loads(sys.stdin.readline())
                if note.get("method") == "notifications/cancelled":
                    p = note["params"]
                    out["stopped"] = p.get("requestId") == rid
                    out["reason"] = p.get("reason")
                    break
        send({"jsonrpc": "2.0", "id": rid, "result": {"content": [{"type": "text", "text": json.dumps(out)}]}})
`

func stopServer(t *testing.T) *Client {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found")
	}
	script := filepath.Join(t.TempDir(), "stop_server.py")
	if err := os.WriteFile(script, []byte(stopServerScript), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewClient()
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.AddStdioServer(ctx, "s", "python3", []string{"-u", script}, nil, ""); err != nil {
		t.Fatalf("AddStdioServer: %v", err)
	}
	return c
}

// A stoppable call is marked cooperative; when its stop signal fires the
// transport sends notifications/cancelled for the call's own request id with
// the stop reason, and keeps reading until the server's normal answer. The
// transport is still usable afterwards.
func TestCallTool_StopSendsCancelledAndWaitsForTheAnswer(t *testing.T) {
	c := stopServer(t)
	stop := make(chan struct{})
	started := make(chan struct{}, 1)
	ctx := WithStop(context.Background(), stop)
	ctx = WithProgress(ctx, func(ProgressUpdate) {
		select {
		case started <- struct{}{}:
		default:
		}
	})
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	type answer struct {
		res *ToolResult
		err error
	}
	done := make(chan answer, 1)
	go func() {
		res, err := c.CallToolOn(ctx, "s", "run_items", map[string]any{})
		done <- answer{res, err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the call never started")
	}
	close(stop)
	got := <-done
	if got.err != nil {
		t.Fatalf("CallToolOn: %v", got.err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(got.res.Content[0].Text), &out); err != nil {
		t.Fatalf("answer %q: %v", got.res.Content[0].Text, err)
	}
	if out["cooperative"] != true || out["stopped"] != true || out["reason"] != StopReasonByUser {
		t.Fatalf("server saw %v, want a cooperative call stopped by its own request id with reason %q", out, StopReasonByUser)
	}

	// A plain call afterwards: no cooperative mark, answered at once on the
	// same transport (nothing was left half-read).
	res, err := c.CallToolOn(context.Background(), "s", "run_items", map[string]any{})
	if err != nil || !strings.Contains(res.Content[0].Text, `"cooperative": false`) {
		t.Fatalf("plain call after a stop = %v, %v", res, err)
	}
}

// Over streamable HTTP the stop is its own POST on the session while the
// call's SSE response is still open; the call then gets its normal answer.
func TestHTTPTransport_StopPostsCancelledDuringTheCall(t *testing.T) {
	shortStopResend(t)
	var mu sync.Mutex
	var cancelled map[string]any
	gotCancel := make(chan struct{})
	var cancelOnce sync.Once
	callArrived := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var msg map[string]any
		_ = json.Unmarshal(body, &msg)
		switch msg["method"] {
		case "notifications/cancelled":
			// A cancellation for a call the server has not registered yet is
			// ignored, as the spec says; the client sends it again.
			select {
			case <-callArrived:
			default:
				w.WriteHeader(http.StatusAccepted)
				return
			}
			if r.Header.Get("Mcp-Session-Id") != "sess-1" {
				t.Errorf("stop sent without the session id")
			}
			mu.Lock()
			cancelled, _ = msg["params"].(map[string]any)
			mu.Unlock()
			cancelOnce.Do(func() { close(gotCancel) })
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			close(callArrived)
			params, _ := msg["params"].(map[string]any)
			meta, _ := params["_meta"].(map[string]any)
			if meta[CooperativeCancelMetaKey] != true {
				t.Errorf("_meta = %v, want the cooperative mark", meta)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			select {
			case <-gotCancel:
			case <-time.After(5 * time.Second):
			}
			resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg["id"], "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "stopped after item 1"}}}})
			_, _ = w.Write([]byte("data: " + string(resp) + "\n\n"))
		}
	}))
	defer srv.Close()
	tr := NewHTTPTransport(srv.URL)
	tr.sessionID = "sess-1"
	// Stop is pressed as soon as the server has the call (a stop pressed
	// before the call is sent sends nothing; see
	// TestTransports_DoNotSendACallStoppedBeforeSend).
	stop := make(chan struct{})
	go func() {
		<-callArrived
		close(stop)
	}()
	ctx, cancel := context.WithTimeout(WithStop(context.Background(), stop), 10*time.Second)
	defer cancel()
	raw, err := tr.Call(ctx, "tools/call", map[string]any{"name": "run_items", "arguments": map[string]any{}, "_meta": map[string]any{CooperativeCancelMetaKey: true}})
	if err != nil || !strings.Contains(string(raw), "stopped after item 1") {
		t.Fatalf("Call = %s, %v", raw, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if cancelled["reason"] != StopReasonByUser || cancelled["requestId"] == nil {
		t.Fatalf("cancelled params = %v", cancelled)
	}
}

// A server that answers tools/call with one JSON body only when the work is
// done (no early headers) still gets the stop while it works: the
// cancellation is sent once the call's request is written, not once its
// response starts.
func TestHTTPTransport_StopReachesAPlainJSONServerMidCall(t *testing.T) {
	shortStopResend(t)
	gotCancel := make(chan struct{})
	var cancelOnce sync.Once
	callArrived := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var msg map[string]any
		_ = json.Unmarshal(body, &msg)
		switch msg["method"] {
		case "notifications/cancelled":
			// A cancellation for a call the server has not registered yet is
			// ignored, as the spec says; the client sends it again.
			select {
			case <-callArrived:
			default:
				w.WriteHeader(http.StatusAccepted)
				return
			}
			cancelOnce.Do(func() { close(gotCancel) })
			w.WriteHeader(http.StatusAccepted)
		case "tools/call":
			close(callArrived)
			text := "ran every item"
			select {
			case <-gotCancel:
				text = "stopped after item 1"
			case <-time.After(5 * time.Second):
			}
			w.Header().Set("Content-Type", "application/json")
			resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg["id"], "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}})
			_, _ = w.Write(resp)
		}
	}))
	defer srv.Close()
	tr := NewHTTPTransport(srv.URL)
	stop := make(chan struct{})
	ctx, cancel := context.WithTimeout(WithStop(context.Background(), stop), 10*time.Second)
	defer cancel()
	go func() {
		<-callArrived
		close(stop)
	}()
	raw, err := tr.Call(ctx, "tools/call", map[string]any{"name": "run_items", "arguments": map[string]any{}})
	if err != nil || !strings.Contains(string(raw), "stopped after item 1") {
		t.Fatalf("Call = %s, %v; want the server to have seen the stop mid-call", raw, err)
	}
}

func shortStopResend(t *testing.T) {
	t.Helper()
	first, maxInterval := stopResendFirst, stopResendMaxInterval
	stopResendFirst, stopResendMaxInterval = 20*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { stopResendFirst, stopResendMaxInterval = first, maxInterval })
}

// A stoppable call whose stop already fired is never written to the server,
// over stdio or HTTP: the transport checks at its send boundary.
func TestTransports_DoNotSendACallStoppedBeforeSend(t *testing.T) {
	c := stopServer(t)
	stop := make(chan struct{})
	close(stop)
	if _, err := c.CallToolOn(WithStop(context.Background(), stop), "s", "run_items", map[string]any{}); !errors.Is(err, ErrStoppedBeforeSend) {
		t.Fatalf("stdio: err = %v, want ErrStoppedBeforeSend", err)
	}
	// The transport is untouched: a plain call still works.
	if _, err := c.CallToolOn(context.Background(), "s", "run_items", map[string]any{}); err != nil {
		t.Fatalf("plain call after a refused stopped call: %v", err)
	}

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	if _, err := NewHTTPTransport(srv.URL).Call(WithStop(context.Background(), stop), "tools/call", map[string]any{}); !errors.Is(err, ErrStoppedBeforeSend) || hits != 0 {
		t.Fatalf("http: err = %v, requests = %d; want ErrStoppedBeforeSend and none sent", err, hits)
	}
}
