package mcpbroker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// stopBackend answers once its call's stop signal fires, the way a
// cooperating MCP server answers after its current item; without a signal it
// answers at once. entered tells the test the call is running.
type stopBackend struct {
	*fakeBroker
	entered  chan struct{}
	sawStop  bool
	ctxEnded bool
}

func (b *stopBackend) CallMCP(ctx context.Context, _, _ string, _ map[string]any) (string, bool, error) {
	stop := mcp.StopSignal(ctx)
	if stop == nil {
		return "no stop asked", false, nil
	}
	close(b.entered)
	select {
	case <-stop:
		b.sawStop = true
		b.ctxEnded = ctx.Err() != nil
		return "stopped after item 1", false, nil
	case <-time.After(5 * time.Second):
		return "", false, context.DeadlineExceeded
	}
}

// A stoppable call's stop crosses the real protocol as a methodStop frame:
// the child's call sees its stop signal close, its context is NOT cancelled,
// and the parent gets the call's normal answer.
func TestClientServer_ForwardsAStop(t *testing.T) {
	backend := &stopBackend{fakeBroker: &fakeBroker{}, entered: make(chan struct{})}
	client := loopback(t, backend)

	stop := make(chan struct{})
	ctx := mcp.WithStop(context.Background(), stop)
	type answer struct {
		text string
		err  error
	}
	done := make(chan answer, 1)
	go func() {
		text, _, err := client.CallMCP(ctx, "deals", "execute_plan", nil)
		done <- answer{text, err}
	}()
	select {
	case <-backend.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the call never reached the child")
	}
	close(stop)
	got := <-done
	if got.err != nil || got.text != "stopped after item 1" {
		t.Fatalf("CallMCP = %q, %v", got.text, got.err)
	}
	if !backend.sawStop || backend.ctxEnded {
		t.Fatalf("child saw stop=%v, ctx ended=%v; want the signal, not a cancelled context", backend.sawStop, backend.ctxEnded)
	}
	// Without a signal nothing is stoppable.
	if text, _, err := client.CallMCP(context.Background(), "deals", "execute_plan", nil); err != nil || text != "no stop asked" {
		t.Fatalf("plain call = %q, %v", text, err)
	}
}

// A stop frame for a call the child does not know (finished, or never
// stoppable) is ignored.
func TestServer_IgnoresAStopForAnUnknownCall(t *testing.T) {
	backend := &stopBackend{fakeBroker: &fakeBroker{}, entered: make(chan struct{})}
	client := loopback(t, backend)
	if err := client.send(request{ID: 999, Method: methodStop}); err != nil {
		t.Fatal(err)
	}
	if text, _, err := client.CallMCP(context.Background(), "deals", "execute_plan", nil); err != nil || text != "no stop asked" {
		t.Fatalf("call after a stray stop = %q, %v", text, err)
	}
}

// A stoppable call stopped before its frame is written never reaches the
// child.
func TestClient_DoesNotSendACallStoppedBeforeSend(t *testing.T) {
	backend := &stopBackend{fakeBroker: &fakeBroker{}, entered: make(chan struct{})}
	client := loopback(t, backend)
	stop := make(chan struct{})
	close(stop)
	if _, _, err := client.CallMCP(mcp.WithStop(context.Background(), stop), "deals", "execute_plan", nil); !errors.Is(err, mcp.ErrStoppedBeforeSend) {
		t.Fatalf("err = %v, want ErrStoppedBeforeSend", err)
	}
	select {
	case <-backend.entered:
		t.Fatal("the stopped call reached the child")
	default:
	}
}
