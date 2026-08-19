package httpapi

import (
	"sync/atomic"
	"testing"
	"time"
)

// A pending timer must be cancelled, not waited out. The retained-buffer eviction
// timer is scheduled 15 minutes ahead by default, so a StopAndWait that waited for
// its timers instead of stopping them would turn every shutdown into a 15-minute
// one.
func TestBackgroundTracker_StopCancelsPendingTimer(t *testing.T) {
	var bg backgroundTracker
	var ran atomic.Bool
	bg.After("test.pending", time.Hour, func() { ran.Store(true) })

	done := make(chan struct{})
	go func() { bg.StopAndWait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StopAndWait blocked on a pending timer instead of cancelling it")
	}
	if ran.Load() {
		t.Error("cancelled timer still ran")
	}
}

// The other half: work that has already started must be waited for, because that
// is the case where letting the caller proceed means closing the store underneath
// a live writer.
func TestBackgroundTracker_StopWaitsForRunningWork(t *testing.T) {
	var bg backgroundTracker
	started := make(chan struct{})
	release := make(chan struct{})
	var finished atomic.Bool

	bg.Go("test.running", func() {
		close(started)
		<-release
		finished.Store(true)
	})
	<-started

	stopped := make(chan struct{})
	go func() { bg.StopAndWait(); close(stopped) }()

	select {
	case <-stopped:
		t.Fatal("StopAndWait returned while background work was still running")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("StopAndWait did not return after the work finished")
	}
	if !finished.Load() {
		t.Error("work did not complete")
	}
}

// Stop has to be final. Work admitted afterwards would re-populate the set the
// caller just drained, so a late submission during shutdown is dropped rather
// than extending the drain indefinitely.
func TestBackgroundTracker_DropsWorkSubmittedAfterStop(t *testing.T) {
	var bg backgroundTracker
	bg.StopAndWait()

	var ran atomic.Bool
	bg.Go("test.late-go", func() { ran.Store(true) })
	bg.After("test.late-after", time.Millisecond, func() { ran.Store(true) })

	// A second StopAndWait must also stay prompt (idempotent) and must not block
	// on anything the calls above might have registered.
	done := make(chan struct{})
	go func() { bg.StopAndWait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StopAndWait is not idempotent")
	}

	time.Sleep(50 * time.Millisecond)
	if ran.Load() {
		t.Error("work submitted after StopAndWait ran anyway")
	}
}

// A panic in detached work must not take the process down, matching the treatment
// of the detached turn goroutine, and must still settle its own count so
// StopAndWait cannot hang on it.
func TestBackgroundTracker_RecoversPanicAndStillSettles(t *testing.T) {
	var bg backgroundTracker
	bg.Go("test.panicking", func() { panic("boom") })

	done := make(chan struct{})
	go func() { bg.StopAndWait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StopAndWait hung after background work panicked")
	}
}

// The property the fixture teardown depends on, and the one that made cancelling
// turns insufficient on its own: once the server is draining, a completed turn's
// tail must not launch the next queued row. Without this, one cancellation starts
// a fresh turn and the chain re-arms itself indefinitely.
func TestMaybeDrainQueueDeclinesWhileDraining(t *testing.T) {
	s := &Server{inflight: make(map[string]inflightEntry)}
	s.BeginShutdown()
	if !s.IsDraining() {
		t.Fatal("BeginShutdown did not mark the server draining")
	}
	// store is nil: reaching ClaimNextQueuedInput would nil-panic, so returning
	// from this call at all is the assertion that the drain stopped at the gate.
	s.maybeDrainQueue("conv-1")
}
