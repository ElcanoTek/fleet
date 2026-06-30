package store

import (
	"context"
	"testing"
)

func TestScanThinkingConfig(t *testing.T) {
	if got := scanThinkingConfig(nil); got != nil {
		t.Errorf("nil → %+v, want nil", got)
	}
	if got := scanThinkingConfig([]byte("")); got != nil {
		t.Errorf("empty → %+v, want nil", got)
	}
	if got := scanThinkingConfig([]byte("null")); got != nil {
		t.Errorf("null literal → %+v, want nil", got)
	}
	if got := scanThinkingConfig([]byte("{not json")); got != nil {
		t.Errorf("malformed → %+v, want nil (tolerant)", got)
	}
	got := scanThinkingConfig([]byte(`{"enabled":true,"budget_tokens":10000}`))
	if got == nil || !got.Enabled || got.BudgetTokens != 10000 {
		t.Fatalf("valid → %+v, want enabled 10000", got)
	}
	got = scanThinkingConfig([]byte(`{"enabled":false}`))
	if got == nil || got.Enabled {
		t.Fatalf("disabled → %+v, want non-nil disabled", got)
	}
}

// TestSetThinkingConfigRoundTrip exercises the migration + JSONB threading
// against a real Postgres: set an override, read it back via Get, change it,
// then clear it back to nil (inherit global default).
func TestSetThinkingConfigRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const email = "brad@elcano.com"

	conv, err := s.CreateConversation(ctx, email, "thinking chat", "assistant", "", false)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Default: no override.
	if conv.ThinkingConfig != nil {
		t.Fatalf("new conversation should have nil thinking_config, got %+v", conv.ThinkingConfig)
	}

	// Set an enabled override.
	if err := s.SetThinkingConfig(ctx, email, conv.ID, &ThinkingConfig{Enabled: true, BudgetTokens: 12000}); err != nil {
		t.Fatalf("set enabled: %v", err)
	}
	got, err := s.Get(ctx, email, conv.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ThinkingConfig == nil || !got.ThinkingConfig.Enabled || got.ThinkingConfig.BudgetTokens != 12000 {
		t.Fatalf("after set: %+v, want enabled 12000", got.ThinkingConfig)
	}

	// Overwrite with an explicit disable.
	if err := s.SetThinkingConfig(ctx, email, conv.ID, &ThinkingConfig{Enabled: false}); err != nil {
		t.Fatalf("set disabled: %v", err)
	}
	got, _ = s.Get(ctx, email, conv.ID)
	if got.ThinkingConfig == nil || got.ThinkingConfig.Enabled {
		t.Fatalf("after disable: %+v, want non-nil disabled", got.ThinkingConfig)
	}

	// Clear → inherit global default (nil).
	if err := s.SetThinkingConfig(ctx, email, conv.ID, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	got, _ = s.Get(ctx, email, conv.ID)
	if got.ThinkingConfig != nil {
		t.Fatalf("after clear: %+v, want nil", got.ThinkingConfig)
	}

	// Unknown conversation / wrong owner → not found.
	if err := s.SetThinkingConfig(ctx, "someone@else.com", conv.ID, nil); err == nil {
		t.Fatal("expected not-found for wrong owner")
	}
}
