package clientconfig

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"
)

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
		"references the removed variable ${CHAT_MAX_COST_USD}; rename it to ${FLEET_MAX_COST_USD}",
		"references the removed variable ${CUTLASS_TEMPERATURE}; rename it to ${FLEET_TEMPERATURE}",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("load log lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "${CUTLASS_ALLOWED_DIRS}") || strings.Contains(out, "7.5") || strings.Contains(out, "0.9") {
		t.Errorf("load log flagged a canonical key or printed a value:\n%s", out)
	}
}

// Codex P2 on #2081: a manifest field whose VALUE is a variable name
// (enabled_env here, api_key_env in cmd/fleet's provider test) and names a
// removed spelling reads it as unset even when the process env holds it, so
// the connector stays disabled; the load warns and goes on.
func TestRetiredLiteralEnvNameIsUnset(t *testing.T) {
	unsetTracked(t, "CHAT_SERVER_TOKEN")
	armEnvFile(t, "")
	t.Setenv("CHAT_SERVER_TOKEN", "stale-token") // what EnvironmentFile= exports
	if v, ok := lookupNonEmpty("CHAT_SERVER_TOKEN"); ok || v != "" {
		t.Fatalf("lookupNonEmpty(CHAT_SERVER_TOKEN) = %q, %v; want unset", v, ok)
	}
	var buf bytes.Buffer
	orig, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(orig); log.SetFlags(flags) })
	dir := writeManifest(t, `
mcp_servers:
  - name: gated
    command: python3
    enabled_env: ["CHAT_SERVER_TOKEN"]
providers:
  - name: p
    type: openai
    api_key_env: CHAT_SERVER_TOKEN
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, on := b.MCPServerConfigs()["gated"]; on {
		t.Error("a connector gated on a removed spelling must stay disabled")
	}
	if !strings.Contains(buf.String(), "names the removed setting CHAT_SERVER_TOKEN: the internal service-token channel was removed (ADR-0108)") || strings.Contains(buf.String(), "stale-token") {
		t.Errorf("load log:\n%s", buf.String())
	}
	if ws, err := RetiredManifestRefWarnings(dir); err != nil || len(ws) != 2 {
		t.Errorf("RetiredManifestRefWarnings = %q, %v; want the two literal-name uses (enabled_env, api_key_env)", ws, err)
	}
}

// ADR-0106 (redesign on #2081): a removed spelling is read as unset, exactly
// like any unset variable. A required reference — a bare ${X} in a field the
// load resolves, whole or embedded in a larger string, or ${X:?m} — fails the
// load with the rename spelled out; ${X:-d} yields d. The stale value is never
// used, wherever it is set.
func TestRetiredRefIsAnUnsetVariable(t *testing.T) {
	for name, tc := range map[string]struct {
		manifest string
		wantErr  string
	}{
		"bare whole field": {`
mcp_servers:
  - name: s
    command: "${CHAT_TLS_MODE}"
    always: true
`, "manifest references removed variable CHAT_TLS_MODE; rename it to FLEET_TLS_MODE"},
		"embedded in a url": {`
mcp_servers:
  - name: s
    type: http
    url: "https://${CHAT_TLS_DOMAIN}/api"
    always: true
`, "manifest references removed variable CHAT_TLS_DOMAIN; rename it to FLEET_TLS_DOMAIN"},
		"required form": {`
mcp_servers:
  - name: s
    command: "python3${CHAT_TLS_MODE:?set it}"
    always: true
`, "manifest references removed variable CHAT_TLS_MODE; rename it to FLEET_TLS_MODE"},
	} {
		t.Run(name, func(t *testing.T) {
			unsetTracked(t, "CHAT_TLS_MODE", "CHAT_TLS_DOMAIN")
			armEnvFile(t, "CHAT_TLS_MODE=stale\nCHAT_TLS_DOMAIN=stale.example\n")
			t.Setenv("CHAT_TLS_MODE", "stale") // exported by the unit: never used
			_, err := Load(writeManifest(t, tc.manifest))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Load error = %v; want one containing %q", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), "stale") {
				t.Errorf("the error printed a value: %v", err)
			}
		})
	}
	t.Run("default form loads", func(t *testing.T) {
		unsetTracked(t, "CUTLASS_TEMPERATURE")
		armEnvFile(t, "CUTLASS_TEMPERATURE=0.9\n")
		b, err := Load(writeManifest(t, `
mcp_servers:
  - name: s
    command: python3
    args: ["${CUTLASS_TEMPERATURE:-0.3}"]
    always: true
`))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if got := b.MCPServerConfigs()["s"].Args; len(got) != 1 || got[0] != "0.3" {
			t.Errorf("args = %q, want the :- default", got)
		}
	})
}

// The read-only scanner finds every use: whole and embedded ${...} references
// in any section (servers, personas, pricing, sandbox), and literal names in
// env-name fields — but not a canonical CUTLASS_ wire key, nor an ordinary
// scalar equal to a removed name.
func TestRetiredManifestRefsFindsEveryUse(t *testing.T) {
	dir := writeManifest(t, `
mcp_servers:
  - name: s
    type: http
    url: "https://${CHAT_TLS_DOMAIN}/api"
    description: "CHAT_SERVER_TOKEN"
    env:
      CUTLASS_ALLOWED_DIRS: "${CUTLASS_ALLOWED_DIRS}"
      TEMP: "${CUTLASS_TEMPERATURE:-0.3}"
    enabled_env: ["CHAT_SERVER_TOKEN"]
personas:
  - name: p
    model: "${CHAT_TITLE_MODEL}"
pricing:
  overrides:
    - model: "${CHAT_METADATA_MODEL:-x}"
sandbox:
  image: "${CHAT_SANDBOX_IMAGE:-}"
`)
	refs, err := RetiredManifestRefs(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range refs {
		got[r.Name] = r.Path
	}
	for name, path := range map[string]string{
		"CHAT_TLS_DOMAIN":     "mcp_servers[0].url",
		"CUTLASS_TEMPERATURE": "mcp_servers[0].env.TEMP",
		"CHAT_SERVER_TOKEN":   "mcp_servers[0].enabled_env[0]",
		"CHAT_TITLE_MODEL":    "personas[0].model",
		"CHAT_METADATA_MODEL": "pricing.overrides[0].model",
		"CHAT_SANDBOX_IMAGE":  "sandbox.image",
	} {
		if got[name] != path {
			t.Errorf("%s found at %q, want %q (all: %+v)", name, got[name], path, refs)
		}
	}
	if len(refs) != 6 {
		t.Errorf("found %d uses, want 6: %+v", len(refs), refs)
	}
}

// Codex P2 on #2081: literal env-name detection follows the typed schema
// path. A same-named key inside an opaque pass-through block (empty_state
// cards) is not an env field and is not flagged — its ${...} references
// still are, because the loader interpolates the whole manifest text.
func TestRetiredManifestRefsFollowsTheSchemaPath(t *testing.T) {
	dir := writeManifest(t, `
mcp_servers:
  - name: s
    command: python3
    enabled_env: ["CHAT_SERVER_TOKEN"]
empty_state:
  cards:
    - title: "x"
      enabled_env: CHAT_SERVER_TOKEN
      api_key_env: CHAT_SERVER_TOKEN
      body: "${CHAT_TITLE_MODEL:-x}"
`)
	refs, err := RetiredManifestRefs(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range refs {
		got[r.Path] = true
	}
	if !got["mcp_servers[0].enabled_env[0]"] {
		t.Errorf("the real mcp_servers[].enabled_env was not flagged: %+v", refs)
	}
	if got["empty_state.cards[0].enabled_env"] || got["empty_state.cards[0].api_key_env"] {
		t.Errorf("an opaque empty_state key was flagged as an env field: %+v", refs)
	}
	if !got["empty_state.cards[0].body"] {
		t.Errorf("a ${...} reference in an opaque block (interpolated) was not flagged: %+v", refs)
	}
	if len(refs) != 2 {
		t.Errorf("found %d uses, want 2: %+v", len(refs), refs)
	}
}
