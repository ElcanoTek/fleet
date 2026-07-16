package tools

import (
	"context"
	"errors"
	"fmt"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/sandbox"
)

// The model-callable file tools (view_file / write_file / edit_file) execute
// their filesystem operations INSIDE the per-turn sandbox via the sandbox
// FileOp seam (#784), exactly like bash and run_python. Host-side path
// resolution + validation (resolveWorkspacePath / ValidatePath) still runs
// first as defense-in-depth INPUT, but it is no longer the execution
// mechanism: the read/write/edit itself happens in the container, so it
// inherits the runtime (crun/kata/krun), seccomp, dropped caps, cgroups, PID
// and disk limits, and lockdown network posture. A nil sandbox is a programmer
// error (the pool was bypassed) and fails closed — there is no host fallback.

// ── write_file ──

// WriteFileParams are the typed parameters for the write_file tool.
type WriteFileParams struct {
	Path    string `json:"path" description:"The file path to write to."`
	Content string `json:"content" description:"The content to write to the file."`
}

// NewWriteFileTool creates a fantasy.AgentTool for writing files, bound to the
// per-turn sandbox (#784).
func NewWriteFileTool(sb *sandbox.Sandbox) fantasy.AgentTool {
	return fantasy.NewAgentTool("write_file",
		"Writes content to a file, creating it if it doesn't exist or overwriting it if it does. Relative paths resolve against the per-conversation workspace (same cwd as bash/run_python).",
		func(ctx context.Context, params WriteFileParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			result, err := runWriteFile(ctx, sb, params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(result), nil
		})
}

func runWriteFile(ctx context.Context, sb *sandbox.Sandbox, params WriteFileParams) (string, error) {
	if sb == nil {
		return "", fmt.Errorf("write_file requires a sandbox; pool.Take returned nil or was bypassed")
	}
	if params.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	resolved, err := resolveWorkspacePath(ctx, params.Path)
	if err != nil {
		return "", fmt.Errorf("path validation failed: %w", err)
	}
	validPath, err := ValidatePath(resolved)
	if err != nil {
		return "", fmt.Errorf("path validation failed: %w", err)
	}
	if _, err := sb.RunFileOp(ctx, sandbox.FileOpRequest{
		Op:   sandbox.FileOpWrite,
		Path: validPath,
		Data: []byte(params.Content),
	}); err != nil {
		return "", fileOpError("write", err)
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

// NewEditFileTool creates a fantasy.AgentTool for editing files, bound to the
// per-turn sandbox (#784).
func NewEditFileTool(sb *sandbox.Sandbox) fantasy.AgentTool {
	return fantasy.NewAgentTool("edit_file",
		"Edits a file by finding and replacing text. Use this to make targeted changes to existing files. Relative paths resolve against the per-conversation workspace (same cwd as bash/run_python).",
		func(ctx context.Context, params EditFileParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			result, err := runEditFile(ctx, sb, params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(result), nil
		})
}

func runEditFile(ctx context.Context, sb *sandbox.Sandbox, params EditFileParams) (string, error) {
	if sb == nil {
		return "", fmt.Errorf("edit_file requires a sandbox; pool.Take returned nil or was bypassed")
	}
	if params.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	if params.OldText == "" {
		return "", fmt.Errorf("old_text is required")
	}
	resolved, err := resolveWorkspacePath(ctx, params.Path)
	if err != nil {
		return "", fmt.Errorf("path validation failed: %w", err)
	}
	validPath, err := ValidatePathForRead(resolved)
	if err != nil {
		return "", fmt.Errorf("path validation failed: %w", err)
	}
	res, err := sb.RunFileOp(ctx, sandbox.FileOpRequest{
		Op:         sandbox.FileOpEdit,
		Path:       validPath,
		OldText:    params.OldText,
		NewText:    params.NewText,
		ReplaceAll: params.ReplaceAll,
	})
	if err != nil {
		return "", fileOpError("edit", err)
	}
	return fmt.Sprintf("Successfully replaced %d occurrence(s) in %s", res.ReplacedCount, validPath), nil
}

// ── view_file ──

// ViewFileParams are the typed parameters for the view_file tool.
type ViewFileParams struct {
	Path   string `json:"path" description:"The file path to view."`
	Offset int64  `json:"offset,omitempty" description:"The byte offset to start reading from. Defaults to 0."`
	Limit  int64  `json:"limit,omitempty" description:"The maximum number of bytes to read. Defaults to 131072 (128KB). Maximum 10MB."`
}

const (
	viewFileDefaultLimit int64 = 131072
	viewFileMaxLimit     int64 = 10 * 1024 * 1024
)

// NewViewFileTool creates a fantasy.AgentTool for reading file contents, bound
// to the per-turn sandbox (#784).
func NewViewFileTool(sb *sandbox.Sandbox) fantasy.AgentTool {
	return fantasy.NewAgentTool("view_file",
		"Reads and displays the contents of a file. Use this to examine file contents before editing. Relative paths resolve against the per-conversation workspace (same cwd as bash/run_python), so `protocols/foo.yaml` works.",
		func(ctx context.Context, params ViewFileParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			result, err := runViewFile(ctx, sb, params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(result), nil
		})
}

func runViewFile(ctx context.Context, sb *sandbox.Sandbox, params ViewFileParams) (string, error) {
	if sb == nil {
		return "", fmt.Errorf("view_file requires a sandbox; pool.Take returned nil or was bypassed")
	}
	if params.Path == "" {
		return "", fmt.Errorf("path is required")
	}
	resolved, err := resolveWorkspacePath(ctx, params.Path)
	if err != nil {
		return "", fmt.Errorf("path validation failed: %w", err)
	}
	validPath, err := ValidatePathForRead(resolved)
	if err != nil {
		return "", fmt.Errorf("path validation failed: %w", err)
	}
	limit := params.Limit
	if limit <= 0 {
		limit = viewFileDefaultLimit
	}
	if limit > viewFileMaxLimit {
		limit = viewFileMaxLimit
	}
	res, err := sb.RunFileOp(ctx, sandbox.FileOpRequest{
		Op:     sandbox.FileOpRead,
		Path:   validPath,
		Offset: params.Offset,
		Limit:  limit,
	})
	if err != nil {
		return "", fileOpError("view", err)
	}
	totalSize := res.Size
	if params.Offset >= totalSize {
		if totalSize == 0 {
			return "", nil
		}
		return "", fmt.Errorf("offset %d is beyond file size %d", params.Offset, totalSize)
	}
	content := string(res.Data)
	if params.Offset+int64(len(res.Data)) < totalSize {
		content += fmt.Sprintf("\n... (reading limit of %d bytes reached. Total size: %d bytes. Use offset/limit to read more)", limit, totalSize)
	}
	return content, nil
}

// fileOpError maps the sandbox FileOp sentinels back to the exact model-facing
// strings the host-side implementation used before #784, so the tool contract
// is unchanged.
func fileOpError(op string, err error) error {
	switch {
	case errors.Is(err, sandbox.ErrFileOpNotFound):
		if op == "view" || op == "edit" {
			return fmt.Errorf("error reading file: file not found")
		}
		return fmt.Errorf("file not found")
	case errors.Is(err, sandbox.ErrFileOpIsDirectory):
		return fmt.Errorf("%s failed: path is a directory", op)
	case errors.Is(err, sandbox.ErrFileOpOldTextAbsent):
		return fmt.Errorf("old_text not found in file")
	case errors.Is(err, sandbox.ErrPoisoned):
		return fmt.Errorf("the sandbox was reset after a cancelled command; retry this %s", op)
	default:
		return fmt.Errorf("%s_file failed: %w", op, err)
	}
}
