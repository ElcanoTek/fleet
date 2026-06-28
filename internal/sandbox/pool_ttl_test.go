package sandbox

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fixedClock returns a settable clock for deterministic TTL tests.
func fixedClock(t *time.Time) func() time.Time {
	return func() time.Time { return *t }
}

func TestPool_Stale(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	p := &Pool{cfg: PoolConfig{WarmTTL: time.Minute}, nowFn: func() time.Time { return now }}

	if !p.stale(parkedSandbox{parkedAt: now.Add(-2 * time.Minute)}) {
		t.Error("a sandbox parked 2m ago must be stale at TTL=1m")
	}
	if p.stale(parkedSandbox{parkedAt: now.Add(-30 * time.Second)}) {
		t.Error("a sandbox parked 30s ago must NOT be stale at TTL=1m")
	}
	// TTL=0 disables reaping: nothing is ever stale.
	p0 := &Pool{cfg: PoolConfig{WarmTTL: 0}, nowFn: func() time.Time { return now }}
	if p0.stale(parkedSandbox{parkedAt: now.Add(-time.Hour)}) {
		t.Error("WarmTTL=0 must never report stale")
	}
}

// TestPool_ReapStaleClosesStaleKeepsFresh drives reapStale directly with a fake
// clock and manually-parked sandboxes, asserting the deterministic outcome: the
// over-TTL container is closed, the fresh one survives and stays usable.
func TestPool_ReapStaleClosesStaleKeepsFresh(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	p := &Pool{
		cfg:   PoolConfig{Size: 3, Mode: ModeHost, WarmTTL: time.Minute},
		slots: make(chan parkedSandbox, 3),
		nowFn: fixedClock(&now),
	}
	fresh := NewHost(nil)
	stale := NewHost(nil)
	p.slots <- parkedSandbox{sb: fresh, parkedAt: now.Add(-10 * time.Second)} // young
	p.slots <- parkedSandbox{sb: stale, parkedAt: now.Add(-5 * time.Minute)}  // over TTL

	p.reapStale()

	ctx := context.Background()
	if _, err := stale.RunBash(ctx, BashRequest{Command: "echo x"}); !errors.Is(err, ErrClosed) {
		t.Errorf("stale sandbox should have been closed by reapStale; RunBash err = %v", err)
	}
	if _, err := fresh.RunBash(ctx, BashRequest{Command: "echo x"}); err != nil {
		t.Errorf("fresh sandbox must survive reapStale and stay usable; RunBash err = %v", err)
	}
	p.Close()
}

// TestPool_TakeSkipsStale verifies Take does not hand out an over-TTL warm
// container: it closes it and returns a fresh, usable sandbox instead.
func TestPool_TakeSkipsStale(t *testing.T) {
	now := time.Unix(3_000_000, 0)
	p := &Pool{
		cfg:   PoolConfig{Size: 2, Mode: ModeHost, WarmTTL: time.Minute},
		slots: make(chan parkedSandbox, 2),
		nowFn: fixedClock(&now),
	}
	stale := NewHost(nil)
	p.slots <- parkedSandbox{sb: stale, parkedAt: now.Add(-5 * time.Minute)}

	sb, cleanup, err := p.Take()
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	defer cleanup()
	if sb == nil {
		t.Fatal("Take returned a nil sandbox")
	}
	if sb == stale {
		t.Fatal("Take handed out the stale sandbox instead of skipping it")
	}
	ctx := context.Background()
	if _, err := stale.RunBash(ctx, BashRequest{Command: "echo x"}); !errors.Is(err, ErrClosed) {
		t.Errorf("skipped stale sandbox should be closed; RunBash err = %v", err)
	}
	if _, err := sb.RunBash(ctx, BashRequest{Command: "echo ok"}); err != nil {
		t.Errorf("Take's returned sandbox must be usable; RunBash err = %v", err)
	}
	p.Close()
}

// TestPool_KeeperLifecycle pins that the TTL keeper goroutine is started only
// when a positive WarmTTL is configured, and stopped by Close.
func TestPool_KeeperLifecycle(t *testing.T) {
	withTTL := NewPool(PoolConfig{Size: 2, Mode: ModeHost, WarmTTL: time.Minute})
	if withTTL.done == nil {
		t.Error("expected a keeper (done channel) when WarmTTL > 0")
	}
	withTTL.Close() // must not panic / deadlock (closes done + slots)

	noTTL := NewPool(PoolConfig{Size: 2, Mode: ModeHost, WarmTTL: 0})
	if noTTL.done != nil {
		t.Error("no keeper should run when WarmTTL == 0")
	}
	noTTL.Close()
}

// TestPool_ConcurrentTakeKeeperClose stresses the pool under -race: a real
// short-TTL keeper reaps while many goroutines Take/cleanup, then Close races in.
// It asserts no panic / data race / deadlock (the keeper draining the channel
// concurrently with Take and Close is the delicate part of #181).
func TestPool_ConcurrentTakeKeeperClose(t *testing.T) {
	p := NewPool(PoolConfig{Size: 4, Mode: ModeHost, WarmTTL: 40 * time.Millisecond})

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				sb, cleanup, err := p.Take()
				if err == nil && sb != nil {
					_, _ = sb.RunBash(context.Background(), BashRequest{Command: "true"})
				}
				cleanup()
			}
		}()
	}
	time.Sleep(200 * time.Millisecond) // let the keeper tick a few times
	close(stop)
	wg.Wait()

	// After the stress, the pool must still hand out a usable sandbox (the keeper
	// reaping under load must not have left it broken).
	sb, cleanup, err := p.Take()
	if err != nil || sb == nil {
		t.Fatalf("pool unusable after concurrent stress: sb=%v err=%v", sb, err)
	}
	cleanup()

	p.Close()
	p.Close() // idempotent
}
