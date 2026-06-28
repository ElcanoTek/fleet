package runner

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func TestClassifyFailure(t *testing.T) {
	cases := []struct {
		err   error
		class string
	}{
		{agentcore.ErrRetryBudgetExhausted, models.FailureTransient},
		{agentcore.ErrStreamBlipPersisted, models.FailureTransient},
		{agentcore.ErrCostCeilingExceeded, models.FailureCostCeiling},
		{agentcore.ErrContextBudgetExhausted, models.FailureContextBudget},
		{errors.New("no model configured"), models.FailureTerminal},
		{fmt.Errorf("wrapped: %w", agentcore.ErrCostCeilingExceeded), models.FailureCostCeiling},
	}
	for _, c := range cases {
		if got := classifyFailure(c.err); got != c.class {
			t.Errorf("classifyFailure(%v) = %q, want %q", c.err, got, c.class)
		}
	}
}

// withinJitter asserts d is base ± 10% (the jitter band), with a tiny epsilon.
func withinJitter(t *testing.T, d, base time.Duration, label string) {
	t.Helper()
	lo := base - base/10 - time.Millisecond
	hi := base + base/10 + time.Millisecond
	if d < lo || d > hi {
		t.Errorf("%s: backoff %v outside [%v, %v] (base %v ±10%%)", label, d, lo, hi, base)
	}
	if d <= 0 {
		t.Errorf("%s: backoff must be strictly positive, got %v", label, d)
	}
}

func TestRetryBackoff_LegacyNilPolicy(t *testing.T) {
	// nil policy reproduces the legacy curve: 30s base, double per attempt, cap 10m.
	withinJitter(t, retryBackoff(0, nil), 30*time.Second, "attempt0")
	withinJitter(t, retryBackoff(1, nil), 60*time.Second, "attempt1")
	withinJitter(t, retryBackoff(3, nil), 240*time.Second, "attempt3")
	// attempt >= 8 → capped at 10m.
	withinJitter(t, retryBackoff(20, nil), 10*time.Minute, "attempt20-capped")
}

func TestRetryBackoff_CustomExponential(t *testing.T) {
	p := &models.RetryPolicy{Backoff: models.BackoffExponential, InitialDelaySeconds: 60, MaxDelaySeconds: 3600}
	withinJitter(t, retryBackoff(0, p), 60*time.Second, "exp-attempt0")
	withinJitter(t, retryBackoff(2, p), 240*time.Second, "exp-attempt2") // 60<<2
	withinJitter(t, retryBackoff(10, p), 3600*time.Second, "exp-capped") // capped at max
}

func TestRetryBackoff_Fixed(t *testing.T) {
	p := &models.RetryPolicy{Backoff: models.BackoffFixed, InitialDelaySeconds: 120, MaxDelaySeconds: 240}
	// Fixed: same base every attempt, never escalating.
	withinJitter(t, retryBackoff(0, p), 120*time.Second, "fixed-attempt0")
	withinJitter(t, retryBackoff(5, p), 120*time.Second, "fixed-attempt5")
}

// TestRetryBackoff_NeverNonPositive guards the scheduler invariant: a re-queued
// task must land strictly in the future, so backoff is always > 0.
func TestRetryBackoff_NeverNonPositive(t *testing.T) {
	for attempt := 0; attempt < 12; attempt++ {
		for _, p := range []*models.RetryPolicy{
			nil,
			{Backoff: models.BackoffFixed, InitialDelaySeconds: 1},
			{Backoff: models.BackoffExponential, InitialDelaySeconds: 5, MaxDelaySeconds: 10},
		} {
			if d := retryBackoff(attempt, p); d <= 0 {
				t.Errorf("attempt %d policy %+v: backoff %v must be > 0", attempt, p, d)
			}
		}
	}
}
