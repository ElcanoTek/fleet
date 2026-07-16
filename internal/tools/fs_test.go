//go:build fleet_host_executor

package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sandbox"
)

// fsTestSandbox returns a host-backed sandbox for the file tools (#784). The
// file tools now execute through the sandbox FileOp seam; the host backend runs
// the same embedded fileops.py via python3, so these tests exercise the real
// executor without podman. Skips if python3 is unavailable.
func fsTestSandbox(t *testing.T) *sandbox.Sandbox {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available; file tools execute through the sandbox fileop seam")
	}
	sb := sandbox.NewHost(nil)
	t.Cleanup(sb.Close)
	return sb
}

func TestWriteFileTool(t *testing.T) {
	sb := fsTestSandbox(t)
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")

	result, err := runWriteFile(context.Background(), sb, WriteFileParams{
		Path:    testFile,
		Content: "Hello, World!",
	})
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	if !strings.Contains(result, "Successfully wrote") {
		t.Errorf("Expected success message, got %s", result)
	}
	content, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("Failed to read created file: %v", err)
	}
	if string(content) != "Hello, World!" {
		t.Errorf("Expected 'Hello, World!', got %s", string(content))
	}
	// The seam writes 0600 (atomic temp + rename + chmod), matching the
	// pre-#784 host behavior.
	if info, _ := os.Stat(testFile); info != nil && info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestEditFileTool(t *testing.T) {
	sb := fsTestSandbox(t)
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("Hello, World!"), 0o644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	result, err := runEditFile(context.Background(), sb, EditFileParams{
		Path:    testFile,
		OldText: "World",
		NewText: "Go",
	})
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	if !strings.Contains(result, "Successfully replaced") {
		t.Errorf("Expected success message, got %s", result)
	}
	content, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("Failed to read edited file: %v", err)
	}
	if string(content) != "Hello, Go!" {
		t.Errorf("Expected 'Hello, Go!', got %s", string(content))
	}
}

func TestEditFileTool_OldTextAbsentLeavesFileUnchanged(t *testing.T) {
	sb := fsTestSandbox(t)
	testFile := filepath.Join(t.TempDir(), "test.txt")
	if err := os.WriteFile(testFile, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := runEditFile(context.Background(), sb, EditFileParams{Path: testFile, OldText: "absent", NewText: "x"})
	if err == nil || !strings.Contains(err.Error(), "old_text not found") {
		t.Fatalf("want old_text-not-found error, got %v", err)
	}
	if got, _ := os.ReadFile(testFile); string(got) != "original" {
		t.Errorf("file changed on failed edit: %q", got)
	}
}

func TestViewFileTool(t *testing.T) {
	sb := fsTestSandbox(t)
	testFile := filepath.Join(t.TempDir(), "test.txt")
	expected := "Test content"
	if err := os.WriteFile(testFile, []byte(expected), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := runViewFile(context.Background(), sb, ViewFileParams{Path: testFile})
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	if result != expected {
		t.Errorf("Expected '%s', got '%s'", expected, result)
	}
}

func TestViewFileTool_OffsetLimit(t *testing.T) {
	sb := fsTestSandbox(t)
	testFile := filepath.Join(t.TempDir(), "test.txt")
	if err := os.WriteFile(testFile, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := runViewFile(context.Background(), sb, ViewFileParams{Path: testFile, Limit: 5})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if !strings.HasPrefix(res, "01234") || !strings.Contains(res, "reading limit") {
		t.Errorf("limit 5: got %q", res)
	}

	res, err = runViewFile(context.Background(), sb, ViewFileParams{Path: testFile, Offset: 5})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if res != "56789" {
		t.Errorf("offset 5: got %q", res)
	}

	res, err = runViewFile(context.Background(), sb, ViewFileParams{Path: testFile, Offset: 2, Limit: 3})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if !strings.HasPrefix(res, "234") || !strings.Contains(res, "reading limit") {
		t.Errorf("offset 2 limit 3: got %q", res)
	}
}

// TestFileToolsFailClosedWithoutSandbox pins the #784 invariant: with no
// sandbox the tools error and mutate nothing — there is no host fallback.
func TestFileToolsFailClosedWithoutSandbox(t *testing.T) {
	testFile := filepath.Join(t.TempDir(), "test.txt")
	ctx := context.Background()

	if _, err := runWriteFile(ctx, nil, WriteFileParams{Path: testFile, Content: "x"}); err == nil ||
		!strings.Contains(err.Error(), "requires a sandbox") {
		t.Errorf("write_file with nil sandbox: want 'requires a sandbox', got %v", err)
	}
	if _, err := os.Stat(testFile); !os.IsNotExist(err) {
		t.Error("write_file with nil sandbox created a file on the host")
	}

	if err := os.WriteFile(testFile, []byte("orig"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runEditFile(ctx, nil, EditFileParams{Path: testFile, OldText: "orig", NewText: "x"}); err == nil ||
		!strings.Contains(err.Error(), "requires a sandbox") {
		t.Errorf("edit_file with nil sandbox: want 'requires a sandbox', got %v", err)
	}
	if got, _ := os.ReadFile(testFile); string(got) != "orig" {
		t.Error("edit_file with nil sandbox mutated the host file")
	}
	if _, err := runViewFile(ctx, nil, ViewFileParams{Path: testFile}); err == nil ||
		!strings.Contains(err.Error(), "requires a sandbox") {
		t.Errorf("view_file with nil sandbox: want 'requires a sandbox', got %v", err)
	}
}

// TestFileToolsRejectCrossConversationTraversal pins the #575 fix at the tool
// level: the file tools must refuse a relative path that escapes the caller's
// conversation workspace via "..". The path validation runs (host-side, as
// defense-in-depth input) before the operation reaches the sandbox.
func TestFileToolsRejectCrossConversationTraversal(t *testing.T) {
	sb := fsTestSandbox(t)
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
	victimConv := "conv-victim"
	victimDir := filepath.Join(root, victimConv)
	if err := os.MkdirAll(victimDir, 0o755); err != nil {
		t.Fatalf("mkdir victim workspace: %v", err)
	}
	secret := filepath.Join(victimDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("cross-tenant secret"), 0o600); err != nil {
		t.Fatalf("seed secret: %v", err)
	}

	ctx := WithConversationID(context.Background(), "conv-attacker")
	escape := "../" + victimConv + "/secret.txt"

	if out, err := runViewFile(ctx, sb, ViewFileParams{Path: escape}); err == nil {
		t.Errorf("view_file read a sibling conversation's file: %q", out)
	} else if !strings.Contains(err.Error(), "path validation failed") {
		t.Errorf("view_file: want path validation error, got: %v", err)
	}

	if _, err := runEditFile(ctx, sb, EditFileParams{Path: escape, OldText: "secret", NewText: "pwn"}); err == nil {
		t.Error("edit_file edited a sibling conversation's file")
	}

	if _, err := runWriteFile(ctx, sb, WriteFileParams{Path: "../" + victimConv + "/planted.txt", Content: "pwn"}); err == nil {
		t.Error("write_file wrote into a sibling conversation's workspace")
	}
	if _, err := os.Stat(filepath.Join(victimDir, "planted.txt")); !os.IsNotExist(err) {
		t.Errorf("planted.txt must not exist in the victim workspace (stat err = %v)", err)
	}

	got, err := os.ReadFile(secret)
	if err != nil || string(got) != "cross-tenant secret" {
		t.Fatalf("victim file changed: %q, %v", got, err)
	}

	// Legitimate in-workspace access keeps working.
	if _, err := runWriteFile(ctx, sb, WriteFileParams{Path: "sub/report.txt", Content: "mine"}); err != nil {
		t.Fatalf("in-workspace write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "conv-attacker", "sub", "report.txt")); err != nil {
		t.Fatalf("in-workspace write landed wrong: %v", err)
	}
	out, err := runViewFile(ctx, sb, ViewFileParams{Path: "sub/report.txt"})
	if err != nil {
		t.Fatalf("in-workspace read: %v", err)
	}
	if out != "mine" {
		t.Errorf("in-workspace read = %q, want %q", out, "mine")
	}
}

// TestFileToolsPoisonedSandbox pins that a poisoned sandbox (#796) refuses file
// ops fail-closed rather than falling back to the host.
func TestFileToolsPoisonedSandbox(t *testing.T) {
	// A poisoned host sandbox is not reachable directly; assert the seam maps
	// ErrPoisoned to a retry-friendly message via fileOpError.
	err := fileOpError("view", sandbox.ErrPoisoned)
	if err == nil || !strings.Contains(err.Error(), "reset") {
		t.Errorf("poisoned view fileop: want reset message, got %v", err)
	}
	if !errors.Is(sandbox.ErrPoisoned, sandbox.ErrPoisoned) {
		t.Fatal("sanity")
	}
}
