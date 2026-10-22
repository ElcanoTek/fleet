package mcp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// metaEchoServerScript is a minimal stdio MCP server whose one tool answers
// with the `_meta` its tools/call carried, as JSON text.
const metaEchoServerScript = `
import json, sys
def send(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()
for line in sys.stdin:
    req = json.loads(line)
    rid, method = req.get("id"), req.get("method")
    if method == "initialize":
        send({"jsonrpc": "2.0", "id": rid, "result": {"protocolVersion": "2024-11-05", "capabilities": {}, "serverInfo": {"name": "m", "version": "1"}}})
    elif method == "tools/list":
        send({"jsonrpc": "2.0", "id": rid, "result": {"tools": [{"name": "echo_meta", "inputSchema": {"type": "object"}}]}})
    elif method == "tools/call":
        meta = req["params"].get("_meta")
        send({"jsonrpc": "2.0", "id": rid, "result": {"content": [{"type": "text", "text": json.dumps(meta)}]}})
`

func metaEchoServer(t *testing.T) *Client {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found")
	}
	script := filepath.Join(t.TempDir(), "meta_server.py")
	if err := os.WriteFile(script, []byte(metaEchoServerScript), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewClient()
	t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.AddStdioServer(ctx, "m", "python3", []string{"-u", script}, nil, ""); err != nil {
		t.Fatalf("AddStdioServer: %v", err)
	}
	return c
}

func callEchoMeta(ctx context.Context, t *testing.T, c *Client) map[string]any {
	t.Helper()
	res, err := c.CallToolOn(ctx, "m", "echo_meta", map[string]any{})
	if err != nil {
		t.Fatalf("CallToolOn: %v", err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("content = %+v", res.Content)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].Text), &meta); err != nil {
		t.Fatalf("echo %q: %v", res.Content[0].Text, err)
	}
	return meta
}

// A call made under WithRequestMeta carries the fleet entries in its
// `_meta`, beside the progress token when the caller also listens for
// progress; a call without them sends no `_meta` at all.
func TestCallTool_SendsFleetRequestMeta(t *testing.T) {
	c := metaEchoServer(t)
	if meta := callEchoMeta(context.Background(), t, c); meta != nil {
		t.Fatalf("a plain call sent _meta %v, want none", meta)
	}

	narrowed := map[string]any{"arg": "deal_ids", "staged": []any{"a", "b", "c"}}
	ctx := WithRequestMeta(context.Background(), map[string]any{
		"fleet/narrowed": narrowed,
		"progressToken":  "forged", // outside the fleet/ namespace: dropped
	})
	meta := callEchoMeta(ctx, t, c)
	if !reflect.DeepEqual(meta, map[string]any{"fleet/narrowed": narrowed}) {
		t.Fatalf("_meta = %v, want only fleet/narrowed", meta)
	}

	ctx = WithProgress(ctx, func(ProgressUpdate) {})
	meta = callEchoMeta(ctx, t, c)
	if _, ok := meta["progressToken"].(string); !ok || meta["progressToken"] == "forged" {
		t.Fatalf("progress token = %v, want the minted one", meta["progressToken"])
	}
	if !reflect.DeepEqual(meta["fleet/narrowed"], narrowed) {
		t.Fatalf("fleet/narrowed = %v beside the progress token, want %v", meta["fleet/narrowed"], narrowed)
	}
}

func TestFleetRequestMeta(t *testing.T) {
	if got := FleetRequestMeta(map[string]any{"progressToken": 1, "fleet/": 2, "other/x": 3}); got != nil {
		t.Fatalf("got %v, want nil: no fleet/<name> key", got)
	}
	got := FleetRequestMeta(map[string]any{"fleet/a": 1, "x": 2})
	if !reflect.DeepEqual(got, map[string]any{"fleet/a": 1}) {
		t.Fatalf("got %v", got)
	}
	if ctx := WithRequestMeta(context.Background(), map[string]any{"x": 1}); RequestMeta(ctx) != nil {
		t.Fatal("a map with no fleet key must attach nothing")
	}
	ctx := WithRequestMeta(WithRequestMeta(context.Background(), map[string]any{"fleet/a": 1, "fleet/b": 1}), map[string]any{"fleet/b": 2})
	if !reflect.DeepEqual(RequestMeta(ctx), map[string]any{"fleet/a": 1, "fleet/b": 2}) {
		t.Fatalf("layered meta = %v, want the inner entry kept and the outer one winning", RequestMeta(ctx))
	}
}
