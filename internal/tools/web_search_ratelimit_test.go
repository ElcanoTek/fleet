package tools

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestRateLimiterWaitCancelDuringInterval reproduces issue #561: cancelling the
// context while a call is blocked in the minimum-interval wait must return
// ctx.Err() and must NOT double-unlock the mutex. A double unlock is a fatal,
// unrecoverable runtime error ("sync: unlock of unlocked mutex") that would take
// the whole process down — so if the bug were present this test binary would
// crash rather than fail. Run under -race.
func TestRateLimiterWaitCancelDuringInterval(t *testing.T) {
	// A large interval guarantees the second call takes the interval-wait branch.
	rl := newRateLimiter(time.Hour)

	// First call is free (lastRequest is zero) and stamps lastRequest = now.
	if err := rl.wait(context.Background(), "k"); err != nil {
		t.Fatalf("first wait returned error: %v", err)
	}

	// Second call: elapsed < minInterval, so it unlocks and blocks in the select.
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // ctx.Done() is ready before the select is entered — deterministic.

	if err := rl.wait(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestRateLimiterWaitCancelDuringPerMinute exercises the other early-return path:
// cancelling while blocked in the per-minute (>=10 requests) wait. Same double-
// unlock hazard, different branch.
func TestRateLimiterWaitCancelDuringPerMinute(t *testing.T) {
	// Zero interval skips the interval-wait branch so we can drive the per-minute
	// counter up quickly.
	rl := newRateLimiter(0)

	for i := 0; i < 10; i++ {
		if err := rl.wait(context.Background(), "k"); err != nil {
			t.Fatalf("wait %d returned error: %v", i, err)
		}
	}

	// The 11th call finds requestCounts["k"] >= 10 and blocks in the per-minute
	// wait (resetTime is up to a minute out). Cancel unblocks it.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := rl.wait(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// TestRateLimiterWaitNormalCompletion confirms the happy path still returns nil
// and leaves the mutex releasable for the next caller.
func TestRateLimiterWaitNormalCompletion(t *testing.T) {
	rl := newRateLimiter(0) // no interval wait, no per-minute wait

	for i := 0; i < 3; i++ {
		if err := rl.wait(context.Background(), "k"); err != nil {
			t.Fatalf("wait %d returned error: %v", i, err)
		}
	}
}
