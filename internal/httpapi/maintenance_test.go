package httpapi

import (
	"sync"
	"testing"
	"time"
)

// withMinInterval swaps the package-level gate width for one test and restores
// it afterwards, so the tests exercise the gate without depending on the
// production five-minute default.
func withMinInterval(t *testing.T, d time.Duration) {
	t.Helper()
	prev := maintenanceMinInterval
	maintenanceMinInterval = d
	t.Cleanup(func() { maintenanceMinInterval = prev })
}

func TestClaimMaintenanceSlotFirstCallAlwaysWins(t *testing.T) {
	withMinInterval(t, time.Hour)
	var s Server

	// Zero timestamp means "never swept": a short-lived process must still
	// reclaim once rather than wait out a full interval it will never see.
	if !s.claimMaintenanceSlot(time.Now()) {
		t.Fatal("the first claim after boot must be granted")
	}
}

func TestClaimMaintenanceSlotRateLimits(t *testing.T) {
	withMinInterval(t, time.Hour)
	var s Server

	start := time.Now()
	if !s.claimMaintenanceSlot(start) {
		t.Fatal("first claim should be granted")
	}
	if s.claimMaintenanceSlot(start.Add(time.Minute)) {
		t.Fatal("a claim inside the interval must be refused")
	}
	if !s.claimMaintenanceSlot(start.Add(time.Hour + time.Second)) {
		t.Fatal("a claim past the interval must be granted")
	}
}

// The gate exists to stop a stampede, so the concurrent case is the one that
// matters: N turns finishing at the same instant all read the same stale
// timestamp, and a load-then-store check would let every one of them through.
func TestClaimMaintenanceSlotConcurrentSingleWinner(t *testing.T) {
	withMinInterval(t, time.Hour)
	var s Server

	const goroutines = 64
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		granted int
	)
	start := make(chan struct{})
	now := time.Now()
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if s.claimMaintenanceSlot(now) {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if granted != 1 {
		t.Fatalf("%d goroutines were granted the slot; exactly 1 must win", granted)
	}
}

// A non-positive interval restores the pre-gate behaviour (every turn sweeps)
// for operators who want it back.
func TestClaimMaintenanceSlotDisabledGate(t *testing.T) {
	withMinInterval(t, 0)
	var s Server

	now := time.Now()
	for i := range 3 {
		if !s.claimMaintenanceSlot(now) {
			t.Fatalf("claim %d refused with the gate disabled", i)
		}
	}
}

// The ticker in cmd/fleet reports its passes through NoteMaintenanceRun so the
// two drivers share one notion of "recently swept" instead of doing twice the
// work.
func TestNoteMaintenanceRunSuppressesPostTurnPass(t *testing.T) {
	withMinInterval(t, time.Hour)
	var s Server

	now := time.Now()
	s.NoteMaintenanceRun(now)
	if s.claimMaintenanceSlot(now.Add(time.Minute)) {
		t.Fatal("a post-turn claim right after a ticker pass must be refused")
	}
	if !s.claimMaintenanceSlot(now.Add(2 * time.Hour)) {
		t.Fatal("a post-turn claim well past the interval must be granted")
	}
}
