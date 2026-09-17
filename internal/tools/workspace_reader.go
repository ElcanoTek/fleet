package tools

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/ElcanoTek/fleet/internal/sandbox"
)

// WorkspaceFileReader is a bounded, sandbox-only input seam for brokered MCP
// binary arguments. References cannot select supporting-doc mounts or sibling
// conversations. The sandbox pins the root and rejects symlink traversal.
func WorkspaceFileReader(sb *sandbox.Sandbox) func(context.Context, string, int64) ([]byte, error) {
	return func(ctx context.Context, path string, maxBytes int64) ([]byte, error) {
		if maxBytes <= 0 {
			return nil, fmt.Errorf("workspace file reader requires a positive byte limit")
		}
		if sb == nil {
			return nil, fmt.Errorf("workspace file reference requires a sandbox")
		}
		if !filepath.IsLocal(path) || containsDotDotComponent(path) {
			return nil, fmt.Errorf("workspace_file must be a relative path without '..'")
		}
		root := ForcedWorkingDirFromContext(ctx)
		if root == "" && ConversationIDFromContext(ctx) != "" {
			root = WorkspaceDirForConversation(ConversationIDFromContext(ctx))
		}
		if root == "" {
			return nil, fmt.Errorf("workspace file reference requires an active workspace")
		}
		result, err := sb.RunFileOp(ctx, sandbox.FileOpRequest{Op: sandbox.FileOpRead, Root: root, Path: filepath.Join(root, path), Limit: maxBytes})
		if err != nil {
			return nil, fmt.Errorf("cannot read workspace file: %w", err)
		}
		if result.Size > maxBytes {
			return nil, fmt.Errorf("workspace file exceeds %d bytes", maxBytes)
		}
		return result.Data, nil
	}
}
