package agentcore

import (
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

// TestWorkspaceDirs pins the directory layout the substitution offers: both
// the shared per-deployment dir and minted per-run dirs live under the
// (FLEET_WORKSPACE_ROOT-configurable) workspace root, and per-run dirs are
// unique per mint.
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

	run1 := PerRunMCPWorkspaceDir("task-abc-")
	run2 := PerRunMCPWorkspaceDir("task-abc-")
	if run1 == run2 {
		t.Errorf("per-run dirs must be unique, both %q", run1)
	}
	base := filepath.Join(root, "mcp-runs") + string(filepath.Separator)
	if !strings.HasPrefix(run1, base) {
		t.Errorf("per-run dir %q not under %q", run1, base)
	}
	if !strings.Contains(filepath.Base(run1), "task-abc-") {
		t.Errorf("per-run dir %q should carry the run prefix", run1)
	}
}

// TestSanitizeWorkdirPrefix pins that a hostile/odd prefix cannot escape the
// per-run base directory via separators or blow up MkdirTemp.
func TestSanitizeWorkdirPrefix(t *testing.T) {
	if got := sanitizeWorkdirPrefix("../weird/name "); strings.ContainsAny(got, "/\\ ") {
		t.Errorf("sanitized prefix %q still contains separators/spaces", got)
	}
	if got := sanitizeWorkdirPrefix("  "); got != "run-" {
		t.Errorf("empty prefix should default to run-, got %q", got)
	}
}
