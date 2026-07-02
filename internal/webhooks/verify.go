// Package webhooks holds the shared inbound-webhook authentication primitives —
// the HMAC / signing-secret verification and the timing-equalization dummy
// secret — used by BOTH the orchestrator's task-trigger endpoint
// (POST /triggers/{slug}, issue #190/#177) and the chat server's
// conversation-trigger endpoint (POST /webhooks/{slug}, issue #268).
//
// There is deliberately ONE copy of this security-critical verification: an
// inbound webhook proves its authenticity by a signature the caller computes
// over the raw request body under a shared secret, and a subtle mistake in that
// path (a non-constant-time compare, an unbounded replay window) must not be
// able to exist in one endpoint but not the other. Both inbound paths import
// this package rather than maintaining parallel implementations that could
// drift.
package webhooks

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// NewDummySecret returns a fresh, per-process random 32-byte hex HMAC key for
// the slug-miss path. Callers cache the result in a package var and feed it to
// VerifyHMACSHA256 when an inbound slug does not match a configured trigger, so
// the unknown-slug path still performs one HMAC-SHA256 before failing closed —
// making it timing-indistinguishable from a known slug with a bad signature and
// so preventing slug enumeration.
//
// Its value is never security-load-bearing (it only ever produces a
// guaranteed-failing comparison), but sourcing it from crypto/rand makes it
// unguessable regardless. crypto/rand should never fail; a fixed fallback keeps
// the package loadable and still yields a failing comparison.
func NewDummySecret() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "fleet-dummy-webhook-secret"
	}
	return hex.EncodeToString(buf)
}

// fallbackSecret is the package-internal HMAC key substituted when a caller
// passes an empty secret: verification must still fail closed, but the
// full-body HMAC is computed anyway so a trigger whose secret env var is unset
// costs the same work as one with a wrong signature (timing equalization,
// #572). Per-process random, never comparable to anything an attacker sends.
var fallbackSecret = NewDummySecret()

// bodyHMACs counts full-body HMAC computations across both verifiers. It is a
// TEST SEAM (#572): the no-enumeration guarantee ("an unknown slug and a bad
// signature perform the same signature work over the body") is asserted
// STRUCTURALLY by diffing this counter across request shapes, instead of a
// flaky wall-clock timing test. One atomic add per verification is the entire
// runtime cost.
var bodyHMACs atomic.Uint64

// BodyHMACComputations reports how many full-body HMAC computations the package
// has performed. Tests diff it around a request to assert timing-equalization
// structurally; it has no production callers.
func BodyHMACComputations() uint64 { return bodyHMACs.Load() }

// VerifyHMACSHA256 reports whether sigHeader is a valid HMAC-SHA256 of body
// under secret (GitHub-style). It accepts the optional "sha256=" prefix, is
// case-insensitive on the hex, and compares in constant time. An empty secret
// or a malformed signature fails closed.
//
// Timing equalization (#572): the full-body HMAC is computed FIRST,
// unconditionally — before the secret and signature-shape checks — so an
// absent/malformed header or an unset secret costs the same body-proportional
// work as a wrong signature. Only fixed-cost checks on the (already computed)
// result differ between outcomes, keeping the caller's slug-miss path
// indistinguishable from a bad signature.
func VerifyHMACSHA256(body []byte, secret, sigHeader string) bool {
	ok := true
	if secret == "" {
		secret = fallbackSecret
		ok = false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	bodyHMACs.Add(1)
	expected := hex.EncodeToString(mac.Sum(nil))
	sig := strings.TrimPrefix(sigHeader, "sha256=")
	if len(sig) != hex.EncodedLen(sha256.Size) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.ToLower(sig)), []byte(expected)) == 1 && ok
}

// SlackReplayWindow is the maximum age of a Slack request timestamp accepted by
// VerifySlackSignature. Slack recommends rejecting requests older than five
// minutes to bound replay of a captured, validly-signed request.
const SlackReplayWindow = 5 * time.Minute

// VerifySlackSignature reports whether sigHeader is a valid Slack v0 request
// signature for body, per
// https://api.slack.com/authentication/verifying-requests-from-slack.
//
// The signature base string is "v0:{timestamp}:{body}"; the HMAC-SHA256 of that
// under the signing secret, hex-encoded and prefixed "v0=", must match
// sigHeader (X-Slack-Signature) in constant time. timestampHeader
// (X-Slack-Request-Timestamp) must parse as a unix time within SlackReplayWindow
// of now — a stale or future timestamp is rejected to bound replay. An empty
// secret, an unparseable timestamp, or a malformed signature fails closed.
//
// Timing equalization (#572): as in VerifyHMACSHA256, the full-body HMAC is
// computed FIRST, unconditionally — an absent/garbage timestamp or an unset
// secret costs the same body-proportional work as a wrong signature — and the
// replay-window and parse checks then gate the (already computed) result.
func VerifySlackSignature(body []byte, secret, timestampHeader, sigHeader string, now time.Time) bool {
	ok := true
	if secret == "" {
		secret = fallbackSecret
		ok = false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("v0:" + timestampHeader + ":"))
	mac.Write(body)
	bodyHMACs.Add(1)
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))

	ts, err := strconv.ParseInt(strings.TrimSpace(timestampHeader), 10, 64)
	if err != nil {
		ok = false // ts is zero → the skew check below fails too
	}
	// Reject a timestamp outside the replay window in EITHER direction (stale or
	// implausibly future); abs(now - ts) must be within the window.
	skew := now.Unix() - ts
	if skew < 0 {
		skew = -skew
	}
	if skew > int64(SlackReplayWindow.Seconds()) {
		ok = false
	}
	return subtle.ConstantTimeCompare([]byte(sigHeader), []byte(expected)) == 1 && ok
}
