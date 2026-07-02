package ratelimit

import (
	"fmt"
	"sync"
	"testing"
)

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
