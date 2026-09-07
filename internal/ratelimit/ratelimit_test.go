package ratelimit

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNewConcurrencyLimiter_ClampsRange(t *testing.T) {
	// A value past int32 saturates to MaxInt32 (the guarded narrowing conversion
	// must not overflow); a negative value floors to 0 (disabled).
	if got := NewConcurrencyLimiter(math.MaxInt32 + 1).Limit(); got != math.MaxInt32 {
		t.Fatalf("over-range limit = %d, want %d", got, int32(math.MaxInt32))
	}
	if got := NewConcurrencyLimiter(-5).Limit(); got != 0 {
		t.Fatalf("negative limit = %d, want 0", got)
	}
	if got := NewConcurrencyLimiter(7).Limit(); got != 7 {
		t.Fatalf("in-range limit = %d, want 7", got)
	}
}

func TestLimiter_AllowsUpToLimit(t *testing.T) {
	l := New(3, 0)
	for i := 0; i < 3; i++ {
		ok, _ := l.Allow("u@x.com")
		if !ok {
			t.Fatalf("request %d blocked unexpectedly", i+1)
		}
	}
	ok, retry := l.Allow("u@x.com")
	if ok {
		t.Fatal("4th request should be blocked")
	}
	if retry <= 0 {
		t.Error("retry-after should be positive")
	}
}

func TestLimiter_PerKeyIsolated(t *testing.T) {
	l := New(1, 0)
	ok, _ := l.Allow("alice@x.com")
	if !ok {
		t.Fatal("alice first should pass")
	}
	ok, _ = l.Allow("alice@x.com")
	if ok {
		t.Fatal("alice second should block")
	}
	ok, _ = l.Allow("bob@x.com")
	if !ok {
		t.Fatal("bob first should pass (isolated from alice)")
	}
}

func TestLimiter_Disabled(t *testing.T) {
	// Zero values disable both windows.
	l := New(0, 0)
	for i := 0; i < 1000; i++ {
		ok, _ := l.Allow("u@x.com")
		if !ok {
			t.Fatalf("disabled limiter blocked at %d", i)
		}
	}
}

func TestLimiter_DailyCap(t *testing.T) {
	// per-minute disabled, per-day cap = 2
	l := New(0, 2)
	_, _ = l.Allow("u@x.com")
	_, _ = l.Allow("u@x.com")
	ok, _ := l.Allow("u@x.com")
	if ok {
		t.Fatal("daily cap should block 3rd request")
	}
}

func TestLimiter_Nil(t *testing.T) {
	// Defensive: a nil *Limiter should allow everything.
	var l *Limiter
	ok, _ := l.Allow("u@x.com")
	if !ok {
		t.Fatal("nil limiter should allow")
	}
}

func TestLimiter_PerMinuteAccessor(t *testing.T) {
	if got := New(60, 500).PerMinute(); got != 60 {
		t.Errorf("PerMinute() = %d, want 60", got)
	}
	var l *Limiter
	if got := l.PerMinute(); got != 0 {
		t.Errorf("nil PerMinute() = %d, want 0", got)
	}
}

func TestLimiter_Snapshot(t *testing.T) {
	l := New(5, 0)
	limit, remaining, _ := l.Snapshot("u")
	if limit != 5 || remaining != 5 {
		t.Fatalf("fresh snapshot = (%d,%d), want (5,5)", limit, remaining)
	}
	l.Allow("u")
	l.Allow("u")
	limit, remaining, reset := l.Snapshot("u")
	if limit != 5 || remaining != 3 {
		t.Errorf("after 2 calls: (%d,%d), want (5,3)", limit, remaining)
	}
	if reset <= 0 {
		t.Errorf("reset should be a future unix time, got %d", reset)
	}
	// Snapshot must not itself consume budget.
	if _, r2, _ := l.Snapshot("u"); r2 != 3 {
		t.Errorf("snapshot consumed budget: remaining %d, want 3", r2)
	}
}

func TestConcurrencyLimiter_AcquireRelease(t *testing.T) {
	c := NewConcurrencyLimiter(2)
	if !c.Acquire("u") {
		t.Fatal("first acquire should succeed")
	}
	if !c.Acquire("u") {
		t.Fatal("second acquire should succeed")
	}
	if c.Acquire("u") {
		t.Fatal("third acquire should fail at limit 2")
	}
	if got := c.Active("u"); got != 2 {
		t.Errorf("Active = %d, want 2", got)
	}
	c.Release("u")
	if !c.Acquire("u") {
		t.Fatal("acquire after release should succeed")
	}
	// Isolation: a different key has its own budget.
	if !c.Acquire("other") {
		t.Fatal("other key should have its own slots")
	}
}

func TestConcurrencyLimiter_Disabled(t *testing.T) {
	c := NewConcurrencyLimiter(0) // disabled
	for i := 0; i < 100; i++ {
		if !c.Acquire("u") {
			t.Fatalf("disabled limiter blocked at %d", i)
		}
	}
	var nilC *ConcurrencyLimiter
	if !nilC.Acquire("u") {
		t.Fatal("nil limiter should allow")
	}
}

func TestConcurrencyLimiter_ReleaseNeverNegative(t *testing.T) {
	c := NewConcurrencyLimiter(1)
	c.Release("u") // release without acquire
	if got := c.Active("u"); got != 0 {
		t.Errorf("Active = %d, want 0 (release must not go negative)", got)
	}
	if !c.Acquire("u") {
		t.Fatal("acquire after spurious release should still work")
	}
}

// entryCount reports how many keys the limiter currently tracks — test-only
// visibility into the eviction behavior (#594).
func (c *ConcurrencyLimiter) entryCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.counts)
}

// TestConcurrencyLimiter_EvictsReleasedKeys is the #594 regression guard: a key
// whose last holder released must not linger in the map, or a long-lived
// process accumulates one entry per user that ever started a chat turn.
func TestConcurrencyLimiter_EvictsReleasedKeys(t *testing.T) {
	c := NewConcurrencyLimiter(2)
	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("user%d@example.com", i)
		if !c.Acquire(key) {
			t.Fatalf("acquire %q failed", key)
		}
		c.Release(key)
	}
	if got := c.entryCount(); got != 0 {
		t.Fatalf("map retains %d zero-count entries, want 0", got)
	}

	// A key with a REMAINING holder must survive its sibling's release …
	for i := 0; i < 2; i++ {
		if !c.Acquire("held") {
			t.Fatalf("acquire held #%d", i+1)
		}
	}
	c.Release("held")
	if got := c.Active("held"); got != 1 {
		t.Fatalf("Active(held) = %d, want 1 (must not evict a live holder)", got)
	}
	// … and be evicted only when the last holder releases.
	c.Release("held")
	if got := c.entryCount(); got != 0 {
		t.Fatalf("map retains %d entries after final release, want 0", got)
	}
}

// TestConcurrencyLimiter_ConcurrentChurn hammers acquire/release across many
// goroutines and distinct keys under -race: the store/delete transition must
// neither leak zero-count entries nor under-count a concurrent holder (the
// classic race a lock-free delete-on-zero would have).
func TestConcurrencyLimiter_ConcurrentChurn(t *testing.T) {
	c := NewConcurrencyLimiter(2)

	// One long-lived holder pins a slot on a contended key for the whole test.
	if !c.Acquire("hot") {
		t.Fatal("acquire hot")
	}

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				// Churn the contended key: the second slot is fought over while
				// the pinned holder must never be evicted or double-counted.
				if c.Acquire("hot") {
					c.Release("hot")
				}
				// And churn a per-goroutine key that must fully evict.
				key := fmt.Sprintf("churn-%d-%d", g, i%10)
				if c.Acquire(key) {
					c.Release(key)
				}
			}
		}(g)
	}
	wg.Wait()

	if got := c.Active("hot"); got != 1 {
		t.Errorf("Active(hot) = %d, want 1 (under/over-count on the store/delete race)", got)
	}
	if got := c.entryCount(); got != 1 {
		t.Errorf("map holds %d entries, want 1 (only the pinned key)", got)
	}
	c.Release("hot")
	if got := c.entryCount(); got != 0 {
		t.Errorf("map holds %d entries after final release, want 0", got)
	}
}

func TestLimiter_KeysCountsBuckets(t *testing.T) {
	l := New(5, 0)
	if got := l.Keys(); got != 0 {
		t.Fatalf("fresh limiter Keys = %d, want 0", got)
	}
	l.Allow("a")
	l.Allow("a")
	l.Allow("b")
	if got := l.Keys(); got != 2 {
		t.Errorf("Keys = %d, want 2 (one bucket per distinct key)", got)
	}
	var nilL *Limiter
	if got := nilL.Keys(); got != 0 {
		t.Errorf("nil limiter Keys = %d, want 0", got)
	}
}

// TestLimiter_SweepDoesNotLoseAnInFlightCount pins the lost-update fix: an
// AllowN that fetched a bucket the sweep then evicts must not count into the
// orphan. The race is simulated by holding a pre-sweep pointer, forcing the
// sweep, then counting through the public path and checking the count landed
// in the LIVE bucket.
func TestLimiter_SweepDoesNotLoseAnInFlightCount(t *testing.T) {
	l := New(5, 0)
	// A stale bucket: its newest sample is older than the day window.
	stale := &bucket{dayTimestamps: []int64{time.Now().Unix() - 2*86400}}
	l.keys["k"] = stale
	l.lastSweep.Store(0) // sweep is due

	// The sweep evicts the stale bucket and flags it.
	l.maybeSweep()
	if _, ok := l.keys["k"]; ok {
		t.Fatal("stale bucket survived the sweep")
	}
	stale.mu.Lock()
	evicted := stale.evicted
	stale.mu.Unlock()
	if !evicted {
		t.Fatal("evicted bucket not flagged; a holder of the old pointer would count into an orphan")
	}

	// A request after the eviction is counted in the live bucket, not lost.
	if ok, _ := l.AllowN("k", 5, 0); !ok {
		t.Fatal("first request after sweep refused")
	}
	if _, remaining, _ := l.Snapshot("k"); remaining != 4 {
		t.Fatalf("remaining = %d, want 4: the post-sweep request was not counted", remaining)
	}
	if l.Keys() != 1 {
		t.Fatalf("keys = %d, want 1", l.Keys())
	}
}

// TestLimiter_SweepIsCheapOnTheHotPath: a due sweep runs exactly once per
// interval, and the not-due check is lock-free (asserted indirectly: a burst of
// AllowN with a fresh lastSweep never resets it).
func TestLimiter_SweepIsCheapOnTheHotPath(t *testing.T) {
	l := New(1000, 0)
	now := time.Now().Unix()
	l.lastSweep.Store(now)
	for i := 0; i < 100; i++ {
		l.Allow("k")
	}
	if got := l.lastSweep.Load(); got != now {
		t.Fatalf("lastSweep moved to %d during a not-due window", got)
	}
	l.lastSweep.Store(0)
	l.Allow("k")
	if got := l.lastSweep.Load(); got < now {
		t.Fatal("a due sweep did not stamp lastSweep")
	}
}

// TestLimiter_ConcurrentAllowWithSweeps runs AllowN across many keys while
// sweeps are repeatedly forced, under -race, and checks the admitted count
// per key is exact — no lost update, no double count.
func TestLimiter_ConcurrentAllowWithSweeps(t *testing.T) {
	const keys, perKey = 8, 50
	l := New(perKey, 0)
	var wg sync.WaitGroup
	var admitted atomic.Int64
	for k := 0; k < keys; k++ {
		key := fmt.Sprintf("k%d", k)
		for i := 0; i < perKey+10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				l.lastSweep.Store(0) // force a sweep attempt on nearly every call
				if ok, _ := l.AllowN(key, perKey, 0); ok {
					admitted.Add(1)
				}
			}()
		}
	}
	wg.Wait()
	if got := admitted.Load(); got != keys*perKey {
		t.Fatalf("admitted %d, want exactly %d", got, keys*perKey)
	}
}

// TestLimiter_SnapshotNUsesPerCallBound: headers for a key admitted under a
// per-key cap must reflect that cap, not the instance default.
func TestLimiter_SnapshotNUsesPerCallBound(t *testing.T) {
	l := New(100, 0)
	l.AllowN("k", 3, 0)
	limit, remaining, _ := l.SnapshotN("k", 3)
	if limit != 3 || remaining != 2 {
		t.Fatalf("SnapshotN = (%d, %d), want (3, 2)", limit, remaining)
	}
	if limit, _, _ := l.Snapshot("k"); limit != 100 {
		t.Fatalf("Snapshot limit = %d, want the instance default 100", limit)
	}
	if limit, remaining, _ := l.SnapshotN("k", 0); limit != 0 || remaining != 0 {
		t.Fatalf("SnapshotN(0) = (%d, %d), want disabled (0, 0)", limit, remaining)
	}
}
