package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config/envreg"
)

// CUTLASS_ALLOWED_DIRS is a live connector wire key, not one of the
// CHAT_/CUTLASS_ aliases ADR-0106 removed: every client bundle sets it as
// `CUTLASS_ALLOWED_DIRS: "${CUTLASS_ALLOWED_DIRS}:${FLEET_WORKSPACE_ROOT}"`
// and the operator keeps its value in the server env file. Pin that the env
// file still supplies it, the bundle interpolation resolves, the spawn-time
// workspace-root expansion follows, and nothing reports it as retired.
func TestCutlassAllowedDirsStillReachesTheConnector(t *testing.T) {
	if _, retired := envreg.RetiredAlias("CUTLASS_ALLOWED_DIRS"); retired || !envreg.IsCanonical("CUTLASS_ALLOWED_DIRS") {
		t.Fatal("CUTLASS_ALLOWED_DIRS must stay a registered, non-retired setting")
	}
	t.Setenv("CUTLASS_ALLOWED_DIRS", "")
	_ = os.Unsetenv("CUTLASS_ALLOWED_DIRS")
	envFile := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(envFile, []byte("CUTLASS_ALLOWED_DIRS=/srv/drops\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_ENV_FILE", envFile)
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)

	bundle, err := clientconfig.Load(mcpTestBundle(t, `mcp_servers:
  - name: mailer
    type: stdio
    command: /bin/true
    always: true
    env:
      CUTLASS_ALLOWED_DIRS: "${CUTLASS_ALLOWED_DIRS}:${FLEET_WORKSPACE_ROOT}"
`))
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	mailer, ok := bundle.MCPServerConfigs()["mailer"]
	if !ok {
		t.Fatal("mailer is not enabled")
	}
	if got, want := mailer.Env["CUTLASS_ALLOWED_DIRS"], "/srv/drops:${FLEET_WORKSPACE_ROOT}"; got != want {
		t.Fatalf("bundle env CUTLASS_ALLOWED_DIRS = %q, want %q (the env-file value, root token kept for spawn)", got, want)
	}
	spawn := agentcore.ExpandWorkspaceRootEnv(mailer.Env)
	if got := spawn["CUTLASS_ALLOWED_DIRS"]; got != "/srv/drops:"+agentcore.WorkspaceRootDir() {
		t.Fatalf("spawn env CUTLASS_ALLOWED_DIRS = %q, want /srv/drops:<workspace root>", got)
	}
	if err := validateConnectorParentEnvSeparation(bundle); err != nil {
		t.Fatalf("CUTLASS_ALLOWED_DIRS refused as a parent-owned name: %v", err)
	}
}
