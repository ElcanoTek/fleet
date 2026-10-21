//go:build fleet_host_executor

package agentcore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/sandbox"
	fleettools "github.com/ElcanoTek/fleet/internal/tools"
)

// blockingBroker holds each call until hold elapses or ctx ends, sampling the
// workspace-file budget on entry. mode picks how a call ends.
type blockingBroker struct {
	hold    time.Duration
	mode    string // "ok", "error", "panic"
	inside  atomic.Int64
	maxIn   atomic.Int64
	mu      sync.Mutex
	samples []brokerSample
	entered chan struct{}
}

type brokerSample struct{ inside, charged, budget int64 }

func (b *blockingBroker) CallMCP(ctx context.Context, _, _ string, args map[string]any) (string, bool, error) {
	n := b.inside.Add(1)
	defer b.inside.Add(-1)
	for {
		m := b.maxIn.Load()
		if n <= m || b.maxIn.CompareAndSwap(m, n) {
			break
		}
	}
	charged, budget := fleettools.WorkspaceFileBudgetUsage()
	b.mu.Lock()
	b.samples = append(b.samples, brokerSample{n, charged, budget})
	b.mu.Unlock()
	if _, ok := args["chunk"].(string); !ok {
		return "", false, errors.New("reference was not expanded")
	}
	if b.entered != nil {
		b.entered <- struct{}{}
	}
	select {
	case <-time.After(b.hold):
	case <-ctx.Done():
		return "", false, ctx.Err()
	}
	switch b.mode {
	case "error":
		return "", false, errors.New("broker failed")
	case "panic":
		panic("broker panicked")
	}
	return `{"accepted":true}`, false, nil
}

func budgetHostFixture(t *testing.T, size int) (context.Context, fleettools.WorkspaceFileReadFunc, string) {
	t.Helper()
	root := t.TempDir()
	sb := sandbox.NewHost(nil)
	t.Cleanup(sb.Close)
	if err := sb.BindFileOpRoot(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	body := make([]byte, size)
	for i := range body {
		body[i] = byte(i*13 + i>>9)
	}
	if err := os.WriteFile(filepath.Join(root, "site.zip"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return fleettools.WithForcedWorkingDir(context.Background(), root), fleettools.WorkspaceFileReader(sb), hex.EncodeToString(sum[:])
}

func budgetTool(broker MCPBroker, reader fleettools.WorkspaceFileReadFunc) *mcpTool {
	return &mcpTool{serverName: "pages", tool: mcp.Tool{Name: "upload_chunk", InputSchema: map[string]any{"properties": map[string]any{
		"chunk": map[string]any{"type": "string", "contentEncoding": "base64"},
		"other": map[string]any{"type": "string", "contentEncoding": "base64"},
	}}}, broker: broker, policy: &gatePolicy{}, readWorkspaceFile: reader}
}

// TestMCPWorkspaceFileWindowsStayCharged: N concurrent calls whose broker is
// slow never hold more than the budget. Each window — cache hits included —
// is charged from before its base64 exists until the broker call returns, so
// calls past what the budget admits queue instead of piling up.
func TestMCPWorkspaceFileWindowsStayCharged(t *testing.T) {
	const size = 8 << 20
	ctx, reader, sum := budgetHostFixture(t, size)
	baseline, budget := fleettools.WorkspaceFileBudgetUsage()
	window := fleettools.WorkspaceWindowCharge(maxMCPWorkspaceWindowBytes)
	admitted := (budget - baseline - size) / window
	if admitted < 1 || admitted >= 4 {
		t.Fatalf("fixture expects 1..3 concurrent windows to fit, budget admits %d", admitted)
	}
	broker := &blockingBroker{hold: 300 * time.Millisecond, mode: "ok"}
	input := mustFileJSON(t, map[string]any{"chunk": workspaceFileArgument{"site.zip", sum, 0, maxMCPWorkspaceWindowBytes}})
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			// One tool per call: the gatePolicy fixture is not concurrency-safe.
			result, err := budgetTool(broker, reader).Run(ctx, fantasy.ToolCall{ID: string(rune('a' + i)), Input: input})
			if err != nil || result.IsError {
				t.Errorf("call %d: %+v %v", i, result, err)
			}
		})
	}
	wg.Wait()
	if got := broker.maxIn.Load(); got > admitted {
		t.Fatalf("%d windows were in the broker at once; the budget admits %d", got, admitted)
	}
	for _, s := range broker.samples {
		if s.charged > s.budget {
			t.Fatalf("charged %d exceeds budget %d", s.charged, s.budget)
		}
		// Every window in the broker (hits too) is charged, plus the file.
		if s.charged < size+s.inside*window {
			t.Fatalf("%d windows in flight but only %d bytes charged", s.inside, s.charged)
		}
	}
	if after, _ := fleettools.WorkspaceFileBudgetUsage(); after > baseline+size {
		t.Fatalf("charges leaked: %d after, baseline %d + cached file %d", after, baseline, size)
	}
}

// TestMCPWorkspaceFileLeaseReleasedOnEveryPath: a broker error, a
// cancellation while the broker is blocked, a broker panic, and a reference
// that fails after an earlier one was resolved all release their charges.
func TestMCPWorkspaceFileLeaseReleasedOnEveryPath(t *testing.T) {
	ctx, reader, sum := budgetHostFixture(t, 1<<20)
	// Other tests may have left verified files cached (never pinned).
	baseline, _ := fleettools.WorkspaceFileBudgetUsage()
	ref := func(offset int) map[string]any {
		return map[string]any{"chunk": workspaceFileArgument{"site.zip", sum, offset, 1024}}
	}
	settle := func(name string) {
		t.Helper()
		// The file stays cached (unpinned); every lease must be gone.
		if charged, _ := fleettools.WorkspaceFileBudgetUsage(); charged > baseline+1<<20 {
			t.Fatalf("%s: %d bytes still charged, more than the cached file", name, charged)
		}
	}

	_, _ = budgetTool(&blockingBroker{mode: "error"}, reader).Run(ctx, fantasy.ToolCall{ID: "e", Input: mustFileJSON(t, ref(0))})
	settle("broker error")

	broker := &blockingBroker{hold: time.Minute, mode: "ok", entered: make(chan struct{}, 1)}
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		_, _ = budgetTool(broker, reader).Run(cctx, fantasy.ToolCall{ID: "c", Input: mustFileJSON(t, ref(1024))})
		close(done)
	}()
	<-broker.entered
	if charged, _ := fleettools.WorkspaceFileBudgetUsage(); charged < 1<<20+fleettools.WorkspaceWindowCharge(1024) {
		// (the file is pinned by this lease, so it cannot have been evicted)
		t.Fatalf("window in flight is not charged: %d", charged)
	}
	cancel()
	<-done
	settle("cancellation")

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("broker panic did not propagate")
			}
		}()
		_, _ = budgetTool(&blockingBroker{mode: "panic"}, reader).Run(ctx, fantasy.ToolCall{ID: "p", Input: mustFileJSON(t, ref(2048))})
	}()
	settle("panic")

	// The second reference names a missing file: the first is released.
	both := ref(0)
	both["other"] = workspaceFileArgument{"missing.zip", sum, 0, 1024}
	broker = &blockingBroker{mode: "ok"}
	if result, _ := budgetTool(broker, reader).Run(ctx, fantasy.ToolCall{ID: "m", Input: mustFileJSON(t, both)}); !result.IsError || len(broker.samples) != 0 {
		t.Fatalf("partially resolved call dispatched: %+v", result)
	}
	settle("resolve failure")
}

// retainingBroker returns at once but, like an out-of-process call cancelled
// mid-flight, reports that it still holds the arguments until letGo.
type retainingBroker struct{ letGo chan func() }

func (b retainingBroker) CallMCP(ctx context.Context, _, _ string, _ map[string]any) (string, bool, error) {
	b.letGo <- mcp.RetainArguments(ctx)
	return "", false, context.Canceled
}

// TestMCPWorkspaceFileLeaseOutlivesRetainedDispatch: when the dispatch still
// holds args after CallMCP returned (a cancelled broker call the child has not
// answered), the windows stay charged until it lets go.
func TestMCPWorkspaceFileLeaseOutlivesRetainedDispatch(t *testing.T) {
	ctx, reader, sum := budgetHostFixture(t, 1<<20)
	baseline, _ := fleettools.WorkspaceFileBudgetUsage()
	broker := retainingBroker{letGo: make(chan func(), 1)}
	input := mustFileJSON(t, map[string]any{"chunk": workspaceFileArgument{"site.zip", sum, 0, 4096}})
	_, _ = budgetTool(broker, reader).Run(ctx, fantasy.ToolCall{ID: "r", Input: input})
	letGo := <-broker.letGo
	window := fleettools.WorkspaceWindowCharge(4096)
	if charged, _ := fleettools.WorkspaceFileBudgetUsage(); charged < baseline+window {
		t.Fatalf("lease released while the dispatch still holds args: %d charged", charged)
	}
	letGo()
	if charged, _ := fleettools.WorkspaceFileBudgetUsage(); charged > baseline+1<<20 {
		t.Fatalf("lease not released after the dispatch let go: %d charged", charged)
	}
}

// TestMCPWorkspaceFileTwoReferencesOneCall: a call with two references is
// admitted as one unit and both windows reach the broker.
func TestMCPWorkspaceFileTwoReferencesOneCall(t *testing.T) {
	ctx, reader, sum := budgetHostFixture(t, 1<<20)
	broker := &recordingBroker{text: `{}`}
	input := mustFileJSON(t, map[string]any{
		"chunk": workspaceFileArgument{"site.zip", sum, 0, 3},
		"other": workspaceFileArgument{"site.zip", sum, 3, 3},
	})
	result, err := budgetTool(broker, reader).Run(ctx, fantasy.ToolCall{ID: "two", Input: input})
	if err != nil || result.IsError || broker.calls != 1 {
		t.Fatalf("two-reference call: %+v %v", result, err)
	}
	if _, ok := broker.gotArgs["chunk"].(string); !ok {
		t.Fatal("chunk not expanded")
	}
	if _, ok := broker.gotArgs["other"].(string); !ok {
		t.Fatal("other not expanded")
	}
}
