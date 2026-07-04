package agent

import (
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
)

// scheduledThinkingConfig precedence (#220): the per-task override wins when
// set (including an explicit 0 = off), else the global default; a resolved
// budget <=0 is thinking-off, a positive budget is clamped to the provider
// bounds.
func TestScheduledThinkingConfigPrecedence(t *testing.T) {
	ptr := func(i int) *int { return &i }
	globalOn := &config.Config{DefaultThinkingBudgetTokens: 4096}
	globalOff := &config.Config{DefaultThinkingBudgetTokens: 0}

	cases := []struct {
		name        string
		cfg         *config.Config
		override    *int
		wantEnabled bool
		wantBudget  int // only checked when enabled
	}{
		{"no override → inherit global on", globalOn, nil, true, 4096},
		{"no override → inherit global off", globalOff, nil, false, 0},
		{"override wins over global-off", globalOff, ptr(8192), true, 8192},
		{"override 0 forces off despite global-on", globalOn, ptr(0), false, 0},
		{"override clamps below min", globalOff, ptr(1), true, agentcore.MinThinkingBudgetTokens},
		{"override clamps above max", globalOff, ptr(10_000_000), true, agentcore.MaxThinkingBudgetTokens},
		{"nil cfg, no override → off", nil, nil, false, 0},
		{"nil cfg, override on → on", nil, ptr(2048), true, 2048},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := scheduledThinkingConfig(c.cfg, c.override)
			if !c.wantEnabled {
				if got != nil {
					t.Fatalf("want thinking off (nil), got %+v", got)
				}
				return
			}
			if got == nil || !got.Enabled {
				t.Fatalf("want thinking enabled, got %+v", got)
			}
			if got.BudgetTokens != c.wantBudget {
				t.Errorf("budget = %d, want %d", got.BudgetTokens, c.wantBudget)
			}
		})
	}
}
