package httpapi

import (
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/store"
)

func TestResolveThinkingConfig(t *testing.T) {
	cases := []struct {
		name          string
		override      *store.ThinkingConfig
		defaultBudget int
		wantNil       bool
		wantEnabled   bool
		wantBudget    int
	}{
		{
			name:          "no override, no default → off",
			override:      nil,
			defaultBudget: 0,
			wantNil:       true,
		},
		{
			name:          "no override, global default → enabled at default",
			override:      nil,
			defaultBudget: 8000,
			wantEnabled:   true,
			wantBudget:    8000,
		},
		{
			name:          "override enabled with explicit budget wins over default",
			override:      &store.ThinkingConfig{Enabled: true, BudgetTokens: 20000},
			defaultBudget: 8000,
			wantEnabled:   true,
			wantBudget:    20000,
		},
		{
			name:          "override enabled, budget 0 → falls back to global default budget",
			override:      &store.ThinkingConfig{Enabled: true, BudgetTokens: 0},
			defaultBudget: 8000,
			wantEnabled:   true,
			wantBudget:    8000,
		},
		{
			name:          "override DISABLED suppresses the global default",
			override:      &store.ThinkingConfig{Enabled: false},
			defaultBudget: 8000,
			wantEnabled:   false,
			wantBudget:    0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveThinkingConfig(tc.override, tc.defaultBudget)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("want nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("want non-nil config")
			}
			if got.Enabled != tc.wantEnabled {
				t.Errorf("Enabled = %v, want %v", got.Enabled, tc.wantEnabled)
			}
			if got.BudgetTokens != tc.wantBudget {
				t.Errorf("BudgetTokens = %d, want %d", got.BudgetTokens, tc.wantBudget)
			}
		})
	}

	// A disabled override must NOT be the nil-equivalent: the producer needs a
	// non-nil disabled config to suppress a global default.
	if got := resolveThinkingConfig(&store.ThinkingConfig{Enabled: false}, 8000); got == nil {
		t.Fatal("disabled override must return a non-nil disabled config, not nil")
	}
	_ = agentcore.ThinkingConfig{} // ensure the agentcore type is the return type
}
