package httpapi

import (
	"sync"
	"time"

	"github.com/ElcanoTek/fleet/internal/safe"
)

// backgroundTracker owns the detached work that deliberately outlives the request
// that started it: the queue-drain re-kick timer, memory-graph extraction, the
// retained-buffer eviction timer, approval push sends.
//
// activeTurns already covers detached *turns*, and DrainTurns blocks on it at
// shutdown. Everything here is the work that is not a turn, and it was tracked by
// nothing at all. Two of them touch the store, which is how a finished test leaked
// into the next one: a `time.AfterFunc` scheduled 2s out fired long after
// t.Cleanup closed the store, and the log filled with `input queue claim: sql:
// database is closed` from a goroutine no one was waiting for. The same gap let a
// shutdown return while a memory-graph extraction was still mid-write.
//
// The zero value is ready to use, which matters because tests build Server as a
// struct literal — a pointer field here would be nil in dozens of them.
type backgroundTracker struct {
	mu     sync.Mutex
	wg     sync.WaitGroup
	timers map[uint64]*time.Timer
	seq    uint64
	// stopped makes StopAndWait final: work submitted after it would re-populate
	// the set the caller just drained, so late arrivals are dropped instead.
	stopped bool
}

// Go runs fn as tracked detached work. It is a no-op once StopAndWait has run —
// during shutdown the right move for background work is to skip it, not to extend
// the drain. fn is panic-guarded so a background fault cannot take the process
// down, matching the treatment of the detached turn goroutine.
func (b *backgroundTracker) Go(name string, fn func()) {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return
	}
	b.wg.Add(1)
	b.mu.Unlock()

	go func() {
		defer b.wg.Done()
		defer safe.Recover(name, nil)
		fn()
	}()
}

// After schedules fn to run once after d, tracked and cancellable. Unlike a bare
// time.AfterFunc, StopAndWait can cancel it before it fires and will block on it
// if it already has.
func (b *backgroundTracker) After(name string, d time.Duration, fn func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		return
	}
	if b.timers == nil {
		b.timers = make(map[uint64]*time.Timer)
	}
	b.seq++
	id := b.seq
	b.wg.Add(1)
	// AfterFunc does not run fn on this goroutine, so registering under the lock is
	// safe: the callback's own Lock simply waits for the unlock below.
	b.timers[id] = time.AfterFunc(d, func() {
		defer b.wg.Done()
		defer safe.Recover(name, nil)
		b.mu.Lock()
		delete(b.timers, id)
		b.mu.Unlock()
		fn()
	})
}

// StopAndWait cancels every pending timer and blocks until work already running
// has returned. Idempotent, and final: later submissions are dropped.
func (b *backgroundTracker) StopAndWait() {
	b.mu.Lock()
	b.stopped = true
	timers := make([]*time.Timer, 0, len(b.timers))
	for id, t := range b.timers {
		timers = append(timers, t)
		delete(b.timers, id)
	}
	b.mu.Unlock()

	for _, t := range timers {
		// Stop reporting true means the callback will never run, so its wg.Done
		// never happens and this stands in for it. False means it is already
		// running (or done) and will settle its own count, which Wait then covers.
		if t.Stop() {
			b.wg.Done()
		}
	}
	b.wg.Wait()
}

// StopBackground cancels and drains the server's detached non-turn work. Call it
// after DrainTurns during shutdown: a turn's completion tail can schedule a drain
// re-kick, so draining turns first and background second settles both. Tests call
// it before closing the store, which is the whole point — a fixture that closes
// the store while this work is in flight is what made an unrelated package's
// TRUNCATE deadlock look like a mystery.
func (s *Server) StopBackground() { s.background.StopAndWait() }
