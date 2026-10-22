package mcpbroker

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/mcp"
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

// cancelOnMarshal ends the caller's context while the request is being
// marshalled, as a slow encode of a large workspace-file argument can.
type cancelOnMarshal struct{ cancel context.CancelFunc }

func (c cancelOnMarshal) MarshalJSON() ([]byte, error) {
	c.cancel()
	return []byte(`"payload"`), nil
}

// TestRoundtripNotSentWhenCancelledWhileEncoding: a call whose context ends
// while its request is being marshalled is not written either.
func TestRoundtripNotSentWhenCancelledWhileEncoding(t *testing.T) {
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := c.roundtrip(ctx, request{ID: c.nextID.Add(1), Method: methodCall, Server: "pages", Tool: "deploy",
		Args: map[string]any{"chunk": cancelOnMarshal{cancel}}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("roundtrip = %v, want canceled", err)
	}
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	if len(seen) != 0 {
		mu.Unlock()
		t.Fatalf("a call cancelled while encoding was written to the child: %q", seen)
	}
	mu.Unlock()
	// The writer is disarmed again: a later live call goes out.
	live, stop := context.WithCancel(context.Background())
	defer stop()
	go func() {
		_, _ = c.roundtrip(live, request{ID: c.nextID.Add(1), Method: methodCall, Server: "pages", Tool: "ping"})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		got := strings.Contains(string(seen), `"tool":"ping"`)
		mu.Unlock()
		if got {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a live call after a refused one was never written")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRoundtripNotSentWhenStoppedWhileQueuedToWrite: a stoppable call whose
// stop signal fires while another frame holds the write lock is never
// written; it answers ErrStoppedBeforeSend, so the outcome says nothing ran.
func TestRoundtripNotSentWhenStoppedWhileQueuedToWrite(t *testing.T) {
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
	stop := make(chan struct{})
	ctx := mcp.WithStop(context.Background(), stop)
	done := make(chan error, 1)
	go func() {
		_, err := c.roundtrip(ctx, request{ID: c.nextID.Add(1), Method: methodCall, Server: "deals", Tool: "execute_plan", Stoppable: true})
		done <- err
	}()
	time.Sleep(20 * time.Millisecond) // let it queue on the write lock
	close(stop)
	c.writeMu.Unlock()
	select {
	case err := <-done:
		if !errors.Is(err, mcp.ErrStoppedBeforeSend) {
			t.Fatalf("roundtrip = %v, want ErrStoppedBeforeSend", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("roundtrip did not return")
	}
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 0 {
		t.Fatalf("a stopped call was written to the child: %q", seen)
	}
}
