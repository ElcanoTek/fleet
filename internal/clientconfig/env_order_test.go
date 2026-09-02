package clientconfig

// Tests for the bundle-load vs FLEET_ENV_FILE ordering fix and the
// unresolved-${VAR} load check (#1123). Load now folds the env file into the
// process env (allowlist-gated, process env wins) BEFORE manifest
// interpolation, so references outside the lazily-resolved connector maps —
// url:, command:/args:, sandbox.image, providers — resolve the operator's
// env-file values instead of silently baking a default or a literal token.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
)

// armEnvFile writes body to a temp env file, points FLEET_ENV_FILE at it, and
// re-arms the once-per-process boot application so THIS Load applies it. The
// caller must t.Setenv every var the file introduces (before unsetting it) so
// the file-sourced process-env writes are restored on teardown.
func armEnvFile(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write env file: %v", err)
	}
	t.Setenv("FLEET_ENV_FILE", path)
	resetBootEnvFileForTest()
	t.Cleanup(resetBootEnvFileForTest)
}

// unsetTracked registers the var for teardown restoration, then genuinely
// unsets it for the test body (a set-but-empty process var would WIN over the
// env file under Load's process-env-wins precedence).
func unsetTracked(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
}

// TestEnvFileOnlyVarResolvesInURL is the issue's first acceptance criterion: a
// bare ${VAR} in url: whose value lives ONLY in the env file resolves at load,
// through the real ordering (Load applies the file before interpolating).
func TestEnvFileOnlyVarResolvesInURL(t *testing.T) {
	unsetTracked(t, "I1123_MCP_URL")
	armEnvFile(t, "I1123_MCP_URL=https://env-file.example/mcp\n")
	dir := writeManifest(t, `
mcp_servers:
  - name: remote
    type: http
    url: "${I1123_MCP_URL}"
    always: true
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := b.MCPServerConfigs()["remote"].URL; got != "https://env-file.example/mcp" {
		t.Errorf("url = %q, want the env-file value", got)
	}
}

// TestEnvFileOverridesURLDefault: the exact silent failure from the issue — a
// ${VAR:-default} url whose override lives only in the env file used to bake
// the default; it must now use the override.
func TestEnvFileOverridesURLDefault(t *testing.T) {
	unsetTracked(t, "I1123_OVERRIDE_URL")
	armEnvFile(t, "I1123_OVERRIDE_URL=https://staging.example/mcp\n")
	dir := writeManifest(t, `
mcp_servers:
  - name: remote
    type: http
    url: "${I1123_OVERRIDE_URL:-https://prod.example/mcp}"
    always: true
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := b.MCPServerConfigs()["remote"].URL; got != "https://staging.example/mcp" {
		t.Errorf("url = %q, want the env-file override, not the baked default", got)
	}
}

// TestProcessEnvBeatsEnvFileInInterpolation pins the precedence contract:
// folding the env file in early must NOT change config.Load's rule that the
// process environment wins over the file.
func TestProcessEnvBeatsEnvFileInInterpolation(t *testing.T) {
	t.Setenv("I1123_PRECEDENCE_URL", "https://process.example/mcp")
	armEnvFile(t, "I1123_PRECEDENCE_URL=https://file.example/mcp\n")
	dir := writeManifest(t, `
mcp_servers:
  - name: remote
    type: http
    url: "${I1123_PRECEDENCE_URL}"
    always: true
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := b.MCPServerConfigs()["remote"].URL; got != "https://process.example/mcp" {
		t.Errorf("url = %q, want the process-env value (process env wins over the file)", got)
	}
	if got := os.Getenv("I1123_PRECEDENCE_URL"); got != "https://process.example/mcp" {
		t.Errorf("process env mutated to %q", got)
	}
}

// TestRequiredFormSatisfiedByEnvFile: a ${VAR:?msg} whose value lives only in
// the env file used to fail the load (validation ran pre-.env); it must now
// pass and substitute the file's value.
func TestRequiredFormSatisfiedByEnvFile(t *testing.T) {
	unsetTracked(t, "I1123_REQUIRED_NAME")
	armEnvFile(t, "I1123_REQUIRED_NAME=Acme\n")
	dir := writeManifest(t, `
branding:
  app_name: "${I1123_REQUIRED_NAME:?app_name must be provided}"
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if b.Branding.AppName != "Acme" {
		t.Errorf("AppName = %q, want the env-file value", b.Branding.AppName)
	}
}

// TestUnresolvedBareRefFailsLoad: an unset bare ${VAR} in any field OUTSIDE
// the lazily-resolved connector env/header maps is a load error naming the
// manifest field and the variable — never a literal token that fails at first
// use.
func TestUnresolvedBareRefFailsLoad(t *testing.T) {
	cases := []struct {
		name     string
		manifest string
		wantPath string
		wantVar  string
	}{
		{
			name: "http server url",
			manifest: `
mcp_servers:
  - name: remote
    type: http
    url: "${I1123_MISSING_URL}"
    always: true
`,
			wantPath: "mcp_servers[0].url",
			wantVar:  "I1123_MISSING_URL",
		},
		{
			name: "stdio command",
			manifest: `
mcp_servers:
  - name: local
    command: "${I1123_MISSING_CMD}"
    args: ["mcp/s.py"]
    always: true
`,
			wantPath: "mcp_servers[0].command",
			wantVar:  "I1123_MISSING_CMD",
		},
		{
			name: "stdio args",
			manifest: `
mcp_servers:
  - name: local
    command: python3
    args: ["${I1123_MISSING_ARG}"]
    always: true
`,
			wantPath: "mcp_servers[0].args[0]",
			wantVar:  "I1123_MISSING_ARG",
		},
		{
			name: "sandbox image",
			manifest: `
sandbox:
  image: "${I1123_MISSING_IMG}"
`,
			wantPath: "sandbox.image",
			wantVar:  "I1123_MISSING_IMG",
		},
		{
			name: "provider base_url",
			manifest: `
providers:
  - name: direct
    type: anthropic
    api_key_env: I1123_PROVIDER_KEY
    base_url: "${I1123_MISSING_BASE}"
`,
			wantPath: "providers[0].base_url",
			wantVar:  "I1123_MISSING_BASE",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			os.Unsetenv(tc.wantVar)
			_, err := Load(writeManifest(t, tc.manifest))
			if err == nil {
				t.Fatalf("expected load error for unset bare ${%s}", tc.wantVar)
			}
			for _, want := range []string{tc.wantPath, tc.wantVar, "unset or empty"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want it to contain %q", err, want)
				}
			}
		})
	}
}

// TestDefaultFormStaysQuietOutsideEnvMaps: an unset ${VAR:-default} outside
// the connector maps resolves to its default with no error — defaulting is
// configuration, not a misconfiguration.
func TestDefaultFormStaysQuietOutsideEnvMaps(t *testing.T) {
	os.Unsetenv("I1123_QUIET_URL")
	dir := writeManifest(t, `
mcp_servers:
  - name: remote
    type: http
    url: "${I1123_QUIET_URL:-https://fallback.example/mcp}"
    always: true
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := b.MCPServerConfigs()["remote"].URL; got != "https://fallback.example/mcp" {
		t.Errorf("url = %q, want the default", got)
	}
}

// TestEscapedLiteralOutsideEnvMapsLoads: a "$${...}" escape outside the
// connector maps is an intentional literal — the unresolved-reference check
// must not fire on it (the raw-bytes scan sees the escape; a post-pass scan
// could not).
func TestEscapedLiteralOutsideEnvMapsLoads(t *testing.T) {
	os.Unsetenv("I1123_NOT_A_VAR")
	dir := writeManifest(t, `
mcp_servers:
  - name: remote
    type: http
    url: "https://example.test/$${I1123_NOT_A_VAR}"
    always: true
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load should honor the $${...} escape: %v", err)
	}
	if got := b.MCPServerConfigs()["remote"].URL; got != "https://example.test/${I1123_NOT_A_VAR}" {
		t.Errorf("url = %q, want the literal ${...} text", got)
	}
}

// TestReservedTokenOutsideEnvMapsFailsLoad: the MCP spawn paths substitute
// ${FLEET_WORKSPACE}/${FLEET_TASK_ID} only in mcp_servers env values; anywhere
// else the token would stay a literal forever, so the load fails loudly.
func TestReservedTokenOutsideEnvMapsFailsLoad(t *testing.T) {
	_, err := Load(writeManifest(t, `
mcp_servers:
  - name: remote
    type: http
    url: "https://example.test/${FLEET_WORKSPACE}"
    always: true
`))
	if err == nil {
		t.Fatal("expected load error for a reserved token outside an env map")
	}
	for _, want := range []string{"mcp_servers[0].url", "FLEET_WORKSPACE", "reserved"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
}

// TestReservedTokenInHeadersFailsLoad: header maps are lazily RESOLVED, but
// reserved tokens are never SUBSTITUTED there (interpolate() preserves them
// verbatim; agentcore expands the env map only) — so a reserved token in a
// headers value would ship on the wire as the literal token forever. The load
// must refuse it even though the site is lazy.
func TestReservedTokenInHeadersFailsLoad(t *testing.T) {
	_, err := Load(writeManifest(t, `
mcp_servers:
  - name: remote
    type: http
    url: "https://example.test/mcp"
    always: true
    headers:
      X-Workdir: "${FLEET_WORKSPACE}"
`))
	if err == nil {
		t.Fatal("expected load error for a reserved token in a headers value")
	}
	for _, want := range []string{"mcp_servers[0].headers", "FLEET_WORKSPACE", "reserved"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
}

// TestReservedTokenInEnvMapStillLoads: an mcp_servers env value is the one
// legitimate substitution point for the reserved tokens — the fail-loud check
// must leave it alone (paired with TestReservedTokenInHeadersFailsLoad).
func TestReservedTokenInEnvMapStillLoads(t *testing.T) {
	b, err := Load(writeManifest(t, `
mcp_servers:
  - name: local
    command: python3
    args: ["mcp/s.py"]
    always: true
    env:
      RUN_WORKDIR: "${FLEET_WORKSPACE}"
      RUN_TASK: "${FLEET_TASK_ID}"
`))
	if err != nil {
		t.Fatalf("reserved tokens in an env map are the substitution contract; load failed: %v", err)
	}
	if got := b.MCPServerConfigs()["local"].Env["RUN_WORKDIR"]; got != "${FLEET_WORKSPACE}" {
		t.Errorf("env value = %q, want the token preserved for spawn-time substitution", got)
	}
}

// TestNestedRefInDefaultFailsLoad: ${VAR:-${OTHER}} outside the lazy maps is
// rejected as a spelling — the interpolator never expands a default body, so
// with VAR unset the field would ship the literal "${OTHER}" (and OTHER never
// reaches the .env allowlist). Set or unset, it is a landmine.
func TestNestedRefInDefaultFailsLoad(t *testing.T) {
	os.Unsetenv("I1123_NESTED_OUTER")
	os.Unsetenv("I1123_NESTED_INNER")
	_, err := Load(writeManifest(t, `
mcp_servers:
  - name: remote
    type: http
    url: "${I1123_NESTED_OUTER:-${I1123_NESTED_INNER}}"
    always: true
`))
	if err == nil {
		t.Fatal("expected load error for a nested reference in a default body")
	}
	for _, want := range []string{"mcp_servers[0].url", "I1123_NESTED_OUTER", "default"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to contain %q", err, want)
		}
	}
	// Rejected as a spelling even when the outer var IS set (the config would
	// otherwise break only when the env changes).
	t.Setenv("I1123_NESTED_OUTER", "https://set.example/mcp")
	if _, err := Load(writeManifest(t, `
mcp_servers:
  - name: remote
    type: http
    url: "${I1123_NESTED_OUTER:-${I1123_NESTED_INNER}}"
    always: true
`)); err == nil {
		t.Error("expected load error for the nested-default spelling even with the outer var set")
	}
}

// TestEscapedNestedDefaultLoads: a "$${...}" inside a default body is an
// intentional literal, not a nested reference — it must load.
func TestEscapedNestedDefaultLoads(t *testing.T) {
	os.Unsetenv("I1123_ESCAPED_OUTER")
	b, err := Load(writeManifest(t, `
branding:
  app_name: "${I1123_ESCAPED_OUTER:-literal $${TOKEN} here}"
`)) //nolint:dupword // $${TOKEN} is the escape under test
	if err != nil {
		t.Fatalf("load should honor the $${...} escape inside a default body: %v", err)
	}
	if got := b.Branding.AppName; got != "literal $${TOKEN} here" {
		t.Errorf("AppName = %q — the default body is emitted verbatim (escapes collapse only at the lazy second pass)", got)
	}
}

// TestNestedDefaultInLazyMapStillLoads: the connector env/header maps keep
// their existing contract — a nested default there is untouched by the new
// rejection (the value stays raw through load and resolves lazily).
func TestNestedDefaultInLazyMapStillLoads(t *testing.T) {
	os.Unsetenv("I1123_LAZY_OUTER")
	b, err := Load(writeManifest(t, `
mcp_servers:
  - name: local
    command: python3
    args: ["mcp/s.py"]
    always: true
    env:
      OUT: "${I1123_LAZY_OUTER:-${I1123_LAZY_INNER}}"
`))
	if err != nil {
		t.Fatalf("lazy-map nested default must keep its existing contract: %v", err)
	}
	if b == nil {
		t.Fatal("nil bundle")
	}
}

// TestRawParseFailureFailsLoad: a manifest whose RAW bytes are invalid YAML
// but whose INTERPOLATED bytes parse (the substitution repairs the syntax)
// must fail the load — otherwise the reference scan is silently skipped and
// the bundle reverts to pre-#1123 behavior (no registration, no fail-loud).
func TestRawParseFailureFailsLoad(t *testing.T) {
	t.Setenv("I1123_REPAIR", "v")
	// Raw: a plain scalar starting inside a flow sequence cannot contain '{',
	// so `[${X}]` fails the raw parse; interpolated it becomes `[v]`, valid.
	dir := writeManifest(t, "key: [${I1123_REPAIR}]\n")
	if _, err := interpolateManifest([]byte("key: [${I1123_REPAIR}]\n"), "probe"); err != nil {
		t.Fatalf("precondition: interpolation itself must succeed: %v", err)
	}
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected load error for raw-unparsable manifest")
	}
	// The connector-inventory raw parse reports it first;
	// scanManifestEnvRefs's error path is the backstop behind it. Either way
	// the load must fail with a parse error naming the manifest.
	if !strings.Contains(err.Error(), "parse") || !strings.Contains(err.Error(), "manifest.yaml") {
		t.Errorf("error = %v, want a parse error naming the manifest", err)
	}
	// And the backstop itself must error on raw-unparsable bytes.
	if _, _, scanErr := scanManifestEnvRefs([]byte("key: [${I1123_REPAIR}]\n")); scanErr == nil {
		t.Error("scanManifestEnvRefs must report a raw parse failure, not skip the scan")
	}
}

// TestLazyMapsKeepDeferringUnsetRefs: the fleet#706 contract is untouched —
// unset bare refs in mcp env/header values and http_tools headers still defer
// to catalog-build time instead of failing the load.
func TestLazyMapsKeepDeferringUnsetRefs(t *testing.T) {
	unsetTracked(t, "I1123_LAZY_SECRET", "I1123_LAZY_HEADER", "I1123_LAZY_TOOL_HEADER")
	dir := writeManifest(t, `
mcp_servers:
  - name: local
    command: python3
    args: ["mcp/s.py"]
    always: true
    env:
      SECRET_OUT: "${I1123_LAZY_SECRET}"
    optional_env: ["SECRET_OUT"]
  - name: remote
    type: http
    url: "https://example.test/mcp"
    always: true
    headers:
      Authorization: "Bearer ${I1123_LAZY_HEADER}"
http_tools:
  - name: lookup
    description: lookup
    method: GET
    url: "https://example.test/things"
    headers:
      Authorization: "Bearer ${I1123_LAZY_TOOL_HEADER}"
    input_schema:
      type: object
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load must not fail on deferred connector refs: %v", err)
	}
	// The refs resolve against the live env at catalog-build time.
	t.Setenv("I1123_LAZY_SECRET", "late-secret")
	if got := b.MCPServerConfigs()["local"].Env["SECRET_OUT"]; got != "late-secret" {
		t.Errorf("env value = %q, want the late-set process value", got)
	}
}

// TestEnvVarNamesInventoriesAllInterpolatedFields: url:/command:/sandbox.image
// refs must register with the .env allowlist, not only env/header values — and
// a ref whose var is already exported (substituted out of the parsed manifest)
// must still be inventoried from the raw bytes.
func TestEnvVarNamesInventoriesAllInterpolatedFields(t *testing.T) {
	os.Unsetenv("I1123_INV_URL")
	os.Unsetenv("I1123_INV_IMG")
	t.Setenv("I1123_INV_CMD", "python3")
	dir := writeManifest(t, `
sandbox:
  image: "${I1123_INV_IMG:-registry.example/sandbox:v1}"
mcp_servers:
  - name: remote
    type: http
    url: "${I1123_INV_URL:-https://prod.example/mcp}"
    always: true
  - name: local
    command: "${I1123_INV_CMD}"
    args: ["mcp/s.py"]
    always: true
`)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	names := b.EnvVarNames()
	for _, want := range []string{"I1123_INV_URL", "I1123_INV_IMG", "I1123_INV_CMD"} {
		if !slices.Contains(names, want) {
			t.Errorf("EnvVarNames = %v, want %q", names, want)
		}
	}
	if slices.Contains(names, "FLEET_WORKSPACE") || slices.Contains(names, "FLEET_TASK_ID") {
		t.Errorf("EnvVarNames = %v, must exclude reserved runtime tokens", names)
	}
}


// TestEnvFileAllowlistStillGatesUnreferencedKeys: the early application admits
// only allowlisted keys — a file key the manifest never references (and the
// static allowlist doesn't know) must NOT enter the process env.
func TestEnvFileAllowlistStillGatesUnreferencedKeys(t *testing.T) {
	unsetTracked(t, "I1123_FILE_URL", "I1123_UNREFERENCED")
	armEnvFile(t, "I1123_FILE_URL=https://env-file.example/mcp\nI1123_UNREFERENCED=leak\n")
	if _, err := Load(writeManifest(t, `
mcp_servers:
  - name: remote
    type: http
    url: "${I1123_FILE_URL}"
    always: true
`)); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := os.Getenv("I1123_UNREFERENCED"); got != "" {
		t.Errorf("unreferenced env-file key entered the process env: %q", got)
	}
}

// TestBootEnvFileAppliesOncePerProcess: the second Load must NOT re-read the
// env file — the broker child's boot-snapshot contract (credential env changes
// require a restart) and the post-scrub parent both depend on it.
func TestBootEnvFileAppliesOncePerProcess(t *testing.T) {
	unsetTracked(t, "I1123_ONCE_URL")
	armEnvFile(t, "I1123_ONCE_URL=https://boot.example/mcp\n")
	manifest := `
mcp_servers:
  - name: remote
    type: http
    url: "${I1123_ONCE_URL:-https://default.example/mcp}"
    always: true
`
	dir := writeManifest(t, manifest)
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := b.MCPServerConfigs()["remote"].URL; got != "https://boot.example/mcp" {
		t.Fatalf("boot url = %q, want the env-file value", got)
	}
	// Simulate a credential rotation in the file + the var vanishing from the
	// process env: a bundle re-load must NOT re-apply the file.
	envPath := os.Getenv("FLEET_ENV_FILE")
	if err := os.WriteFile(envPath, []byte("I1123_ONCE_URL=https://rotated.example/mcp\n"), 0o600); err != nil {
		t.Fatalf("rewrite env file: %v", err)
	}
	os.Unsetenv("I1123_ONCE_URL")
	b2, err := Load(dir)
	if err != nil {
		t.Fatalf("re-load: %v", err)
	}
	if got := b2.MCPServerConfigs()["remote"].URL; got != "https://default.example/mcp" {
		t.Errorf("re-load url = %q, want the default (the rotated file must not be re-read)", got)
	}
}

// TestBootOrderingEndToEnd mirrors the serve/probe/eval boot sequence —
// clientconfig.Load, RegisterAllowedEnvVars(EnvVarNames), config.Load — and
// proves the pieces agree: the url resolved the env-file value at bundle load,
// and config.Load's own application of the same file is an idempotent
// re-read.
func TestBootOrderingEndToEnd(t *testing.T) {
	unsetTracked(t, "I1123_E2E_URL")
	armEnvFile(t, "I1123_E2E_URL=https://env-file.example/mcp\n")
	bundle, err := Load(writeManifest(t, `
mcp_servers:
  - name: remote
    type: http
    url: "${I1123_E2E_URL}"
    always: true
`))
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	config.RegisterAllowedEnvVars(bundle.EnvVarNames()...)
	if _, err := config.Load(os.Getenv("FLEET_ENV_FILE")); err != nil {
		t.Fatalf("load config: %v", err)
	}
	if got := bundle.MCPServerConfigs()["remote"].URL; got != "https://env-file.example/mcp" {
		t.Errorf("url = %q, want the env-file value", got)
	}
	if got := os.Getenv("I1123_E2E_URL"); got != "https://env-file.example/mcp" {
		t.Errorf("process env = %q after config.Load, want the env-file value", got)
	}
}
