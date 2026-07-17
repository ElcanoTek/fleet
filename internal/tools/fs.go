package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

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
	root, err := fileOpRoot(ctx, resolved, validPath, true)
	if err != nil {
		return "", fmt.Errorf("path validation failed: %w", err)
	}
	res, err := sb.RunFileOp(ctx, sandbox.FileOpRequest{
		Op:   sandbox.FileOpWrite,
		Path: validPath,
		Root: root,
		Data: []byte(params.Content),
	})
	if err != nil {
		return "", fileOpError("write", err)
	}
	out := fmt.Sprintf("Successfully wrote %d bytes to %s", len(params.Content), validPath)
	if res.SHA256 != "" {
		out += "\nsha256: " + res.SHA256
	}
	return out, nil
}

// ── edit_file ──

// EditFileParams are the typed parameters for the edit_file tool.
type EditFileParams struct {
	Path         string `json:"path" description:"The file path to edit."`
	OldText      string `json:"old_text" description:"The exact text to find and replace. Must match EXACTLY ONE location unless replace_all is set — if it matches several, the edit fails and asks for more surrounding context."`
	NewText      string `json:"new_text" description:"The text to replace with."`
	ReplaceAll   bool   `json:"replace_all,omitempty" description:"If true, replace ALL occurrences. If false (default), old_text must match exactly one location or the edit is rejected."`
	ExpectedHash string `json:"expected_hash,omitempty" description:"Optional SHA-256 (hex, as returned by view_file/edit_file/write_file) of the file's current content. If set and the file changed since, the edit fails without modifying it — pass it to guard against clobbering a concurrent change."`
}

// NewEditFileTool creates a fantasy.AgentTool for editing files, bound to the
// per-turn sandbox (#784) with ambiguity/stale/no-op safety (#787).
func NewEditFileTool(sb *sandbox.Sandbox) fantasy.AgentTool {
	return fantasy.NewAgentTool("edit_file",
		"Edits a file by finding and replacing text. old_text must match EXACTLY ONE location (else the edit is rejected — add surrounding context or set replace_all=true); pass expected_hash from a prior view_file to fail safely if the file changed. Returns a unified diff plus the old/new SHA-256. Relative paths resolve against the per-conversation workspace (same cwd as bash/run_python).",
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
	if params.OldText == params.NewText {
		return "", fmt.Errorf("edit is a no-op (old_text and new_text are identical)")
	}
	resolved, err := resolveWorkspacePath(ctx, params.Path)
	if err != nil {
		return "", fmt.Errorf("path validation failed: %w", err)
	}
	validPath, err := ValidatePathForRead(resolved)
	if err != nil {
		return "", fmt.Errorf("path validation failed: %w", err)
	}
	root, err := fileOpRoot(ctx, resolved, validPath, true)
	if err != nil {
		return "", fmt.Errorf("path validation failed: %w", err)
	}
	res, err := sb.RunFileOp(ctx, sandbox.FileOpRequest{
		Op:             sandbox.FileOpEdit,
		Path:           validPath,
		Root:           root,
		OldText:        params.OldText,
		NewText:        params.NewText,
		ReplaceAll:     params.ReplaceAll,
		ExpectedSHA256: params.ExpectedHash,
	})
	if err != nil {
		return "", fileOpError("edit", err)
	}
	out := fmt.Sprintf("Successfully replaced %d occurrence(s) in %s (+%d/-%d lines)\nold_sha256: %s\nnew_sha256: %s",
		res.ReplacedCount, validPath, res.Added, res.Removed, res.OldSHA256, res.SHA256)
	if res.Diff != "" {
		out += "\n\n" + res.Diff
	}
	return out, nil
}

// validateFileOpReadPath admits the boot-registered supporting-document mounts
// as a narrow read-only capability. Managed deployments intentionally exclude
// client-config directories from the general host AllowedBaseDirs, so routing
// `protocols/...` through ordinary ValidatePathForRead rejects the very
// read-only mounts the sandbox exposes. Resolve the configured symlink, verify
// the final target remains beneath one registered doc root, and never use this
// exception for write/edit.
func validateFileOpReadPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err == nil {
		if resolvedPath, evalErr := filepath.EvalSymlinks(abs); evalErr == nil {
			if root := supportingDocRootForPath(resolvedPath); root != "" {
				info, statErr := os.Stat(resolvedPath)
				if statErr != nil {
					return "", fmt.Errorf("cannot access file: %w", statErr)
				}
				if info.IsDir() {
					return "", fmt.Errorf("path is a directory, not a file: %s", path)
				}
				return filepath.Clean(resolvedPath), nil
			}
		}
	}
	return ValidatePathForRead(path)
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
	validPath, err := validateFileOpReadPath(resolved)
	if err != nil {
		return "", fmt.Errorf("path validation failed: %w", err)
	}
	root, err := fileOpRoot(ctx, resolved, validPath, false)
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
		Root:   root,
		Offset: params.Offset,
		Limit:  limit,
	})
	if err != nil {
		return "", fileOpError("view", err)
	}
	totalSize := res.Size
	if params.Offset >= totalSize {
		// offset == size is a clean EOF read (offset += limit paging that
		// lands exactly on the file size), not an error — only an offset
		// strictly past the end signals a caller arithmetic bug.
		if totalSize == 0 || params.Offset == totalSize {
			return "", nil
		}
		return "", fmt.Errorf("offset %d is beyond file size %d", params.Offset, totalSize)
	}
	content := string(res.Data)
	if params.Offset+int64(len(res.Data)) < totalSize {
		content += fmt.Sprintf("\n... (reading limit of %d bytes reached. Total size: %d bytes. Use offset/limit to read more)", limit, totalSize)
	}
	// Content-version trailer (#787): the full-file SHA-256 the model can pass
	// back as edit_file's expected_hash to guard against a stale edit.
	if res.SHA256 != "" {
		content += fmt.Sprintf("\n\n(file metadata: sha256=%s size=%d bytes — pass sha256 as expected_hash to edit_file to guard against concurrent changes)", res.SHA256, totalSize)
	}
	return content, nil
}

// fileOpRoot turns host policy into the narrow capability the in-container
// executor receives. Interactive turns are confined to their own conversation
// directory even though Podman bind-mounts the shared workspace root. A
// host-resolved supporting-doc symlink may cross that directory only into one
// of the explicitly registered read-only document mounts. Scheduled worktree
// runs are confined to their forced worktree; unscoped scheduled/CLI calls use
// the most-specific configured allowlist root containing the validated path.
func fileOpRoot(ctx context.Context, resolved, valid string, writable bool) (string, error) {
	validAbs, err := filepath.Abs(valid)
	if err != nil {
		return "", err
	}
	resolvedAbs, err := filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	if docsRoot := supportingDocRootForPath(validAbs); writable && docsRoot != "" {
		return "", &PathSecurityError{Path: valid, Reason: "supporting-document mounts are read-only", BaseDir: docsRoot}
	}

	if forced := ForcedWorkingDirFromContext(ctx); forced != "" {
		root, absErr := filepath.Abs(forced)
		if absErr != nil {
			return "", absErr
		}
		if !isSubPath(root, validAbs) {
			return "", &PathSecurityError{Path: valid, Reason: "path escapes the scheduled-run worktree", BaseDir: root}
		}
		return root, nil
	}

	if convID := ConversationIDFromContext(ctx); convID != "" {
		root, absErr := filepath.Abs(WorkspaceDirForConversation(convID))
		if absErr != nil {
			return "", absErr
		}
		if isSubPath(root, validAbs) {
			return root, nil
		}
		// Host validation resolves a legitimate `protocols/...` (etc.)
		// symlink to the read-only supporting-doc mount. Only honor that
		// exception when the model's unresolved path originated beneath its
		// own workspace and the resolved target is in a registered mount.
		if isSubPath(root, resolvedAbs) {
			if docsRoot := supportingDocRootForPath(validAbs); docsRoot != "" {
				return docsRoot, nil
			}
		}
		return "", &PathSecurityError{Path: valid, Reason: "path escapes the per-conversation workspace", BaseDir: root}
	}

	allowed, err := AllowedBaseDirs()
	if err != nil {
		return "", err
	}
	best := ""
	for _, dir := range allowed {
		abs, absErr := filepath.Abs(dir)
		if absErr == nil && isSubPath(abs, validAbs) && len(abs) > len(best) {
			best = filepath.Clean(abs)
		}
	}
	if best == "" {
		return "", &PathSecurityError{Path: valid, Reason: "path is outside allowed directories"}
	}
	return best, nil
}

func supportingDocRootForPath(path string) string {
	supportingDocDirsMu.RLock()
	defer supportingDocDirsMu.RUnlock()
	best := ""
	for _, root := range supportingDocDirs {
		if isSubPath(root, path) && len(root) > len(best) {
			best = filepath.Clean(root)
		}
	}
	return best
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
		// The seam may append a CRLF hint; preserve the full message.
		return fmt.Errorf("%s", err.Error())
	case errors.Is(err, sandbox.ErrFileOpAmbiguous), errors.Is(err, sandbox.ErrFileOpStale), errors.Is(err, sandbox.ErrFileOpNoOp):
		// Ambiguous/stale/no-op edits (#787): the seam's message already
		// explains the recovery (add context / set replace_all / re-read).
		return fmt.Errorf("%s", err.Error())
	case errors.Is(err, sandbox.ErrFileOpUnsafePath):
		return fmt.Errorf("%s_file refused an unsafe path change", op)
	case errors.Is(err, sandbox.ErrPoisoned):
		return fmt.Errorf("the sandbox was reset after a cancelled command; retry this %s", op)
	default:
		return fmt.Errorf("%s_file failed: %w", op, err)
	}
}
