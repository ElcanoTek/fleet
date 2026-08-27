// #1274: scoped rotation, generation retirement and the grace window — the
// bounded-memory half of runtime-secret redaction. These tests sit beside the
// #1124 literal tests in redact_test.go and extend that package's
// AddLiteral/Redact concurrency contract to retirement.
//
// Every credential-shaped string below is an obviously fake placeholder. No
// test in this package may carry a real token (gitleaks gates CI, and a
// realistic-looking fixture is a leak waiting to be copied).
package redact

import (
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock drives literalRetireGrace deterministically. Atomic because the
// concurrency test advances it from one goroutine while Redact reads it from
// others.
type fakeClock struct{ nanos atomic.Int64 }

func newFakeClock() *fakeClock {
	c := &fakeClock{}
	c.nanos.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	return c
}

func (c *fakeClock) now() time.Time { return time.Unix(0, c.nanos.Load()) }

func (c *fakeClock) advance(d time.Duration) { c.nanos.Add(int64(d)) }

// rotationFixtures are the placeholder access/refresh pair one simulated OAuth
// refresh mints.
func rotationFixtures(gen int) (access, refresh string) {
	return fmt.Sprintf("placeholder-access-value-gen-%04d", gen),
		fmt.Sprintf("placeholder-refresh-value-gen-%04d", gen)
}

// TestRedactor_ScopedRotationPlateaus is the #1274 acceptance test. It replays
// the exact registration sequence remotemcp performs per refresh — the stored
// refresh token + client secret join the CURRENT generation before the token
// request, then the fresh pair opens a NEW one — and pins all three properties:
// the scan set plateaus instead of growing per rotation, the CURRENT
// credentials are always redacted, and a just-rotated-out credential stays
// redacted for the whole grace window and only then leaves the set.
func TestRedactor_ScopedRotationPlateaus(t *testing.T) {
	clock := newFakeClock()
	r := NewRedactor(nil)
	r.now = clock.now

	const scope = "remotemcp:server-1"
	const clientSecret = "placeholder-client-secret-fixture"
	const rotations = 200

	var prevAccess, prevRefresh string
	var plateau int
	for gen := 1; gen <= rotations; gen++ {
		access, refresh := rotationFixtures(gen)
		if prevRefresh == "" {
			r.AddScopedLiterals(scope, clientSecret)
		} else {
			r.AddScopedLiterals(scope, clientSecret, prevRefresh)
		}
		r.RotateScopedLiterals(scope, clientSecret, access, refresh)

		// The live generation is always scrubbed.
		for _, live := range []string{clientSecret, access, refresh} {
			if got := r.Redact("upstream quoted " + live); strings.Contains(got, live) {
				t.Fatalf("gen %d: live secret not redacted: %q", gen, got)
			}
		}

		if prevAccess != "" {
			// Inside the grace window a rotated-out secret is STILL scrubbed:
			// an in-flight request that was using it can still fail and echo it.
			clock.advance(literalRetireGrace - time.Minute)
			if got := r.Redact("stale error quoted " + prevAccess); strings.Contains(got, prevAccess) {
				t.Fatalf("gen %d: rotated-out secret dropped inside the grace window: %q", gen, got)
			}
			// Past it, the superseded generation leaves the scan set.
			clock.advance(2 * time.Minute)
			if got := r.Redact("stale error quoted " + prevAccess); !strings.Contains(got, prevAccess) {
				t.Fatalf("gen %d: superseded access value still scanned for after the grace window: %q", gen, got)
			}
			if got := r.Redact("stale error quoted " + prevRefresh); !strings.Contains(got, prevRefresh) {
				t.Fatalf("gen %d: superseded refresh value still scanned for after the grace window: %q", gen, got)
			}
		}

		// Steady state is the live set only: client secret + access + refresh.
		count := r.LiteralCount()
		switch {
		case gen == 1: // nothing has been superseded yet
		case plateau == 0:
			plateau = count
		case count != plateau:
			t.Fatalf("gen %d: literal count %d drifted from the plateau of %d — retirement is not bounding the scan set", gen, count, plateau)
		}
		prevAccess, prevRefresh = access, refresh
	}
	if plateau != 3 {
		t.Errorf("plateau = %d literals, want 3 (client secret + live access + live refresh)", plateau)
	}
}

// TestRedactor_RetirementIsScopedAndSparesPermanentLiterals pins the safety
// direction of retirement: it may only ever drop a value the SAME scope has
// superseded. Another server's literals and boot-time permanent literals are
// untouched, and a value that is both a boot secret and a scope's credential
// stays permanent.
func TestRedactor_RetirementIsScopedAndSparesPermanentLiterals(t *testing.T) {
	clock := newFakeClock()
	r := NewRedactor(nil)
	r.now = clock.now

	const bootSecret = "placeholder-boot-env-secret-value"
	const shared = "placeholder-shared-with-connector-value"
	r.AddLiteral(bootSecret)
	r.AddLiteral(shared)

	const scopeA, scopeB = "remotemcp:server-a", "remotemcp:server-b"
	accessA1, refreshA1 := rotationFixtures(1)
	accessB1, refreshB1 := "placeholder-access-value-server-b", "placeholder-refresh-value-server-b"
	r.RotateScopedLiterals(scopeA, accessA1, refreshA1, shared)
	r.RotateScopedLiterals(scopeB, accessB1, refreshB1)

	// Server A rotates; server B does not.
	accessA2, refreshA2 := rotationFixtures(2)
	r.RotateScopedLiterals(scopeA, accessA2, refreshA2, shared)
	clock.advance(literalRetireGrace + time.Minute)

	for _, keep := range []string{bootSecret, shared, accessA2, refreshA2, accessB1, refreshB1} {
		if got := r.Redact("saw " + keep); strings.Contains(got, keep) {
			t.Errorf("still-live literal was retired: %q", got)
		}
	}
	for _, gone := range []string{accessA1, refreshA1} {
		if got := r.Redact("saw " + gone); !strings.Contains(got, gone) {
			t.Errorf("superseded literal %q outlived the grace window", gone)
		}
	}
}

// TestRedactor_FastRotationHitsGenerationCap covers the backstop: when a scope
// rotates far faster than the grace window (a refresh storm), the generation
// cap keeps the scan set bounded, and the live pair is never what gets dropped.
func TestRedactor_FastRotationHitsGenerationCap(t *testing.T) {
	clock := newFakeClock()
	r := NewRedactor(nil)
	r.now = clock.now
	const scope = "remotemcp:storm"
	const rotations = 100

	var access, refresh string
	for gen := 1; gen <= rotations; gen++ {
		access, refresh = rotationFixtures(gen)
		r.RotateScopedLiterals(scope, access, refresh)
		clock.advance(time.Second) // 900x faster than literalRetireGrace
	}
	if n := r.LiteralCount(); n > 2*maxScopeGenerations {
		t.Errorf("literal count = %d after %d fast rotations, want <= %d (generation cap)", n, rotations, 2*maxScopeGenerations)
	}
	for _, live := range []string{access, refresh} {
		if got := r.Redact("upstream quoted " + live); strings.Contains(got, live) {
			t.Errorf("the generation cap dropped a LIVE secret: %q", got)
		}
	}
}

// TestRedactor_ConcurrentRotationRedact extends the #1124 concurrency contract
// to retirement (#1274): rotations on several scopes, a clock another goroutine
// advances (so sweeps fire mid-flight) and concurrent Redact calls must be
// race-free, and every scope's final live pair must still be scrubbed.
func TestRedactor_ConcurrentRotationRedact(t *testing.T) {
	clock := newFakeClock()
	r := NewRedactor(nil)
	r.now = clock.now

	const scopes, rounds = 4, 150
	fixture := func(s, gen int) (string, string) {
		return fmt.Sprintf("placeholder-access-%d-%04d", s, gen),
			fmt.Sprintf("placeholder-refresh-%d-%04d", s, gen)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for s := 0; s < scopes; s++ {
		wg.Add(2)
		go func(s int) {
			defer wg.Done()
			<-start
			scope := fmt.Sprintf("remotemcp:server-%d", s)
			for gen := 1; gen <= rounds; gen++ {
				access, refresh := fixture(s, gen)
				r.AddScopedLiterals(scope, access)
				r.RotateScopedLiterals(scope, access, refresh)
			}
		}(s)
		go func(s int) {
			defer wg.Done()
			<-start
			for gen := 1; gen <= rounds; gen++ {
				access, _ := fixture(s, gen)
				_ = r.Redact("error text quoting " + access)
				_ = r.LiteralCount()
			}
		}(s)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < rounds; i++ {
			clock.advance(30 * time.Second)
		}
	}()
	close(start)
	wg.Wait()

	for s := 0; s < scopes; s++ {
		access, refresh := fixture(s, rounds)
		for _, live := range []string{access, refresh} {
			if got := r.Redact("final check " + live); strings.Contains(got, live) {
				t.Errorf("scope %d: live secret not redacted after concurrent rotation: %q", s, got)
			}
		}
	}
}
