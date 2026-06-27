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
