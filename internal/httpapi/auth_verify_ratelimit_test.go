package httpapi

import (
	"testing"
	"time"
)

// Every /auth/verify attempt burns a bcrypt, pre-login, so the endpoint needs
// its own limiter: per attempted email (online brute force of one account)
// and global (email rotation as a CPU DoS).
func TestVerifyLimiter_PerEmailAndGlobal(t *testing.T) {
	var l verifyLimiter
	now := time.Unix(1_000_000, 0)

	for i := 0; i < verifyPerEmailPerMinute; i++ {
		if !l.allow("a@x.com", now) {
			t.Fatalf("attempt %d for one email blocked early", i+1)
		}
	}
	if l.allow("a@x.com", now) {
		t.Fatal("per-email cap not enforced")
	}
	// A different email is still admitted (per-email, not global, tripped).
	if !l.allow("b@x.com", now) {
		t.Fatal("unrelated email blocked by another email's cap")
	}

	// Rotating emails runs into the global cap.
	var g verifyLimiter
	for i := 0; g.allow(string(rune('a'+i%26))+"@x.com", now) && i < 1000; i++ {
		if i > verifyGlobalPerMinute {
			t.Fatal("global cap not enforced")
		}
	}

	// The next window resets both.
	if !l.allow("a@x.com", now.Add(time.Minute)) {
		t.Fatal("window did not reset")
	}
}
