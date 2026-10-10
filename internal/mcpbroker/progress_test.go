package mcpbroker

import (
	"context"
	"sync"
	"testing"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// progressBackend reports progress through whatever sink its call context
// carries, the way mcp.Server.callTool's transport would, then answers.
type progressBackend struct {
	*fakeBroker
	sawSink bool
}

func (b *progressBackend) CallMCP(ctx context.Context, _, _ string, _ map[string]any) (string, bool, error) {
	if sink := mcp.ProgressSink(ctx); sink != nil {
		b.sawSink = true
		sink(mcp.ProgressUpdate{Progress: 1, Total: 3, Message: "first"})
		sink(mcp.ProgressUpdate{Progress: 2, Total: 3}) // inside the forward interval: held
		sink(mcp.ProgressUpdate{Progress: 3, Total: 3, Message: "last"})
	}
	return "done", false, nil
}

// A call whose context carries a progress sink gets the child's progress as
// intermediate frames before its answer; the answer is unchanged. The child
// stops forwarding before the final frame, so nothing arrives after it.
func TestClientServer_ForwardsProgressFrames(t *testing.T) {
	backend := &progressBackend{fakeBroker: &fakeBroker{}}
	client := loopback(t, backend)

	var mu sync.Mutex
	var got []mcp.ProgressUpdate
	ctx := mcp.WithProgress(context.Background(), func(u mcp.ProgressUpdate) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, u)
	})
	text, isErr, err := client.CallMCP(ctx, "deals", "execute_plan", nil)
	if err != nil || text != "done" || isErr {
		t.Fatalf("CallMCP = (%q, %v, %v), want the call's answer", text, isErr, err)
	}
	if !backend.sawSink {
		t.Fatal("the child must re-attach a progress sink when the parent asks for progress")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].Message != "first" {
		t.Fatalf("progress = %+v, want the first update forwarded at once (later ones inside the interval are dropped when the call ends first)", got)
	}
	// The client is still usable: the pending slot was freed by the final frame.
	if _, _, err := client.CallMCP(context.Background(), "deals", "execute_plan", nil); err != nil {
		t.Fatalf("second call: %v", err)
	}
}

// Without a sink the child is not asked for progress and re-attaches none:
// every other call crosses exactly as before.
func TestClientServer_NoProgressWithoutASink(t *testing.T) {
	backend := &progressBackend{fakeBroker: &fakeBroker{}}
	client := loopback(t, backend)
	if _, _, err := client.CallMCP(context.Background(), "deals", "execute_plan", nil); err != nil {
		t.Fatal(err)
	}
	if backend.sawSink {
		t.Fatal("a call without a progress sink must not ask the child for progress")
	}
}
