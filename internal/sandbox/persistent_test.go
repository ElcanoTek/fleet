package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"
)

// newPersistentTestPool builds a persistent-mode pool wired to the host backend
// with no warm slots (every create cold-starts a NewHost sandbox) and a fake
// clock, so the lifecycle (reuse / release / idle-reap / cap) is exercised
// deterministically without podman or a real kernel. The bridge script is nil
// — the persistent machinery only ever runs bash ("true") liveness probes here,
// which the host backend serves without python.
func newPersistentTestPool(now *time.Time, idleTTL time.Duration, maxSessions int) *Pool {
	return &Pool{
		cfg: PoolConfig{
			Mode:                  ModeHost,
			PersistentREPL:        true,
			PersistentIdleTTL:     idleTTL,
			PersistentMaxSessions: maxSessions,
		},
		persistent: make(map[string]*persistentEntry),
		nowFn:      func() time.Time { return *now },
	}
}

func TestTakePersistent_ReusesPerConversation(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	p := newPersistentTestPool(&now, time.Hour, 0)
	defer p.Close()

	sb1, release1, err := p.TakePersistent("conv-A")
	if err != nil {
		t.Fatalf("TakePersistent A#1: %v", err)
	}
	release1() // turn ends; sandbox must NOT be closed (persistent)

	sb2, release2, err := p.TakePersistent("conv-A")
	if err != nil {
		t.Fatalf("TakePersistent A#2: %v", err)
	}
	defer release2()
	if sb1 != sb2 {
		t.Fatal("same conversation must reuse the same sandbox across turns")
	}
	// The reused sandbox is still alive after the first turn's release.
	if _, err := sb2.RunBash(context.Background(), BashRequest{Command: "true"}); err != nil {
		t.Fatalf("reused persistent sandbox must stay usable: %v", err)
	}

	sbB, releaseB, err := p.TakePersistent("conv-B")
	if err != nil {
		t.Fatalf("TakePersistent B: %v", err)
	}
	defer releaseB()
	if sbB == sb1 {
		t.Fatal("different conversations must NOT share a sandbox (isolation invariant)")
	}
}

func TestTakePersistent_DisabledFallsBackToPerTurn(t *testing.T) {
	// PersistentREPL off → TakePersistent behaves like Take: the cleanup closes.
	p := &Pool{cfg: PoolConfig{Mode: ModeHost}}
	defer p.Close()
	sb, cleanup, err := p.TakePersistent("conv-A")
	if err != nil {
		t.Fatalf("TakePersistent: %v", err)
	}
	cleanup()
	if _, err := sb.RunBash(context.Background(), BashRequest{Command: "true"}); !errors.Is(err, ErrClosed) {
		t.Errorf("per-turn fallback cleanup must close the sandbox; RunBash err = %v", err)
	}
	// An empty conversation ID also falls back to per-turn.
	sb2, cleanup2, err := p.TakePersistent("")
	if err != nil {
		t.Fatalf("TakePersistent empty convID: %v", err)
	}
	cleanup2()
	if _, err := sb2.RunBash(context.Background(), BashRequest{Command: "true"}); !errors.Is(err, ErrClosed) {
		t.Errorf("empty-convID fallback must close the sandbox; RunBash err = %v", err)
	}
}

func TestReleaseChatSession_ClosesAndRecreates(t *testing.T) {
	now := time.Unix(2_000_000, 0)
	p := newPersistentTestPool(&now, time.Hour, 0)
	defer p.Close()

	sb1, release1, _ := p.TakePersistent("conv-A")
	release1()

	p.ReleaseChatSession("conv-A")
	if _, err := sb1.RunBash(context.Background(), BashRequest{Command: "true"}); !errors.Is(err, ErrClosed) {
		t.Errorf("ReleaseChatSession must close the conversation's sandbox; RunBash err = %v", err)
	}
	if live, _ := p.PersistentStats(); live != 0 {
		t.Errorf("after release, live persistent sandboxes = %d, want 0", live)
	}

	// A subsequent take for the same conversation builds a fresh one.
	sb2, release2, err := p.TakePersistent("conv-A")
	if err != nil {
		t.Fatalf("TakePersistent after release: %v", err)
	}
	defer release2()
	if sb2 == sb1 {
		t.Fatal("after release a new sandbox must be created, not the closed one")
	}
}

func TestReleaseChatSession_DefersCloseWhileInUse(t *testing.T) {
	now := time.Unix(3_000_000, 0)
	p := newPersistentTestPool(&now, time.Hour, 0)
	defer p.Close()

	sb, release, _ := p.TakePersistent("conv-A") // borrow held (inUse=1)

	// Delete the conversation mid-turn: the close must be DEFERRED, not forced,
	// so the running turn doesn't lose its sandbox out from under it.
	p.ReleaseChatSession("conv-A")
	if _, err := sb.RunBash(context.Background(), BashRequest{Command: "true"}); err != nil {
		t.Fatalf("sandbox must stay usable while a turn still borrows it: %v", err)
	}

	// Last borrow release triggers the deferred close.
	release()
	if _, err := sb.RunBash(context.Background(), BashRequest{Command: "true"}); !errors.Is(err, ErrClosed) {
		t.Errorf("deferred close must fire on last borrow release; RunBash err = %v", err)
	}
}

func TestReapIdlePersistent_ClosesIdleKeepsBusyAndFresh(t *testing.T) {
	now := time.Unix(4_000_000, 0)
	p := newPersistentTestPool(&now, time.Minute, 0)
	defer p.Close()

	// idle: borrowed then released long ago.
	idleSb, idleRelease, _ := p.TakePersistent("conv-idle")
	idleRelease()
	// busy: still borrowed (inUse=1) — must survive the reap.
	busySb, busyRelease, _ := p.TakePersistent("conv-busy")
	defer busyRelease()

	// Advance the clock past the idle TTL and reap.
	now = now.Add(2 * time.Minute)
	// fresh: taken AFTER the clock advance, so it is within TTL.
	freshSb, freshRelease, _ := p.TakePersistent("conv-fresh")
	defer freshRelease()

	p.reapIdlePersistent()

	ctx := context.Background()
	if _, err := idleSb.RunBash(ctx, BashRequest{Command: "true"}); !errors.Is(err, ErrClosed) {
		t.Errorf("idle-past-TTL sandbox must be reaped; RunBash err = %v", err)
	}
	if _, err := busySb.RunBash(ctx, BashRequest{Command: "true"}); err != nil {
		t.Errorf("in-use sandbox must NOT be reaped even past TTL; RunBash err = %v", err)
	}
	if _, err := freshSb.RunBash(ctx, BashRequest{Command: "true"}); err != nil {
		t.Errorf("within-TTL sandbox must survive the reap; RunBash err = %v", err)
	}
}

func TestTakePersistent_MaxSessionsLRUEviction(t *testing.T) {
	now := time.Unix(5_000_000, 0)
	p := newPersistentTestPool(&now, time.Hour, 2) // cap = 2
	defer p.Close()

	// Create three idle sessions, each at a distinct, increasing lastUsed.
	sbA, relA, _ := p.TakePersistent("conv-A")
	relA()
	now = now.Add(time.Second)
	sbB, relB, _ := p.TakePersistent("conv-B")
	relB()
	now = now.Add(time.Second)
	// Creating the third over a cap of 2 must evict the LRU idle one (conv-A).
	sbC, relC, _ := p.TakePersistent("conv-C")
	defer relC()

	// Eviction Close runs async (safe.Go) — give it a moment to settle.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := sbA.RunBash(context.Background(), BashRequest{Command: "true"}); errors.Is(err, ErrClosed) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("LRU (conv-A) was not evicted after exceeding the session cap")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// conv-B and conv-C must survive.
	if _, err := sbB.RunBash(context.Background(), BashRequest{Command: "true"}); err != nil {
		t.Errorf("conv-B (newer) must survive eviction; RunBash err = %v", err)
	}
	if _, err := sbC.RunBash(context.Background(), BashRequest{Command: "true"}); err != nil {
		t.Errorf("conv-C (newest) must survive eviction; RunBash err = %v", err)
	}
}

func TestTakePersistent_RecreatesDeadSandbox(t *testing.T) {
	now := time.Unix(6_000_000, 0)
	p := newPersistentTestPool(&now, time.Hour, 0)
	defer p.Close()

	sb1, rel1, _ := p.TakePersistent("conv-A")
	rel1()
	// Simulate the container dying between turns (OOM-kill, host reap).
	sb1.Close()

	sb2, rel2, err := p.TakePersistent("conv-A")
	if err != nil {
		t.Fatalf("TakePersistent after death: %v", err)
	}
	defer rel2()
	if sb2 == sb1 {
		t.Fatal("a dead persistent sandbox must be recreated, not handed back")
	}
	if _, err := sb2.RunBash(context.Background(), BashRequest{Command: "true"}); err != nil {
		t.Errorf("recreated sandbox must be usable: %v", err)
	}
}

func TestPoolClose_DrainsPersistent(t *testing.T) {
	now := time.Unix(7_000_000, 0)
	p := newPersistentTestPool(&now, time.Hour, 0)

	sbA, relA, _ := p.TakePersistent("conv-A")
	relA()
	sbB, relB, _ := p.TakePersistent("conv-B")
	relB()

	p.Close()

	ctx := context.Background()
	for name, sb := range map[string]*Sandbox{"conv-A": sbA, "conv-B": sbB} {
		if _, err := sb.RunBash(ctx, BashRequest{Command: "true"}); !errors.Is(err, ErrClosed) {
			t.Errorf("%s sandbox must be closed by Pool.Close; RunBash err = %v", name, err)
		}
	}
	p.Close() // idempotent
}
