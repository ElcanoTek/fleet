//go:build fleet_host_executor

package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestWorkspaceFileReaderConfinement(t *testing.T) {
	root := t.TempDir()
	sb := fsTestSandbox(t)
	if err := sb.BindFileOpRoot(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	ctx := WithForcedWorkingDir(context.Background(), root)
	if err := os.WriteFile(filepath.Join(root, "payload"), []byte("exact bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	reader := WorkspaceFileReader(sb)
	data, err := reader(ctx, "payload", 100)
	if err != nil || string(data) != "exact bytes" {
		t.Fatalf("read failed: %q %v", data, err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../secret", filepath.Join(outside, "secret"), "escape", "missing", ""} {
		if _, err := reader(ctx, path, 100); err == nil {
			t.Fatalf("read permitted for %q", path)
		}
	}
	if _, err := reader(ctx, "payload", 3); err == nil {
		t.Fatal("oversized file accepted")
	}
	if _, err := reader(context.Background(), "payload", 100); err == nil {
		t.Fatal("missing workspace accepted")
	}
	if _, err := WorkspaceFileReader(nil)(ctx, "payload", 100); err == nil {
		t.Fatal("nil sandbox accepted")
	}
}
