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
	if len(rts) != 2 {
		t.Fatalf("default runtimes = %d, want 2 (native-inprocess, native-acp)", len(rts))
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
