package agent

import (
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
)

// TestResolveWarmSize pins the #1264 semantics: an explicit
// FLEET_SANDBOX_WARM_SIZE pins the depth — including 0, which disables
// warming — and only the unset sentinel (-1) derives from
// MaxConcurrentAgents (clamped 2..8, the #181 default).
func TestResolveWarmSize(t *testing.T) {
	cases := []struct {
		name          string
		warmSize      int
		maxConcurrent int
		want          int
	}{
		{"unset derives from concurrency", -1, 4, 4},
		{"unset clamps to the floor", -1, 1, 2},
		{"unset clamps to the ceiling", -1, 50, 8},
		{"explicit zero disables warming", 0, 4, 0},
		{"explicit value pins the depth", 5, 4, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{SandboxWarmSize: tc.warmSize, MaxConcurrentAgents: tc.maxConcurrent}
			if got := resolveWarmSize(cfg); got != tc.want {
				t.Errorf("resolveWarmSize(warm=%d, concurrent=%d) = %d, want %d",
					tc.warmSize, tc.maxConcurrent, got, tc.want)
			}
		})
	}
}
