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
// container: it reaps it (asynchronously since #1124) and returns a fresh,
// usable sandbox instead.
func TestPool_TakeSkipsStale(t *testing.T) {
	now := time.Unix(3_000_000, 0)
	p := &Pool{
		cfg:   PoolConfig{Size: 2, Mode: ModeHost, WarmTTL: time.Minute},
		slots: make(chan parkedSandbox, 2),
		nowFn: fixedClock(&now),
	}
	stale := NewHost(nil)
	p.slots <- parkedSandbox{sb: stale, parkedAt: now.Add(-5 * time.Minute)}

	sb, cleanup, err := p.Take(context.Background())
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
	// The reap is async (#1124), so poll for the close rather than assert it
	// happened before Take returned.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := stale.RunBash(ctx, BashRequest{Command: "echo x"}); errors.Is(err, ErrClosed) {
			break
		}
		if time.Now().After(deadline) {
			t.Error("skipped stale sandbox was never closed")
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := sb.RunBash(ctx, BashRequest{Command: "echo ok"}); err != nil {
		t.Errorf("Take's returned sandbox must be usable; RunBash err = %v", err)
	}
	p.Close()
}

// slowCloseImpl is a backend stub whose close() blocks until released — the
// unit stand-in for a podman teardown that takes seconds.
type slowCloseImpl struct {
	unblock chan struct{}
	closed  chan struct{}
}

func (s *slowCloseImpl) runBash(context.Context, BashRequest) (BashResult, error) {
	return BashResult{}, nil
}
func (s *slowCloseImpl) runPython(context.Context, PythonRequest) (PythonResult, error) {
	return PythonResult{Status: "success"}, nil
}
func (s *slowCloseImpl) runFileOp(context.Context, FileOpRequest) (FileOpResult, error) {
	return FileOpResult{}, nil
}
func (s *slowCloseImpl) bindFileOpRoot(context.Context, string) (FileOpRootIdentity, error) {
	return FileOpRootIdentity{Dev: 1, Ino: 1}, nil
}
func (s *slowCloseImpl) resourceUsage() (ResourceUsageSummary, bool) {
	return ResourceUsageSummary{}, false
}
func (s *slowCloseImpl) poisoned() bool { return false }
func (s *slowCloseImpl) close() {
	<-s.unblock
	close(s.closed)
}

// TestPool_TakeReapsStaleAsynchronously pins the #1124 latency fix: a stale
// warm container's teardown (up to ~10s of podman kill in production) must not
// run on the turn's critical path. The stale sandbox here BLOCKS in close()
// until the test releases it; Take must still return the fresh sandbox
// promptly.
func TestPool_TakeReapsStaleAsynchronously(t *testing.T) {
	now := time.Unix(4_000_000, 0)
	p := &Pool{
		cfg:   PoolConfig{Size: 2, Mode: ModeHost, WarmTTL: time.Minute},
		slots: make(chan parkedSandbox, 2),
		nowFn: fixedClock(&now),
	}
	slow := &slowCloseImpl{unblock: make(chan struct{}), closed: make(chan struct{})}
	released := false
	release := func() {
		if !released {
			released = true
			close(slow.unblock)
		}
	}
	defer release() // whatever happens, let the reap goroutine finish
	p.slots <- parkedSandbox{sb: &Sandbox{mode: ModeHost, impl: slow}, parkedAt: now.Add(-5 * time.Minute)}
	fresh := NewHost(nil)
	p.slots <- parkedSandbox{sb: fresh, parkedAt: now}

	type takeResult struct {
		sb      *Sandbox
		cleanup func()
		err     error
	}
	done := make(chan takeResult, 1)
	go func() {
		sb, cleanup, err := p.Take(context.Background())
		done <- takeResult{sb, cleanup, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("Take: %v", res.err)
		}
		defer res.cleanup()
		if res.sb != fresh {
			t.Errorf("Take returned %p, want the fresh warm sandbox", res.sb)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Take blocked on the stale sandbox's teardown — the reap must be asynchronous")
	}

	// The reap still happens once the (slow) teardown completes.
	select {
	case <-slow.closed:
		t.Fatal("stale close finished before it was released — the blocking seam is broken")
	default:
	}
	release()
	select {
	case <-slow.closed:
	case <-time.After(5 * time.Second):
		t.Error("stale sandbox was never closed after Take")
	}
}

// TestPool_TakeThreadsCallerContext pins the #1124 ctx plumbing: the caller's
// ctx must reach cold-start container construction (observed here at the
// storage-opt probe seam, the first ctx consumer inside newSandbox) instead of
// the pool's background FillCtx — that is what lets a cancelled turn stop
// paying container spin-up. The podman binary is deliberately nonexistent so
// construction fails fast after the probe; the assertion is the plumbing, not
// the (podman-gated) construction itself.
func TestPool_TakeThreadsCallerContext(t *testing.T) {
	type probeCtxKey struct{}
	sawMarker := make(chan any, 1)
	p := &Pool{
		cfg: PoolConfig{
			Mode: ModeContainer,
			Container: ContainerConfig{
				Image:        "example.test/sandbox:none",
				PodmanBinary: "/nonexistent/podman-for-ctx-test",
				DiskLimitGB:  1, // >0 so the probe seam runs
			},
		},
		nowFn: time.Now,
		storageProbeFn: func(ctx context.Context, _, _ string) (supported, conclusive bool) {
			var marker any
			if ctx != nil {
				marker = ctx.Value(probeCtxKey{})
			}
			sawMarker <- marker
			return false, true
		},
	}
	ctx := context.WithValue(context.Background(), probeCtxKey{}, "turn-ctx")
	if _, _, err := p.Take(ctx); err == nil {
		t.Fatal("Take should fail without a podman binary")
	}
	select {
	case marker := <-sawMarker:
		if marker != "turn-ctx" {
			t.Errorf("cold start ran under ctx marker %v, want the caller's %q", marker, "turn-ctx")
		}
	default:
		t.Fatal("storage probe never ran")
	}
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
// short-TTL keeper reaps while many goroutines Take/cleanup and Close races
// those callers. It asserts no panic / data race / deadlock and pins the closed
// pool as a fail-closed lifecycle boundary.
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
				sb, cleanup, err := p.Take(context.Background())
				if errors.Is(err, ErrClosed) {
					cleanup()
					return
				}
				if err == nil && sb != nil {
					_, _ = sb.RunBash(context.Background(), BashRequest{Command: "true"})
				}
				cleanup()
			}
		}()
	}
	time.Sleep(200 * time.Millisecond) // let the keeper tick a few times

	// Close while workers are still taking, rather than stopping them first.
	// They must converge on ErrClosed instead of cold-starting replacements.
	p.Close()
	close(stop)
	wg.Wait()

	sb, cleanup, err := p.Take(context.Background())
	if !errors.Is(err, ErrClosed) || sb != nil {
		t.Fatalf("take after close: sb=%v err=%v, want nil/ErrClosed", sb, err)
	}
	cleanup()

	p.Close() // idempotent
}
