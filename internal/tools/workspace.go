package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Per-turn context plumbing for conversation-scoped workspace paths.
//
// The agent manager threads a conversation id through the turn context
// via `WithConversationID`; individual tools read it via
// `ConversationIDFromContext` to pick a per-chat workspace directory
// like `workspace/<convID>/` instead of a shared scratch root.
//
// Why not pass the id explicitly through tool args? fantasy tools have
// a fixed JSON schema exposed to the model — we don't want the LLM to
// see or forge this id. Context is the right level: set by the harness,
// read by tools, invisible to prompts.

type ctxKey int

const (
	ctxKeyConversationID ctxKey = iota + 1
	ctxKeyForcedWorkingDir
)

// WithForcedWorkingDir returns a context carrying a per-run base working
// directory that the bash / run_python / file tools resolve unscoped relative
// paths against, taking precedence over the per-conversation workspace and the
// process cwd (but NOT over an explicit per-call working_dir the model passes).
//
// This is the in-process seam for git worktree isolation (#180): the scheduled
// runner sets it to the per-run worktree path so the agent's tool calls land in
// the worktree. It is absent (and therefore a no-op) for every non-worktree run,
// so existing behaviour is unchanged. A bash/run_python call that arrives with an
// empty per-call working dir is scoped via the host sandbox's default working dir
// instead (see Sandbox.SetDefaultWorkingDir).
func WithForcedWorkingDir(ctx context.Context, dir string) context.Context {
	return context.WithValue(ctx, ctxKeyForcedWorkingDir, dir)
}

// ForcedWorkingDirFromContext returns the per-run forced working directory, or
// "" if none was set.
func ForcedWorkingDirFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyForcedWorkingDir).(string); ok {
		return v
	}
	return ""
}

// WithConversationID returns a context carrying the per-turn
// conversation id.
func WithConversationID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyConversationID, id)
}

// ConversationIDFromContext returns the per-turn conversation id
// stashed by the agent harness, or "" if the context wasn't threaded
// through a turn (tests, direct tool invocations).
func ConversationIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyConversationID).(string); ok {
		return v
	}
	return ""
}

// WorkspaceDirForConversation returns the absolute-or-relative path to
// the per-conversation workspace root. Resolution order:
//   - $FLEET_WORKSPACE_ROOT/<convID> (or legacy $CHAT_WORKSPACE_ROOT)
//   - ./workspace/<convID>        (fallback when the env var isn't set)
//
// When conversationID is "", returns the shared workspace root —
// harmless for tests but should not happen in a live turn.
//
// A conversation id is used as a single path segment directly under the
// workspace root, so it must be lexically local. filepath.IsLocal rejects an
// empty id, an absolute path, a "..", or an embedded separator — none of which
// can be a real id (they are all uuid.NewString() values) and any of which could
// otherwise widen the resolved path outside the workspace tree. IsLocal is also
// the barrier CodeQL's path-injection query recognizes, so a caller that later
// os.Open(join(dir, rel))s is provably confined even though the id arrives as a
// request path segment. A non-local id (including "") falls back to the shared
// root rather than escaping it.
func WorkspaceDirForConversation(conversationID string) string {
	root := fleetEnv("WORKSPACE_ROOT")
	if root == "" {
		root = "workspace"
	}
	if !filepath.IsLocal(conversationID) {
		return root
	}
	return filepath.Join(root, conversationID)
}

// EnsureWorkspaceDir creates the per-conversation workspace directory
// (and the supporting-docs symlinks inside it) if they don't already
// exist. Best-effort: returns an error so callers can surface it, but
// they should generally fall back to the shared root rather than
// failing the tool call.
//
// We also drop symlinks inside the scoped workspace pointing at the
// agent's supporting docs (protocols, personas, system_prompts, skills).
// The bash/run_python tools cd into this workspace, so without these the
// bare paths in the system prompt ("protocols/foo.yaml",
// "skills/<name>/SKILL.md") would fail to resolve. Using absolute targets
// means the symlinks keep working even if the scoped dir is moved around.
func EnsureWorkspaceDir(conversationID string) (string, error) {
	dir := WorkspaceDirForConversation(conversationID)
	// 0o755 because the per-turn sandbox container runs as uid 1000
	// (sandbox), while chat-server creates this dir as the chat host
	// user. Under rootless podman, host-chat maps to container-root,
	// so a 0o750 dir owned by chat appears as root:root 0o750 inside
	// the container — and the sandbox user can neither chdir nor read
	// it, breaking every bash + run_python call in lockdown mode. The
	// data here is per-conversation already (isolation enforced at the
	// DB row layer); other-readable on the host costs nothing because
	// the workspace tree is single-user-owned.
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // see comment above — readable to the lockdown container user
		return dir, err
	}
	// Older chat-server versions created this dir with 0o750. MkdirAll
	// is a no-op on existing dirs, so without an explicit Chmod
	// in-flight conversations on upgraded boxes would stay at 0o750
	// and keep failing in lockdown until rotated. Chmod is best-effort:
	// if the operator deliberately tightened perms, the next turn just
	// re-asserts our default — they can lock down via container
	// userns instead, which is the right layer for that.
	_ = os.Chmod(dir, 0o755) //nolint:gosec // see comment above
	// Resolve absolute paths for each supporting-doc dir. We prefer
	// the ones at /opt/chat root (which themselves are symlinks to
	// server/* — see scripts/bootstrap.sh) because those are stable
	// and script-maintained. Fall back to server/* directly if the
	// root-level symlinks don't exist (fresh dev checkouts).
	cwd, err := os.Getwd()
	if err != nil {
		return dir, nil //nolint:nilerr // skip supporting-doc links but return the dir
	}
	for _, name := range []string{"protocols", "personas", "system_prompts", "skills"} {
		link := filepath.Join(dir, name)
		// Don't replace an existing file — could be a real file the
		// agent wrote. Only create the symlink if nothing is there.
		if _, err := os.Lstat(link); err == nil {
			// A stale link from before a boot-time dir change (e.g. the
			// merged skills dir moved) is repointed; a real file is never
			// touched.
			if configured := configuredSupportingDocDir(name); configured != "" {
				if cur, lerr := os.Readlink(link); lerr == nil && cur != configured {
					_ = os.Remove(link)
					_ = os.Symlink(configured, link)
				}
			}
			continue
		}
		// An explicitly configured dir (cmd/fleet wires the loaded bundle's
		// dirs here, including the merged bundle+builtin skills dir) wins over
		// the legacy cwd-relative convention.
		if target := configuredSupportingDocDir(name); target != "" {
			_ = os.Symlink(target, link)
			continue
		}
		target := filepath.Join(cwd, name)
		if _, err := os.Stat(target); err != nil {
			// Fall back to server/<name> if top-level symlink is
			// missing (dev/test environments).
			target = filepath.Join(cwd, "server", name)
			if _, err := os.Stat(target); err != nil {
				continue
			}
		}
		_ = os.Symlink(target, link)
	}
	return dir, nil
}

// Managed workspace-root registry: cmd/fleet registers the absolute workspace
// root (the dir the sandbox bind-mounts) at boot so AllowedBaseDirs confines the
// host-side file tools to the workspace tree instead of the process cwd — which
// under systemd is the whole StateDirectory and would otherwise expose DataDir
// state the sandbox never mounts. Unset (tests / CLI) keeps the legacy
// cwd-based allowlist. Set once at boot before any turn runs.
var (
	managedWorkspaceRootMu sync.RWMutex
	managedWorkspaceRoot   string
)

// SetWorkspaceRoot registers the absolute workspace root as the authoritative
// base for host-side file operations (see AllowedBaseDirs). cmd/fleet calls this
// with the same root the agent manager bind-mounts into the sandbox. An empty
// value is ignored so a misconfigured caller can't silently widen the allowlist
// back to cwd.
func SetWorkspaceRoot(abs string) {
	abs = strings.TrimSpace(abs)
	if abs == "" {
		return
	}
	managedWorkspaceRootMu.Lock()
	defer managedWorkspaceRootMu.Unlock()
	managedWorkspaceRoot = abs
}

// workspaceRootBase returns the registered workspace root, or "" when none was
// registered (tests / CLI / dev).
func workspaceRootBase() string {
	managedWorkspaceRootMu.RLock()
	defer managedWorkspaceRootMu.RUnlock()
	return managedWorkspaceRoot
}

// Supporting-doc dir registry: cmd/fleet wires the loaded client bundle's
// personas/protocols/system_prompts/skills dirs here at boot so workspace
// symlinks point at the REAL content (in particular the merged bundle+builtin
// skills dir — see clientconfig/builtin_skills.go) instead of relying on the
// legacy $CWD/<name> symlink convention. Unset names keep the legacy behavior.
var (
	supportingDocDirsMu sync.RWMutex
	supportingDocDirs   map[string]string
)

// SetSupportingDocDirs registers the absolute supporting-doc dirs (keys:
// "personas", "protocols", "system_prompts", "skills"). Empty values are
// ignored; relative paths are absolutized.
func SetSupportingDocDirs(dirs map[string]string) {
	abs := make(map[string]string, len(dirs))
	for name, d := range dirs {
		if d == "" {
			continue
		}
		if a, err := filepath.Abs(d); err == nil {
			abs[name] = a
		}
	}
	supportingDocDirsMu.Lock()
	supportingDocDirs = abs
	supportingDocDirsMu.Unlock()
}

func configuredSupportingDocDir(name string) string {
	supportingDocDirsMu.RLock()
	defer supportingDocDirsMu.RUnlock()
	return supportingDocDirs[name]
}

// resolveWorkspacePath turns a user-supplied path from the file-ops
// tools (view_file / write_file / edit_file) into an absolute path
// rooted in the per-conversation workspace.
//
// Absolute paths are returned unchanged. Relative paths are joined
// against `workspace/<convID>/` — matching the cwd the bash and
// run_python tools use — so `protocols/foo.yaml`, `personas/foo.yaml`,
// and plain filenames written in the same turn all resolve to the
// same place across every tool. Without this, the system prompt's
// promise that "supporting docs are exposed as symlinks inside your
// scratch so bare paths still resolve" was only true for bash/python,
// and view_file failed with "file does not exist" on paths the agent
// had just successfully listed via bash.
//
// If no conversation id is in ctx (tests, direct invocations) the
// path is returned unchanged so filepath.Abs falls back to process
// cwd, preserving legacy behavior.
//
// The per-conversation workspace is an isolation boundary — conversations
// can belong to different users. A relative path with ".." components is
// therefore rejected outright (#575): filepath.Join collapses ".." before
// the downstream ValidatePath check runs, and ValidatePath only contains
// to AllowedBaseDirs (cwd/temp), NOT to workspace/<convID>/ — so without
// this reject, `../<otherConvID>/file` resolves into a sibling
// conversation's workspace and passes validation.
func resolveWorkspacePath(ctx context.Context, path string) (string, error) {
	if path == "" || filepath.IsAbs(path) {
		return path, nil
	}
	// A forced working dir takes precedence over the per-conversation workspace,
	// matching resolveBashWorkingDir and fileOpRoot, so every tool surface agrees
	// on the cwd. Before #1043 the two never coexisted (scheduled runs have no
	// conversation id; chat set no forced dir); now an interactive turn's spawned
	// sub-agent carries BOTH — the conversation id it inherited and its own
	// isolated forced subdir — and the forced dir must win or its relative file
	// writes would land in the shared conversation workspace and then fail
	// fileOpRoot's forced-subtree containment check. Escapes via '..' are caught
	// by that same fileOpRoot check (as they always were on the scheduled path).
	if forced := ForcedWorkingDirFromContext(ctx); forced != "" {
		return filepath.Join(forced, path), nil
	}
	convID := ConversationIDFromContext(ctx)
	if convID == "" {
		// No per-conversation workspace and no forced dir (tests, direct
		// invocations): preserve legacy behavior (return unchanged → resolved
		// against the process cwd).
		return path, nil
	}
	if containsDotDotComponent(path) {
		return "", &PathSecurityError{
			Path:    path,
			Reason:  "path must not contain '..' components (per-conversation workspace isolation)",
			BaseDir: WorkspaceDirForConversation(convID),
		}
	}
	dir, err := EnsureWorkspaceDir(convID)
	if err != nil {
		dir = WorkspaceDirForConversation(convID)
	}
	return filepath.Join(dir, path), nil
}
