package httpapi

import (
	"testing"
)

func TestRateLimiter_AllowsUpToLimit(t *testing.T) {
	r := newRateLimiter(3, 0)
	for i := 0; i < 3; i++ {
		ok, _ := r.allow("u@x.com")
		if !ok {
			t.Fatalf("request %d blocked unexpectedly", i+1)
		}
	}
	ok, retry := r.allow("u@x.com")
	if ok {
		t.Fatal("4th request should be blocked")
	}
	if retry <= 0 {
		t.Error("retry-after should be positive")
	}
}

func TestRateLimiter_PerUserIsolated(t *testing.T) {
	r := newRateLimiter(1, 0)
	ok, _ := r.allow("alice@x.com")
	if !ok {
		t.Fatal("alice first should pass")
	}
	ok, _ = r.allow("alice@x.com")
	if ok {
		t.Fatal("alice second should block")
	}
	ok, _ = r.allow("bob@x.com")
	if !ok {
		t.Fatal("bob first should pass (isolated from alice)")
	}
}

func TestRateLimiter_Disabled(t *testing.T) {
	// Zero values disable both windows.
	r := newRateLimiter(0, 0)
	for i := 0; i < 1000; i++ {
		ok, _ := r.allow("u@x.com")
		if !ok {
			t.Fatalf("disabled limiter blocked at %d", i)
		}
	}
}

func TestRateLimiter_DailyCap(t *testing.T) {
	// per-minute disabled, per-day cap = 2
	r := newRateLimiter(0, 2)
	_, _ = r.allow("u@x.com")
	_, _ = r.allow("u@x.com")
	ok, _ := r.allow("u@x.com")
	if ok {
		t.Fatal("daily cap should block 3rd request")
	}
}

func TestRateLimiter_Nil(t *testing.T) {
	// Defensive: a nil *rateLimiter should allow everything.
	var r *rateLimiter
	ok, _ := r.allow("u@x.com")
	if !ok {
		t.Fatal("nil limiter should allow")
	}
}
