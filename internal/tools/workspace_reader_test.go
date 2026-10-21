//go:build fleet_host_executor

package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
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
	window, err := reader(ctx, "payload", 0, 11, 100)
	sum := sha256.Sum256([]byte("exact bytes"))
	if err != nil || string(window.Data) != "exact bytes" || window.Size != 11 || window.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("read failed: %+v %v", window, err)
	}
	// Only the requested window crosses out; the hash still names the whole file.
	window, err = reader(ctx, "payload", 6, 5, 100)
	if err != nil || string(window.Data) != "bytes" || window.Size != 11 || window.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("window read failed: %+v %v", window, err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../secret", filepath.Join(outside, "secret"), "escape", "missing", ""} {
		if _, err := reader(ctx, path, 0, 1, 100); err == nil {
			t.Fatalf("read permitted for %q", path)
		}
	}
	// The cap is on the whole file, so even a 1-byte window of it is refused.
	if _, err := reader(ctx, "payload", 0, 1, 10); err == nil || !strings.Contains(err.Error(), "exceeding the 10-byte cap") {
		t.Fatalf("oversized file accepted: %v", err)
	}
	for _, r := range [][3]int64{{-1, 1, 100}, {0, 0, 100}, {0, 101, 100}, {100, 1, 100}, {0, 1, 0}} {
		if _, err := reader(ctx, "payload", r[0], r[1], r[2]); err == nil {
			t.Fatalf("invalid range %v accepted", r)
		}
	}
	if _, err := reader(context.Background(), "payload", 0, 1, 100); err == nil {
		t.Fatal("missing workspace accepted")
	}
	if _, err := WorkspaceFileReader(nil)(ctx, "payload", 0, 1, 100); err == nil {
		t.Fatal("nil sandbox accepted")
	}
}
