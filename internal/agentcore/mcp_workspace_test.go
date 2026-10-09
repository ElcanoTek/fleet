package agentcore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExpandWorkspaceEnv pins the reserved ${FLEET_WORKSPACE} substitution
// contract the client bundles rely on for CUTLASS_RUN_WORKDIR-style vars:
// token replaced when a workdir is offered, key DROPPED (not blanked, not a
// literal token) when none is, token-free maps returned untouched.
func TestExpandWorkspaceEnv(t *testing.T) {
	t.Run("substitutes the workdir", func(t *testing.T) {
		env := map[string]string{
			"CUTLASS_RUN_WORKDIR": WorkspaceEnvToken,
			"CUTLASS_REPORT_DIR":  WorkspaceEnvToken + "/reports",
			"STATIC":              "value",
		}
		out := ExpandWorkspaceEnv(env, "/var/lib/fleet/run-1")
		if got := out["CUTLASS_RUN_WORKDIR"]; got != "/var/lib/fleet/run-1" {
			t.Errorf("CUTLASS_RUN_WORKDIR = %q", got)
		}
		if got := out["CUTLASS_REPORT_DIR"]; got != "/var/lib/fleet/run-1/reports" {
			t.Errorf("CUTLASS_REPORT_DIR = %q (token must compose with a suffix path)", got)
		}
		if got := out["STATIC"]; got != "value" {
			t.Errorf("STATIC = %q, want passthrough", got)
		}
		// The input map is never mutated (bases are shared across runs).
		if env["CUTLASS_RUN_WORKDIR"] != WorkspaceEnvToken {
			t.Error("input map was mutated")
		}
	})

	t.Run("empty workdir drops token-bearing keys", func(t *testing.T) {
		env := map[string]string{
			"CUTLASS_RUN_WORKDIR": WorkspaceEnvToken,
			"STATIC":              "value",
		}
		out := ExpandWorkspaceEnv(env, "")
		if _, ok := out["CUTLASS_RUN_WORKDIR"]; ok {
			t.Errorf("token-bearing key must be dropped when no workdir is offered, got %q", out["CUTLASS_RUN_WORKDIR"])
		}
		if out["STATIC"] != "value" {
			t.Error("token-free key must survive")
		}
	})

	t.Run("token-free map returned as-is", func(t *testing.T) {
		env := map[string]string{"A": "1"}
		if out := ExpandWorkspaceEnv(env, "/anywhere"); out["A"] != "1" || len(out) != 1 {
			t.Errorf("unexpected result %v", out)
		}
	})
}

// TestEnvReferencesWorkspace pins the lazy-directory-creation predicate.
func TestEnvReferencesWorkspace(t *testing.T) {
	if EnvReferencesWorkspace(map[string]string{"A": "x"}) {
		t.Error("token-free env must not report a workspace reference")
	}
	if !EnvReferencesWorkspace(map[string]string{"A": "pre" + WorkspaceEnvToken + "post"}) {
		t.Error("embedded token must be detected")
	}
}

// TestEnvReferencesTaskID pins the per-task-identity-requested predicate: a
// bundle that references ${FLEET_TASK_ID} is asking for an identity only the
// dedicated per-run client can supply, and the scheduler warns when a run
// cannot offer one.
func TestEnvReferencesTaskID(t *testing.T) {
	if EnvReferencesTaskID(map[string]string{"A": "x"}) {
		t.Error("token-free env must not report a task-id reference")
	}
	if EnvReferencesTaskID(map[string]string{"A": WorkspaceEnvToken}) {
		t.Error("the workspace token is a different token and must not match")
	}
	if !EnvReferencesTaskID(map[string]string{"CUTLASS_MOC_TASK_ID": TaskIDEnvToken}) {
		t.Error("task-id token must be detected")
	}
	if !EnvReferencesTaskID(map[string]string{"A": "pre" + TaskIDEnvToken + "post"}) {
		t.Error("embedded token must be detected")
	}
}

// TestWorkspaceDirs pins the directory layout the substitution offers: both
// the shared per-deployment dir and per-run dirs live under the
// (FLEET_WORKSPACE_ROOT-configurable) workspace root, and per-run dirs are
// stable per key.
func TestWorkspaceDirs(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)

	shared := SharedMCPWorkspaceDir()
	if want := filepath.Join(root, "mcp-shared"); shared != want {
		t.Errorf("SharedMCPWorkspaceDir = %q, want %q", shared, want)
	}
	if again := SharedMCPWorkspaceDir(); again != shared {
		t.Errorf("shared dir must be stable across calls: %q vs %q", again, shared)
	}

	run1 := openStable(t, "task-abc")
	run2 := openStable(t, "task-def")
	if run1 == run2 {
		t.Errorf("distinct occurrences must get distinct dirs, both %q", run1)
	}
	base := filepath.Join(root, "mcp-runs") + string(filepath.Separator)
	if !strings.HasPrefix(run1, base) {
		t.Errorf("per-run dir %q not under %q", run1, base)
	}
	if filepath.Base(run1) != "task-abc" {
		t.Errorf("per-run dir %q should be named by the run key", run1)
	}
}

// openStable opens a stable run dir and closes its root at test end.
func openStable(t *testing.T, key string) string {
	t.Helper()
	dir, run, err := OpenStableMCPWorkspace(key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	return dir
}

// TestStableMCPWorkspaceDir pins the retry contract: the same key re-mounts the
// same directory with its ledger intact, and unsafe keys are refused rather
// than folded onto another occurrence's directory.
func TestStableMCPWorkspaceDir(t *testing.T) {
	t.Setenv("FLEET_WORKSPACE_ROOT", t.TempDir())

	first := openStable(t, "task-abc")
	ledger := filepath.Join(first, "creates.jsonl")
	if err := os.WriteFile(ledger, []byte("{\"deal\":1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := openStable(t, "task-abc")
	if first != second {
		t.Fatalf("same key must reuse the dir: %q vs %q", first, second)
	}
	if got, err := os.ReadFile(ledger); err != nil || string(got) != "{\"deal\":1}\n" {
		t.Fatalf("ledger must survive re-mount: %q, %v", got, err)
	}
	if fi, err := os.Stat(first); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, %v; want 0700", fi.Mode().Perm(), err)
	}
	for _, bad := range []string{"", ".", "..", "../x", "a/b", "a b", "task-é"} {
		if _, run, err := OpenStableMCPWorkspace(bad); err == nil {
			_ = run.Close()
			t.Errorf("key %q must be rejected", bad)
		}
	}
}

// TestOpenStableMCPWorkspaceRefusesSymlinks pins Codex P1 on #1709: the run dir
// lives in the sandbox-writable workspace mount, so a symlink the sandbox
// planted in place of the mcp-runs base or the run dir is refused (never
// followed), a symlink planted inside the run dir (e.g. over the ledger name)
// is unlinked without touching its target, and the returned root cannot
// write outside the run dir.
func TestOpenStableMCPWorkspaceRefusesSymlinks(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
	victim := t.TempDir()
	victimFile := filepath.Join(victim, "host.txt")
	if err := os.WriteFile(victimFile, []byte("host"), 0o600); err != nil {
		t.Fatal(err)
	}

	dir, run, err := OpenStableMCPWorkspace("task-abc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.OpenFile("../escape", os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		t.Error("the run root must not resolve outside the run dir")
	}
	_ = run.Close()

	// A symlink planted over the ledger name is removed, its target untouched.
	planted := filepath.Join(dir, "creates.jsonl")
	if err := os.Symlink(victimFile, planted); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "real.jsonl"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	openStable(t, "task-abc")
	if _, err := os.Lstat(planted); !os.IsNotExist(err) {
		t.Fatalf("planted symlink must be removed: %v", err)
	}
	if b, err := os.ReadFile(victimFile); err != nil || string(b) != "host" {
		t.Fatalf("symlink target touched: %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "real.jsonl")); err != nil || string(b) != "keep" {
		t.Fatalf("regular ledger file must survive: %q, %v", b, err)
	}

	// The run dir itself replaced by a symlink: refused.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, dir); err != nil {
		t.Fatal(err)
	}
	if _, run, err := OpenStableMCPWorkspace("task-abc"); err == nil {
		_ = run.Close()
		t.Fatal("symlinked run dir must be refused")
	}

	// The mcp-runs base replaced by a symlink: refused, nothing created there.
	base := filepath.Join(root, "mcp-runs")
	if err := os.RemoveAll(base); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, base); err != nil {
		t.Fatal(err)
	}
	if _, run, err := OpenStableMCPWorkspace("task-new"); err == nil {
		_ = run.Close()
		t.Fatal("symlinked mcp-runs base must be refused")
	}
	if _, err := os.Lstat(filepath.Join(victim, "task-new")); !os.IsNotExist(err) {
		t.Fatalf("run dir created through the symlinked base: %v", err)
	}
}

// TestStableMCPWorkspaceDirFailsLoud pins that a creation failure is an error,
// never a silent fallback to the unscoped shared dir.
func TestStableMCPWorkspaceDirFailsLoud(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
	if err := os.WriteFile(filepath.Join(root, "mcp-runs"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if dir, run, err := OpenStableMCPWorkspace("task-abc"); err == nil {
		_ = run.Close()
		t.Fatalf("expected error, got dir %q", dir)
	}
}

// TestExpandWorkspaceRootEnv pins the ${FLEET_WORKSPACE_ROOT} contract: the
// token resolves to the absolute workspace root on every path, composes with
// a suffix or a neighbouring value, is never dropped, does not collide with
// ${FLEET_WORKSPACE}, and leaves token-free maps untouched.
func TestExpandWorkspaceRootEnv(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)

	env := map[string]string{
		"CUTLASS_ALLOWED_DIRS": "/etc/extra:" + WorkspaceRootEnvToken,
		"REPORT_ROOT":          WorkspaceRootEnvToken + "/reports",
		"CUTLASS_RUN_WORKDIR":  WorkspaceEnvToken,
		"STATIC":               "value",
	}
	if EnvReferencesWorkspace(map[string]string{"A": WorkspaceRootEnvToken}) {
		t.Fatal("a root-only reference must not read as a ${FLEET_WORKSPACE} reference (it would mint a per-run dir)")
	}
	if !EnvReferencesWorkspaceRoot(env) || EnvReferencesWorkspaceRoot(map[string]string{"A": WorkspaceEnvToken}) {
		t.Fatal("EnvReferencesWorkspaceRoot must match exactly the root token")
	}

	out := ExpandWorkspaceRootEnv(env)
	if got := out["CUTLASS_ALLOWED_DIRS"]; got != "/etc/extra:"+root {
		t.Errorf("CUTLASS_ALLOWED_DIRS = %q, want the root composed after the operator value", got)
	}
	if got := out["REPORT_ROOT"]; got != filepath.Join(root, "reports") {
		t.Errorf("REPORT_ROOT = %q, want root + suffix", got)
	}
	if got := out["CUTLASS_RUN_WORKDIR"]; got != WorkspaceEnvToken {
		t.Errorf("CUTLASS_RUN_WORKDIR = %q, the per-run token belongs to ExpandWorkspaceEnv", got)
	}
	if out["STATIC"] != "value" || len(out) != len(env) {
		t.Errorf("token-free keys must pass through, got %v", out)
	}
	if env["CUTLASS_ALLOWED_DIRS"] != "/etc/extra:"+WorkspaceRootEnvToken {
		t.Error("input map was mutated")
	}

	// The per-run expansion with NO workdir drops ${FLEET_WORKSPACE} keys but
	// must leave a root-only key alone: the root is always offerable.
	dropped := ExpandWorkspaceEnv(env, "")
	if _, ok := dropped["CUTLASS_RUN_WORKDIR"]; ok {
		t.Error("per-run token must still be dropped without a workdir")
	}
	if dropped["CUTLASS_ALLOWED_DIRS"] != env["CUTLASS_ALLOWED_DIRS"] {
		t.Error("root-only key must survive the per-run drop")
	}

	plain := map[string]string{"A": "1"}
	if got := ExpandWorkspaceRootEnv(plain); got["A"] != "1" || len(got) != 1 {
		t.Errorf("token-free map must come back as-is, got %v", got)
	}
	if !filepath.IsAbs(WorkspaceRootDir()) {
		t.Errorf("WorkspaceRootDir must be absolute, got %q", WorkspaceRootDir())
	}
}
