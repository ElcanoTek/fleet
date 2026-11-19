package tools

import (
	"context"
	"errors"
	"fmt"
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

// SharedFilesDirName is the single path segment under the workspace root
// where the cross-chat shared file library's staged tree lives
// (docs/SHARED-FILES.md) — and the name of the per-conversation symlink
// EnsureWorkspaceDir plants so `shared/<name>` resolves from a chat's cwd.
// Declared here (not in internal/sharedfiles, which owns the staging logic)
// because this package sits below both the agent manager and the store in the
// import graph, so every consumer — mounts, symlinks, staging, prompt text —
// can share the one constant.
const SharedFilesDirName = "shared"

// SkillsDirName is the per-conversation symlink name for the bundle's skills
// tree — `skills/<name>/SKILL.md` is the roster handle every prompt uses — and,
// on the kubernetes sandbox backend, the single path segment under the
// workspace root where fleet STAGES that tree so every sandbox pod sees it
// (clientconfig.Bundle.StageSkillsAt, ADR-0055). Same shape, same reasoning
// as SharedFilesDirName: the workspace root is the one directory both sandbox
// backends make visible.
const SkillsDirName = "skills"

// StagedSkillsDir returns the staged skills tree's path for a workspace root.
func StagedSkillsDir(workspaceRoot string) string {
	dir := filepath.Join(workspaceRoot, SkillsDirName)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return dir
}

// SharedFilesDir returns the staged library root for a workspace root.
func SharedFilesDir(workspaceRoot string) string {
	dir := filepath.Join(workspaceRoot, SharedFilesDirName)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return dir
}

// TaskWorkspacesDirName is the single path segment under the workspace root
// that holds the per-job scheduled workspaces (#1543): `tasks/<lineage_id>/`.
const TaskWorkspacesDirName = "tasks"

// TaskWorkspaceDir returns the working directory for a scheduled job:
// <root>/tasks/<lineageID>. lineageID is a task UUID used as ONE path segment:
// an empty value, anything filepath.IsLocal rejects (absolute, ".."), or a
// value carrying a separator falls back to the root rather than escaping or
// nesting under it. IsLocal is the barrier CodeQL's path-injection query
// recognizes; the separator check keeps the "one segment" contract that
// IsLocal alone does not (it accepts "a/b").
func TaskWorkspaceDir(root, lineageID string) string {
	if lineageID == "" || !filepath.IsLocal(lineageID) || strings.ContainsAny(lineageID, `/\`) {
		return root
	}
	return filepath.Join(root, TaskWorkspacesDirName, lineageID)
}

// WorkspaceDirForConversation returns the absolute-or-relative path to
// the per-conversation workspace root. Resolution order:
//   - $FLEET_WORKSPACE_ROOT/<convID>
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
//
// The result is a PATH: it names whatever is at that name now, and a sandbox
// can replace the conversation directory with a symbolic link. Hand it to the
// sandbox, a prompt or a message; host-side code that reads, lists, writes,
// chmods, copies or removes inside a conversation's workspace opens it with
// OpenConversationDir / EnsureConversationDir instead.
// TestConversationPathCallersAreReviewed lists every caller with its reason.
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
// exist, and returns its path. Best-effort: returns an error so callers can
// surface it, but they should generally fall back to the shared root rather
// than failing the tool call — except for ErrWorkspaceDirLink, which means
// the directory is a symbolic link and must not be used at all.
//
// The directory is created and opened through EnsureConversationDir, so a
// conversation directory that is a symbolic link is refused rather than
// followed, and neither the mode change nor the seeded links can land
// anywhere else. The returned path is for the sandbox (its working
// directory, the file-op root), prompts and messages: host-side code that
// reads or writes the directory opens it with OpenConversationDir /
// EnsureConversationDir instead of using this path.
//
// We also drop symlinks inside the scoped workspace pointing at the
// agent's supporting docs (protocols, personas, system_prompts, skills).
// The bash/run_python tools cd into this workspace, so without these the
// bare paths in the system prompt ("protocols/foo.yaml",
// "skills/<name>/SKILL.md") would fail to resolve. Using absolute targets
// means the symlinks keep working even if the scoped dir is moved around.
//
// 0o755 because the per-turn sandbox container runs as uid 1000
// (sandbox), while chat-server creates this dir as the chat host
// user. Under rootless podman, host-chat maps to container-root,
// so a 0o750 dir owned by chat appears as root:root 0o750 inside
// the container — and the sandbox user can neither chdir nor read
// it, breaking every bash + run_python call in lockdown mode. The
// data here is per-conversation already (isolation enforced at the
// DB row layer); other-readable on the host costs nothing because
// the workspace tree is single-user-owned. Older chat-server versions
// created this dir with 0o750, so the mode is re-asserted on every call
// (on the opened directory, never through a path).
func EnsureWorkspaceDir(conversationID string) (string, error) {
	d, err := EnsureConversationDir(context.Background(), conversationID)
	if err != nil {
		return WorkspaceDirForConversation(conversationID), err
	}
	_ = d.Close()
	return WorkspaceDirForConversation(conversationID), nil
}

// SeedSupportingDocSymlinks plants the supporting-doc symlinks inside dir so
// the bare relative paths the system prompt advertises ("protocols/foo.yaml",
// "skills/<name>/SKILL.md") resolve from that directory's cwd. Chat
// workspaces get this via EnsureWorkspaceDir; scheduled and one-shot
// (`fleet task run`) workspaces call it directly from
// scheduledrun.configureRunWorkspace (#1290) — before that, only chat
// workspaces were seeded and scheduled work could not resolve the very
// protocol paths the audit enforcement demands. Best-effort by design: a
// failed symlink degrades to absolute-path access, it never fails the run.
//
// Symlink targets resolve inside the sandbox because both backends expose the
// doc roots at the SAME absolute paths the host sees — read-only bind mounts
// under podman; under kubernetes, docs baked into the image at those paths
// (`bundle_docs_in_image`, ADR-0049) and the skills tree staged INSIDE the
// workspace claim every pod mounts (StagedSkillsDir, ADR-0055).
func SeedSupportingDocSymlinks(dir string) {
	// The links are created through a descriptor for dir, opened without
	// following a symbolic link anywhere below the workspace root when dir is
	// inside it (a scheduled job's directory, a worktree): a whole-root
	// sandbox could have replaced one of those levels with a link, and the
	// seeding must not plant (or remove) links wherever it points. A dir
	// outside the workspace root (a `fleet task run --workspace` directory) is
	// opened as given — no sandbox mounts its parent.
	d, err := openSeedDir(dir)
	if err != nil {
		return // skip supporting-doc links; absolute paths still work
	}
	defer d.Close()
	seedSupportingDocSymlinksIn(d.Root, d.Path())
}

// openSeedDir opens dir for SeedSupportingDocSymlinks.
func openSeedDir(dir string) (*WorkspaceDir, error) {
	return OpenRunDir(context.Background(), workspaceRootForContainment(), dir)
}

// OpenRunDir opens a run's working directory: without following a symbolic
// link at any level below workspaceRoot when dir lies under it (a
// conversation's directory, a scheduled job's, a worktree, or the root
// itself), and as given when it does not (a `fleet task run --workspace`
// directory, which no sandbox's writable mount contains). "Under it" is judged
// against the root as configured and against the root with its own symlinks
// resolved, so a dir spelled through either path gets the no-follow open.
func OpenRunDir(ctx context.Context, workspaceRoot, dir string) (*WorkspaceDir, error) {
	if strings.TrimSpace(workspaceRoot) != "" {
		rootAbs, rerr := filepath.Abs(workspaceRoot)
		dirAbs, derr := filepath.Abs(dir)
		if rerr == nil && derr == nil {
			bases := []string{rootAbs}
			if resolved, err := filepath.EvalSymlinks(rootAbs); err == nil && resolved != rootAbs {
				bases = append(bases, resolved)
			}
			for _, base := range bases {
				if rel, err := filepath.Rel(base, dirAbs); err == nil && (rel == "." || filepath.IsLocal(rel)) {
					return OpenWorkspaceDir(ctx, rootAbs, filepath.Join(rootAbs, rel))
				}
			}
		}
	}
	return OpenHostDir(dir)
}

// seedSupportingDocSymlinksIn is SeedSupportingDocSymlinks on an opened
// directory: every Lstat, Readlink, Remove and Symlink goes through dirRoot,
// so none of them can act anywhere but in that directory. dirAbs is its
// absolute host path, used only to recognise a self-referential link.
func seedSupportingDocSymlinksIn(dirRoot *os.Root, dirAbs string) {
	// Resolve absolute paths for each supporting-doc dir. We prefer
	// the ones at /opt/chat root (which themselves are symlinks to
	// server/* — see scripts/bootstrap.sh) because those are stable
	// and script-maintained. Fall back to server/* directly if the
	// root-level symlinks don't exist (fresh dev checkouts).
	cwd, err := os.Getwd()
	if err != nil {
		return // skip supporting-doc links; absolute paths still work
	}
	// SharedFilesDirName rides the same symlink machinery so `shared/<name>`
	// (the path the shared-library prompt block advertises) resolves from the
	// chat's cwd exactly like `protocols/<name>` does — but ONLY when cmd/fleet
	// registered the library dir: it has no legacy $CWD fallback, because a
	// stray ./shared dir in a dev checkout must not masquerade as the library.
	for _, name := range []string{"protocols", "personas", "system_prompts", SkillsDirName, SharedFilesDirName} {
		// A non-worktree scheduled run seeds the shared workspace ROOT — where
		// the staged shared-file library (and, on kubernetes, the staged skills
		// tree) itself lives — so a configured target can BE the link path.
		// Never plant (or repoint through) a self-referential symlink.
		// Registered dirs are absolute (SetSupportingDocDirs absolutizes), so
		// compare absolutes.
		linkAbs := filepath.Join(dirAbs, name)
		// Don't replace an existing file — could be a real file the
		// agent wrote. Only create the symlink if nothing is there.
		if _, err := dirRoot.Lstat(name); err == nil {
			// A stale link from before a boot-time dir change (e.g. the
			// merged skills dir moved) is repointed; a real file is never
			// touched.
			if configured := configuredSupportingDocDir(name); configured != "" && configured != linkAbs {
				if cur, lerr := dirRoot.Readlink(name); lerr == nil && cur != configured {
					_ = dirRoot.Remove(name)
					_ = dirRoot.Symlink(configured, name)
				}
			}
			continue
		}
		// An explicitly configured dir (cmd/fleet wires the loaded bundle's
		// dirs here, including the merged bundle+builtin skills dir) wins over
		// the legacy cwd-relative convention.
		if target := configuredSupportingDocDir(name); target != "" {
			if target != linkAbs {
				_ = dirRoot.Symlink(target, name)
			}
			continue
		}
		if name == SharedFilesDirName {
			continue // no legacy fallback — see the loop comment
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
		_ = dirRoot.Symlink(target, name)
	}
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
	// on the cwd. Escapes via '..' are caught by that same fileOpRoot check.
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
	if errors.Is(err, ErrWorkspaceDirLink) {
		return "", fmt.Errorf("conversation workspace unavailable: %w", err)
	}
	if err != nil {
		dir = WorkspaceDirForConversation(convID)
	}
	return filepath.Join(dir, path), nil
}
