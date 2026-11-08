package clientconfig

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
)

// ADR-0106: a manifest reference to a removed CHAT_/CUTLASS_ spelling is
// reported with its FLEET_ name; the canonical connector wire key
// CUTLASS_ALLOWED_DIRS (every client bundle interpolates it) is not.
func TestRetiredManifestRefWarnings(t *testing.T) {
	got := retiredManifestRefWarnings([]string{"CUTLASS_ALLOWED_DIRS", "FLEET_WORKSPACE_ROOT", "CHAT_WORKSPACE_ROOT", "PROVIDER_API_KEY"})
	if len(got) != 1 || !strings.Contains(got[0], "${CHAT_WORKSPACE_ROOT}, a removed alias of FLEET_WORKSPACE_ROOT") {
		t.Fatalf("warnings = %q, want exactly one for CHAT_WORKSPACE_ROOT", got)
	}
}

// ADR-0106 (Codex P2 on #2081): a bundle that references a removed spelling
// set only in FLEET_ENV_FILE gets a warning, and the value is neither
// admitted into the process env nor interpolated — even when the process env
// carries it. The canonical CUTLASS_ALLOWED_DIRS beside it is still admitted
// and interpolated.
func TestRetiredManifestRefIsNotAdmittedOrInterpolated(t *testing.T) {
	unsetTracked(t, "CHAT_MAX_COST_USD", "CUTLASS_ALLOWED_DIRS", "CUTLASS_TEMPERATURE")
	armEnvFile(t, "CHAT_MAX_COST_USD=7.5\nCUTLASS_ALLOWED_DIRS=/srv/drops\n")
	t.Setenv("CUTLASS_TEMPERATURE", "0.9") // a process-env value is not used either
	dir := writeManifest(t, `
mcp_servers:
  - name: mailer
    command: python3
    args: ["mcp/mailer.py"]
    always: true
    env:
      COST_CAP: "${CHAT_MAX_COST_USD:-none}"
      TEMP: "${CUTLASS_TEMPERATURE:-none}"
      CUTLASS_ALLOWED_DIRS: "${CUTLASS_ALLOWED_DIRS}"
`)
	var buf bytes.Buffer
	orig, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(orig); log.SetFlags(flags) })

	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if v, ok := os.LookupEnv("CHAT_MAX_COST_USD"); ok && v != "" {
		t.Errorf("CHAT_MAX_COST_USD was admitted from the env file: %q", v)
	}
	if got := os.Getenv("CUTLASS_ALLOWED_DIRS"); got != "/srv/drops" {
		t.Errorf("canonical CUTLASS_ALLOWED_DIRS not admitted: %q", got)
	}
	env := b.MCPServerConfigs()["mailer"].Env
	if env["COST_CAP"] != "none" || env["TEMP"] != "none" {
		t.Errorf("a removed spelling was interpolated: COST_CAP=%q TEMP=%q, want the defaults", env["COST_CAP"], env["TEMP"])
	}
	if env["CUTLASS_ALLOWED_DIRS"] != "/srv/drops" {
		t.Errorf("CUTLASS_ALLOWED_DIRS = %q, want /srv/drops", env["CUTLASS_ALLOWED_DIRS"])
	}
	out := buf.String()
	for _, want := range []string{
		"${CHAT_MAX_COST_USD}, a removed alias of FLEET_MAX_COST_USD",
		"${CUTLASS_TEMPERATURE}, a removed alias of FLEET_TEMPERATURE",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("load log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "${CUTLASS_ALLOWED_DIRS}") || strings.Contains(out, "7.5") || strings.Contains(out, "0.9") {
		t.Errorf("load log flagged a canonical key or printed a value:\n%s", out)
	}
}
