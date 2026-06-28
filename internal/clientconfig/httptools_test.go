package clientconfig

import (
	"path/filepath"
	"slices"
	"testing"
)

// writeManifest is defined in interpolate_test.go (same package).

// theManifest is the http_tools fixture both parse tests load. A header carries a
// ${DEMO_TOKEN} reference so the env-resolution and allowlist behaviors can be
// exercised by toggling whether DEMO_TOKEN is set.
const httpToolFixture = `branding: {}
http_tools:
  - name: get_thing
    description: "Get a thing by id"
    method: get
    url: "https://api.example.com/things/{id}"
    headers:
      Authorization: "Bearer ${DEMO_TOKEN}"
      Content-Type: "application/json"
    input_schema:
      type: object
      properties:
        id: { type: string }
      required: ["id"]
    response_jq: ".name"
`

// TestHTTPToolsParse asserts the http_tools[] section parses into the Bundle and,
// when the header's env var is SET, HTTPToolConfigs resolves it host-side.
func TestHTTPToolsParse(t *testing.T) {
	t.Setenv("DEMO_TOKEN", "shh-secret")
	b, err := Load(writeManifest(t, httpToolFixture))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(b.HTTPTools) != 1 {
		t.Fatalf("HTTPTools len = %d, want 1", len(b.HTTPTools))
	}
	d := b.HTTPTools[0]
	if d.Name != "get_thing" {
		t.Errorf("name = %q", d.Name)
	}
	// Method is normalized to upper-case at validate time.
	if d.Method != "GET" {
		t.Errorf("method = %q, want normalized GET", d.Method)
	}

	cfgs := b.HTTPToolConfigs()
	if len(cfgs) != 1 {
		t.Fatalf("HTTPToolConfigs len = %d, want 1", len(cfgs))
	}
	// The env reference resolved host-side to the secret value.
	if got := cfgs[0].Headers["Authorization"]; got != "Bearer shh-secret" {
		t.Errorf("resolved Authorization = %q, want the host-side secret applied", got)
	}
	if cfgs[0].Headers["Content-Type"] != "application/json" {
		t.Errorf("static header dropped: %v", cfgs[0].Headers)
	}
}

// TestHTTPToolsEnvVarNamesSurvivesAllowlist asserts that when the header's env var
// is UNSET at load (the deferred case), its name is surfaced via EnvVarNames so it
// survives the .env-file allowlist and resolves later at call time — exactly as MCP
// server header references do. (When the var IS set, the manifest pass expands it at
// load, so it does not appear in EnvVarNames — that is the SET path above.)
func TestHTTPToolsEnvVarNamesSurvivesAllowlist(t *testing.T) {
	t.Setenv("DEMO_TOKEN", "") // unset/empty => deferred token left intact
	b, err := Load(writeManifest(t, httpToolFixture))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !slices.Contains(b.EnvVarNames(), "DEMO_TOKEN") {
		t.Errorf("EnvVarNames = %v, want DEMO_TOKEN (deferred header secret)", b.EnvVarNames())
	}
}

// TestHTTPToolsValidation covers the fail-fast Load-time checks.
func TestHTTPToolsValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "missing name",
			body: "branding: {}\nhttp_tools:\n  - method: GET\n    url: http://x\n",
		},
		{
			name: "missing method",
			body: "branding: {}\nhttp_tools:\n  - name: t\n    url: http://x\n",
		},
		{
			name: "bad method",
			body: "branding: {}\nhttp_tools:\n  - name: t\n    method: FETCH\n    url: http://x\n",
		},
		{
			name: "missing url",
			body: "branding: {}\nhttp_tools:\n  - name: t\n    method: GET\n",
		},
		{
			name: "bad jq",
			body: "branding: {}\nhttp_tools:\n  - name: t\n    method: GET\n    url: http://x\n    response_jq: \".fields | {\"\n",
		},
		{
			name: "duplicate http tool name",
			body: "branding: {}\nhttp_tools:\n  - name: t\n    method: GET\n    url: http://x\n  - name: t\n    method: GET\n    url: http://y\n",
		},
		{
			name: "collides with mcp server name",
			body: "branding: {}\nmcp_servers:\n  - name: t\n    type: http\n    url: http://m\n    always: true\nhttp_tools:\n  - name: t\n    method: GET\n    url: http://x\n",
		},
		{
			name: "mcp server claims reserved name",
			body: "branding: {}\nmcp_servers:\n  - name: _http\n    type: http\n    url: http://m\n    always: true\n",
		},
		{
			name: "input_schema not object",
			body: "branding: {}\nhttp_tools:\n  - name: t\n    method: GET\n    url: http://x\n    input_schema:\n      type: array\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Load(writeManifest(t, tc.body)); err == nil {
				t.Fatalf("Load accepted invalid manifest (%s); want an error", tc.name)
			}
		})
	}
}

// TestHTTPToolsValidConfigs asserts a valid http_tools section with a no-param
// tool and a separate critical tool loads, and that critical: true folds the
// tool's name into the agent policy's critical-tool suffixes.
func TestHTTPToolsCriticalFoldsIntoPolicy(t *testing.T) {
	body := `branding: {}
http_tools:
  - name: list_things
    method: GET
    url: "https://api.example.com/things"
  - name: delete_thing
    method: DELETE
    url: "https://api.example.com/things/{id}"
    critical: true
    input_schema:
      type: object
      properties:
        id: { type: string }
      required: ["id"]
`
	b, err := Load(writeManifest(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	pol := b.AgentPolicy()
	if !slices.Contains(pol.CriticalToolSuffixes, "delete_thing") {
		t.Errorf("CriticalToolSuffixes = %v, want delete_thing", pol.CriticalToolSuffixes)
	}
	if slices.Contains(pol.CriticalToolSuffixes, "list_things") {
		t.Errorf("non-critical tool wrongly marked critical: %v", pol.CriticalToolSuffixes)
	}
}

// TestDefaultBundleHasNoHTTPTools asserts the generic bundle ships none, so the
// feature is opt-in and the default behavior is unchanged.
func TestDefaultBundleHasNoHTTPTools(t *testing.T) {
	root := repoRoot(t)
	b, err := Load(filepath.Join(root, "config", "default"))
	if err != nil {
		t.Fatalf("load default bundle: %v", err)
	}
	if len(b.HTTPTools) != 0 {
		t.Errorf("default bundle HTTPTools = %d, want 0 (commented-out example only)", len(b.HTTPTools))
	}
	if b.HTTPToolConfigs() != nil {
		t.Errorf("default bundle HTTPToolConfigs should be nil")
	}
}
