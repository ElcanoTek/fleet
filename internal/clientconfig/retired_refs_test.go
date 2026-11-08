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

// Codex P2 on #2081: a removed spelling never fails the load, in any form —
// ${X:?msg}, a bare ${X} in a non-lazy field, ${X:-d} — whether the variable
// is absent, in the env file, in the process env or both; its value is never
// used (bare and :? yield "", :- yields its default), and the load warns.
func TestRetiredManifestRefNeverFailsTheLoad(t *testing.T) {
	const manifest = `
mcp_servers:
  - name: remote
    type: http
    url: "https://mcp.example/${CHAT_TLS_DOMAIN}/x"
    always: true
  - name: local
    command: "python3${CHAT_TLS_MODE:?set CHAT_TLS_MODE}"
    args: ["${CUTLASS_TEMPERATURE:-d}", "${CUTLASS_ALLOWED_DIRS:-none}"]
    always: true
`
	for _, where := range []string{"absent", "file", "process", "both"} {
		t.Run(where, func(t *testing.T) {
			names := []string{"CHAT_TLS_DOMAIN", "CHAT_TLS_MODE", "CUTLASS_TEMPERATURE", "CUTLASS_ALLOWED_DIRS"}
			unsetTracked(t, names...)
			body := "CUTLASS_ALLOWED_DIRS=/srv/drops\n"
			if where == "file" || where == "both" {
				body += "CHAT_TLS_DOMAIN=stale.example\nCHAT_TLS_MODE=stale\nCUTLASS_TEMPERATURE=stale\n"
			}
			armEnvFile(t, body)
			if where == "process" || where == "both" {
				t.Setenv("CHAT_TLS_DOMAIN", "stale.example")
				t.Setenv("CHAT_TLS_MODE", "stale")
				t.Setenv("CUTLASS_TEMPERATURE", "stale")
			}
			var buf bytes.Buffer
			orig, flags := log.Writer(), log.Flags()
			log.SetOutput(&buf)
			log.SetFlags(0)
			t.Cleanup(func() { log.SetOutput(orig); log.SetFlags(flags) })

			b, err := Load(writeManifest(t, manifest))
			if err != nil {
				t.Fatalf("a removed spelling failed the load: %v", err)
			}
			cfgs := b.MCPServerConfigs()
			if got := cfgs["remote"].URL; got != "https://mcp.example//x" {
				t.Errorf("url = %q, want the bare removed reference as empty", got)
			}
			local := cfgs["local"]
			if local.Command != "python3" {
				t.Errorf("command = %q, want the :? removed reference as empty", local.Command)
			}
			if len(local.Args) != 2 || local.Args[0] != "d" || local.Args[1] != "/srv/drops" {
				t.Errorf("args = %q, want [d /srv/drops]", local.Args)
			}
			out := buf.String()
			for _, n := range []string{"CHAT_TLS_DOMAIN", "CHAT_TLS_MODE", "CUTLASS_TEMPERATURE"} {
				if !strings.Contains(out, "${"+n+"}, a removed alias of FLEET_") {
					t.Errorf("no warning for ${%s}:\n%s", n, out)
				}
			}
			if strings.Contains(out, "stale") || strings.Contains(out, "${CUTLASS_ALLOWED_DIRS}") {
				t.Errorf("log printed a value or flagged a canonical key:\n%s", out)
			}
			refs, err := RetiredManifestRefWarnings(b.Dir)
			if err != nil || len(refs) != 3 {
				t.Errorf("RetiredManifestRefWarnings = %q, %v; want 3 warnings", refs, err)
			}
		})
	}
}
