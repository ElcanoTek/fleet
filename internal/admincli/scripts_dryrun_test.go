// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repoRootFromTest walks up from the package dir to the repo root (the dir that
// holds scripts/bootstrap.sh) so the script smoke tests run regardless of cwd.
func repoRootFromTest(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "scripts", "bootstrap.sh")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate repo root (no scripts/bootstrap.sh above %s)", dir)
		}
		dir = parent
	}
}

// runScript executes a repo script and returns combined output + error. Callers
// asserting success use runScriptDryRun; callers asserting a refusal path use
// the error directly. Skips when bash is unavailable. extraEnv entries are
// appended after the baseline env (so they win).
func runScript(t *testing.T, extraEnv []string, script string, args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping operator-script smoke test")
	}
	root := repoRootFromTest(t)
	full := append([]string{filepath.Join(root, "scripts", script)}, args...)
	cmd := exec.Command("bash", full...)
	cmd.Dir = root
	// A dry-run reads the bundle manifest; point at the in-repo generic bundle so
	// the test is self-contained and never depends on an external checkout.
	cmd.Env = append(os.Environ(),
		"FLEET_CLIENT_CONFIG_DIR="+filepath.Join(root, "config", "default"),
		"TERM=dumb",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runScriptDryRun executes a repo script with --dry-run and returns combined
// output. It fails the test on a non-zero exit (a dry-run must never touch the
// box, so it should always succeed). Skips when bash is unavailable.
func runScriptDryRun(t *testing.T, script string, args ...string) string {
	t.Helper()
	out, err := runScript(t, nil, script, args...)
	if err != nil {
		t.Fatalf("%s %v exited non-zero: %v\n--- output ---\n%s", script, args, err, out)
	}
	return out
}

// TestBootstrapDryRunSmoke is the regression guard for #91 (operator-script
// coverage): `bootstrap.sh --dry-run` must succeed and its plan must still
// include the steps the readiness audit fixed — the pg_hba scram rewrite (#78)
// and, with --enable-service, the build+install of the fleet binary (#71).
func TestBootstrapDryRunSmoke(t *testing.T) {
	out := runScriptDryRun(t, "bootstrap.sh", "--dry-run", "--postgres=local", "--enable-service")
	for _, want := range []string{
		// The toolchain-install STEP must be in the plan. We assert only the
		// step header (always printed), NOT the dnf package line — that line only
		// renders on a dnf host, and CI runs on a non-dnf (apt) runner where the
		// step prints the "install these yourself" warning instead.
		"Installing system dependencies",
		"client bundle manifest found",
		"pg_hba",                                 // the scram-sha-256 loopback rewrite step (#78)
		"Building + installing the fleet binary", // the binary build+install step (#71)
		"would install fleet + fleet-admin",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("bootstrap --dry-run plan missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// TestBootstrapFreshDBNameFlags — #718: --chat-db-name/--chat-db-user (and the
// sched twins) must override the colliding legacy defaults, and the planned SQL
// must provision exactly the overridden names.
func TestBootstrapFreshDBNameFlags(t *testing.T) {
	out := runScriptDryRun(t, "bootstrap.sh", "--dry-run", "--postgres=local",
		"--chat-db-name", "fleet_chat", "--chat-db-user", "fleet_chat_user",
		"--sched-db-name", "fleet_sched", "--sched-db-user", "fleet_sched_user")
	for _, want := range []string{
		"CREATE ROLE fleet_chat_user",
		"CREATE DATABASE fleet_chat OWNER fleet_chat_user",
		"CREATE ROLE fleet_sched_user",
		"CREATE DATABASE fleet_sched OWNER fleet_sched_user",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("bootstrap --dry-run plan missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// TestBootstrapRejectsUnsafeDBIdentifier — the names are interpolated into the
// provisioning SQL, so anything beyond a plain identifier must die up front.
func TestBootstrapRejectsUnsafeDBIdentifier(t *testing.T) {
	out, err := runScript(t, nil, "bootstrap.sh", "--dry-run", "--postgres=local",
		"--chat-db-name", "bad-name;drop")
	if err == nil {
		t.Fatalf("bootstrap accepted an unsafe DB identifier\n--- output ---\n%s", out)
	}
	if !strings.Contains(out, "plain identifier") {
		t.Errorf("expected the identifier-validation error, got:\n%s", out)
	}
}

// TestBootstrapAdoptExistingDBFlags — #718: adoption must skip provisioning for
// that pair (no CREATE, no ALTER — a pre-existing role's password is never
// rotated) and validate the operator-supplied DSN instead.
func TestBootstrapAdoptExistingDBFlags(t *testing.T) {
	env := []string{
		// An isolated env file so a developer's real .env.local can't leak DSNs in.
		"FLEET_ENV_FILE=" + filepath.Join(t.TempDir(), "fleet.env"),
		"FLEET_CHAT_DATABASE_URL=postgres://legacychat:pw@127.0.0.1:5432/legacychat?sslmode=disable",
		"FLEET_SCHED_DATABASE_URL=postgres://legacysched:pw@127.0.0.1:5432/legacysched?sslmode=disable",
	}
	out, err := runScript(t, env, "bootstrap.sh", "--dry-run", "--postgres=local",
		"--adopt-existing-chat-db", "--adopt-existing-sched-db")
	if err != nil {
		t.Fatalf("adopt dry-run exited non-zero: %v\n--- output ---\n%s", err, out)
	}
	for _, want := range []string{
		"--adopt-existing-chat-db — skipping role/database provisioning",
		"--adopt-existing-sched-db — skipping role/database provisioning",
		"both databases adopted — nothing to provision",
		"would validate the adopted chat DSN",
		"would validate the adopted sched DSN",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("adopt dry-run plan missing %q\n--- output ---\n%s", want, out)
		}
	}
	// The adopted pair must never appear in provisioning SQL.
	for _, forbid := range []string{"CREATE ROLE", "ALTER ROLE"} {
		if strings.Contains(out, forbid) {
			t.Errorf("adopt dry-run plan still provisions roles (%q present)\n--- output ---\n%s", forbid, out)
		}
	}
}

// TestBootstrapAdoptRequiresDSN — adoption without the operator's DSN must fail
// fast (before any provisioning work), never guess a password.
func TestBootstrapAdoptRequiresDSN(t *testing.T) {
	env := []string{"FLEET_ENV_FILE=" + filepath.Join(t.TempDir(), "fleet.env")}
	out, err := runScript(t, env, "bootstrap.sh", "--dry-run", "--postgres=local",
		"--adopt-existing-chat-db")
	if err == nil {
		t.Fatalf("bootstrap accepted --adopt-existing-chat-db without a DSN\n--- output ---\n%s", out)
	}
	if !strings.Contains(out, "--adopt-existing-chat-db needs the existing database's working DSN") {
		t.Errorf("expected the missing-DSN error, got:\n%s", out)
	}
}

// TestBootstrapAdoptRejectsExternalMode — external mode already takes the
// operator's DSNs verbatim; combining it with adopt flags is a config error.
func TestBootstrapAdoptRejectsExternalMode(t *testing.T) {
	out, err := runScript(t, nil, "bootstrap.sh", "--dry-run", "--postgres=external",
		"--adopt-existing-chat-db")
	if err == nil {
		t.Fatalf("bootstrap accepted adopt flags with --postgres=external\n--- output ---\n%s", out)
	}
	if !strings.Contains(out, "applies to --postgres=local") {
		t.Errorf("expected the external-mode rejection, got:\n%s", out)
	}
}

// TestBootstrapDBGuardAndCaddyProtectionPresent — #718: the load-bearing safety
// strings must stay in the script. The guard refuses to provision over a
// pre-existing role/db this script did not record in its env file (the ALTER
// ROLE therefore only ever converges roles the script itself provisioned), and
// the Caddy path refuses to overwrite a foreign /etc/caddy/Caddyfile without
// --force-caddy (and then keeps a timestamped backup).
func TestBootstrapDBGuardAndCaddyProtectionPresent(t *testing.T) {
	root := repoRootFromTest(t)
	body, err := os.ReadFile(filepath.Join(root, "scripts", "bootstrap.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	for _, want := range []string{
		"guard_preexisting chat",
		"guard_preexisting sched",
		"--adopt-existing-chat-db",
		"--adopt-existing-sched-db",
		"--force-caddy",
		"/etc/caddy/Caddyfile.fleet-backup.",
		"caddyfile_is_foreign",
		`CADDY_MARKER="# Managed by fleet (scripts/bootstrap.sh)`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("bootstrap must contain %q", want)
		}
	}
}

// TestFleetServiceWantsPostgres — #718: After= only orders units; Wants= is
// what pulls the local cluster up with fleet after a reboot.
func TestFleetServiceWantsPostgres(t *testing.T) {
	root := repoRootFromTest(t)
	body, err := os.ReadFile(filepath.Join(root, "deploy", "fleet.service"))
	if err != nil {
		t.Fatal(err)
	}
	unit := string(body)
	for _, want := range []string{
		"After=network-online.target postgresql.service",
		"Wants=network-online.target postgresql.service",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("deploy/fleet.service must contain %q", want)
		}
	}
}

func TestBootstrapWiresRemoteMCPPublicOrigin(t *testing.T) {
	root := repoRootFromTest(t)
	body, err := os.ReadFile(filepath.Join(root, "scripts", "bootstrap.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	for _, want := range []string{
		`upsert_env FLEET_PUBLIC_BASE_URL "$origin"`,
		`ensure_env_b64_key FLEET_MCP_OAUTH_ENCRYPTION_KEY 32`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("bootstrap must contain %q", want)
		}
	}
}

func TestUpdateReconcilesRemoteMCPPublicOrigin(t *testing.T) {
	root := repoRootFromTest(t)
	body, err := os.ReadFile(filepath.Join(root, "scripts", "update.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	for _, want := range []string{
		`upsert_env_file "$backend_env_file" FLEET_PUBLIC_BASE_URL "$web_origin"`,
		`upsert_env_file "$backend_env_file" FLEET_MCP_OAUTH_ENCRYPTION_KEY`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("update must contain %q", want)
		}
	}
}

// TestUpdateDryRunSmoke is the regression guard for #91: `update.sh
// --dry-run --no-pull` must succeed and its plan must include the binary
// build + the install-to-deploy-path step (#71 — without which an update is a
// silent no-op against the live binary).
func TestUpdateDryRunSmoke(t *testing.T) {
	out := runScriptDryRun(t, "update.sh", "--dry-run", "--no-pull")
	for _, want := range []string{
		"make build",
		"would install fleet + fleet-admin", // the install-to-ExecStart step (#71)
		"Restarting",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("update --dry-run plan missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// TestFleetUpgradeDryRunSmoke is the regression guard for #305 (drain-and-restart
// upgrade): `fleet-upgrade.sh --dry-run --yes` must succeed and its plan must
// include the load-bearing steps — build, back up the live binary (so rollback is
// possible), restart (which sends SIGTERM → the binary's graceful drain), and gate
// on the /readyz probe. --yes skips the confirm prompt; the script also guards the
// prompt behind a TTY so the test (no TTY) never blocks regardless.
func TestFleetUpgradeDryRunSmoke(t *testing.T) {
	out := runScriptDryRun(t, "fleet-upgrade.sh", "--dry-run", "--yes")
	for _, want := range []string{
		"make build",
		"Backing up the live binaries", // the rollback-backup step
		"would install",                // the swap-in-new-binary step
		// The step header is always printed; the literal "systemctl restart" line
		// only renders on a systemd host, and CI may run without systemd (mirrors
		// TestUpdateDryRunSmoke asserting "Restarting", not "systemctl restart").
		"Restarting",
		"/readyz",           // the readiness gate
		"NOT zero-downtime", // honest brief-blip disclosure
	} {
		if !strings.Contains(out, want) {
			t.Errorf("fleet-upgrade --dry-run plan missing %q\n--- output ---\n%s", want, out)
		}
	}
}
