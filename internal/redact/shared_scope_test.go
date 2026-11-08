package redact

import (
	"strings"
	"testing"
	"time"
)

// A literal held by two rotating scopes (a client secret shared by two
// hosted-MCP rows) stays live while either still holds it: one scope
// rotating away from it does not start its grace clock, and only when the
// last holder lets go does the window begin (#2074 review).
func TestRedactor_SharedLiteralSurvivesOneScopesRotation(t *testing.T) {
	clock := newFakeClock()
	r := NewRedactor(nil)
	r.now = clock.now
	const shared = "sharedClientSecret-9Wq2Lx"
	r.RegisterSecrets("remotemcp:a", true, shared, "accessA1-Zk81Qw02")
	r.RegisterSecrets("remotemcp:b", true, shared, "accessB1-Lm73Pq55")

	// Scope a rotates and no longer lists the shared secret.
	r.RegisterSecrets("remotemcp:a", true, "accessA2-Xc44Vb19")
	clock.advance(literalRetireGrace + time.Minute)
	if got := r.Redact("saw " + shared); strings.Contains(got, shared) {
		t.Fatalf("a literal scope b still holds was retired by scope a's rotation: %q", got)
	}
	if got := r.Redact("saw accessA1-Zk81Qw02"); !strings.Contains(got, "accessA1-Zk81Qw02") {
		t.Fatalf("scope a's superseded access token outlived its window: %q", got)
	}

	// Now scope b lets go too: the window starts, and ends.
	r.RegisterSecrets("remotemcp:b", true, "accessB2-Rt66Yu20")
	clock.advance(literalRetireGrace - time.Minute)
	if got := r.Redact("saw " + shared); strings.Contains(got, shared) {
		t.Fatalf("retired before its own window ended: %q", got)
	}
	clock.advance(2 * time.Minute)
	if got := r.Redact("saw " + shared); !strings.Contains(got, shared) {
		t.Fatalf("the shared literal outlived the last holder's window: %q", got)
	}
}
