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

func TestSandboxDefaultBundleResolvesToTag(t *testing.T) {
	root := repoRoot(t)
	// Ensure no FLEET_SANDBOX_IMAGE override leaks in from the env so the
	// default bundle's sandbox.image (${FLEET_SANDBOX_IMAGE:-}) resolves empty
	// and ResolvedImageRef falls back to the tag.
	t.Setenv("FLEET_SANDBOX_IMAGE", "")
	b, err := Load(filepath.Join(root, "config", "default"))
	if err != nil {
		t.Fatalf("load default bundle: %v", err)
	}
	sb := b.Sandbox()
	if sb.Tag != "localhost/fleet-sandbox:latest" {
		t.Errorf("Tag = %q, want localhost/fleet-sandbox:latest", sb.Tag)
	}
	if sb.Image != "" {
		t.Errorf("Image = %q, want empty (build-on-box default)", sb.Image)
	}
	if got := sb.ResolvedImageRef(); got != "localhost/fleet-sandbox:latest" {
		t.Errorf("ResolvedImageRef() = %q, want the tag", got)
	}
	if filepath.Base(sb.ContainerfileAbsPath) != "Containerfile" {
		t.Errorf("ContainerfileAbsPath = %q, want .../Containerfile", sb.ContainerfileAbsPath)
	}
	if _, err := os.Stat(sb.ContainerfileAbsPath); err != nil {
		t.Errorf("default bundle Containerfile should exist: %v", err)
	}
}

func TestSandboxImageOverrideWins(t *testing.T) {
	dir := t.TempDir()
	// An explicit image override means the Containerfile is NOT required.
	manifest := `
sandbox:
  containerfile: sandbox/Containerfile
  tag: localhost/fleet-sandbox:latest
  image: "ghcr.io/acme/sandbox@sha256:deadbeef"
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	sb := b.Sandbox()
	if sb.Image != "ghcr.io/acme/sandbox@sha256:deadbeef" {
		t.Errorf("Image = %q", sb.Image)
	}
	if got := sb.ResolvedImageRef(); got != "ghcr.io/acme/sandbox@sha256:deadbeef" {
		t.Errorf("ResolvedImageRef() = %q, want the image override to win over the tag", got)
	}
}

func TestSandboxImageOverrideViaEnv(t *testing.T) {
	dir := t.TempDir()
	// The generic-style manifest defers the image to ${FLEET_SANDBOX_IMAGE:-};
	// when that env is set the override resolves at Load time and wins.
	manifest := `
sandbox:
  tag: localhost/fleet-sandbox:latest
  image: "${FLEET_SANDBOX_IMAGE:-}"
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_SANDBOX_IMAGE", "ghcr.io/acme/sandbox:pinned")
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := b.Sandbox().ResolvedImageRef(); got != "ghcr.io/acme/sandbox:pinned" {
		t.Errorf("ResolvedImageRef() = %q, want the env-provided image", got)
	}
}

func TestSandboxMissingContainerfileWithEmptyImageErrors(t *testing.T) {
	dir := t.TempDir()
	// No image override AND the Containerfile does not exist => Load must fail.
	manifest := `
sandbox:
  containerfile: sandbox/Containerfile
  tag: localhost/fleet-sandbox:latest
  image: ""
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("expected error: missing containerfile with empty image")
	}
}

func TestSandboxOmittedBlockIsNotEnforced(t *testing.T) {
	dir := t.TempDir()
	// A minimal/legacy bundle with NO sandbox: block must still load (it never
	// opted into the sandbox-as-config contract). The descriptor falls back to
	// the conventional defaults so a consumer still gets a usable tag.
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("branding: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("minimal bundle with no sandbox block should load: %v", err)
	}
	sb := b.Sandbox()
	if sb.Tag != "localhost/fleet-sandbox:latest" {
		t.Errorf("default Tag = %q", sb.Tag)
	}
	if got := sb.ResolvedImageRef(); got != "localhost/fleet-sandbox:latest" {
		t.Errorf("ResolvedImageRef() = %q", got)
	}
}

func TestSandboxDeclaredBlockRequiresContainerfile(t *testing.T) {
	dir := t.TempDir()
	// A DECLARED sandbox block (with no image override) enforces the Containerfile
	// invariant: drop one at the conventional path and Load succeeds.
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("sandbox:\n  tag: localhost/x:latest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("expected error: declared sandbox block, no image override, missing Containerfile")
	}
	if err := os.MkdirAll(filepath.Join(dir, "sandbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sandbox", "Containerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load with default Containerfile: %v", err)
	}
	if got := b.Sandbox().Tag; got != "localhost/x:latest" {
		t.Errorf("Tag = %q, want localhost/x:latest", got)
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

// TestAllowUngovernedScheduledAgents_DefaultFalse pins the FAIL-CLOSED default:
// a manifest that does NOT set the flag (the generic-bundle case) leaves
// AllowUngovernedScheduledAgents false, so the scheduled-external gate refuses to
// run an external flavor on the scheduler.
func TestAllowUngovernedScheduledAgents_DefaultFalse(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("branding: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if b.AgentPolicy().AllowUngovernedScheduledAgents {
		t.Error("default must be false (fail-closed): an unset flag must not admit ungoverned scheduled agents")
	}
}

// TestAllowUngovernedScheduledAgents_OptIn proves the manifest opt-in plumbs
// through agent_policy → Bundle.AgentPolicy().
func TestAllowUngovernedScheduledAgents_OptIn(t *testing.T) {
	dir := t.TempDir()
	manifest := `
agent_policy:
  allow_ungoverned_scheduled_agents: true
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !b.AgentPolicy().AllowUngovernedScheduledAgents {
		t.Error("agent_policy.allow_ungoverned_scheduled_agents=true must plumb through to AgentPolicy()")
	}
}
