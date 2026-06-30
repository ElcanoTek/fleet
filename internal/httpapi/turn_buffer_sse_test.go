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
