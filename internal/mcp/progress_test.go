package mcp

import (
	"context"
	"encoding/json"
	"fmt"
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

// progressServerScript is a minimal stdio MCP server whose one tool reports
// progress the way the spec says: notifications/progress carrying the
// request's _meta.progressToken. Around the two real updates it also sends a
// notification for another token, a log notification and a malformed update,
// which a caller must never see. A call without a token gets no
// notifications, and the tool echoes whether it saw a token.
const progressServerScript = `
import json, sys
def send(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()
for line in sys.stdin:
    req = json.loads(line)
    rid, method = req.get("id"), req.get("method")
    if method == "initialize":
        send({"jsonrpc": "2.0", "id": rid, "result": {"protocolVersion": "2024-11-05", "capabilities": {}, "serverInfo": {"name": "p", "version": "1"}}})
    elif method == "tools/list":
        send({"jsonrpc": "2.0", "id": rid, "result": {"tools": [{"name": "work", "inputSchema": {"type": "object"}}]}})
    elif method == "tools/call":
        token = (req["params"].get("_meta") or {}).get("progressToken")
        if token is not None:
            send({"jsonrpc": "2.0", "method": "notifications/progress", "params": {"progressToken": "someone-else", "progress": 9, "total": 9}})
            send({"jsonrpc": "2.0", "method": "notifications/message", "params": {"level": "info", "data": "hi"}})
            send({"jsonrpc": "2.0", "method": "notifications/progress", "params": {"progressToken": token, "progress": 1, "total": 2, "message": "deal 1"}})
            send({"jsonrpc": "2.0", "method": "notifications/progress", "params": {"progressToken": token, "progress": -1}})
            send({"jsonrpc": "2.0", "method": "notifications/progress", "params": {"progressToken": token, "progress": 2, "total": 2}})
        send({"jsonrpc": "2.0", "id": rid, "result": {"content": [{"type": "text", "text": "token=" + json.dumps(token is not None)}]}})
`

func progressServer(t *testing.T) *Client {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found")
	}
	script := filepath.Join(t.TempDir(), "progress_server.py")
	if err := os.WriteFile(script, []byte(progressServerScript), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewClient()
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.AddStdioServer(ctx, "p", "python3", []string{"-u", script}, nil, ""); err != nil {
		t.Fatalf("AddStdioServer: %v", err)
	}
	return c
}

type progressRecorder struct {
	mu  sync.Mutex
	got []ProgressUpdate
}

func (r *progressRecorder) sink(u ProgressUpdate) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, u)
}

func (r *progressRecorder) updates() []ProgressUpdate {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ProgressUpdate(nil), r.got...)
}

// A call with a sink sends _meta.progressToken and receives exactly its own
// valid updates, in order; the result is the same as without.
func TestCallTool_DeliversItsProgressNotifications(t *testing.T) {
	c := progressServer(t)
	rec := &progressRecorder{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := c.CallToolOn(WithProgress(ctx, rec.sink), "p", "work", nil)
	if err != nil {
		t.Fatalf("CallToolOn: %v", err)
	}
	if len(res.Content) != 1 || res.Content[0].Text != "token=true" {
		t.Fatalf("result = %+v, want the server to have seen a progress token", res.Content)
	}
	want := []ProgressUpdate{{Progress: 1, Total: 2, Message: "deal 1"}, {Progress: 2, Total: 2}}
	if got := rec.updates(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("updates = %+v, want %+v (other tokens, other notifications and negative values ignored)", got, want)
	}
}

// Without a sink the request carries no _meta: the server sees exactly the
// call it always saw.
func TestCallTool_NoSinkSendsNoProgressToken(t *testing.T) {
	c := progressServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := c.CallToolOn(ctx, "p", "work", nil)
	if err != nil {
		t.Fatalf("CallToolOn: %v", err)
	}
	if res.Content[0].Text != "token=false" {
		t.Fatalf("result = %+v, want no progress token without a sink", res.Content)
	}
}

// The HTTP transport delivers progress events from a call's SSE stream too.
func TestHTTPTransport_DeliversSSEProgress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int `json:"id"`
			Params struct {
				Meta struct {
					ProgressToken string `json:"progressToken"`
				} `json:"_meta"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "text/event-stream")
		token, _ := json.Marshal(req.Params.Meta.ProgressToken)
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progressToken\":%s,\"progress\":3,\"total\":4,\"message\":\"almost\"}}\n\n", token)
		fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":{\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}}\n\n", req.ID)
	}))
	defer srv.Close()

	tr := NewHTTPTransportWithHeaders(srv.URL, nil)
	rec := &progressRecorder{}
	ctx, meta, ok := withProgressToken(WithProgress(context.Background(), rec.sink))
	if !ok {
		t.Fatal("a sink must mint a token")
	}
	if _, err := tr.Call(ctx, "tools/call", map[string]any{"name": "work", "arguments": map[string]any{}, "_meta": meta}); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if got := rec.updates(); len(got) != 1 || got[0] != (ProgressUpdate{Progress: 3, Total: 4, Message: "almost"}) {
		t.Fatalf("updates = %+v", got)
	}
}

// A long message is cut, so no hop carries an unbounded string.
func TestDeliverProgress_BoundsTheMessage(t *testing.T) {
	rec := &progressRecorder{}
	ctx, _, _ := withProgressToken(WithProgress(context.Background(), rec.sink))
	token := ctx.Value(progressTokenKey{}).(string)
	params, _ := json.Marshal(map[string]any{"progressToken": token, "progress": 1, "message": strings.Repeat("é", 2*MaxProgressMessageRunes)})
	deliverProgress(ctx, "notifications/progress", params)
	got := rec.updates()
	if len(got) != 1 || len([]rune(got[0].Message)) != MaxProgressMessageRunes+1 {
		t.Fatalf("updates = %d, message runes = %d", len(got), len([]rune(got[0].Message)))
	}
}

// ThrottleProgress forwards the first update at once, holds the ones inside
// the interval and delivers only the latest of them when it ends, and drops a
// held update on stop.
func TestThrottleProgress(t *testing.T) {
	rec := &progressRecorder{}
	push, stop := ThrottleProgress(40*time.Millisecond, rec.sink)
	push(ProgressUpdate{Progress: 1})
	push(ProgressUpdate{Progress: 2})
	push(ProgressUpdate{Progress: 3})
	if got := rec.updates(); len(got) != 1 || got[0].Progress != 1 {
		t.Fatalf("immediately = %+v, want only the first", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(rec.updates()) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := rec.updates(); len(got) != 2 || got[1].Progress != 3 {
		t.Fatalf("after the interval = %+v, want the latest held update (3) delivered once", got)
	}

	push(ProgressUpdate{Progress: 4})
	stop()
	time.Sleep(80 * time.Millisecond)
	push(ProgressUpdate{Progress: 5})
	if got := rec.updates(); len(got) != 2 {
		t.Fatalf("after stop = %+v, want nothing more", got)
	}
}
