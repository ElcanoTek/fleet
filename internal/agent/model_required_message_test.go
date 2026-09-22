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
