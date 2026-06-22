package clientconfig

import (
	"os"
	"path/filepath"
	"testing"
)

// repoRoot walks up from the test's cwd (the package dir) to the repo root so
// the test can load the shipped config/default bundle regardless of where `go
// test` is invoked from.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate repo root (no go.mod above %s)", dir)
		}
		dir = parent
	}
}

func TestLoadDefaultBundle(t *testing.T) {
	root := repoRoot(t)
	b, err := Load(filepath.Join(root, "config", "default"))
	if err != nil {
		t.Fatalf("load default bundle: %v", err)
	}
	if b.Branding.AppName != "Fleet" {
		t.Errorf("AppName = %q, want Fleet", b.Branding.AppName)
	}
	if b.Branding.LoginTagline == "" {
		t.Error("LoginTagline should be set")
	}
	// Generic default ships an EMPTY MCP catalog.
	if len(b.MCPCatalog) != 0 {
		t.Errorf("default MCPCatalog should be empty, got %d", len(b.MCPCatalog))
	}
	if cfgs := b.MCPServerConfigs(); len(cfgs) != 0 {
		t.Errorf("default MCPServerConfigs should be empty, got %d", len(cfgs))
	}
	// Empty-state cards are neutral examples (passed through opaque).
	if len(b.EmptyState.Cards) == 0 {
		t.Error("default empty_state.cards should have neutral examples")
	}
	// Resolved dirs point inside the bundle.
	if filepath.Base(b.SystemPromptsDir) != "system_prompts" {
		t.Errorf("SystemPromptsDir = %q", b.SystemPromptsDir)
	}
}

func TestLoadDefaultsBlankDir(t *testing.T) {
	// Blank dir resolves via FLEET_CLIENT_CONFIG_DIR / DefaultDir.
	t.Setenv(EnvDir, filepath.Join(repoRoot(t), "config", "default"))
	b, err := Load("")
	if err != nil {
		t.Fatalf("load via env: %v", err)
	}
	if b.Branding.AppName == "" {
		t.Error("AppName should be set")
	}
}

func TestBrandingDefaultsForSparseManifest(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("branding: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load sparse: %v", err)
	}
	if b.Branding.AppName != "Fleet" {
		t.Errorf("sparse AppName = %q, want neutral default Fleet", b.Branding.AppName)
	}
	if b.Branding.ShareTitle != "Fleet" {
		t.Errorf("ShareTitle should fall back to AppName, got %q", b.Branding.ShareTitle)
	}
}

func TestMCPCatalogEnableGateAndEnv(t *testing.T) {
	dir := t.TempDir()
	manifest := `
mcp_servers:
  - name: always_on
    command: python3
    args: ["mcp/a.py"]
    always: true
    env:
      A_OUT: "${A_SECRET}"
      A_LITERAL: "fixed"
      A_OPT: "${A_MISSING}"
    optional_env: ["A_OPT"]
    tools: ["t1", "t2"]
  - name: gated_off
    command: python3
    args: ["mcp/b.py"]
    enabled_env: ["B_TOKEN"]
  - name: gated_on
    command: python3
    args: ["mcp/c.py"]
    enabled_env: ["C_TOKEN"]
  - name: groups_or
    command: python3
    args: ["mcp/d.py"]
    enabled_groups:
      - ["D_TOKEN"]
      - ["D_USER", "D_PASS"]
  - name: http_one
    type: http
    url: "https://example.test/mcp"
    enabled_env: ["H_TOKEN"]
    headers:
      Authorization: "Bearer ${H_TOKEN}"
    tools: ["x"]
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("A_SECRET", "sekret")
	t.Setenv("C_TOKEN", "ctok")
	t.Setenv("D_USER", "u")
	t.Setenv("D_PASS", "p")
	t.Setenv("H_TOKEN", "htok")
	// B_TOKEN intentionally unset → gated_off must be excluded.

	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cfgs := b.MCPServerConfigs()

	if _, ok := cfgs["gated_off"]; ok {
		t.Error("gated_off should be excluded (B_TOKEN unset)")
	}
	for _, name := range []string{"always_on", "gated_on", "groups_or", "http_one"} {
		if _, ok := cfgs[name]; !ok {
			t.Errorf("%s should be enabled", name)
		}
	}

	a := cfgs["always_on"]
	if a.Type != "stdio" || a.Command != "python3" {
		t.Errorf("always_on spawn spec wrong: %+v", a)
	}
	if a.Env["A_OUT"] != "sekret" {
		t.Errorf("A_OUT interpolation = %q, want sekret", a.Env["A_OUT"])
	}
	if a.Env["A_LITERAL"] != "fixed" {
		t.Errorf("A_LITERAL = %q, want fixed", a.Env["A_LITERAL"])
	}
	if _, ok := a.Env["A_OPT"]; ok {
		t.Error("A_OPT should be dropped (empty + optional)")
	}
	if len(a.ToolAllowlist) != 2 {
		t.Errorf("always_on tools = %v", a.ToolAllowlist)
	}

	h := cfgs["http_one"]
	if h.Type != "http" || h.URL != "https://example.test/mcp" {
		t.Errorf("http_one spec wrong: %+v", h)
	}
	if h.Headers["Authorization"] != "Bearer htok" {
		t.Errorf("http_one header = %q", h.Headers["Authorization"])
	}
}

func TestMissingManifestIsError(t *testing.T) {
	if _, err := Load(t.TempDir()); err == nil {
		t.Error("expected error for bundle with no manifest.yaml")
	}
}

func TestDuplicateServerNameRejected(t *testing.T) {
	dir := t.TempDir()
	manifest := `
mcp_servers:
  - name: dup
    command: python3
    args: ["a.py"]
    always: true
  - name: dup
    command: python3
    args: ["b.py"]
    always: true
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("expected duplicate-name error")
	}
}
