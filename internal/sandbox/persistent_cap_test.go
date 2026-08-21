package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"
)

// newCapPool builds a persistent-mode Pool with no background keeper, so the
// tests drive reapIdlePersistent themselves and nothing races them.
func newCapPool(maxSessions int, idleTTL time.Duration, now *time.Time) *Pool {
	return &Pool{
		cfg: PoolConfig{
			Mode:                  ModeHost,
			PersistentREPL:        true,
			PersistentMaxSessions: maxSessions,
			PersistentIdleTTL:     idleTTL,
		},
		persistent: make(map[string]*persistentEntry),
		nowFn:      fixedClock(now),
	}
}

// seatEntry installs a persistent entry directly, the way TakePersistent would,
// with an explicit lastUsed and borrow count.
func seatEntry(p *Pool, convID string, lastUsed time.Time, inUse int) *persistentEntry {
	e := &persistentEntry{sb: NewHost(nil), convID: convID, lastUsed: lastUsed, inUse: inUse}
	p.persistent[convID] = e
	return e
}

func closed(sb *Sandbox) bool {
	_, err := sb.RunBash(context.Background(), BashRequest{Command: "echo x"})
	return errors.Is(err, ErrClosed)
}

// The regression this locks in: eviction used to run on the CREATE path only.
// A burst that overshot the cap while every other session was mid-turn stayed
// overshot, because nothing revisited the decision once those turns finished.
// Since the cap is what bounds container memory, the reaper must enforce it.
func TestReapIdlePersistentEnforcesCap(t *testing.T) {
	now := time.Unix(3_000_000, 0)
	// A long idle TTL so nothing is reaped for age — the cap alone must act.
	p := newCapPool(2, time.Hour, &now)

	oldest := seatEntry(p, "conv-a", now.Add(-30*time.Minute), 0)
	middle := seatEntry(p, "conv-b", now.Add(-20*time.Minute), 0)
	newest := seatEntry(p, "conv-c", now.Add(-1*time.Minute), 0)

	p.reapIdlePersistent()

	if got := len(p.persistent); got != 2 {
		t.Fatalf("live sessions = %d, want 2 (the cap)", got)
	}
	if _, still := p.persistent["conv-a"]; still {
		t.Error("the least-recently-used session should have been evicted")
	}
	for _, keep := range []string{"conv-b", "conv-c"} {
		if _, ok := p.persistent[keep]; !ok {
			t.Errorf("%s should have survived — it is not the LRU", keep)
		}
	}
	// Close runs in a goroutine (safe.Go) so the lock isn't held across a
	// podman teardown; give it a moment before asserting.
	waitFor(t, func() bool { return closed(oldest.sb) }, "evicted sandbox should be closed")
	if closed(middle.sb) || closed(newest.sb) {
		t.Error("a surviving session's sandbox must stay usable")
	}
	p.Close()
}

// A session with a turn in flight is never evicted: pulling the sandbox out
// from under a running turn would destroy work. The cap stays SOFT, so the live
// count may exceed it while those turns run.
func TestReapIdlePersistentNeverEvictsBusySessions(t *testing.T) {
	now := time.Unix(3_000_000, 0)
	p := newCapPool(1, time.Hour, &now)

	busyOld := seatEntry(p, "conv-busy", now.Add(-time.Hour), 1) // LRU, but mid-turn
	idleNew := seatEntry(p, "conv-idle", now.Add(-time.Minute), 0)

	p.reapIdlePersistent()

	if _, ok := p.persistent["conv-busy"]; !ok {
		t.Fatal("a session with a turn in flight must never be evicted, even as LRU")
	}
	if closed(busyOld.sb) {
		t.Fatal("a busy session's sandbox must not be closed")
	}
	if _, ok := p.persistent["conv-idle"]; ok {
		t.Error("the idle session should have been evicted to honour the cap")
	}
	waitFor(t, func() bool { return closed(idleNew.sb) }, "evicted idle sandbox should be closed")

	// Now the turn finishes and the next tick corrects the overshoot — the
	// property the create-path-only eviction lacked.
	p.persistentMu.Lock()
	busyOld.inUse = 0
	p.persistentMu.Unlock()
	seatEntry(p, "conv-second", now, 0)

	p.reapIdlePersistent()
	if got := len(p.persistent); got != 1 {
		t.Fatalf("live sessions = %d after the busy turn finished, want 1 (the cap self-corrects)", got)
	}
	p.Close()
}

// Age-based reaping and the cap must both apply on one pass, and a zero TTL
// (age reaping disabled) must not disable the cap with it.
func TestReapIdlePersistentTTLAndCapTogether(t *testing.T) {
	now := time.Unix(3_000_000, 0)
	p := newCapPool(2, 10*time.Minute, &now)

	seatEntry(p, "conv-ancient", now.Add(-time.Hour), 0) // past the TTL
	seatEntry(p, "conv-old", now.Add(-9*time.Minute), 0) // inside the TTL, LRU of the rest
	seatEntry(p, "conv-mid", now.Add(-5*time.Minute), 0) // inside the TTL
	seatEntry(p, "conv-new", now.Add(-1*time.Minute), 0) // inside the TTL

	p.reapIdlePersistent()

	if _, ok := p.persistent["conv-ancient"]; ok {
		t.Error("a session idle past the TTL must be reaped for age")
	}
	if got := len(p.persistent); got != 2 {
		t.Fatalf("live sessions = %d, want 2 (TTL reap then cap)", got)
	}
	if _, ok := p.persistent["conv-old"]; ok {
		t.Error("after the TTL reap, the LRU survivor should be evicted to reach the cap")
	}
	p.Close()

	// TTL disabled, cap still enforced.
	p2 := newCapPool(1, 0, &now)
	seatEntry(p2, "conv-x", now.Add(-time.Hour), 0)
	seatEntry(p2, "conv-y", now, 0)
	p2.reapIdlePersistent()
	if got := len(p2.persistent); got != 1 {
		t.Fatalf("live sessions = %d with TTL=0, want 1 — a zero idle TTL must not disable the cap", got)
	}
	p2.Close()
}

// A zero cap means "unbounded", the documented meaning of the knob.
func TestReapIdlePersistentZeroCapIsUnbounded(t *testing.T) {
	now := time.Unix(3_000_000, 0)
	p := newCapPool(0, time.Hour, &now)
	for _, id := range []string{"a", "b", "c", "d"} {
		seatEntry(p, id, now, 0)
	}
	p.reapIdlePersistent()
	if got := len(p.persistent); got != 4 {
		t.Fatalf("live sessions = %d, want 4 — a zero cap disables eviction", got)
	}
	p.Close()
}

// waitFor polls cond briefly. Used for the eviction Close, which runs off the
// lock in a goroutine.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error(msg)
}
