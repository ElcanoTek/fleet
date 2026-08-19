package clientconfig

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// writeManifest writes a manifest.yaml into a fresh temp bundle dir and returns
// the dir.
func writeManifest(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestInterpolateEnvSetOverridesDefault: env set AND non-empty wins over the
// ${VAR:-default} literal.
func TestInterpolateEnvSetOverridesDefault(t *testing.T) {
	t.Setenv("PUBMATIC_OWNER_ID", "99999")
	dir := writeManifest(t, `
mcp_servers:
  - name: pm
    command: python3
    args: ["mcp/pm.py"]
    always: true
    env:
      PUBMATIC_OWNER_ID: "${PUBMATIC_OWNER_ID:-60067}"
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := b.MCPServerConfigs()["pm"].Env["PUBMATIC_OWNER_ID"]
	if got != "99999" {
		t.Errorf("PUBMATIC_OWNER_ID = %q, want 99999 (env override)", got)
	}
}

// TestInterpolateUnsetUsesDefault: VAR unset -> ${VAR:-default} resolves to the
// literal default.
func TestInterpolateUnsetUsesDefault(t *testing.T) {
	os.Unsetenv("PUBMATIC_OWNER_ID")
	dir := writeManifest(t, `
mcp_servers:
  - name: pm
    command: python3
    args: ["mcp/pm.py"]
    always: true
    env:
      PUBMATIC_OWNER_ID: "${PUBMATIC_OWNER_ID:-60067}"
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := b.MCPServerConfigs()["pm"].Env["PUBMATIC_OWNER_ID"]
	if got != "60067" {
		t.Errorf("PUBMATIC_OWNER_ID = %q, want default 60067", got)
	}
}

// TestInterpolateEmptyEnvUsesDefault: empty env value counts as unset for the
// POSIX :- form, so the default is used.
func TestInterpolateEmptyEnvUsesDefault(t *testing.T) {
	t.Setenv("PUBMATIC_OWNER_ID", "")
	dir := writeManifest(t, `
mcp_servers:
  - name: pm
    command: python3
    args: ["mcp/pm.py"]
    always: true
    env:
      PUBMATIC_OWNER_ID: "${PUBMATIC_OWNER_ID:-60067}"
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := b.MCPServerConfigs()["pm"].Env["PUBMATIC_OWNER_ID"]
	if got != "60067" {
		t.Errorf("empty-env PUBMATIC_OWNER_ID = %q, want default 60067", got)
	}
}

// TestInterpolateColonBearingDefaultSurvivesYAML: a default that contains ':'
// (an https:// URL) must survive interpolation AND the YAML round-trip — which
// is exactly why the field MUST be quoted. The unset path keeps the URL default
// intact; the set path overrides it.
func TestInterpolateColonBearingDefaultSurvivesYAML(t *testing.T) {
	os.Unsetenv("PUBMATIC_BASE_URL")
	dir := writeManifest(t, `
mcp_servers:
  - name: pm
    command: python3
    args: ["mcp/pm.py"]
    always: true
    env:
      PUBMATIC_BASE_URL: "${PUBMATIC_BASE_URL:-https://api.pubmatic.com}"
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := b.MCPServerConfigs()["pm"].Env["PUBMATIC_BASE_URL"]
	if got != "https://api.pubmatic.com" {
		t.Errorf("colon-bearing default = %q, want https://api.pubmatic.com", got)
	}

	// And the env override path with a colon-bearing override value.
	t.Setenv("PUBMATIC_BASE_URL", "https://staging.pubmatic.example:8443/api")
	b2, err := Load(dir)
	if err != nil {
		t.Fatalf("load (override): %v", err)
	}
	got2 := b2.MCPServerConfigs()["pm"].Env["PUBMATIC_BASE_URL"]
	if got2 != "https://staging.pubmatic.example:8443/api" {
		t.Errorf("colon-bearing override = %q", got2)
	}
}

// TestInterpolateColonDefaultAtTopLevel exercises a :- default on a top-level
// (non-deferred) field — branding — to prove the pre-unmarshal pass + YAML
// quoting works outside the per-server env maps too.
func TestInterpolateColonDefaultAtTopLevel(t *testing.T) {
	os.Unsetenv("FLEET_SHARE_DESC")
	dir := writeManifest(t, `
branding:
  app_name: "Acme"
  share_description: "${FLEET_SHARE_DESC:-Visit https://acme.example: it's great}"
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	const want = "Visit https://acme.example: it's great"
	if b.Branding.ShareDescription != want {
		t.Errorf("ShareDescription = %q, want %q", b.Branding.ShareDescription, want)
	}
}

// TestInterpolateRequiredFormMissingErrors: ${VAR:?message} with VAR unset/empty
// fails the load and the error carries the message.
func TestInterpolateRequiredFormMissingErrors(t *testing.T) {
	os.Unsetenv("REQUIRED_THING")
	dir := writeManifest(t, `
branding:
  app_name: "${REQUIRED_THING:?app_name must be provided}"
`)
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error for unset ${VAR:?message}")
	}
	if !strings.Contains(err.Error(), "app_name must be provided") {
		t.Errorf("error = %v, want it to contain the :? message", err)
	}
	if !strings.Contains(err.Error(), "REQUIRED_THING") {
		t.Errorf("error = %v, want it to name the var", err)
	}
}

// TestInterpolateRequiredFormSetPasses: ${VAR:?message} with VAR set resolves to
// the value.
func TestInterpolateRequiredFormSetPasses(t *testing.T) {
	t.Setenv("REQUIRED_THING", "Acme")
	dir := writeManifest(t, `
branding:
  app_name: "${REQUIRED_THING:?must be provided}"
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if b.Branding.AppName != "Acme" {
		t.Errorf("AppName = %q, want Acme", b.Branding.AppName)
	}
}

// TestInterpolateBareUnsetIsDeferred: a bare ${VAR} that is unset is LEFT INTACT
// by the pre-unmarshal pass and resolved (to empty, then dropped) at spawn time
// via optional_env — i.e. it does NOT hard-fail the load. This is the deferred
// credential contract the per-server env maps rely on.
func TestInterpolateBareUnsetIsDeferred(t *testing.T) {
	os.Unsetenv("SOME_SECRET")
	dir := writeManifest(t, `
mcp_servers:
  - name: s
    command: python3
    args: ["mcp/s.py"]
    always: true
    env:
      SECRET_OUT: "${SOME_SECRET}"
    optional_env: ["SECRET_OUT"]
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load should not fail on deferred bare ${VAR}: %v", err)
	}
	// The bundle keeps the literal token so EnvVarNames sees it...
	names := b.EnvVarNames()
	found := false
	for _, n := range names {
		if n == "SOME_SECRET" {
			found = true
		}
	}
	if !found {
		t.Errorf("EnvVarNames should include deferred SOME_SECRET, got %v", names)
	}
	// ...and at spawn time the empty value is dropped (optional_env).
	if _, ok := b.MCPServerConfigs()["s"].Env["SECRET_OUT"]; ok {
		t.Error("SECRET_OUT should be dropped (empty + optional)")
	}
}

// TestInterpolateBareSetIsSubstituted: a bare ${VAR} that IS set at load time is
// substituted in place.
func TestInterpolateBareSetIsSubstituted(t *testing.T) {
	t.Setenv("SOME_SECRET", "topsecret")
	dir := writeManifest(t, `
mcp_servers:
  - name: s
    command: python3
    args: ["mcp/s.py"]
    always: true
    env:
      SECRET_OUT: "${SOME_SECRET}"
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := b.MCPServerConfigs()["s"].Env["SECRET_OUT"]; got != "topsecret" {
		t.Errorf("SECRET_OUT = %q, want topsecret", got)
	}
}

// TestInterpolateEscape: "$${" emits a literal "${".
func TestInterpolateEscape(t *testing.T) {
	dir := writeManifest(t, `
branding:
  app_name: "literal $${NOT_A_VAR} here"
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	const want = "literal ${NOT_A_VAR} here"
	if b.Branding.AppName != want {
		t.Errorf("AppName = %q, want %q", b.Branding.AppName, want)
	}
}

// TestInterpolateNestedBraceDefault: a :- default whose body itself contains
// balanced braces survives without the inner '}' terminating the expression.
func TestInterpolateNestedBraceDefault(t *testing.T) {
	os.Unsetenv("TEMPLATE_VAR")
	dir := writeManifest(t, `
branding:
  app_name: "${TEMPLATE_VAR:-pre {inner} post}"
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	const want = "pre {inner} post"
	if b.Branding.AppName != want {
		t.Errorf("AppName = %q, want %q", b.Branding.AppName, want)
	}
}

// TestInterpolateUnterminatedErrors: an unterminated ${ is a clear error.
func TestInterpolateUnterminatedErrors(t *testing.T) {
	dir := writeManifest(t, `
branding:
  app_name: "oops ${UNTERMINATED"
`)
	if _, err := Load(dir); err == nil {
		t.Error("expected error for unterminated ${...}")
	}
}

// TestInterpolateManifestUnit exercises interpolateManifest directly for the
// table of forms, independent of YAML.
func TestInterpolateManifestUnit(t *testing.T) {
	t.Setenv("SET_VAR", "setval")
	t.Setenv("EMPTY_VAR", "")
	os.Unsetenv("UNSET_VAR")

	cases := []struct {
		name    string
		in      string
		want    string
		wantErr string
	}{
		{name: "bare set", in: `x: "${SET_VAR}"`, want: `x: "setval"`},
		{name: "bare unset deferred", in: `x: "${UNSET_VAR}"`, want: `x: "${UNSET_VAR}"`},
		{name: "default used when unset", in: `x: "${UNSET_VAR:-def}"`, want: `x: "def"`},
		{name: "default used when empty", in: `x: "${EMPTY_VAR:-def}"`, want: `x: "def"`},
		{name: "set overrides default", in: `x: "${SET_VAR:-def}"`, want: `x: "setval"`},
		{name: "colon url default", in: `x: "${UNSET_VAR:-https://h.example/p}"`, want: `x: "https://h.example/p"`},
		{name: "required set", in: `x: "${SET_VAR:?msg}"`, want: `x: "setval"`},
		{name: "required unset errors", in: `x: "${UNSET_VAR:?boom}"`, wantErr: "boom"},
		{name: "escape", in: `x: "$${LIT}"`, want: `x: "${LIT}"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := interpolateManifest([]byte(tc.in), "test.yaml")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want contains %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestInterpolateEnvFileOverrideOfDefaultWins pins the fleet#706 lazy
// contract for connector env/header values: a ${VAR:-default} key keeps its
// raw form through Load — its name surfaced via EnvVarNames so an env-file
// value survives the allowlist — and resolves against the LIVE env at
// catalog-build time, where a var set after Load (the broker child's own
// environment, an account overlay, config.Load admitting a literal-named key)
// wins over the baked default. Since #1123 the FLEET_ENV_FILE file itself is
// folded in BEFORE interpolation, but the lazy catalog-build resolution must
// keep working for env changes that land after the bundle loads.
func TestInterpolateEnvFileOverrideOfDefaultWins(t *testing.T) {
	os.Unsetenv("PUBMATIC_OWNER_ID")
	os.Unsetenv("DEMO_TOKEN")
	dir := writeManifest(t, `
mcp_servers:
  - name: pm
    command: python3
    args: ["mcp/pm.py"]
    always: true
    env:
      PUBMATIC_OWNER_ID: "${PUBMATIC_OWNER_ID:-60067}"
http_tools:
  - name: get_thing
    method: GET
    url: "https://api.example.com/things"
    headers:
      Authorization: "Bearer ${DEMO_TOKEN:-anon}"
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	names := b.EnvVarNames()
	for _, want := range []string{"PUBMATIC_OWNER_ID", "DEMO_TOKEN"} {
		if !slices.Contains(names, want) {
			t.Errorf("EnvVarNames = %v, want %q so the env-file value survives the allowlist", names, want)
		}
	}
	// config.Load applies the .env file between Load and the catalog build.
	t.Setenv("PUBMATIC_OWNER_ID", "99999")
	t.Setenv("DEMO_TOKEN", "shh-secret")
	if got := b.MCPServerConfigs()["pm"].Env["PUBMATIC_OWNER_ID"]; got != "99999" {
		t.Errorf("PUBMATIC_OWNER_ID = %q, want the post-load env override 99999, not the baked default", got)
	}
	if got := b.HTTPToolConfigs()[0].Headers["Authorization"]; got != "Bearer shh-secret" {
		t.Errorf("Authorization = %q, want the post-load env override applied", got)
	}
}

// TestInterpolateDefaultStillAppliesAtSpawn: with the var still unset after the
// .env load, a deferred ${VAR:-default} connector value resolves to its default
// at catalog-build time.
func TestInterpolateDefaultStillAppliesAtSpawn(t *testing.T) {
	os.Unsetenv("PUBMATIC_OWNER_ID")
	dir := writeManifest(t, `
mcp_servers:
  - name: pm
    command: python3
    args: ["mcp/pm.py"]
    always: true
    env:
      PUBMATIC_OWNER_ID: "${PUBMATIC_OWNER_ID:-60067}"
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := b.MCPServerConfigs()["pm"].Env["PUBMATIC_OWNER_ID"]; got != "60067" {
		t.Errorf("PUBMATIC_OWNER_ID = %q, want default 60067", got)
	}
}

// TestInterpolateEscapeSurvivesSpawnPass: a "$${" escape in an MCP env value
// must reach the subprocess as a literal "${" — regression: the load pass
// consumed the escape, and the spawn-time pass then expanded (blanked) the
// author's escaped literal. The escaped name is a literal, not an env
// reference, so it must not be registered for the .env allowlist either.
func TestInterpolateEscapeSurvivesSpawnPass(t *testing.T) {
	os.Unsetenv("NOT_A_VAR")
	dir := writeManifest(t, `
mcp_servers:
  - name: s
    command: python3
    args: ["mcp/s.py"]
    always: true
    env:
      TEMPLATE: "prefix $${NOT_A_VAR} suffix"
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	const want = "prefix ${NOT_A_VAR} suffix"
	if got := b.MCPServerConfigs()["s"].Env["TEMPLATE"]; got != want {
		t.Errorf("TEMPLATE = %q, want %q", got, want)
	}
	if slices.Contains(b.EnvVarNames(), "NOT_A_VAR") {
		t.Errorf("EnvVarNames = %v, must exclude the escaped literal NOT_A_VAR", b.EnvVarNames())
	}
}

// TestInterpolateSpawnTimeTrimsVarName: a "${ FOO }" reference (interior
// whitespace) registers as FOO at load time, so spawn-time resolution must
// read the same trimmed key — regression: interpolate() called os.Getenv on
// the untrimmed name and resolved blank while validate-config passed.
func TestInterpolateSpawnTimeTrimsVarName(t *testing.T) {
	// Unset at Load so the reference DEFERS to spawn-time resolution — the
	// path under test. (Set at load, the load-time pass resolves it.)
	os.Unsetenv("PUBMATIC_OWNER_ID")
	dir := writeManifest(t, `
mcp_servers:
  - name: pm
    command: python3
    args: ["mcp/pm.py"]
    always: true
    env:
      PUBMATIC_OWNER_ID: "${ PUBMATIC_OWNER_ID }"
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	t.Setenv("PUBMATIC_OWNER_ID", "12345")
	got := b.MCPServerConfigs()["pm"].Env["PUBMATIC_OWNER_ID"]
	if got != "12345" {
		t.Errorf("PUBMATIC_OWNER_ID = %q, want 12345 (trimmed spawn-time lookup)", got)
	}
}
