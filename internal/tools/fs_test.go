package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFileTool(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")

	result, err := runWriteFile(context.Background(), WriteFileParams{
		Path:    testFile,
		Content: "Hello, World!",
	})

	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	if !strings.Contains(result, "Successfully wrote") {
		t.Errorf("Expected success message, got %s", result)
	}

	// Verify file was created
	content, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("Failed to read created file: %v", err)
	}

	if string(content) != "Hello, World!" {
		t.Errorf("Expected 'Hello, World!', got %s", string(content))
	}
}

func TestEditFileTool(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")

	// Create a file first
	if err := os.WriteFile(testFile, []byte("Hello, World!"), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	result, err := runEditFile(context.Background(), EditFileParams{
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

	// Verify file was edited
	content, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("Failed to read edited file: %v", err)
	}

	if string(content) != "Hello, Go!" {
		t.Errorf("Expected 'Hello, Go!', got %s", string(content))
	}
}

func TestViewFileTool(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")
	expectedContent := "Test content"

	// Create a file
	if err := os.WriteFile(testFile, []byte(expectedContent), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	result, err := runViewFile(context.Background(), ViewFileParams{
		Path: testFile,
	})

	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}

	if result != expectedContent {
		t.Errorf("Expected '%s', got '%s'", expectedContent, result)
	}
}

func TestViewFileTool_OffsetLimit(t *testing.T) {
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")
	// Create content: "0123456789"
	content := "0123456789"
	if err := os.WriteFile(testFile, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	// Case 1: Limit 5
	res, err := runViewFile(context.Background(), ViewFileParams{
		Path:  testFile,
		Limit: 5,
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	// Content should be "01234" + truncated msg
	if !strings.HasPrefix(res, "01234") {
		t.Errorf("Expected prefix '01234', got '%s'", res)
	}
	if !strings.Contains(res, "reading limit") {
		t.Errorf("Expected truncated message, got '%s'", res)
	}

	// Case 2: Offset 5
	res, err = runViewFile(context.Background(), ViewFileParams{
		Path:   testFile,
		Offset: 5,
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	// Content should be "56789"
	// No truncated message because we read to end
	if res != "56789" {
		t.Errorf("Expected '56789', got '%s'", res)
	}

	// Case 3: Offset 2, Limit 3
	res, err = runViewFile(context.Background(), ViewFileParams{
		Path:   testFile,
		Offset: 2,
		Limit:  3,
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	// Content should be "234" + truncated msg
	if !strings.HasPrefix(res, "234") {
		t.Errorf("Expected prefix '234', got '%s'", res)
	}
	if !strings.Contains(res, "reading limit") {
		t.Errorf("Expected truncated message, got '%s'", res)
	}
}

// TestFileToolsRejectCrossConversationTraversal pins the #575 fix at the tool
// level: view_file / edit_file / write_file must refuse a relative path that
// escapes the caller's conversation workspace via "..". Pre-fix,
// "../<otherConvID>/file" resolved into a sibling conversation's workspace
// (still under an allowed base dir) and passed ValidatePath — a cross-tenant
// read/write, since conversations can belong to different users.
func TestFileToolsRejectCrossConversationTraversal(t *testing.T) {
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

	if out, err := runViewFile(ctx, ViewFileParams{Path: escape}); err == nil {
		t.Errorf("view_file read a sibling conversation's file: %q", out)
	} else if !strings.Contains(err.Error(), "path validation failed") {
		t.Errorf("view_file: want path validation error, got: %v", err)
	}

	if _, err := runEditFile(ctx, EditFileParams{Path: escape, OldText: "secret", NewText: "pwn"}); err == nil {
		t.Error("edit_file edited a sibling conversation's file")
	}

	if _, err := runWriteFile(ctx, WriteFileParams{Path: "../" + victimConv + "/planted.txt", Content: "pwn"}); err == nil {
		t.Error("write_file wrote into a sibling conversation's workspace")
	}
	if _, err := os.Stat(filepath.Join(victimDir, "planted.txt")); !os.IsNotExist(err) {
		t.Errorf("planted.txt must not exist in the victim workspace (stat err = %v)", err)
	}

	// The victim's file is untouched.
	got, err := os.ReadFile(secret)
	if err != nil || string(got) != "cross-tenant secret" {
		t.Fatalf("victim file changed: %q, %v", got, err)
	}

	// Legitimate in-workspace access keeps working: a nested relative write +
	// read-back lands under the caller's OWN workspace.
	if _, err := runWriteFile(ctx, WriteFileParams{Path: "sub/report.txt", Content: "mine"}); err != nil {
		t.Fatalf("in-workspace write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "conv-attacker", "sub", "report.txt")); err != nil {
		t.Fatalf("in-workspace write landed wrong: %v", err)
	}
	out, err := runViewFile(ctx, ViewFileParams{Path: "sub/report.txt"})
	if err != nil {
		t.Fatalf("in-workspace read: %v", err)
	}
	if out != "mine" {
		t.Errorf("in-workspace read = %q, want %q", out, "mine")
	}
}
