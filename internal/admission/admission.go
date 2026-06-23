// Package admission is the process-wide concurrency governor for agent turns. It
// is the single box-wide bound the README sizing table assumes: the TOTAL number
// of agent turns in flight at once — interactive chat AND scheduled tasks
// combined — never exceeds the configured cap (FLEET_MAX_CONCURRENT_AGENTS).
//
// Interactive chat is prioritized over background work. A number of slots are
// RESERVED for interactive turns: scheduled tasks may hold at most
// (total - reserved) slots, so even when the scheduler is saturated the reserved
// slots stay available to a human-facing chat turn — background jobs can never
// starve chat. Interactive turns themselves may use the whole pool (they are not
// limited to the reserve), so chat bursts to full capacity when the scheduler is
// idle and is guaranteed a floor when it is not.
//
// The limiter is shared: cmd/fleet constructs ONE Limiter and hands it to both
// the interactive Manager (which admits chat turns) and the scheduled worker pool
// (which admits tasks). One box, one bound.
package admission

import "sync"

// Limiter bounds concurrently-running agent turns process-wide.
//
//   - all:   one slot per in-flight turn of EITHER kind; cap = total.
//   - sched: scheduled turns additionally hold one of these; cap = total-reserved.
//
// Because a scheduled turn must hold both an `all` slot and a `sched` slot, and
// there are only total-reserved `sched` slots, scheduled concurrency is bounded
// below total and can never consume the `reserved` interactive headroom.
type Limiter struct {
	all   chan struct{}
	sched chan struct{}
}

// New builds a Limiter for `total` concurrent turns with `reserved` of them held
// back for interactive chat. total is floored at 1; reserved is clamped to
// [0, total-1] so at least one slot is always schedulable.
func New(total, reserved int) *Limiter {
	if total < 1 {
		total = 1
	}
	if reserved < 0 {
		reserved = 0
	}
	if reserved > total-1 {
		reserved = total - 1
	}
	return &Limiter{
		all:   make(chan struct{}, total),
		sched: make(chan struct{}, total-reserved),
	}
}

// DefaultReserved is the conventional interactive reserve for a given cap: about
// a quarter of the slots (so an 8-cap box reserves 2 for chat, a 32-cap box 8).
// Clamped by New for small caps, where it resolves to 0 (chat and scheduled share
// the whole pool).
func DefaultReserved(total int) int { return total / 4 }

// AcquireInteractive admits an interactive (chat) turn, waiting until a slot is
// free or `done` is closed — pass a context.Context's Done() channel with a short
// admit deadline so a saturated box yields a fast "at capacity" signal rather than
// a hung turn. Returns a release func (call once when the turn ends) and true on
// success; nil and false if `done` fired first. Interactive turns draw only from
// the shared pool, so they may use every slot, including the reserved ones.
func (l *Limiter) AcquireInteractive(done <-chan struct{}) (func(), bool) {
	select {
	case l.all <- struct{}{}:
		return releaseOnce(func() { <-l.all }), true
	case <-done:
		return nil, false
	}
}

// TryAcquireScheduled admits a scheduled task without blocking: it takes a
// schedulable slot (capped at total-reserved) and a shared slot, or returns false
// immediately if either is unavailable. The worker pool calls this in its claim
// loop, leaving over-cap work pending. Returns a release func (call once after the
// task finishes) and true on success.
func (l *Limiter) TryAcquireScheduled() (func(), bool) {
	select {
	case l.sched <- struct{}{}:
	default:
		return nil, false
	}
	select {
	case l.all <- struct{}{}:
		return releaseOnce(func() { <-l.all; <-l.sched }), true
	default:
		<-l.sched // give back the schedulable slot we just took
		return nil, false
	}
}

// Total is the box-wide cap (max in-flight turns of any kind).
func (l *Limiter) Total() int { return cap(l.all) }

// SchedulableSlots is the max number of scheduled tasks that may run at once
// (total - reserved).
func (l *Limiter) SchedulableSlots() int { return cap(l.sched) }

// InFlight reports the number of slots currently held (both modes). For
// observability and tests.
func (l *Limiter) InFlight() int { return len(l.all) }

// releaseOnce wraps a release in sync.Once so a double-call can't under-count the
// pool by draining a slot a different holder still owns.
func releaseOnce(fn func()) func() {
	var once sync.Once
	return func() { once.Do(fn) }
}
