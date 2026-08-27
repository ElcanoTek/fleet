// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package sandbox

// Regression tests for the PR #1285 review findings: cappedBuffer must be
// safe against the copy goroutines client-go abandons on a cancelled
// StreamWithContext (a reproduced -race failure), and writeStdin must never
// park forever on a stream that ends — or never establishes — since its
// callers hold the sandbox mutex.

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestCappedBufferConcurrentSnapshot fails under -race if cappedBuffer loses
// its locking: writers keep writing while snapshot() reads, exactly the
// abandoned-copier-vs-result-assembly interleaving from the cancelled-bash
// path.
func TestCappedBufferConcurrentSnapshot(t *testing.T) {
	buf := &cappedBuffer{cap: 1 << 20}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			payload := []byte(strings.Repeat("x", 1024))
			for {
				select {
				case <-stop:
					return
				default:
					_, _ = buf.Write(payload)
				}
			}
		}()
	}
	deadline := time.After(200 * time.Millisecond)
	for done := false; !done; {
		select {
		case <-deadline:
			done = true
		default:
			data, discarded := buf.snapshot()
			if int64(len(data)) > 1<<20 {
				t.Fatalf("snapshot exceeded the cap: %d bytes", len(data))
			}
			if discarded < 0 {
				t.Fatalf("negative discarded count %d", discarded)
			}
		}
	}
	close(stop)
	wg.Wait()
	data, _ := buf.snapshot()
	// The snapshot must be a copy: mutating it must not reach the buffer.
	if len(data) > 0 {
		data[0] = 'Z'
		again, _ := buf.snapshot()
		if again[0] == 'Z' {
			t.Fatal("snapshot returned the live buffer, not a copy")
		}
	}
}

// TestWriteStdinUnblocksWhenStreamEnds pins the no-wedge contract: a
// writeStdin whose payload can never be consumed (no reader — the shape of a
// stalled dial or a dead stream) returns once the stream ends instead of
// parking the caller forever.
func TestWriteStdinUnblocksWhenStreamEnds(t *testing.T) {
	pr, pw := io.Pipe()
	_ = pr // never read: the stream never came up
	_, cancel := context.WithCancel(context.Background())
	s := &k8sExecSession{stdinW: pw, cancel: cancel, done: make(chan struct{}), exitCode: -1}

	errCh := make(chan error, 1)
	go func() { errCh <- s.writeStdin([]byte(strings.Repeat("y", 64*1024))) }()

	// Simulate StreamWithContext returning (stream over) the way execPod's
	// goroutine does: close done, then unblock the pipe.
	time.Sleep(20 * time.Millisecond)
	close(s.done)
	_ = s.stdinW.CloseWithError(io.ErrClosedPipe)

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("writeStdin reported success for a payload nothing consumed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writeStdin stayed parked after the stream ended")
	}
}

// TestWriteStdinTimesOutOnWedgedStream pins the timeout backstop itself: with
// the stream neither consuming nor ending, writeStdin returns within the
// bound and cancels the session.
func TestWriteStdinTimesOutOnWedgedStream(t *testing.T) {
	old := k8sStdinWriteTimeout
	k8sStdinWriteTimeout = 50 * time.Millisecond
	t.Cleanup(func() { k8sStdinWriteTimeout = old })

	pr, pw := io.Pipe()
	defer pr.Close()
	ctx, cancel := context.WithCancel(context.Background())
	s := &k8sExecSession{stdinW: pw, cancel: cancel, done: make(chan struct{}), exitCode: -1}

	start := time.Now()
	err := s.writeStdin([]byte(strings.Repeat("z", 64*1024)))
	if err == nil {
		t.Fatal("writeStdin reported success for a wedged stream")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("writeStdin took %v, want the ~50ms bound", elapsed)
	}
	if ctx.Err() == nil {
		t.Fatal("timeout did not cancel the session context")
	}
	// Reap the parked write goroutine the way the stream goroutine would.
	_ = pw.CloseWithError(io.ErrClosedPipe)
}
