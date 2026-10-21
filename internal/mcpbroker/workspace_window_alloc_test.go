package mcpbroker

import (
	"context"
	"encoding/base64"
	"io"
	"runtime"
	"testing"

	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// stdioEncodingBroker is a broker backend that does exactly what the child's
// mcp.StdioTransport does with a tools/call: build the JSON-RPC request and
// encode it as one line (mcp.EncodeJSONLine), then hand it to the transport.
type stdioEncodingBroker struct {
	*fakeBroker
}

func (b stdioEncodingBroker) CallMCP(_ context.Context, _, tool string, args map[string]any) (string, bool, error) {
	line, err := mcp.EncodeJSONLine(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		return "", false, err
	}
	_, _ = io.Discard.Write(line)
	return `{"accepted":true}`, false, nil
}

// TestWorkspaceWindowDispatchAllocsWithinCharge is the calibration behind
// tools.WorkspaceWindowCharge (ADR-0087). It measures every byte allocated
// to dispatch one workspace-file window through the out-of-process broker
// (parent frame encode, child frame decode, child JSON-RPC line encode),
// starting from the base64 argument the parent builds. Both ends run in this
// process, so TotalAlloc sees all of it, growth intermediates included —
// which bounds what can be live at once from above. The charge must not be
// lower.
func TestWorkspaceWindowDispatchAllocsWithinCharge(t *testing.T) {
	client := loopback(t, stdioEncodingBroker{&fakeBroker{}})
	if _, _, err := client.CallMCP(context.Background(), "pages", "warmup", map[string]any{"x": "y"}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{1 << 20, 8 << 20} {
		window := make([]byte, n)
		for i := range window {
			window[i] = byte(i * 7)
		}
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		args := map[string]any{"chunk": base64.StdEncoding.EncodeToString(window), "sequence": 1}
		if _, _, err := client.CallMCP(context.Background(), "pages", "upload_chunk", args); err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&after)
		allocated := int64(after.TotalAlloc - before.TotalAlloc)
		charge := tools.WorkspaceWindowCharge(int64(n))
		t.Logf("window %d bytes: dispatch allocated %d bytes (%.1fx base64), charged %d", n, allocated,
			float64(allocated)/float64(base64.StdEncoding.EncodedLen(n)), charge)
		if allocated > charge {
			t.Fatalf("window of %d bytes allocated %d bytes to dispatch, more than its %d-byte charge", n, allocated, charge)
		}
	}
	// The server replaced its decoder after those large frames; the
	// connection keeps working, concurrent frames included.
	errs := make(chan error, 8)
	for i := range 8 {
		go func() {
			_, _, err := client.CallMCP(context.Background(), "pages", "after", map[string]any{"i": i})
			errs <- err
		}()
	}
	for range 8 {
		if err := <-errs; err != nil {
			t.Fatalf("call after decoder reset: %v", err)
		}
	}
}
