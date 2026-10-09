package agentcore

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// Reserved ${FLEET_WORKSPACE} manifest-env token.
//
// The cutlass-family Python MCP servers key several behaviors on writable
// working directories handed to them via env vars: CUTLASS_RUN_WORKDIR (the
// cross-restart run ledger + managed-run detection, e.g. the SendGrid
// fail-closed recipient allowlist), CUTLASS_REPORT_DIR, CUTLASS_INPUT_DIR,
// DEAL_SHEET_OUTPUT_DIR. A client bundle cannot hardcode those paths — they are
// deployment-specific — and plain ${VAR} interpolation can only reference the
// operator's static process env, never a fleet-managed per-run directory.
//
// ${FLEET_WORKSPACE} closes that gap: it is a RESERVED interpolation token a
// bundle may use inside an mcp_servers[].env value (stdio servers only). The
// clientconfig loader passes it through untouched (see the reserved-name
// handling in internal/clientconfig), and the spawn paths substitute it with a
// fleet-provided writable directory at subprocess-launch time:
//
//   - Shared spawns (the boot-time catalog, the mcp-broker, hot reload, and
//     load-on-demand onto a shared client) substitute SharedMCPWorkspaceDir():
//     one stable per-deployment directory under the workspace root. Stable
//     means run-ledger entries persist across runs/restarts — a dedupe window,
//     not a per-run ledger.
//   - Per-run spawns (a scheduled task with an explicit mcp_selection, which
//     gets its own MCP client) substitute OpenStableMCPWorkspace("task-<id>"):
//     one directory per task occurrence, re-mounted by every retry of that
//     occurrence (cutlass-parity ledger semantics) and never shared with
//     another occurrence.
//
// A spawn path that has NO directory to offer (workdir == "") DROPS every env
// key whose value still references the token, so the server sees the var as
// unset — the servers' documented inert/fail-safe posture — rather than as a
// literal "${FLEET_WORKSPACE}" path or a confusing blank.
//
// ${FLEET_WORKSPACE_ROOT} is the third reserved token: the deployment's
// workspace root itself — the directory a scheduled run's sandbox works in and
// the parent of both mcp-shared/ and the per-occurrence mcp-runs/ dirs. EVERY spawn
// path substitutes it (shared, per-run, broker scope, probe) because there is
// always exactly one root, so unlike ${FLEET_WORKSPACE} it is never dropped
// and it creates nothing on disk. A connector that allowlists the files it may
// read — an outbound mailer's content_file / attachments — declares it so a
// report the run wrote at the workspace root is admissible: the per-run dir's
// name is not knowable from inside the sandbox, so pointing such an
// allowlist at ${FLEET_WORKSPACE} alone leaves the model no path it can name.
const (
	// WorkspaceEnvToken is the reserved token as it appears in a manifest env
	// value. Only this bare spelling is reserved; a ${FLEET_WORKSPACE:-x} /
	// :? spelling fails the bundle load (internal/clientconfig).
	//nolint:gosec // G101 false positive: an interpolation placeholder name, not a credential.
	WorkspaceEnvToken = "${FLEET_WORKSPACE}"
	// TaskIDEnvToken is replaced for dedicated scheduled-task MCP clients with
	// the current task UUID. Bundles map it to whatever compatibility variable
	// their connector consumes; fleet itself remains connector-agnostic.
	TaskIDEnvToken = "${FLEET_TASK_ID}" //nolint:gosec // interpolation placeholder, not a credential.
	// WorkspaceRootEnvToken is replaced on every spawn path with
	// WorkspaceRootDir(): the absolute workspace root. See the package doc
	// above for why a file-allowlisting connector needs it.
	WorkspaceRootEnvToken = "${FLEET_WORKSPACE_ROOT}" //nolint:gosec // interpolation placeholder, not a credential.

	// sharedMCPWorkspaceSubdir is the stable per-deployment directory (under
	// the workspace root) substituted for shared, process-lifetime spawns.
	sharedMCPWorkspaceSubdir = "mcp-shared"

	// perRunMCPWorkspaceSubdir holds the minted per-run directories.
	perRunMCPWorkspaceSubdir = "mcp-runs"
)

// EnvReferencesWorkspace reports whether any value in env carries the reserved
// ${FLEET_WORKSPACE} token. Callers use it to avoid creating directories on
// disk for catalogs that never opted into the token.
func EnvReferencesWorkspace(env map[string]string) bool {
	for _, v := range env {
		if strings.Contains(v, WorkspaceEnvToken) {
			return true
		}
	}
	return false
}

// EnvReferencesTaskID reports whether any value still carries the reserved
// ${FLEET_TASK_ID} token — i.e. the bundle asked for a per-task identity, which
// only a dedicated per-run client can supply.
func EnvReferencesTaskID(env map[string]string) bool {
	for _, v := range env {
		if strings.Contains(v, TaskIDEnvToken) {
			return true
		}
	}
	return false
}

// ExpandTaskIDEnv resolves the reserved scheduled-task identity token. Shared
// or interactive spawns pass an empty taskID and therefore drop token-bearing
// keys instead of leaking a literal placeholder to a connector.
func ExpandTaskIDEnv(env map[string]string, taskID string) map[string]string {
	found := false
	for _, v := range env {
		found = found || strings.Contains(v, TaskIDEnvToken)
	}
	if !found {
		return env
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		if !strings.Contains(v, TaskIDEnvToken) {
			out[k] = v
			continue
		}
		if strings.TrimSpace(taskID) != "" {
			out[k] = strings.ReplaceAll(v, TaskIDEnvToken, taskID)
		}
	}
	return out
}

// ExpandWorkspaceEnv returns a copy of env with every ${FLEET_WORKSPACE}
// occurrence replaced by workdir. When workdir is empty, keys whose value still
// references the token are DROPPED (fail-safe: the server sees the var as
// unset, its documented inert posture). A map with no token references is
// returned as-is (no copy), so the common no-token catalog pays nothing.
func ExpandWorkspaceEnv(env map[string]string, workdir string) map[string]string {
	if !EnvReferencesWorkspace(env) {
		return env
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		if !strings.Contains(v, WorkspaceEnvToken) {
			out[k] = v
			continue
		}
		if strings.TrimSpace(workdir) == "" {
			// No directory to offer: drop the key so the server treats the
			// var as unset rather than receiving a literal token.
			continue
		}
		out[k] = strings.ReplaceAll(v, WorkspaceEnvToken, workdir)
	}
	return out
}

// EnvReferencesWorkspaceRoot reports whether any value carries the reserved
// ${FLEET_WORKSPACE_ROOT} token. The two workspace tokens never collide:
// "${FLEET_WORKSPACE}" (closing brace included) is not a substring of
// "${FLEET_WORKSPACE_ROOT}".
func EnvReferencesWorkspaceRoot(env map[string]string) bool {
	for _, v := range env {
		if strings.Contains(v, WorkspaceRootEnvToken) {
			return true
		}
	}
	return false
}

// WorkspaceRootDir returns the absolute workspace root every spawn path
// substitutes for ${FLEET_WORKSPACE_ROOT}: FLEET_WORKSPACE_ROOT (legacy
// CHAT_/CUTLASS_ aliases honored), else ./workspace resolved against the
// process cwd — the same root SharedMCPWorkspaceDir and OpenStableMCPWorkspace
// nest under. It creates nothing on disk.
func WorkspaceRootDir() string {
	root := mcpWorkspaceRoot()
	if abs, err := filepath.Abs(root); err == nil {
		return abs
	}
	return root
}

// ExpandWorkspaceRootEnv returns a copy of env with every
// ${FLEET_WORKSPACE_ROOT} occurrence replaced by WorkspaceRootDir(). There is
// always a root to offer, so — unlike ExpandWorkspaceEnv — no key is ever
// dropped. A map with no token references is returned as-is (no copy).
func ExpandWorkspaceRootEnv(env map[string]string) map[string]string {
	if !EnvReferencesWorkspaceRoot(env) {
		return env
	}
	root := WorkspaceRootDir()
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = strings.ReplaceAll(v, WorkspaceRootEnvToken, root)
	}
	return out
}

// mcpWorkspaceRoot resolves the base directory MCP workspace dirs live under:
// FLEET_WORKSPACE_ROOT (or the legacy CHAT_/CUTLASS_ aliases), else the
// conventional ./workspace — the same root the per-conversation workspaces use
// (internal/tools.WorkspaceDirForConversation), kept as an inlined lookup so
// agentcore stays dependency-free of the driver tool package.
func mcpWorkspaceRoot() string {
	if root := EnvPrefix("").lookup("WORKSPACE_ROOT"); root != "" {
		return root
	}
	return "workspace"
}

// SharedMCPWorkspaceDir returns the stable per-deployment directory substituted
// for ${FLEET_WORKSPACE} on shared (process-lifetime) MCP spawns, creating it
// best-effort. Creation failure is logged and the path still returned: the
// subprocess env stays deterministic and the server surfaces its own I/O error
// if it truly cannot write there.
func SharedMCPWorkspaceDir() string {
	dir := filepath.Join(mcpWorkspaceRoot(), sharedMCPWorkspaceSubdir)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		log.Printf("mcp workspace: could not create shared dir %s: %v", dir, err)
	}
	return dir
}

// OpenStableMCPWorkspace returns the writable directory for one run's
// dedicated MCP client, keyed by a caller-supplied occurrence identity (e.g.
// "task-<uuid>"): <workspace-root>/mcp-runs/<key>, plus an *os.Root anchored at
// it for the caller's own host-side writes (the caller closes it). The SAME key
// always yields the SAME directory, created idempotently, so every attempt of
// one occurrence (a max_retries retry, a connector-unavailable infra retry, a
// lease recovery) re-mounts the ledger the earlier attempt wrote. That is the
// cutlass contract, and it is what lets Fleet's start-of-run create
// reconciliation tell a retry which deals/emails the previous attempt already
// booked. A different key (the next recurring occurrence, a re-run, a clone)
// gets its own directory, so one occurrence's ledger never leaks into another.
//
// The directory lives under the workspace root, which the sandbox mounts
// read-write, so an earlier attempt may have tampered with it. Every host-side
// operation here therefore goes through an os.Root opened at the workspace root
// (nothing resolves outside it), the mcp-runs base and the run dir must be real
// directories — a symlink or file squatting on either fails the setup closed
// rather than being mounted — and any symlink, FIFO, socket or device planted
// at the top of the run dir (e.g. creates.jsonl -> a host file, or a FIFO that
// would hang the ledger read) is removed before a connector is spawned on it. Writes through the returned root cannot leave the run dir.
//
// The directory is deliberately NOT cleaned up here: it holds the run ledger,
// which is post-run evidence of the critical actions the run recorded, and
// nothing in Fleet prunes mcp-runs/ today (run-history retention only deletes
// DB rows). There is NO fallback to the shared per-deployment dir on failure:
// that dir has no task scoping, so using it would silently mix this task's
// ledger with every other task's. The error is returned and the run's MCP
// setup fails loudly instead.
func OpenStableMCPWorkspace(key string) (string, *os.Root, error) {
	if err := validateWorkdirKey(key); err != nil {
		return "", nil, err
	}
	rootDir := mcpWorkspaceRoot()
	if abs, err := filepath.Abs(rootDir); err == nil {
		rootDir = abs
	}
	// The workspace root itself is operator configuration, not
	// sandbox-writable (only its contents are mounted), so plain MkdirAll is
	// fine for it; everything below is resolved through the root.
	if err := os.MkdirAll(rootDir, 0o750); err != nil {
		return "", nil, fmt.Errorf("mcp workspace: create workspace root %s: %w", rootDir, err)
	}
	ws, err := os.OpenRoot(rootDir)
	if err != nil {
		return "", nil, fmt.Errorf("mcp workspace: open workspace root %s: %w", rootDir, err)
	}
	defer func() { _ = ws.Close() }()
	if err := mkdirNoFollow(ws, perRunMCPWorkspaceSubdir, 0o750); err != nil {
		return "", nil, err
	}
	rel := filepath.Join(perRunMCPWorkspaceSubdir, key)
	if err := mkdirNoFollow(ws, rel, 0o700); err != nil {
		return "", nil, err
	}
	run, err := ws.OpenRoot(rel)
	if err != nil {
		return "", nil, fmt.Errorf("mcp workspace: open run dir %s: %w", rel, err)
	}
	// Pin the handle to the directory just validated: a swap between the
	// Lstat and the open would otherwise anchor the root at another directory.
	opened, err := run.Stat(".")
	if err == nil {
		var checked fs.FileInfo
		if checked, err = ws.Lstat(rel); err == nil && !os.SameFile(opened, checked) {
			err = errors.New("changed while it was being opened")
		}
	}
	if err == nil {
		err = removeTopLevelSpecialFiles(run)
	}
	if err != nil {
		_ = run.Close()
		return "", nil, fmt.Errorf("mcp workspace: run dir %s: %w", rel, err)
	}
	return filepath.Join(rootDir, rel), run, nil
}

// mkdirNoFollow creates name inside root if absent and requires that what is
// there is a real directory: a symlink (even one to a directory) or a file is
// refused, never followed.
func mkdirNoFollow(root *os.Root, name string, perm fs.FileMode) error {
	if err := root.Mkdir(name, perm); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("mcp workspace: create %s: %w", name, err)
	}
	info, err := root.Lstat(name)
	if err != nil {
		return fmt.Errorf("mcp workspace: stat %s: %w", name, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("mcp workspace: %s exists and is not a directory (a symlink is refused, not followed)", name)
	}
	return nil
}

// removeTopLevelSpecialFiles unlinks every entry directly inside the run dir
// that is neither a regular file nor a directory. Fleet and the connectors
// only ever create those two there, so anything else was planted by the
// sandbox: a symlink to redirect a host-side write (a connector appending its
// ledger, Fleet staging inputs), or a FIFO/socket/device that would block or
// misdirect a host-side read such as the start-of-run ledger reconciliation.
func removeTopLevelSpecialFiles(run *os.Root) error {
	dir, err := run.Open(".")
	if err != nil {
		return err
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		return err
	}
	for _, e := range entries {
		if t := e.Type(); t.IsRegular() || t.IsDir() {
			continue
		}
		if err := run.Remove(e.Name()); err != nil {
			return fmt.Errorf("remove planted %s %s: %w", e.Type(), e.Name(), err)
		}
		log.Printf("mcp workspace: removed planted %s %s from %s", e.Type(), e.Name(), run.Name())
	}
	return nil
}

// validateWorkdirKey rejects keys that are not a single safe path segment.
// Unlike a MkdirTemp prefix, a stable key cannot be sanitized by folding
// characters: two distinct identities must never collapse to one directory.
func validateWorkdirKey(key string) error {
	if key == "" || key == "." || key == ".." || len(key) > 200 {
		return fmt.Errorf("mcp workspace: invalid run key %q", key)
	}
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("mcp workspace: invalid run key %q", key)
		}
	}
	return nil
}

// StdioCwd decides the working directory a stdio MCP subprocess launches in.
//
// A server writes a RELATIVE output path — one the model passed as `output_dir`,
// or one of its own defaults — against its cwd. While that cwd was the client
// bundle root, every such write landed in the operator's git checkout: invisible
// to the agent (the sandbox never mounts the bundle writable), untouched by
// reclamation (which sweeps the data dir, not the bundle), and accumulating
// client data inside a git repo. Two shipped servers additionally allowlist
// os.getcwd() as a readable root for email attachments, which made the whole
// checkout an attachable source.
//
// So when a spawn path has a fleet-managed workspace to offer, the subprocess
// launches THERE — the same directory ${FLEET_WORKSPACE} resolves to, which on
// the interactive broker path is the per-conversation workspace the agent's own
// bash/run_python already work in. A stray relative write then lands somewhere
// the agent can actually read, and in the worst case somewhere reclaimable.
//
// Two cases keep the fallback:
//   - pinned (Agent Plugins): the spec requires the plugin root, and its args
//     are opaque strings fleet may not rewrite (ADR-0054);
//   - no workspace on offer: nothing better to point at, so behaviour is
//     unchanged rather than guessed.
//
// The workspace is used ONLY if it already exists as a directory. exec refuses
// to start a process whose cwd is missing ("chdir ...: no such file or
// directory"), so an un-materialized workspace path — a scope may carry one
// that nothing has created yet — would turn a cosmetic improvement into a
// server that will not boot. This function therefore never creates anything and
// never fails: worst case it returns the old fallback.
func StdioCwd(fallbackDir string, pinned bool, workdir string) string {
	if pinned {
		return fallbackDir
	}
	w := strings.TrimSpace(workdir)
	if w == "" {
		return fallbackDir
	}
	if info, err := os.Stat(w); err != nil || !info.IsDir() {
		return fallbackDir
	}
	return w
}
