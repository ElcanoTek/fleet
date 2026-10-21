package mcpbroker

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"runtime"
	"testing"
	"time"

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
// tools.WorkspaceWindowCharge (ADR-0088). It measures every byte allocated
// to dispatch one workspace-file window through the out-of-process broker
// (parent frame encode, child frame decode, child JSON-RPC line encode),
// starting from the base64 argument the parent builds. Both ends run in this
// process, so TotalAlloc sees all of it, growth intermediates included —
// which bounds what can be live at once from above. The charge must not be
// lower.
func TestWorkspaceWindowDispatchAllocsWithinCharge(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector's instrumentation inflates allocations; the calibration runs in the plain test lane")
	}
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

// heldBroker blocks every call until unblock is closed, ignoring ctx — a
// call queued in the child behind another one, still holding its args.
type heldBroker struct {
	*fakeBroker
	entered chan struct{}
	unblock chan struct{}
}

func (b heldBroker) CallMCP(context.Context, string, string, map[string]any) (string, bool, error) {
	b.entered <- struct{}{}
	<-b.unblock
	return "late", false, nil
}

// TestCancelledCallRetainsArgumentsUntilChildAnswers: cancelling a call
// returns at once, but the caller's argument hold is not released until the
// broker child has answered — until then the child still holds the args.
func TestCancelledCallRetainsArgumentsUntilChildAnswers(t *testing.T) {
	backend := heldBroker{&fakeBroker{}, make(chan struct{}, 1), make(chan struct{})}
	client := loopback(t, backend)
	released := make(chan struct{})
	ctx, hold := mcp.WithArgumentHold(context.Background(), func() { close(released) })
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, _, err := client.CallMCP(ctx, "pages", "upload_chunk", map[string]any{"chunk": "AAAA"})
		done <- err
	}()
	<-backend.entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("CallMCP = %v, want canceled", err)
	}
	hold.Done()
	select {
	case <-released:
		t.Fatal("arguments released while the child still holds them")
	case <-time.After(100 * time.Millisecond):
	}
	close(backend.unblock)
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("arguments not released after the child answered")
	}
}
