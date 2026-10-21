package mcpbroker

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

// TestRoundtripNotSentWhenCancelledWhileQueuedToWrite: a call whose context
// ends while another frame holds the write lock is never written, so the
// child never runs a tool for a caller that already gave up.
func TestRoundtripNotSentWhenCancelledWhileQueuedToWrite(t *testing.T) {
	parent, child := net.Pipe()
	defer child.Close()
	c := NewClient(parent)
	defer c.Close()
	var (
		mu   sync.Mutex
		seen []byte
	)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := child.Read(buf)
			mu.Lock()
			seen = append(seen, buf[:n]...)
			mu.Unlock()
			if err != nil {
				return // the pipe closed
			}
		}
	}()

	c.writeMu.Lock() // another frame is being written
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.roundtrip(ctx, request{ID: c.nextID.Add(1), Method: methodCall, Server: "pages", Tool: "deploy"})
		done <- err
	}()
	time.Sleep(20 * time.Millisecond) // let it queue on the write lock
	cancel()
	c.writeMu.Unlock()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("roundtrip = %v, want canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("roundtrip did not return")
	}
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 0 {
		t.Fatalf("a cancelled call was written to the child: %q", seen)
	}
}
