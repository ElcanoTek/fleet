package acpruntime

import (
	"context"
	"errors"
	"testing"
)

// TestHostCeilingBackstop is the regression guard for #87: the host
// INDEPENDENTLY tears down a native-acp run when the agent's reconciled usage
// crosses the spec-supplied cost/token ceiling — defense-in-depth across the
// container boundary, on top of the in-container soft gate.
func TestHostCeilingBackstop(t *testing.T) {
	t.Run("cost ceiling", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		cl := &hostClient{deps: Deps{Observer: &recordingObserver{}}, maxCostUSD: 1.0, cancel: cancel}

		cl.accumulateUsage(map[string]any{usageKeyCostUSD: 0.50}) // under
		if ctx.Err() != nil {
			t.Fatal("backstop tripped under the cost ceiling")
		}
		cl.accumulateUsage(map[string]any{usageKeyCostUSD: 1.50}) // over
		if !errors.Is(context.Cause(ctx), ErrHostCeilingBackstop) {
			t.Fatalf("backstop did not fire over the cost ceiling; cause=%v", context.Cause(ctx))
		}
	})

	t.Run("token ceiling (uncached formula)", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		cl := &hostClient{deps: Deps{Observer: &recordingObserver{}}, maxTotalTokens: 1000, cancel: cancel}
		// 900 prompt - 100 cached + 250 completion = 1050 uncached >= 1000.
		cl.accumulateUsage(map[string]any{
			usageKeyPromptTokens:     900,
			usageKeyCachedTokens:     100,
			usageKeyCompletionTokens: 250,
		})
		if !errors.Is(context.Cause(ctx), ErrHostCeilingBackstop) {
			t.Fatalf("backstop did not fire over the token ceiling; cause=%v", context.Cause(ctx))
		}
	})

	t.Run("zero ceiling never trips", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		cl := &hostClient{deps: Deps{Observer: &recordingObserver{}}, cancel: cancel} // 0 = unlimited
		cl.accumulateUsage(map[string]any{usageKeyCostUSD: 9999, usageKeyPromptTokens: 9_000_000})
		if ctx.Err() != nil {
			t.Fatal("backstop tripped with no ceiling configured")
		}
	})
}
