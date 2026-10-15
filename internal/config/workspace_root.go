package config

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

// workspaceRootEnv is the canonical spelling PinWorkspaceRoot exports.
const workspaceRootEnv = "FLEET_WORKSPACE_ROOT"

// PinWorkspaceRoot resolves the workspace root ONCE — the operator's
// FLEET_WORKSPACE_ROOT (legacy CHAT_/CUTLASS_ aliases honored) when set, else
// ./workspace under the process cwd (/var/lib/fleet/workspace under
// deploy/fleet.service) — makes it absolute, and writes it back to both
// c.WorkspaceRoot and the canonical FLEET_WORKSPACE_ROOT env var. `fleet serve`
// calls it right after Load.
//
// Why the env write: the root is not read in one place. The sandbox pool and
// the file-tool confinement read cfg, but the per-conversation workspace
// (tools.WorkspaceDirForConversation), the MCP workspace dirs and the
// ${FLEET_WORKSPACE_ROOT} token (agentcore), `fleet worktree`, and the
// out-of-process MCP broker (which inherits FLEET_WORKSPACE_ROOT through its
// env passthrough) each read the env lazily, each with its own "./workspace"
// fallback — and not all of them absolutize it. With the variable unset, a
// conversation-scoped MCP server got the RELATIVE workspace/<convID> as both
// its cwd and its ${FLEET_WORKSPACE}, so $FLEET_WORKSPACE/x resolved to
// workspace/<convID>/workspace/<convID>/x. Pinning one absolute value means
// the default behaves exactly as if the operator had set it, and setting it
// stays a pure override.
//
// The pin is recorded on the reload state so Reload re-asserts it after
// re-applying the env file and the boot env snapshot; otherwise a relative
// operator spelling would come back on the first reload and split the lazy
// readers from cfg again. The root stays boot-only (cfg.WorkspaceRoot is not a
// reloadable field), so a changed value takes a restart, as before.
func (c *Config) PinWorkspaceRoot() {
	root := strings.TrimSpace(c.WorkspaceRoot)
	if root == "" {
		root = "workspace"
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		// Only an unreadable cwd gets here; every reader would fail the same
		// way, so keep the boot going on the unresolved value as before.
		log.Printf("workspace root: could not resolve an absolute path: %v", err)
		return
	}
	c.WorkspaceRoot = abs
	_ = os.Setenv(workspaceRootEnv, abs)
	if c.reload != nil {
		c.reload.serialize.Lock()
		c.reload.pinnedWorkspaceRoot = abs
		c.reload.serialize.Unlock()
	}
}
