// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package agent

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// A retry-exhausted turn whose cause was the first-chunk watchdog is a model
// that never STARTED answering — usually a reasoning model still thinking —
// not a provider that failed. The card must say so (#1585), and the other
// retry-exhausted causes keep their wording.
func TestHumanMessageForReason_FirstChunkTimeoutIsNotAProviderFailure(t *testing.T) {
	watchdog := fmt.Errorf("stream blip persisted: %w", fmt.Errorf("%w after 75s (prompt ≈ 0 tokens): context canceled", agentcore.ErrFirstChunkTimeout))
	got := humanMessageForReason(agentcore.ReasonRetryExhausted, 0, watchdog)
	if !strings.Contains(got, "did not start responding within the time allowed") {
		t.Fatalf("watchdog cause must be named, got %q", got)
	}
	// streamErr is the last attempt's error, not a history: the card must not
	// claim a number of expiries it cannot know.
	if strings.Contains(got, "twice") {
		t.Fatalf("the card must not count watchdog expiries: %q", got)
	}
	if strings.Contains(got, "failing repeatedly") {
		t.Fatalf("a healthy-but-slow model must not be called a failing provider: %q", got)
	}

	other := humanMessageForReason(agentcore.ReasonRetryExhausted, 0, errors.New("provider reset the connection"))
	if !strings.Contains(other, "failing repeatedly") {
		t.Fatalf("a genuine provider failure keeps its wording, got %q", other)
	}
	if got := humanMessageForReason(agentcore.ReasonRetryExhausted, 429, watchdog); !strings.Contains(got, "rate-limiting") {
		t.Fatalf("a 429 keeps the rate-limit wording regardless of cause, got %q", got)
	}
}

// A watchdog expiry that FOLLOWED provider errors is the provider's failure,
// not a silent model: the inner retry backoff (5+10+20+40 s) can outlast the
// deadline, and the cancellation then wears the watchdog's sentinel. Reporting
// that as "the model did not start responding" would send a user off a model
// that is merely being throttled (#1585).
func TestHumanMessageForReason_WatchdogAfterProviderErrorsKeepsTheProviderStory(t *testing.T) {
	for _, tc := range []struct {
		name           string
		providerStatus int
		want           string
	}{
		{"rate limited", 429, "rate-limiting"},
		{"server errors", 503, "failing repeatedly"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := agentcore.NewFirstChunkTimeoutAfterProviderErrorForTest(tc.providerStatus)
			got := humanMessageForReason(agentcore.ReasonRetryExhausted, 0, err)
			if !strings.Contains(got, tc.want) {
				t.Fatalf("want %q in the message, got %q", tc.want, got)
			}
			if strings.Contains(got, "did not start responding") {
				t.Fatalf("a provider that answered with errors is not a silent model: %q", got)
			}
		})
	}
}

// The card's message and its structured status must tell the same story: a
// 429 whose backoff outlasted the watchdog arrives classified as a stream blip
// with status 0, and emitting "rate limiting" next to status_code 0 corrupts
// telemetry and any client branching on the field (#1585).
func TestEmitModelSelectionRequired_CarriesTheRecoveredProviderStatus(t *testing.T) {
	sink := &capturingSink{}
	err := agentcore.NewFirstChunkTimeoutAfterProviderErrorForTest(429)
	emitModelSelectionRequired(sink, agentcore.ReasonRetryExhausted, "vendor/model", 0, err)

	if len(sink.events) != 1 {
		t.Fatalf("events = %d, want 1", len(sink.events))
	}
	payload, ok := sink.events[0].payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T", sink.events[0].payload)
	}
	if got := payload["status_code"]; got != 429 {
		t.Errorf("status_code = %v, want 429", got)
	}
	msg, _ := payload["message"].(string)
	if !strings.Contains(msg, "rate-limiting") {
		t.Errorf("message = %q, want the rate-limit wording", msg)
	}
}

// A minimal EventSink that records what it was given.
type capturingSink struct {
	events []struct {
		event   string
		payload any
	}
}

func (c *capturingSink) Emit(event string, payload any) {
	c.events = append(c.events, struct {
		event   string
		payload any
	}{event: event, payload: payload})
}
