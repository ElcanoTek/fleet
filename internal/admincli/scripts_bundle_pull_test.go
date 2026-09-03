package admincli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// seedBundleRepo creates a bundle checkout parked on a branch with no upstream —
// the shape a box lands in after someone checks out a feature branch there and
// forgets. `git pull --ff-only` refuses in that state, which is how a bundle
// silently stops tracking its remote while fleet itself keeps updating.
func seedBundleRepo(t *testing.T, branch string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("mcp_servers: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "--quiet", "-m", "seed")
	if branch != "main" {
		run("checkout", "--quiet", "-b", branch)
	}
	return dir
}

// TestUpdateReportsBundleIdentity — every `fleet update` must say which bundle
// it touched and whether that bundle moved.
//
// "✓ client config pulled" printed identically for a checkout that advanced 40
// commits and one that advanced none, and the final banner reported only
// fleet's SHA. A deployment is fleet AND its bundle — connector display names
// and descriptions, personas, protocols and the MCP catalog all live in the
// bundle — so an update that advanced fleet while the bundle stood still looked
// like complete success and showed up only as stale copy in the UI.
func TestUpdateReportsBundleIdentity(t *testing.T) {
	dir := seedBundleRepo(t, "leftover-feature-branch")
	out := runScriptDryRun(t, "update.sh", "--dry-run", "--no-pull",
		"--client-config", dir)
	if !strings.Contains(out, "bundle leftover-feature-branch:") {
		t.Errorf("update must name the bundle's branch and sha\n--- output ---\n%s", out)
	}
}

// TestUpdateReconcilesBundleAgainstServiceEnv — update.sh deliberately does not
// source the service's 0600 env file, so its own resolution (env →
// --client-config → state file → the in-repo generic bundle) can land on a
// DIFFERENT checkout than the one fleet loads. It then pulls a bundle nobody
// reads and reports success. When the dir was merely fallback-resolved, the
// service's own configuration wins; when the operator named one explicitly,
// theirs does — but never silently.
func TestUpdateReconcilesBundleAgainstServiceEnv(t *testing.T) {
	svcBundle := seedBundleRepo(t, "main")
	envFile := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(envFile, []byte("FLEET_CLIENT_CONFIG_DIR="+svcBundle+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("adopts the service's bundle when ours was only a fallback", func(t *testing.T) {
		// FLEET_CLIENT_CONFIG_DIR cleared so the script falls back the way an
		// interactive `fleet update` on a real box does.
		out, err := runScript(t, []string{
			"FLEET_CLIENT_CONFIG_DIR=",
			"FLEET_ENV_FILE=" + envFile,
		}, "update.sh", "--dry-run", "--no-pull")
		if err != nil {
			t.Fatalf("dry run failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, "bundle dir reconciled") || !strings.Contains(out, svcBundle) {
			t.Errorf("must adopt the bundle the service actually reads\n--- output ---\n%s", out)
		}
	})

	t.Run("warns but honours an explicit --client-config", func(t *testing.T) {
		mine := seedBundleRepo(t, "main")
		out, err := runScript(t, []string{"FLEET_ENV_FILE=" + envFile},
			"update.sh", "--dry-run", "--no-pull", "--client-config", mine)
		if err != nil {
			t.Fatalf("dry run failed: %v\n%s", err, out)
		}
		if !strings.Contains(out, "bundle mismatch") {
			t.Errorf("an explicit choice that differs from the service's must warn\n--- output ---\n%s", out)
		}
		if !strings.Contains(out, "honouring your explicit choice") {
			t.Errorf("the operator's explicit choice must still win\n--- output ---\n%s", out)
		}
	})
}

// TestUpdateReexecPreservesBundleExplicitness — the self-update re-exec passes
// no argv, so it restates every flag as an env var; FLEET_CLIENT_CONFIG_DIR is
// therefore ALWAYS set on the re-exec'd run. Inferring "the operator named this
// dir explicitly" from that variable alone would make the re-exec'd run treat a
// fallback-resolved dir as an explicit choice — and then REFUSE to reconcile it
// against the bundle the service actually loads, which is the exact failure the
// reconcile exists to prevent, on the one run an operator gets after pulling
// the fix. FLEET_CLIENT_CONFIG_EXPLICIT carries the original answer across.
func TestUpdateReexecPreservesBundleExplicitness(t *testing.T) {
	root := repoRootFromTest(t)
	body, err := os.ReadFile(filepath.Join(root, "scripts", "update.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	if !strings.Contains(script, `FLEET_CLIENT_CONFIG_EXPLICIT="$CLIENT_DIR_EXPLICIT"`) {
		t.Error("the self-update re-exec must forward CLIENT_DIR_EXPLICIT, or a re-exec'd run mistakes a fallback dir for an explicit one")
	}
	if !strings.Contains(script, `[[ -n "${FLEET_CLIENT_CONFIG_EXPLICIT:-}" ]] && CLIENT_DIR_EXPLICIT="$FLEET_CLIENT_CONFIG_EXPLICIT"`) {
		t.Error("the forwarded value must override the env-var inference, which the re-exec always trips")
	}

	// End to end: a re-exec'd run whose bundle dir came from a FALLBACK must
	// still reconcile to the service's bundle rather than treating the
	// re-exec's own export as the operator's choice.
	svcBundle := seedBundleRepo(t, "main")
	envFile := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(envFile, []byte("FLEET_CLIENT_CONFIG_DIR="+svcBundle+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := seedBundleRepo(t, "main")
	out, err := runScript(t, []string{
		"FLEET_ENV_FILE=" + envFile,
		"FLEET_CLIENT_CONFIG_DIR=" + other,
		"FLEET_CLIENT_CONFIG_EXPLICIT=0", // as the re-exec sets it after a fallback
	}, "update.sh", "--dry-run", "--no-pull")
	if err != nil {
		t.Fatalf("dry run failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "bundle dir reconciled") {
		t.Errorf("a re-exec'd fallback must still reconcile to the service's bundle\n--- output ---\n%s", out)
	}
}
