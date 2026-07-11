package clientconfig

import (
	"reflect"
	"strings"
	"testing"
)

// TestReservedWorkspaceTokenSurvivesLoad pins the ${FLEET_WORKSPACE} contract:
// the reserved token passes through BOTH interpolation passes intact — the
// pre-unmarshal manifest pass (even when an operator exports a FLEET_WORKSPACE
// env var, which must NOT hijack the reserved name) and the spawn-map
// resolution (which blanks ordinary unset ${VAR} references) — so the MCP
// spawn paths can substitute the fleet-provided workdir at launch time.
func TestReservedWorkspaceTokenSurvivesLoad(t *testing.T) {
	// A hostile/accidental process-env value must not be substituted.
	t.Setenv("FLEET_WORKSPACE", "/should/never/appear")
	dir := writeManifest(t, `
mcp_servers:
  - name: sspd
    command: python3
    args: ["mcp/sspd.py"]
    always: true
    env:
      CUTLASS_RUN_WORKDIR: "${FLEET_WORKSPACE}"
      CUTLASS_REPORT_DIR: "${FLEET_WORKSPACE}/reports"
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	env := b.MCPServerConfigs()["sspd"].Env
	if got := env["CUTLASS_RUN_WORKDIR"]; got != "${FLEET_WORKSPACE}" {
		t.Errorf("CUTLASS_RUN_WORKDIR = %q, want the reserved token preserved verbatim", got)
	}
	if got := env["CUTLASS_REPORT_DIR"]; got != "${FLEET_WORKSPACE}/reports" {
		t.Errorf("CUTLASS_REPORT_DIR = %q, want token + suffix preserved", got)
	}
}

// TestReservedWorkspaceTokenNotDroppedByOptionalEnv pins that a token-bearing
// key survives optional_env (its resolved value is non-empty — the token), so
// the spawn-time substitution still sees it.
func TestReservedWorkspaceTokenNotDroppedByOptionalEnv(t *testing.T) {
	dir := writeManifest(t, `
mcp_servers:
  - name: sspd
    command: python3
    args: ["mcp/sspd.py"]
    always: true
    env:
      CUTLASS_RUN_WORKDIR: "${FLEET_WORKSPACE}"
    optional_env: ["CUTLASS_RUN_WORKDIR"]
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := b.MCPServerConfigs()["sspd"].Env["CUTLASS_RUN_WORKDIR"]; got != "${FLEET_WORKSPACE}" {
		t.Errorf("token-bearing optional key = %q, want preserved", got)
	}
}

// TestIdentityEnvPropagationAndValidation pins the manifest identity_env
// contract: valid entries flow into MCPServerConfigs and EnvVarNames (so
// suffixed <VAR>_<ACCOUNT> forms survive the .env allowlist), and the fail-loud
// validations reject a typo'd key, an http placement, and a blank entry.
func TestIdentityEnvPropagationAndValidation(t *testing.T) {
	t.Run("propagates to runtime config and env-var names", func(t *testing.T) {
		dir := writeManifest(t, `
mcp_servers:
  - name: pm
    command: python3
    args: ["mcp/pm.py"]
    always: true
    env:
      PUBMATIC_API_KEY: "${PUBMATIC_API_KEY}"
      PUBMATIC_OWNER_ID: "${PUBMATIC_OWNER_ID:-60067}"
    identity_env: ["PUBMATIC_OWNER_ID"]
`)
		b, err := Load(dir)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		got := b.MCPServerConfigs()["pm"].IdentityEnv
		if !reflect.DeepEqual(got, []string{"PUBMATIC_OWNER_ID"}) {
			t.Errorf("IdentityEnv = %v", got)
		}
		names := b.EnvVarNames()
		found := false
		for _, n := range names {
			if n == "PUBMATIC_OWNER_ID" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("EnvVarNames must include the identity var so its account-suffixed forms survive the .env allowlist; got %v", names)
		}
	})

	t.Run("unknown env key rejected", func(t *testing.T) {
		dir := writeManifest(t, `
mcp_servers:
  - name: pm
    command: python3
    args: ["mcp/pm.py"]
    always: true
    env:
      PUBMATIC_API_KEY: "${PUBMATIC_API_KEY}"
    identity_env: ["PUBMATIC_OWNRE_ID"]
`)
		_, err := Load(dir)
		if err == nil || !strings.Contains(err.Error(), "PUBMATIC_OWNRE_ID") {
			t.Fatalf("typo'd identity_env key must fail the load naming the key, got: %v", err)
		}
	})

	t.Run("http server rejected", func(t *testing.T) {
		dir := writeManifest(t, `
mcp_servers:
  - name: remote
    type: http
    url: https://example.com/mcp
    always: true
    identity_env: ["ANY"]
`)
		_, err := Load(dir)
		if err == nil || !strings.Contains(err.Error(), "identity_env") {
			t.Fatalf("identity_env on an http server must fail the load, got: %v", err)
		}
	})

	t.Run("blank entry rejected", func(t *testing.T) {
		dir := writeManifest(t, `
mcp_servers:
  - name: pm
    command: python3
    args: ["mcp/pm.py"]
    always: true
    env:
      PUBMATIC_API_KEY: "${PUBMATIC_API_KEY}"
    identity_env: [""]
`)
		if _, err := Load(dir); err == nil {
			t.Fatal("blank identity_env entry must fail the load")
		}
	})
}
