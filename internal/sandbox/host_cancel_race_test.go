//go:build fleet_host_executor

package sandbox

import (
	"bufio"
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// TestHostReadLocked_CancelMidReadNoRace mirrors the container-backend #583
// regression test for the test-only host executor: readLocked's cancel arm
// calls terminateBridgeLocked (nils h.bridgeStdout) while the reader goroutine
// is blocked in ReadBytes. The snapshot fix makes this pass under -race.
func TestHostReadLocked_CancelMidReadNoRace(t *testing.T) {
	pr, pw := io.Pipe()
	h := &hostImpl{
		bridgeStdout:  bufio.NewReader(pr),
		bridgeStarted: true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	h.mu.Lock()
	_, err := h.readLocked(ctx, time.Minute)
	h.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("readLocked = %v, want a cancellation error", err)
	}

	// Unblock and reap the orphaned reader goroutine.
	_ = pw.Close()
}
