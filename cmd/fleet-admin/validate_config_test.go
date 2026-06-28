package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
)

// writeBundle writes a minimal bundle dir with the given manifest body and any
// extra files (relative path -> contents), returning the bundle dir. It is the
// no-Postgres fixture the pure validation tests load.
func writeBundle(t *testing.T, manifest string, extra map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	for rel, body := range extra {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	return dir
}

// TestCheckBundleShapeClean asserts a well-formed bundle (a stdio server whose
// script arg resolves on disk) produces no failures. This exercises the pure,
// no-Postgres validation path validate-config layers on top of the doctor checks.
func TestCheckBundleShapeClean(t *testing.T) {
	dir := writeBundle(t, "mcp_servers:\n"+
		"  - name: present\n"+
		"    command: python\n"+
		"    args: [\"mcp/present.py\"]\n",
		map[string]string{"mcp/present.py": "# ok\n"})

	b, err := clientconfig.Load(dir)
	if err != nil {
		t.Fatalf("Load clean bundle: %v", err)
	}
	r := &report{}
	checkBundleShape(r, b)
	if r.failed != 0 {
		t.Errorf("checkBundleShape on clean bundle: failed=%d, want 0", r.failed)
	}
}

// TestCheckBundleShapeMissingScript asserts an unresolved stdio script-path arg —
// which clientconfig.Load only LOGS as a warning — is promoted to a blocking
// failure by validate-config.
func TestCheckBundleShapeMissingScript(t *testing.T) {
	dir := writeBundle(t, "mcp_servers:\n"+
		"  - name: broken\n"+
		"    command: python\n"+
		"    args: [\"mcp/missing.py\"]\n", nil)

	b, err := clientconfig.Load(dir)
	if err != nil {
		t.Fatalf("Load bundle with missing script: %v", err)
	}
	r := &report{}
	checkBundleShape(r, b)
	if r.failed == 0 {
		t.Error("checkBundleShape on bundle with missing script arg: failed=0, want >0")
	}
}

// TestCheckBundleShapeNilBundle asserts a nil bundle (the load already failed and
// was reported by checkBundle) is a no-op — no double-counted failure.
func TestCheckBundleShapeNilBundle(t *testing.T) {
	r := &report{}
	checkBundleShape(r, nil)
	if r.failed != 0 {
		t.Errorf("checkBundleShape(nil): failed=%d, want 0", r.failed)
	}
}
