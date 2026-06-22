package clientconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultBundleRuntimes(t *testing.T) {
	t.Setenv("FLEET_SANDBOX_IMAGE", "")
	b, err := Load(filepath.Join(repoRoot(t), "config", "default"))
	if err != nil {
		t.Fatalf("load default bundle: %v", err)
	}
	rts := b.Runtimes()
	if len(rts) != 4 {
		t.Fatalf("default runtimes = %d, want 4 (native-inprocess, native-acp, claude-code, goose)", len(rts))
	}
	// native-inprocess is first (canonical order) and the default.
	if rts[0].Name != RuntimeNativeInprocess {
		t.Errorf("runtimes[0] = %q, want %q", rts[0].Name, RuntimeNativeInprocess)
	}
	if b.DefaultRuntime() != RuntimeNativeInprocess {
		t.Errorf("DefaultRuntime() = %q, want %q", b.DefaultRuntime(), RuntimeNativeInprocess)
	}
	acp, ok := b.Runtime(RuntimeNativeACP)
	if !ok {
		t.Fatal("native-acp flavor should be present")
	}
	if acp.Type != RuntimeTypeNativeACP {
		t.Errorf("native-acp type = %q", acp.Type)
	}
	if acp.Image == "" {
		t.Error("native-acp flavor should resolve an image")
	}
	if acp.Network != RuntimeNetworkRestricted {
		t.Errorf("native-acp network = %q, want restricted", acp.Network)
	}
	if acp.DelegatedPolicy {
		t.Error("native-acp must NOT be delegated_policy (it is fully governed)")
	}

	// The two external (containment-tier) provider flavors ship in the generic
	// bundle documenting the wiring: type acp, model_only egress, delegated
	// policy, and the provider's own model-cred env var name(s) declared.
	cc, ok := b.Runtime("claude-code")
	if !ok {
		t.Fatal("claude-code external flavor should be present")
	}
	if cc.Type != RuntimeTypeACP {
		t.Errorf("claude-code type = %q, want acp", cc.Type)
	}
	if !cc.DelegatedPolicy {
		t.Error("claude-code must be delegated_policy (containment tier)")
	}
	if cc.Network != RuntimeNetworkModelOnly {
		t.Errorf("claude-code network = %q, want model_only", cc.Network)
	}
	if len(cc.ModelEnv) != 1 || cc.ModelEnv[0] != "ANTHROPIC_API_KEY" {
		t.Errorf("claude-code model_env = %v, want [ANTHROPIC_API_KEY]", cc.ModelEnv)
	}
	goose, ok := b.Runtime("goose")
	if !ok {
		t.Fatal("goose external flavor should be present")
	}
	if !goose.DelegatedPolicy || goose.Network != RuntimeNetworkModelOnly {
		t.Errorf("goose = %+v, want delegated_policy + model_only", goose)
	}
	if len(goose.Args) != 1 || goose.Args[0] != "acp" {
		t.Errorf("goose args = %v, want [acp]", goose.Args)
	}
}

func TestRuntimesAbsentBlockDefaults(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("branding: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load minimal bundle: %v", err)
	}
	if got := b.DefaultRuntime(); got != RuntimeNativeInprocess {
		t.Errorf("DefaultRuntime() = %q, want native-inprocess fallback", got)
	}
	if len(b.Runtimes()) != 2 {
		t.Errorf("absent block should yield 2 default runtimes, got %d", len(b.Runtimes()))
	}
}

func TestRuntimesExternalACPShape(t *testing.T) {
	dir := t.TempDir()
	manifest := `
runtimes:
  native-inprocess:
    type: native-inprocess
    default: true
  claude-code:
    type: acp
    image: "ghcr.io/acme/claude-code@sha256:abc"
    network: model_only
    delegated_policy: true
    display_name: "Claude Code"
    beta: true
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	cc, ok := b.Runtime("claude-code")
	if !ok {
		t.Fatal("claude-code flavor should be present")
	}
	if cc.Type != RuntimeTypeACP || cc.Image == "" {
		t.Errorf("claude-code = %+v", cc)
	}
	if !cc.DelegatedPolicy {
		t.Error("external acp flavor should carry delegated_policy")
	}
	if cc.Network != RuntimeNetworkModelOnly {
		t.Errorf("claude-code network = %q, want model_only", cc.Network)
	}
	// Canonical order: native-inprocess, then acp-typed external.
	if b.Runtimes()[0].Name != RuntimeNativeInprocess {
		t.Errorf("runtimes[0] = %q", b.Runtimes()[0].Name)
	}
}

func TestRuntimesACPRequiresImage(t *testing.T) {
	dir := t.TempDir()
	manifest := `
runtimes:
  broken:
    type: acp
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("expected error: acp flavor with no image")
	}
}

func TestRuntimesAtMostOneDefault(t *testing.T) {
	dir := t.TempDir()
	manifest := `
runtimes:
  native-inprocess:
    type: native-inprocess
    default: true
  native-acp:
    type: native-acp
    image: "localhost/x:latest"
    default: true
`
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Error("expected error: more than one default runtime")
	}
}
