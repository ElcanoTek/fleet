package runner

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// sseRecorder implements http.ResponseWriter + http.Flusher for SSE buffer tests.
type sseRecorder struct {
	mu     sync.Mutex
	header http.Header
	status int
	body   bytes.Buffer
}

func newSSERecorder() *sseRecorder { return &sseRecorder{header: http.Header{}} }

func (r *sseRecorder) Header() http.Header { return r.header }
func (r *sseRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.Write(p)
}
func (r *sseRecorder) WriteHeader(s int) { r.status = s }
func (r *sseRecorder) Flush()            {}
func (r *sseRecorder) Body() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.String()
}

// TestTaskStreamBuffer_AttachReplaysFromZero: a sealed buffer attached at
// lastEventID=0 replays every event in order with monotonic ids.
func TestTaskStreamBuffer_AttachReplaysFromZero(t *testing.T) {
	buf := newTaskStreamBuffer()
	buf.Emit("a", map[string]any{"n": 1})
	buf.Emit("b", map[string]any{"n": 2})
	buf.Finish()

	rw := newSSERecorder()
	if err := buf.Attach(context.Background(), 0, rw); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	body := rw.Body()
	if !strings.Contains(body, "id: 1\nevent: a\n") {
		t.Errorf("missing first event frame:\n%s", body)
	}
	if !strings.Contains(body, "id: 2\nevent: b\n") {
		t.Errorf("missing second event frame:\n%s", body)
	}
}

// TestTaskStreamBuffer_AttachReplaysFromLastEventID: a reconnect with
// Last-Event-ID only replays events strictly newer than that id.
func TestTaskStreamBuffer_AttachReplaysFromLastEventID(t *testing.T) {
	buf := newTaskStreamBuffer()
	for i := 0; i < 4; i++ {
		buf.Emit("e", map[string]any{"i": i})
	}
	buf.Finish()

	rw := newSSERecorder()
	if err := buf.Attach(context.Background(), 2, rw); err != nil { // client last saw id 2
		t.Fatalf("Attach: %v", err)
	}
	body := rw.Body()
	if strings.Contains(body, "id: 1\n") || strings.Contains(body, "id: 2\n") {
		t.Errorf("replay should skip events <= lastEventID:\n%s", body)
	}
	if !strings.Contains(body, "id: 3\n") || !strings.Contains(body, "id: 4\n") {
		t.Errorf("replay should include events > lastEventID:\n%s", body)
	}
}

// TestTaskStreamBuffer_SlidingWindowEviction: past the byte cap the oldest events
// are evicted while ids stay monotonic and the newest event survives.
func TestTaskStreamBuffer_SlidingWindowEviction(t *testing.T) {
	buf := newTaskStreamBuffer()
	buf.maxBytes = 200 // small cap to force eviction

	for i := 1; i <= 10; i++ {
		buf.Emit("delta", map[string]any{"pad": strings.Repeat("x", 40)})
	}

	buf.mu.Lock()
	n := len(buf.events)
	first := buf.events[0].ID
	last := buf.events[n-1].ID
	total := buf.totalBytes
	buf.mu.Unlock()

	if total > buf.maxBytes {
		t.Errorf("totalBytes %d exceeds cap %d after eviction", total, buf.maxBytes)
	}
	if first == 1 {
		t.Errorf("expected oldest events evicted, but first surviving id is still 1")
	}
	if last != 10 {
		t.Errorf("last id = %d, want 10 (IDs must stay monotonic across eviction)", last)
	}
}

// TestTaskStreamBuffer_LiveThenFinish: a subscriber attached to an OPEN buffer
// receives events emitted after attach, and returns cleanly (nil) when Finish
// seals the buffer.
func TestTaskStreamBuffer_LiveThenFinish(t *testing.T) {
	buf := newTaskStreamBuffer()
	rw := newSSERecorder()

	done := make(chan error, 1)
	go func() { done <- buf.Attach(context.Background(), 0, rw) }()

	// Wait for the subscription to register before emitting, so the event lands on
	// the live channel rather than only the (already-snapshotted) replay slice.
	waitForSubscribers(t, buf, 1)
	buf.Emit("agent_message", map[string]any{"content": "hello"})
	buf.Finish()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Attach returned error on clean seal: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Attach did not return after Finish (subscriber hung)")
	}
	if !strings.Contains(rw.Body(), "event: agent_message") {
		t.Errorf("live event not delivered:\n%s", rw.Body())
	}
}

// TestTaskStreamBuffer_AttachAfterFinish: attaching to an already-sealed buffer
// replays the full history and returns immediately (no live channel, no hang).
func TestTaskStreamBuffer_AttachAfterFinish(t *testing.T) {
	buf := newTaskStreamBuffer()
	buf.Emit("status", map[string]any{"status": "running"})
	buf.Emit("status", map[string]any{"status": "succeeded"})
	buf.Finish()

	rw := newSSERecorder()
	done := make(chan error, 1)
	go func() { done <- buf.Attach(context.Background(), 0, rw) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Attach: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Attach to a sealed buffer hung")
	}
	if c := strings.Count(rw.Body(), "event: status"); c != 2 {
		t.Errorf("expected 2 status frames replayed, got %d:\n%s", c, rw.Body())
	}
}

// TestTaskStreamBuffer_EmitAfterFinishIsNoop: Finish makes Emit a no-op, so a
// late event never appears and ids do not advance.
func TestTaskStreamBuffer_EmitAfterFinishIsNoop(t *testing.T) {
	buf := newTaskStreamBuffer()
	buf.Emit("a", map[string]any{})
	buf.Finish()
	buf.Emit("b", map[string]any{}) // dropped

	rw := newSSERecorder()
	if err := buf.Attach(context.Background(), 0, rw); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if strings.Contains(rw.Body(), "event: b") {
		t.Errorf("event emitted after Finish should be dropped:\n%s", rw.Body())
	}
}

// TestTaskStreamBuffer_FinishReportsSealOnce: Finish returns true only on the call
// that actually seals, so the registry arms the cleanup timer exactly once.
func TestTaskStreamBuffer_FinishReportsSealOnce(t *testing.T) {
	buf := newTaskStreamBuffer()
	if !buf.Finish() {
		t.Fatal("first Finish should report it sealed the buffer")
	}
	if buf.Finish() {
		t.Fatal("second Finish should report no-op (already sealed)")
	}
}

// TestTaskStreamBuffer_ObserveMapsRunEvents: the Observer adapter maps the run's
// internal event names onto the stable SSE types the UI tails, and ignores loop
// internals (reasoning/enforcement).
func TestTaskStreamBuffer_ObserveMapsRunEvents(t *testing.T) {
	buf := newTaskStreamBuffer()
	buf.Observe("text.delta", map[string]any{"text": "partial answer"})
	buf.Observe("tool.call", map[string]any{"id": "call-1", "name": "bash", "input": `{"command":"ls"}`})
	buf.Observe("tool.result", map[string]any{"id": "call-1", "name": "bash", "text": "total 0", "is_err": false})
	buf.Observe("reasoning.delta", map[string]any{"text": "thinking"}) // must NOT forward
	buf.Observe("enforcement", map[string]any{"message": "nudge"})     // must NOT forward
	buf.Observe("text.delta", map[string]any{"text": ""})              // empty chunk skipped
	buf.Finish()

	rw := newSSERecorder()
	if err := buf.Attach(context.Background(), 0, rw); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	body := rw.Body()
	for _, want := range []string{"event: agent_message", "event: tool_call", "event: tool_result", `"call_id":"call-1"`, `"content":"partial answer"`} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in stream:\n%s", want, body)
		}
	}
	if strings.Contains(body, "thinking") || strings.Contains(body, "nudge") {
		t.Errorf("loop-internal events should not be forwarded to the live stream:\n%s", body)
	}
	// Exactly three forwarded frames (the empty text.delta and the two ignored
	// events produce nothing).
	if c := strings.Count(body, "\nevent: "); c != 3 {
		t.Errorf("expected 3 forwarded SSE frames, got %d:\n%s", c, body)
	}
}

// TestTaskStreamRegistry_Lifecycle: register exposes a buffer via Lookup; release
// seals it (a subsequent Attach returns immediately) but keeps it retained for the
// replay window so a late joiner still finds it.
func TestTaskStreamRegistry_Lifecycle(t *testing.T) {
	reg := newTaskStreamRegistry()
	id := uuid.New()

	if _, ok := reg.Lookup(id); ok {
		t.Fatal("Lookup before register should miss")
	}

	buf := reg.register(id)
	got, ok := reg.Lookup(id)
	if !ok {
		t.Fatal("Lookup after register should hit")
	}
	if got == nil {
		t.Fatal("Lookup returned a nil stream")
	}

	buf.Emit("status", map[string]any{"status": "running"})
	reg.release(id, buf)

	// Still retained (within the 2-minute window): a late joiner replays + returns.
	got2, ok := reg.Lookup(id)
	if !ok {
		t.Fatal("buffer should remain retained immediately after release")
	}
	rw := newSSERecorder()
	done := make(chan error, 1)
	go func() { done <- got2.Attach(context.Background(), 0, rw) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Attach to released buffer: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Attach to a released (sealed) buffer hung")
	}
	if !strings.Contains(rw.Body(), "event: status") {
		t.Errorf("retained buffer should replay its events:\n%s", rw.Body())
	}
}

// TestTaskStreamBuffer_AttachClientDisconnect: a cancelled request context unwinds
// a live Attach with the context error, evicting the subscriber.
func TestTaskStreamBuffer_AttachClientDisconnect(t *testing.T) {
	buf := newTaskStreamBuffer()
	rw := newSSERecorder()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- buf.Attach(ctx, 0, rw) }()
	waitForSubscribers(t, buf, 1)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a context error on client disconnect")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Attach did not return after ctx cancel")
	}
	// Subscriber must be evicted on unwind.
	buf.mu.Lock()
	n := len(buf.subscribers)
	buf.mu.Unlock()
	if n != 0 {
		t.Errorf("subscriber not evicted on disconnect: %d remain", n)
	}
}

// waitForSubscribers polls until the buffer has at least n subscribers (a barrier
// that avoids a fixed sleep racing the Attach goroutine's registration).
func waitForSubscribers(t *testing.T, buf *taskStreamBuffer, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		buf.mu.Lock()
		got := len(buf.subscribers)
		buf.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d subscriber(s)", n)
}
