package clientconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadRejectsUnknownManifestKey is the regression guard for strict manifest
// parsing: a typo'd / unmodeled key must FAIL the load loudly rather than being
// silently dropped (which could, e.g., leave a `tools:` allowlist unset and
// expose a connector's full tool surface).
func TestLoadRejectsUnknownManifestKey(t *testing.T) {
	dir := t.TempDir()
	// `toolz` is a typo for `tools` under an mcp_servers entry.
	bad := "branding: {}\n" +
		"mcp_servers:\n" +
		"  - name: demo\n" +
		"    type: stdio\n" +
		"    command: python3\n" +
		"    args: [\"mcp/demo.py\"]\n" +
		"    toolz: [\"only_this\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("Load accepted a manifest with an unknown key (toolz); want a strict-parse error")
	}
}

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
	if filepath.Base(b.SkillsDir) != "skills" {
		t.Errorf("SkillsDir = %q", b.SkillsDir)
	}
}

// TestDefaultBundleTaskTemplates asserts the shipped generic bundle parses its
// task_templates block (#262): the five neutral starters load in manifest order
// and carry the partial-TaskCreate fields the UI pre-fills. This is the parse
// half of the feature's "test the template parse + the from-template field
// application" requirement.
func TestDefaultBundleTaskTemplates(t *testing.T) {
	root := repoRoot(t)
	b, err := Load(filepath.Join(root, "config", "default"))
	if err != nil {
		t.Fatalf("load default bundle: %v", err)
	}
	if len(b.TaskTemplates) != 5 {
		t.Fatalf("default bundle TaskTemplates = %d, want 5", len(b.TaskTemplates))
	}
	// First entry is the Daily Standup starter — assert its fields decoded.
	first := b.TaskTemplates[0]
	if first.Name != "Daily Standup" {
		t.Errorf("TaskTemplates[0].Name = %q, want Daily Standup", first.Name)
	}
	if first.Icon == "" || first.Description == "" {
		t.Errorf("TaskTemplates[0] missing icon/description: %+v", first)
	}
	if first.Task.Prompt == "" {
		t.Error("TaskTemplates[0].Task.Prompt should be set")
	}
	if first.Task.Recurrence != "0 8 * * 1-5" {
		t.Errorf("TaskTemplates[0].Task.Recurrence = %q", first.Task.Recurrence)
	}
	if first.Task.MaxIterations == nil || *first.Task.MaxIterations != 15 {
		t.Errorf("TaskTemplates[0].Task.MaxIterations = %v, want 15", first.Task.MaxIterations)
	}
	if len(first.Task.Tags) == 0 {
		t.Error("TaskTemplates[0].Task.Tags should be set")
	}
	// Every shipped template must name itself and carry a non-empty prompt.
	for i, tmpl := range b.TaskTemplates {
		if tmpl.Name == "" {
			t.Errorf("TaskTemplates[%d].Name is empty", i)
		}
		if tmpl.Task.Prompt == "" {
			t.Errorf("TaskTemplates[%d] (%q) has an empty prompt", i, tmpl.Name)
		}
	}
}

// TestTaskTemplateOptionalPointersDistinguishUnset confirms an omitted optional
// key (model, max_iterations, …) stays nil rather than collapsing to a zero
// value, so the UI can tell "template said nothing, keep my form default" from
// "template explicitly set zero".
func TestTaskTemplateOptionalPointersDistinguishUnset(t *testing.T) {
	dir := t.TempDir()
	manifest := `
task_templates:
  - name: "Bare"
    description: "only a prompt"
    task:
      prompt: "do the thing"
  - name: "Pinned"
    description: "model + iterations set"
    task:
      prompt: "review {repo_path}"
      model: "anthropic/claude-opus-4.8"
      max_iterations: 7
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(b.TaskTemplates) != 2 {
		t.Fatalf("TaskTemplates = %d, want 2", len(b.TaskTemplates))
	}
	bare := b.TaskTemplates[0].Task
	if bare.Model != nil {
		t.Errorf("Bare.Model = %v, want nil (key omitted)", *bare.Model)
	}
	if bare.MaxIterations != nil {
		t.Errorf("Bare.MaxIterations = %v, want nil (key omitted)", *bare.MaxIterations)
	}
	pinned := b.TaskTemplates[1].Task
	if pinned.Model == nil || *pinned.Model != "anthropic/claude-opus-4.8" {
		t.Errorf("Pinned.Model = %v, want the pinned slug", pinned.Model)
	}
	if pinned.MaxIterations == nil || *pinned.MaxIterations != 7 {
		t.Errorf("Pinned.MaxIterations = %v, want 7", pinned.MaxIterations)
	}
}

// TestTaskTemplatesAbsentSectionIsEmpty: a bundle with no task_templates key
// loads fine and exposes an empty (nil) catalog — the generic no-templates case
// the orchestrator turns into a [] response.
func TestTaskTemplatesAbsentSectionIsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("branding: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(b.TaskTemplates) != 0 {
		t.Errorf("TaskTemplates = %d, want 0 for a bundle with no section", len(b.TaskTemplates))
	}
}

// TestDefaultBundleSkillsValid asserts the shipped generic bundle's skills/ dir
// is well-formed: it ships the example skill, and ValidateSkills finds no
// problems. This is the skills analogue of the ValidateMCPArgPaths CI guard.
func TestDefaultBundleSkillsValid(t *testing.T) {
	root := repoRoot(t)
	b, err := Load(filepath.Join(root, "config", "default"))
	if err != nil {
		t.Fatalf("load default bundle: %v", err)
	}
	if problems := b.ValidateSkills(); len(problems) != 0 {
		t.Errorf("default bundle skills should be clean, got problems: %v", problems)
	}
	skills := b.Skills()
	if len(skills) == 0 {
		t.Fatal("default bundle should ship at least the example skill")
	}
	var found bool
	for _, sk := range skills {
		if sk.Name == "example-skill" {
			found = true
			if sk.Path != filepath.Join("skills", "example-skill", "SKILL.md") {
				t.Errorf("example-skill Path = %q", sk.Path)
			}
			if sk.Description == "" {
				t.Error("example-skill Description should be non-empty")
			}
		}
	}
	if !found {
		t.Errorf("example-skill not found among %d skills", len(skills))
	}
}

// TestReadSkills exercises the parser/validator against a hand-built skills dir
// covering the well-formed case and every problem class.
func TestReadSkills(t *testing.T) {
	dir := t.TempDir()
	skills := filepath.Join(dir, "skills")

	writeSkill := func(name, body string) {
		t.Helper()
		p := filepath.Join(skills, name, "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// Well-formed.
	writeSkill("good-skill", "---\nname: good-skill\ndescription: Does a good thing when X.\n---\n\n# Good\n")
	// Name does not match folder → skipped + problem.
	writeSkill("mismatch", "---\nname: not-mismatch\ndescription: whatever\n---\n")
	// Empty description → skipped + problem.
	writeSkill("no-desc", "---\nname: no-desc\ndescription: \"\"\n---\n")
	// No frontmatter → skipped + problem.
	writeSkill("no-front", "# just a heading, no frontmatter\n")
	// Folder with no SKILL.md → problem.
	if err := os.MkdirAll(filepath.Join(skills, "empty-folder"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Stray file at skills/ root → ignored silently (not a skill, not a problem).
	if err := os.WriteFile(filepath.Join(skills, "README.md"), []byte("docs\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, problems := ReadSkills(skills)
	if len(got) != 1 || got[0].Name != "good-skill" {
		t.Fatalf("expected only good-skill in roster, got %+v", got)
	}
	if got[0].Path != filepath.Join("skills", "good-skill", "SKILL.md") {
		t.Errorf("good-skill path = %q", got[0].Path)
	}
	// Four malformed entries each produce at least one problem; the README and the
	// good skill produce none.
	if len(problems) < 4 {
		t.Errorf("expected >=4 problems for the malformed skills, got %d: %v", len(problems), problems)
	}

	// An absent skills/ dir is not a problem.
	none, noProblems := ReadSkills(filepath.Join(dir, "does-not-exist"))
	if len(none) != 0 || len(noProblems) != 0 {
		t.Errorf("absent skills dir should yield (nil,nil), got skills=%v problems=%v", none, noProblems)
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

// TestDefaultBranding locks the exported no-bundle branding helper to the SAME
// defaults a sparse manifest gets (one source of truth — the HTTP no-bundle
// fallback builds from this rather than a divergent hardcoded literal, #134).
func TestDefaultBranding(t *testing.T) {
	d := DefaultBranding()
	if d.AppName != "Fleet" {
		t.Errorf("DefaultBranding AppName = %q, want Fleet", d.AppName)
	}
	if d.ShareTitle != "Fleet" {
		t.Errorf("DefaultBranding ShareTitle = %q, want Fleet (= AppName)", d.ShareTitle)
	}
	if d.LoginTitle == "" || d.ShareDescription == "" {
		t.Errorf("DefaultBranding left a field empty: %+v", d)
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
    optional: true
    enabled_by_default: true
    display_name: "Always On"
    description: "an optional connector"
    beta: true
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
	// Optional-server metadata must propagate into the runtime config (the
	// 128-tool-ceiling regression: these were silently dropped here).
	if !a.Optional || !a.EnabledByDefault || !a.Beta {
		t.Errorf("always_on optional flags dropped: optional=%v enabled_by_default=%v beta=%v", a.Optional, a.EnabledByDefault, a.Beta)
	}
	if a.DisplayName != "Always On" || a.Description != "an optional connector" {
		t.Errorf("always_on optional labels dropped: display=%q desc=%q", a.DisplayName, a.Description)
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

func TestSandboxRuntimeParsedFromManifest(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sandbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sandbox", "Containerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := `
sandbox:
  tag: localhost/x:latest
  runtime: kata
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := b.Sandbox().Runtime; got != "kata" {
		t.Errorf("Sandbox().Runtime = %q, want kata", got)
	}
}

func TestSandboxRuntimeOmittedIsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sandbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sandbox", "Containerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A sandbox block with no runtime: key leaves Runtime empty (podman default).
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("sandbox:\n  tag: localhost/x:latest\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := b.Sandbox().Runtime; got != "" {
		t.Errorf("Sandbox().Runtime = %q, want empty", got)
	}
}

func TestSandboxRuntimeFromEnvInterpolation(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sandbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sandbox", "Containerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Mirror the default bundle's `runtime: "${FLEET_SANDBOX_RUNTIME:-}"` form.
	manifest := `
sandbox:
  tag: localhost/x:latest
  runtime: "${FLEET_SANDBOX_RUNTIME:-}"
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_SANDBOX_RUNTIME", "libkrun")
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// clientconfig stores the value VERBATIM (normalization happens at consume time).
	if got := b.Sandbox().Runtime; got != "libkrun" {
		t.Errorf("Sandbox().Runtime = %q, want libkrun (verbatim)", got)
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

// TestValidateMCPArgPaths is the regression guard for #90: a stdio server whose
// relative script arg is missing from the bundle is flagged at load time (so a
// misspelled/absent mcp/*.py is caught, not left as a silent runtime launch
// failure), while a present script + the shipped generic bundle report none.
func TestValidateMCPArgPaths(t *testing.T) {
	root := repoRoot(t)
	// The shipped generic bundle must be clean.
	def, err := Load(filepath.Join(root, "config", "default"))
	if err != nil {
		t.Fatalf("load config/default: %v", err)
	}
	if probs := def.ValidateMCPArgPaths(); len(probs) != 0 {
		t.Fatalf("config/default should have no MCP arg-path problems, got: %v", probs)
	}

	// A bundle referencing a present vs a missing script.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "mcp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp", "present.py"), []byte("# ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := "branding: {}\n" +
		"mcp_servers:\n" +
		"  - name: good\n    type: stdio\n    command: python3\n    args: [\"mcp/present.py\"]\n" +
		"  - name: bad\n    type: stdio\n    command: python3\n    args: [\"mcp/missing.py\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load synthetic bundle: %v", err)
	}
	probs := b.ValidateMCPArgPaths()
	if len(probs) != 1 {
		t.Fatalf("expected exactly 1 problem (the missing script), got %d: %v", len(probs), probs)
	}
	if !strings.Contains(probs[0], "missing.py") || !strings.Contains(probs[0], `"bad"`) {
		t.Fatalf("problem should name the bad server + missing script, got: %s", probs[0])
	}
}

// TestPricingBlockParsesAndNormalizes confirms the manifest pricing: block loads
// into the bundle, the accessor lower-cases the fallback, and the per-million
// rates survive the round-trip (#297).
func TestPricingBlockParsesAndNormalizes(t *testing.T) {
	dir := t.TempDir()
	manifest := "branding: {}\n" +
		"pricing:\n" +
		"  fallback: OpenRouter\n" +
		"  overrides:\n" +
		"    - model: \"anthropic/claude-opus-4-8\"\n" +
		"      input_cost_per_million_tokens: 7.50\n" +
		"      output_cost_per_million_tokens: 22.50\n" +
		"      cache_read_cost_per_million_tokens: 0.75\n" +
		"      cache_write_cost_per_million_tokens: 1.875\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load pricing bundle: %v", err)
	}
	p := b.Pricing()
	if p.Fallback != "openrouter" {
		t.Errorf("fallback = %q, want normalized %q", p.Fallback, "openrouter")
	}
	if len(p.Overrides) != 1 {
		t.Fatalf("overrides = %d, want 1", len(p.Overrides))
	}
	o := p.Overrides[0]
	if o.Model != "anthropic/claude-opus-4-8" {
		t.Errorf("model = %q", o.Model)
	}
	if o.InputCostPerMillionTokens != 7.50 || o.OutputCostPerMillionTokens != 22.50 {
		t.Errorf("input/output rates = %v/%v, want 7.5/22.5", o.InputCostPerMillionTokens, o.OutputCostPerMillionTokens)
	}
	if o.CacheReadCostPerMillionTokens != 0.75 || o.CacheWriteCostPerMillionTokens != 1.875 {
		t.Errorf("cache rates = %v/%v, want 0.75/1.875", o.CacheReadCostPerMillionTokens, o.CacheWriteCostPerMillionTokens)
	}
}

// TestPricingAbsentBlockIsEmptyDefault confirms a bundle with no pricing: block
// loads with an empty config (no overrides, blank fallback) — the agentcore layer
// then applies its OpenRouter default, so behavior is unchanged.
func TestPricingAbsentBlockIsEmptyDefault(t *testing.T) {
	root := repoRoot(t)
	b, err := Load(filepath.Join(root, "config", "default"))
	if err != nil {
		t.Fatalf("load config/default: %v", err)
	}
	p := b.Pricing()
	if len(p.Overrides) != 0 {
		t.Errorf("default bundle should ship no pricing overrides, got %d", len(p.Overrides))
	}
	if p.Fallback != "" {
		t.Errorf("default bundle fallback = %q, want empty (OpenRouter default applied downstream)", p.Fallback)
	}
}

// TestPricingRejectsUnknownFallback guards the strict-validation posture: a typo'd
// fallback fails the load loudly rather than silently mis-accounting cost.
func TestPricingRejectsUnknownFallback(t *testing.T) {
	dir := t.TempDir()
	manifest := "branding: {}\n" +
		"pricing:\n" +
		"  fallback: free\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("Load accepted an unknown pricing.fallback (free); want a validation error")
	}
}

// TestPricingRejectsNegativeRateAndMissingModel guards the other two validation
// rules: an empty model slug and a negative rate both fail the load.
func TestPricingRejectsNegativeRateAndMissingModel(t *testing.T) {
	cases := map[string]string{
		"missing model": "branding: {}\n" +
			"pricing:\n" +
			"  overrides:\n" +
			"    - model: \"\"\n" +
			"      input_cost_per_million_tokens: 1\n",
		"negative rate": "branding: {}\n" +
			"pricing:\n" +
			"  overrides:\n" +
			"    - model: \"x/y\"\n" +
			"      input_cost_per_million_tokens: -1\n",
	}
	for name, manifest := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(dir); err == nil {
				t.Fatalf("Load accepted a malformed pricing override (%s); want a validation error", name)
			}
		})
	}
}
