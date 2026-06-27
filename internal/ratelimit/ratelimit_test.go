package ratelimit

import (
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
