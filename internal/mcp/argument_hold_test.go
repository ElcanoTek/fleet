package mcp

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestArgumentHoldReleasesAfterCallerAndRetentions(t *testing.T) {
	released := 0
	ctx, hold := WithArgumentHold(context.Background(), func() { released++ })
	letGo := RetainArguments(ctx)
	hold.Done()
	if released != 0 {
		t.Fatal("released while a retention was open")
	}
	letGo()
	letGo()
	hold.Done()
	if released != 1 {
		t.Fatalf("released %d times, want 1", released)
	}
	// No hold on ctx: a no-op.
	RetainArguments(context.Background())()
}

// TestLockWithContextGivesUp: a call queued behind a slow one on the same
// server gives up its wait when cancelled — so it stops holding its
// arguments — and the lock it would have taken is released, not leaked.
func TestLockWithContextGivesUp(t *testing.T) {
	var mu sync.Mutex
	mu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := lockWithContext(ctx, &mu); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lockWithContext = %v, want deadline exceeded", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("did not give up promptly")
	}
	mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for !mu.TryLock() {
		if time.Now().After(deadline) {
			t.Fatal("the abandoned acquisition leaked the lock")
		}
		time.Sleep(time.Millisecond)
	}
	mu.Unlock()
	if err := lockWithContext(context.Background(), &mu); err != nil {
		t.Fatal(err)
	}
	mu.Unlock()
}
