package agentcore

import (
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
//     gets its own MCP client) substitute a fresh PerRunMCPWorkspaceDir, giving
//     cutlass-parity per-run ledger semantics.
//
// A spawn path that has NO directory to offer (workdir == "") DROPS every env
// key whose value still references the token, so the server sees the var as
// unset — the servers' documented inert/fail-safe posture — rather than as a
// literal "${FLEET_WORKSPACE}" path or a confusing blank.
const (
	// WorkspaceEnvToken is the reserved token as it appears in a manifest env
	// value. Only this bare spelling is reserved; ${FLEET_WORKSPACE:-x} forms
	// are not supported.
	//nolint:gosec // G101 false positive: an interpolation placeholder name, not a credential.
	WorkspaceEnvToken = "${FLEET_WORKSPACE}"

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

// PerRunMCPWorkspaceDir mints a fresh writable directory for one run's
// dedicated MCP client (prefix names the run, e.g. "task-<id>-"). The directory
// is deliberately NOT cleaned up at run end: it holds the run ledger, which is
// post-run evidence of the critical actions the run recorded (mirroring the
// cutlass per-run workdir contract). On failure it falls back to the shared
// per-deployment dir so the spawn still gets managed-run semantics.
func PerRunMCPWorkspaceDir(prefix string) string {
	base := filepath.Join(mcpWorkspaceRoot(), perRunMCPWorkspaceSubdir)
	if abs, err := filepath.Abs(base); err == nil {
		base = abs
	}
	if err := os.MkdirAll(base, 0o750); err != nil {
		log.Printf("mcp workspace: could not create per-run base %s (falling back to shared): %v", base, err)
		return SharedMCPWorkspaceDir()
	}
	dir, err := os.MkdirTemp(base, sanitizeWorkdirPrefix(prefix))
	if err != nil {
		log.Printf("mcp workspace: could not mint per-run dir under %s (falling back to shared): %v", base, err)
		return SharedMCPWorkspaceDir()
	}
	return dir
}

// sanitizeWorkdirPrefix bounds a caller-supplied per-run prefix to a safe
// single-segment MkdirTemp pattern (path separators folded, empty defaulted).
func sanitizeWorkdirPrefix(prefix string) string {
	prefix = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, strings.TrimSpace(prefix))
	if prefix == "" {
		prefix = "run-"
	}
	return prefix
}
