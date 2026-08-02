package agent

import (
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// Shared (process-lifetime) spawns have no task identity, so a bundle env value
// carrying the reserved ${FLEET_TASK_ID} token must have its key DROPPED — not
// passed through as a literal placeholder the connector would read as a real
// value. Only the scheduled per-run path substitutes a real task ID.
// MCPServerDefs mirrors BuildMCPClient's shared-spawn env exactly (the
// reload diff compares like with like), so it is the pure seam that pins the
// behavior for both.
func TestSpecsToServerDefs_DropsUnresolvedTaskIDToken(t *testing.T) {
	specs := map[string]MCPServerSpec{
		"legacy": {
			Enabled: true,
			Command: "python",
			Args:    []string{"mcp/legacy.py"},
			Env: map[string]string{
				"LEGACY_TASK_ID": agentcore.TaskIDEnvToken,
				"LEGACY_MODE":    "prod",
			},
		},
	}

	defs := MCPServerDefs(specs)
	if len(defs) != 1 {
		t.Fatalf("expected 1 def, got %d", len(defs))
	}
	env := defs[0].Env
	if v, ok := env["LEGACY_TASK_ID"]; ok {
		t.Fatalf("token-bearing key must be dropped on a shared spawn, got LEGACY_TASK_ID=%q", v)
	}
	if env["LEGACY_MODE"] != "prod" {
		t.Fatalf("token-free key must survive, got LEGACY_MODE=%q", env["LEGACY_MODE"])
	}
}
