package tools

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/ElcanoTek/fleet/internal/sandbox"
)

// WorkspaceFileWindow is one raw byte window of a workspace file plus the
// identity of the WHOLE file it was cut from: its size and lowercase hex
// SHA-256, computed by the sandbox helper in the same streaming pass that read
// the window (ADR-0085).
type WorkspaceFileWindow struct {
	Data   []byte
	Size   int64
	SHA256 string
}

// WorkspaceFileReadFunc reads [offset, offset+length) of a workspace file,
// refusing a file larger than maxFileBytes before any of it is read.
type WorkspaceFileReadFunc func(ctx context.Context, path string, offset, length, maxFileBytes int64) (WorkspaceFileWindow, error)

// WorkspaceFileReader is a bounded, sandbox-only input seam for brokered MCP
// binary arguments. References cannot select supporting-doc mounts or sibling
// conversations. The sandbox pins the root and rejects symlink traversal.
//
// Only the requested window crosses out of the sandbox. The helper still hashes
// the whole file (streamed in 1 MiB reads, never held whole) so the reference's
// whole-file sha256 binds exactly one version of the file, and it refuses a
// file over maxFileBytes from its size alone, before hashing anything.
func WorkspaceFileReader(sb *sandbox.Sandbox) WorkspaceFileReadFunc {
	return func(ctx context.Context, path string, offset, length, maxFileBytes int64) (WorkspaceFileWindow, error) {
		if maxFileBytes <= 0 {
			return WorkspaceFileWindow{}, fmt.Errorf("workspace file reader requires a positive byte limit")
		}
		if offset < 0 || length <= 0 || offset > maxFileBytes || length > maxFileBytes-offset {
			return WorkspaceFileWindow{}, fmt.Errorf("workspace file reader requires a byte range within %d bytes", maxFileBytes)
		}
		if sb == nil {
			return WorkspaceFileWindow{}, fmt.Errorf("workspace file reference requires a sandbox")
		}
		if !filepath.IsLocal(path) || containsDotDotComponent(path) {
			return WorkspaceFileWindow{}, fmt.Errorf("workspace_file must be a relative path without '..'")
		}
		root := ForcedWorkingDirFromContext(ctx)
		if root == "" && ConversationIDFromContext(ctx) != "" {
			root = WorkspaceDirForConversation(ConversationIDFromContext(ctx))
		}
		if root == "" {
			return WorkspaceFileWindow{}, fmt.Errorf("workspace file reference requires an active workspace")
		}
		result, err := sb.RunFileOp(ctx, sandbox.FileOpRequest{
			Op: sandbox.FileOpRead, Root: root, Path: filepath.Join(root, path),
			Offset: offset, Limit: length, MaxSize: maxFileBytes,
		})
		if err != nil {
			return WorkspaceFileWindow{}, fmt.Errorf("cannot read workspace file: %w", err)
		}
		if result.Size > maxFileBytes {
			return WorkspaceFileWindow{}, fmt.Errorf("workspace file exceeds %d bytes", maxFileBytes)
		}
		if int64(len(result.Data)) > length {
			return WorkspaceFileWindow{}, fmt.Errorf("workspace file read returned more than the requested range")
		}
		return WorkspaceFileWindow{Data: result.Data, Size: result.Size, SHA256: result.SHA256}, nil
	}
}
