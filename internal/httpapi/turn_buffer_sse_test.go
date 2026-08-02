package httpapi

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestTurnBuffer_SlidingWindowEviction verifies the byte cap evicts the oldest
// events while keeping IDs monotonic and the newest event present (#295).
func TestTurnBuffer_SlidingWindowEviction(t *testing.T) {
	buf := newTurnBuffer("c", "t")
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

// TestTurnBuffer_ReconnectFrameOnGap verifies a client reconnecting after the
// sliding window dropped events it hadn't seen receives a synthetic `reconnect`
// frame before the replay.
func TestTurnBuffer_ReconnectFrameOnGap(t *testing.T) {
	buf := newTurnBuffer("c", "t")
	buf.maxBytes = 200
	for i := 1; i <= 10; i++ {
		buf.Emit("delta", map[string]any{"pad": strings.Repeat("x", 40)})
	}
	buf.Finish()

	rw := newRecorder()
	if err := buf.Attach(context.Background(), 1, rw, nil); err != nil { // client last saw id 1
		t.Fatalf("Attach: %v", err)
	}
	body := rw.Body()
	if !strings.Contains(body, "event: reconnect") {
		t.Errorf("expected a reconnect frame after a sliding-window gap:\n%s", body)
	}
	if !strings.Contains(body, "missed_events") {
		t.Errorf("reconnect frame missing missed_events:\n%s", body)
	}
}

// TestTurnBuffer_NoReconnectFrameWhenContiguous verifies a clean reconnect (no
// evicted events) does NOT inject a reconnect frame.
func TestTurnBuffer_NoReconnectFrameWhenContiguous(t *testing.T) {
	buf := newTurnBuffer("c", "t") // maxBytes 0 = unlimited, no eviction
	for i := 1; i <= 5; i++ {
		buf.Emit("delta", map[string]any{"i": i})
	}
	buf.Finish()

	rw := newRecorder()
	if err := buf.Attach(context.Background(), 3, rw, nil); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if strings.Contains(rw.Body(), "event: reconnect") {
		t.Errorf("unexpected reconnect frame on a contiguous replay:\n%s", rw.Body())
	}
}

// TestTurnBuffer_Heartbeat verifies idle keepalive comment frames are emitted.
func TestTurnBuffer_Heartbeat(t *testing.T) {
	old := sseHeartbeatInterval
	sseHeartbeatInterval = 20 * time.Millisecond
	defer func() { sseHeartbeatInterval = old }()

	buf := newTurnBuffer("c", "t") // not finished → live subscription stays open
	rw := newRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = buf.Attach(ctx, 0, rw, nil)
		close(done)
	}()

	time.Sleep(120 * time.Millisecond) // ~6 heartbeat intervals
	cancel()
	<-done

	if !strings.Contains(rw.Body(), ": keepalive") {
		t.Errorf("expected at least one heartbeat keepalive frame, got:\n%q", rw.Body())
	}
}

// TestTurnBuffer_EvictedSubscriberGetsEvictedFrame: a subscriber whose channel
// filled up is closed by Emit, but the turn is still running — the stream must
// end with an explicit evicted `reconnect` frame, not the same clean EOF a
// finished turn produces (which the client renders as turn-complete).
func TestTurnBuffer_EvictedSubscriberGetsEvictedFrame(t *testing.T) {
	buf := newTurnBuffer("c", "t")

	rw := newRecorder()
	done := make(chan error, 1)
	go func() { done <- buf.Attach(context.Background(), 0, rw, nil) }()
	for buf.subscriberCount() == 0 {
		time.Sleep(time.Millisecond)
	}

	// Take the buffer lock so the attached reader cannot drain its channel
	// (the reader takes b.mu per unsubscribe only; simpler: flood far past
	// the 256-slot channel in one burst before the reader goroutine can keep
	// up is racy — instead, flood while holding no lock but with enough
	// events that the non-blocking send must overflow at least once).
	for i := 0; i < 5000; i++ {
		buf.Emit("delta", map[string]any{"i": i})
	}
	// The reader either got evicted (desired path) or drained everything;
	// keep flooding until it detaches.
	deadline := time.After(5 * time.Second)
	for buf.subscriberCount() > 0 {
		select {
		case <-deadline:
			t.Fatal("subscriber never evicted; cannot exercise the eviction path")
		default:
			buf.Emit("delta", map[string]any{"pad": strings.Repeat("x", 64)})
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("Attach returned error: %v", err)
	}
	body := rw.Body()
	if !strings.Contains(body, `"type":"evicted"`) {
		t.Errorf("evicted subscriber's stream ended without an evicted frame:\n…%s", body[max(0, len(body)-300):])
	}
	if buf.Sealed() {
		t.Error("buffer must still be live — eviction is not Finish")
	}
}
