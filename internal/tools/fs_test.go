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
