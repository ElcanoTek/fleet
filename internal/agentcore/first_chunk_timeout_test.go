// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package agentcore

import (
	"errors"
	"testing"
	"time"

	"charm.land/fantasy"
)

// The first-chunk watchdog was a flat 30 s regardless of prompt size, so a
// ~115K-token prompt on a slower provider timed out twice and swapped the run
// to its fallback model in the audit tail (Reklaim 6bd0c212). The deadline now
// scales with the previous step's prompt size and both ends are knobs (#1537).
// The base is 75 s since #1585: a reasoning model's hidden thinking can run
// past 30 s before its first visible token, on a route that streams no
// reasoning to prove the model is alive.
func TestFirstChunkTimeoutScalesWithPromptAndClamps(t *testing.T) {
	p := EnvPrefix(CanonicalEnvPrefix)
	t.Setenv("FLEET_PROVIDER_FIRST_CHUNK_TIMEOUT_SECONDS", "")
	t.Setenv("FLEET_PROVIDER_FIRST_CHUNK_TIMEOUT_MAX_SECONDS", "")

	if got := firstChunkTimeoutFor(p, 0); got != 75*time.Second {
		t.Fatalf("unknown prompt size must yield the 75 s base, got %s", got)
	}
	if got := firstChunkTimeoutFor(p, 9_999); got != 75*time.Second {
		t.Fatalf("under 10K tokens adds nothing, got %s", got)
	}
	if got := firstChunkTimeoutFor(p, 115_000); got != 97*time.Second {
		t.Fatalf("115K tokens = 75 s + 11×2 s = 97 s, got %s", got)
	}
	if got := firstChunkTimeoutFor(p, 2_000_000); got != 180*time.Second {
		t.Fatalf("a huge prompt must clamp to the 180 s default cap, got %s", got)
	}
	if got := firstChunkTimeoutFor(p, -5); got != 75*time.Second {
		t.Fatalf("a negative estimate is treated as unknown, got %s", got)
	}

	t.Setenv("FLEET_PROVIDER_FIRST_CHUNK_TIMEOUT_SECONDS", "45")
	t.Setenv("FLEET_PROVIDER_FIRST_CHUNK_TIMEOUT_MAX_SECONDS", "60")
	if got := firstChunkTimeoutFor(p, 0); got != 45*time.Second {
		t.Fatalf("base knob not honoured, got %s", got)
	}
	if got := firstChunkTimeoutFor(p, 200_000); got != 60*time.Second {
		t.Fatalf("cap knob not honoured, got %s", got)
	}
	// A cap below the base can never shorten the base wait.
	t.Setenv("FLEET_PROVIDER_FIRST_CHUNK_TIMEOUT_MAX_SECONDS", "10")
	if got := firstChunkTimeoutFor(p, 500_000); got != 45*time.Second {
		t.Fatalf("cap below base must be lifted to the base, got %s", got)
	}
	// The floor protects against a knob that would make every call a blip.
	t.Setenv("FLEET_PROVIDER_FIRST_CHUNK_TIMEOUT_SECONDS", "1")
	t.Setenv("FLEET_PROVIDER_FIRST_CHUNK_TIMEOUT_MAX_SECONDS", "")
	if got := firstChunkTimeoutFor(p, 0); got != minFirstChunkTimeout {
		t.Fatalf("base below the floor must be lifted to %s, got %s", minFirstChunkTimeout, got)
	}
	// Unparseable falls back to the default rather than failing the turn.
	t.Setenv("FLEET_PROVIDER_FIRST_CHUNK_TIMEOUT_SECONDS", "soon")
	if got := firstChunkTimeoutFor(p, 0); got != 75*time.Second {
		t.Fatalf("unparseable knob must fall back to the default, got %s", got)
	}
}

// The watchdog's error still classifies as a stream blip (errors.Is on the
// sentinel) and carries the deadline and prompt size the retry log and the
// turn.retry event report.
func TestFirstChunkTimeoutErrorClassifiesAsBlipAndCarriesDetail(t *testing.T) {
	cause := errors.New("stream closed")
	err := error(&firstChunkTimeoutError{timeout: 52 * time.Second, promptTokens: 115_000, cause: cause})
	if !errors.Is(err, ErrFirstChunkTimeout) || !errors.Is(err, cause) {
		t.Fatalf("watchdog error must unwrap to the sentinel and its cause: %v", err)
	}
	if class, _ := classifyStreamError(err); class != streamErrorStreamBlip {
		t.Fatalf("watchdog error classified as %v, want stream blip", class)
	}
	timeout, tokens, ok := firstChunkTimeoutDetail(err)
	if !ok || timeout != 52*time.Second || tokens != 115_000 {
		t.Fatalf("detail = (%s, %d, %v), want (52s, 115000, true)", timeout, tokens, ok)
	}
	if _, _, ok := firstChunkTimeoutDetail(cause); ok {
		t.Fatal("a plain error must carry no watchdog detail")
	}
}

// The status belongs to the LAST provider error. Fantasy passes nil for a
// retryable transport failure, and a cached 429 would then have the card call
// a connection reset a rate limit (#1585).
func TestWatchdogProviderErrorStatusTracksTheLatestCallback(t *testing.T) {
	w := newFirstChunkWatchdog(time.Hour, func() {})
	defer w.stop()

	w.noteProviderError(&fantasy.ProviderError{StatusCode: 429}, time.Minute)
	if !w.providerErrorLive() || w.providerErrStatus.Load() != 429 {
		t.Fatalf("status = %d, want 429", w.providerErrStatus.Load())
	}
	// A transport error carries no status: the 429 must not linger.
	w.noteProviderError(nil, time.Minute)
	if !w.providerErrorLive() {
		t.Errorf("a transport error is still a provider error")
	}
	if got := w.providerErrStatus.Load(); got != 0 {
		t.Fatalf("stale status survived a statusless retry: %d", got)
	}
	// An out-of-range code is not a status worth reporting either.
	w.noteProviderError(&fantasy.ProviderError{StatusCode: 99_999}, time.Minute)
	if got := w.providerErrStatus.Load(); got != 0 {
		t.Fatalf("out-of-range status was stored: %d", got)
	}
}

// The record explains the silence of the backoff it bought, not the whole
// round. Two quick 429s followed by an attempt that reasons silently past the
// deadline is a silent model, and must not be reported as rate limiting
// (#1585).
func TestWatchdogProviderErrorExpiresWithItsBackoff(t *testing.T) {
	w := newFirstChunkWatchdog(time.Hour, func() {})
	defer w.stop()

	// A retry whose backoff has since elapsed: the next attempt is under way,
	// so its silence is the model's own. A zero delay is enough to show it —
	// the record covers the sleep it bought and not a moment more.
	w.noteProviderError(&fantasy.ProviderError{StatusCode: 429}, 0)
	if w.providerErrorLive() {
		t.Fatalf("a lapsed provider-error record still explained the silence")
	}
	if seen, status := w.providerErrorAtExpiry(); seen || status != 0 {
		t.Fatalf("nothing expired yet, so the snapshot must be empty: seen=%v status=%d", seen, status)
	}

	// Inside the backoff it is the explanation.
	w.noteProviderError(&fantasy.ProviderError{StatusCode: 503}, time.Minute)
	if !w.providerErrorLive() || w.providerErrStatus.Load() != 503 {
		t.Fatalf("inside its backoff the record must stand: seen=%v status=%d",
			w.providerErrorLive(), w.providerErrStatus.Load())
	}

	// A watchdog that never saw one reports nothing.
	fresh := newFirstChunkWatchdog(time.Hour, func() {})
	defer fresh.stop()
	if fresh.providerErrorLive() {
		t.Fatalf("a watchdog with no provider error must report none")
	}
}

// The verdict is formed when the timer wins, so the provider-error state is
// snapshotted THERE. Reading the live record afterwards — the caller reads it
// once the cancelled stream has unwound — could miss a record that lapsed in
// between and put "the model never started" on a card for a provider that was
// throttling us (#1585).
func TestWatchdogSnapshotsProviderErrorWhenItFires(t *testing.T) {
	fired := make(chan struct{})
	w := newFirstChunkWatchdog(10*time.Millisecond, func() { close(fired) })
	defer w.stop()

	// A backoff that is still running when the watchdog fires.
	w.noteProviderError(&fantasy.ProviderError{StatusCode: 429}, time.Second)
	<-fired

	// Let the record lapse, exactly as an unwinding stream would.
	w.providerErrUntil.Store(time.Now().Add(-time.Millisecond).UnixNano())
	if w.providerErrorLive() {
		t.Fatalf("the live record should have lapsed by now")
	}
	seen, status := w.providerErrorAtExpiry()
	if !seen || status != 429 {
		t.Fatalf("the snapshot must survive the unwind: seen=%v status=%d", seen, status)
	}

	// A watchdog that fires with no provider error in play reports none.
	quiet := newFirstChunkWatchdog(time.Millisecond, func() {})
	defer quiet.stop()
	time.Sleep(20 * time.Millisecond)
	if seen, status := quiet.providerErrorAtExpiry(); seen || status != 0 {
		t.Fatalf("a silent model must not acquire a provider error: seen=%v status=%d", seen, status)
	}
}
