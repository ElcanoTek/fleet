package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/store"
)

// testRecorder implements http.ResponseWriter + http.Flusher for SSE
// tests. Shared between turnBuffer tests and any handler test that
// needs to inspect SSE output.
type testRecorder struct {
	mu      sync.Mutex
	header  http.Header
	status  int
	body    bytes.Buffer
	flushed int
}

func newRecorder() *testRecorder {
	return &testRecorder{header: http.Header{}}
}

func (r *testRecorder) Header() http.Header { return r.header }
func (r *testRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.Write(p)
}
func (r *testRecorder) WriteHeader(s int) { r.status = s }
func (r *testRecorder) Flush() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushed++
}
func (r *testRecorder) Body() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.String()
}

func TestTurnBuffer_AttachReplaysFromZero(t *testing.T) {
	buf := newTurnBuffer("conv-1", "turn-1")
	buf.Emit("a", map[string]any{"n": 1})
	buf.Emit("b", map[string]any{"n": 2})
	buf.Finish()

	rw := newRecorder()
	if err := buf.Attach(context.Background(), 0, rw, nil); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	body := rw.Body()
	for _, want := range []string{"id: 1\n", "event: a\n", "id: 2\n", "event: b\n"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}

func TestTurnBuffer_AttachRespectsLastEventID(t *testing.T) {
	buf := newTurnBuffer("conv-1", "turn-1")
	for i := 1; i <= 5; i++ {
		buf.Emit("delta", map[string]any{"i": i})
	}
	buf.Finish()

	rw := newRecorder()
	if err := buf.Attach(context.Background(), 3, rw, nil); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	body := rw.Body()
	// Events 1-3 must be absent; events 4-5 must appear.
	for _, skipped := range []string{"id: 1\n", "id: 2\n", "id: 3\n"} {
		if strings.Contains(body, skipped) {
			t.Errorf("replay included skipped id %q:\n%s", skipped, body)
		}
	}
	for _, kept := range []string{"id: 4\n", "id: 5\n"} {
		if !strings.Contains(body, kept) {
			t.Errorf("replay missing kept id %q:\n%s", kept, body)
		}
	}
}

// Attach on an already-finished buffer must return immediately after
// replay — no hanging waiting on subscriber channel.
func TestTurnBuffer_AttachAfterFinishReturns(t *testing.T) {
	buf := newTurnBuffer("conv-1", "turn-1")
	buf.Emit("x", nil)
	buf.Finish()

	done := make(chan error, 1)
	go func() {
		rw := newRecorder()
		done <- buf.Attach(context.Background(), 0, rw, nil)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Attach: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Attach on finished buffer hung")
	}
}

// Emit must fan out to every live subscriber in order.
func TestTurnBuffer_FanOutToMultipleSubscribers(t *testing.T) {
	buf := newTurnBuffer("conv-1", "turn-1")
	var wg sync.WaitGroup
	rws := []*testRecorder{newRecorder(), newRecorder()}
	for _, rw := range rws {
		wg.Add(1)
		go func(rw *testRecorder) {
			defer wg.Done()
			_ = buf.Attach(context.Background(), 0, rw, nil)
		}(rw)
	}

	// Wait until both Attach goroutines have registered (poll, not a fixed
	// sleep) so Emit can't fire before a subscriber attaches.
	eventually(t, func() bool { return buf.subscriberCount() == len(rws) }, "subscribers register")

	for i := 1; i <= 3; i++ {
		buf.Emit("delta", map[string]any{"i": i})
	}
	buf.Finish()
	wg.Wait()

	for idx, rw := range rws {
		body := rw.Body()
		for i := 1; i <= 3; i++ {
			want := fmt.Sprintf("id: %d\n", i)
			if !strings.Contains(body, want) {
				t.Errorf("subscriber %d missing %q", idx, want)
			}
		}
	}
}

// Client disconnect (ctx cancel) must release the subscriber slot so
// the publisher can keep working without blocking.
func TestTurnBuffer_ClientDisconnectReleasesSubscriber(t *testing.T) {
	buf := newTurnBuffer("conv-1", "turn-1")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		rw := newRecorder()
		_ = buf.Attach(ctx, 0, rw, nil)
		close(done)
	}()

	// Wait until the subscriber has registered (poll, not a fixed sleep).
	eventually(t, func() bool { return buf.subscriberCount() == 1 }, "subscriber registers")
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Attach did not return after ctx cancel")
	}

	// Emit after cancel must not block or panic.
	buf.Emit("post-disconnect", nil)
	buf.Finish()
}

// Emit after Finish is a no-op (does not panic, does not grow buffer).
func TestTurnBuffer_EmitAfterFinishIsNoOp(t *testing.T) {
	buf := newTurnBuffer("conv-1", "turn-1")
	buf.Emit("a", nil)
	buf.Finish()
	buf.Emit("should-be-dropped", nil)

	rw := newRecorder()
	_ = buf.Attach(context.Background(), 0, rw, nil)
	if strings.Contains(rw.Body(), "should-be-dropped") {
		t.Error("post-Finish Emit leaked into replay")
	}
}

// failingCreateTurnPersister records what the buffer asks of it while every
// CreateTurn fails — the seam for a turn whose row never got inserted.
type failingCreateTurnPersister struct {
	mu       sync.Mutex
	finishes int
	inserts  int
}

func (p *failingCreateTurnPersister) CreateTurn(context.Context, string, string, int64) error {
	return fmt.Errorf("simulated CreateTurn failure")
}

func (p *failingCreateTurnPersister) InsertTurnEvents(context.Context, []store.TurnEvent) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inserts++
	return nil
}

func (p *failingCreateTurnPersister) FinishTurn(context.Context, string, store.TurnStatus, int64, bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.finishes++
	return nil
}

// A failed attach leaves the buffer with NO persister: Finish must not call
// FinishTurn (or backfill) against a turn row CreateTurn never inserted.
// The persister used to be assigned before CreateTurn ran, so the abandoned
// turn still tried to seal a row that did not exist.
func TestTurnBuffer_AttachPersisterFailureLeavesNoPersister(t *testing.T) {
	buf := newTurnBuffer("conv-1", "turn-1")
	p := &failingCreateTurnPersister{}
	if err := buf.attachPersister(context.Background(), p); err == nil {
		t.Fatal("attachPersister should surface the CreateTurn failure")
	}
	if buf.persister != nil {
		t.Fatal("persister must not be wired when CreateTurn failed")
	}
	buf.Emit("a", map[string]any{"n": 1})
	buf.Finish()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finishes != 0 || p.inserts != 0 {
		t.Errorf("FinishTurn calls = %d, InsertTurnEvents calls = %d, want 0/0 for a turn that was never created", p.finishes, p.inserts)
	}
}
