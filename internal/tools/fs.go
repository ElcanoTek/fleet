package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"charm.land/fantasy"
)

// ── write_file ──

// WriteFileParams are the typed parameters for the write_file tool.
type WriteFileParams struct {
	Path    string `json:"path" description:"The file path to write to."`
	Content string `json:"content" description:"The content to write to the file."`
}

// NewWriteFileTool creates a fantasy.AgentTool for writing files.
func NewWriteFileTool() fantasy.AgentTool {
	return fantasy.NewAgentTool("write_file",
		"Writes content to a file, creating it if it doesn't exist or overwriting it if it does. Relative paths resolve against the per-conversation workspace (same cwd as bash/run_python).",
		func(ctx context.Context, params WriteFileParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			result, err := runWriteFile(ctx, params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(result), nil
		})
}

func runWriteFile(ctx context.Context, params WriteFileParams) (string, error) {
	if params.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	validPath, err := ValidatePath(resolveWorkspacePath(ctx, params.Path))
	if err != nil {
		return "", fmt.Errorf("path validation failed: %w", err)
	}
	dir := filepath.Dir(validPath)
	if err := os.MkdirAll(dir, 0750); err != nil { //nolint:gosec
		return "", fmt.Errorf("error creating directories: %w", err)
	}
	if err := os.WriteFile(validPath, []byte(params.Content), 0600); err != nil { //nolint:gosec
		return "", fmt.Errorf("error writing file: %w", err)
	}
	return fmt.Sprintf("Successfully wrote %d bytes to %s", len(params.Content), validPath), nil
}

// ── edit_file ──

// EditFileParams are the typed parameters for the edit_file tool.
type EditFileParams struct {
	Path       string `json:"path" description:"The file path to edit."`
	OldText    string `json:"old_text" description:"The text to find and replace."`
	NewText    string `json:"new_text" description:"The text to replace with."`
	ReplaceAll bool   `json:"replace_all,omitempty" description:"If true, replace all occurrences. If false, replace only the first. Defaults to false."`
}

// NewEditFileTool creates a fantasy.AgentTool for editing files.
func NewEditFileTool() fantasy.AgentTool {
	return fantasy.NewAgentTool("edit_file",
		"Edits a file by finding and replacing text. Use this to make targeted changes to existing files. Relative paths resolve against the per-conversation workspace (same cwd as bash/run_python).",
		func(ctx context.Context, params EditFileParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			result, err := runEditFile(ctx, params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(result), nil
		})
}

func runEditFile(ctx context.Context, params EditFileParams) (string, error) {
	if params.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	if params.OldText == "" {
		return "", fmt.Errorf("old_text is required")
	}
	validPath, err := ValidatePathForRead(resolveWorkspacePath(ctx, params.Path))
	if err != nil {
		return "", fmt.Errorf("path validation failed: %w", err)
	}
	content, err := os.ReadFile(validPath) //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("error reading file: %w", err)
	}
	contentStr := string(content)
	if !strings.Contains(contentStr, params.OldText) {
		return "", fmt.Errorf("old_text not found in file")
	}
	var newContent string
	var count int
	if params.ReplaceAll {
		count = strings.Count(contentStr, params.OldText)
		newContent = strings.ReplaceAll(contentStr, params.OldText, params.NewText)
	} else {
		newContent = strings.Replace(contentStr, params.OldText, params.NewText, 1)
		count = 1
	}
	if err := os.WriteFile(validPath, []byte(newContent), 0600); err != nil { //nolint:gosec
		return "", fmt.Errorf("error writing file: %w", err)
	}
	return fmt.Sprintf("Successfully replaced %d occurrence(s) in %s", count, validPath), nil
}

// ── view_file ──

// ViewFileParams are the typed parameters for the view_file tool.
type ViewFileParams struct {
	Path   string `json:"path" description:"The file path to view."`
	Offset int64  `json:"offset,omitempty" description:"The byte offset to start reading from. Defaults to 0."`
	Limit  int64  `json:"limit,omitempty" description:"The maximum number of bytes to read. Defaults to 131072 (128KB). Maximum 10MB."`
}

// NewViewFileTool creates a fantasy.AgentTool for reading file contents.
func NewViewFileTool() fantasy.AgentTool {
	return fantasy.NewAgentTool("view_file",
		"Reads and displays the contents of a file. Use this to examine file contents before editing. Relative paths resolve against the per-conversation workspace (same cwd as bash/run_python), so `protocols/foo.yaml` works.",
		func(ctx context.Context, params ViewFileParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			result, err := runViewFile(ctx, params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(result), nil
		})
}

func runViewFile(ctx context.Context, params ViewFileParams) (string, error) {
	if params.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	validPath, err := ValidatePathForRead(resolveWorkspacePath(ctx, params.Path))
	if err != nil {
		return "", fmt.Errorf("path validation failed: %w", err)
	}
	limit := params.Limit
	if limit <= 0 {
		limit = 131072
	}
	const maxLimit = 10 * 1024 * 1024
	if limit > maxLimit {
		limit = maxLimit
	}
	f, err := os.Open(validPath) //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("error opening file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("error getting file info: %w", err)
	}
	totalSize := info.Size()
	if params.Offset >= totalSize {
		if totalSize == 0 {
			return "", nil
		}
		return "", fmt.Errorf("offset %d is beyond file size %d", params.Offset, totalSize)
	}
	if _, err := f.Seek(params.Offset, 0); err != nil {
		return "", fmt.Errorf("error seeking file: %w", err)
	}
	buf := make([]byte, limit)
	n, err := f.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("error reading file: %w", err)
	}
	content := string(buf[:n])
	if params.Offset+int64(n) < totalSize {
		content += fmt.Sprintf("\n... (reading limit of %d bytes reached. Total size: %d bytes. Use offset/limit to read more)", limit, totalSize)
	}
	return content, nil
}
