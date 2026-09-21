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
