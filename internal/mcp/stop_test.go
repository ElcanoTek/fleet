package mcp

import (
	"context"
	"encoding/json"
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
	var mu sync.Mutex
	var cancelled map[string]any
	gotCancel := make(chan struct{})
	callArrived := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var msg map[string]any
		_ = json.Unmarshal(body, &msg)
		switch msg["method"] {
		case "notifications/cancelled":
			select {
			case <-callArrived:
			default:
				t.Errorf("the stop reached the server before the call it cancels")
			}
			if r.Header.Get("Mcp-Session-Id") != "sess-1" {
				t.Errorf("stop sent without the session id")
			}
			mu.Lock()
			cancelled, _ = msg["params"].(map[string]any)
			mu.Unlock()
			close(gotCancel)
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
	// The stop is already pressed when the call starts: the cancellation
	// must still follow the call, never precede it.
	stop := make(chan struct{})
	close(stop)
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
