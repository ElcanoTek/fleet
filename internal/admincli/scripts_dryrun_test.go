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

// TestBootstrapInstallsBackupTimer — #966: an --enable-service run must plan the
// backup timer (a deployment that was never told about backups is the one that
// has none), including the sensitive-by-default backup directory and the env
// keys the unit resolves its output directory and retention from.
func TestBootstrapInstallsBackupTimer(t *testing.T) {
	out := runScriptDryRun(t, "bootstrap.sh", "--dry-run", "--postgres=local", "--enable-service")
	for _, want := range []string{
		"Scheduled database backups",
		"would create /var/backups/fleet if missing (0700 root-owned",
		"would set FLEET_BACKUP_DIR + FLEET_BACKUP_RETENTION_DAYS=30",
		"would install deploy/fleet-backup.service + deploy/fleet-backup.timer",
		"systemctl enable --now fleet-backup.timer",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("bootstrap --dry-run plan missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// TestBootstrapNoBackupTimerOptOut — the opt-out must skip the whole timer path
// (no unit install, no env keys) and say what the operator now owns.
func TestBootstrapNoBackupTimerOptOut(t *testing.T) {
	out := runScriptDryRun(t, "bootstrap.sh", "--dry-run", "--postgres=local",
		"--enable-service", "--no-backup-timer")
	if !strings.Contains(out, "--no-backup-timer: no scheduled backup on this box") {
		t.Errorf("opt-out plan missing the no-backup notice\n--- output ---\n%s", out)
	}
	for _, forbid := range []string{
		"would install deploy/fleet-backup.service",
		"systemctl enable --now fleet-backup.timer",
		"FLEET_BACKUP_RETENTION_DAYS=",
	} {
		if strings.Contains(out, forbid) {
			t.Errorf("opt-out plan still installs the timer (%q present)\n--- output ---\n%s", forbid, out)
		}
	}
}

// TestBootstrapRejectsUnsafeBackupSettings — both settings land in the env file
// the timer reads: a relative directory would put dumps in the unit's "/" cwd,
// and a non-numeric retention would make --prune's cutoff silently default.
func TestBootstrapRejectsUnsafeBackupSettings(t *testing.T) {
	for _, tc := range []struct{ env, wantErr string }{
		{"FLEET_BACKUP_DIR=backups", "FLEET_BACKUP_DIR must be an absolute path"},
		{"FLEET_BACKUP_RETENTION_DAYS=0", "FLEET_BACKUP_RETENTION_DAYS must be a positive integer"},
		{"FLEET_BACKUP_RETENTION_DAYS=thirty", "FLEET_BACKUP_RETENTION_DAYS must be a positive integer"},
	} {
		out, err := runScript(t, []string{tc.env}, "bootstrap.sh", "--dry-run", "--postgres=local", "--enable-service")
		if err == nil {
			t.Errorf("bootstrap accepted %s\n--- output ---\n%s", tc.env, out)
			continue
		}
		if !strings.Contains(out, tc.wantErr) {
			t.Errorf("%s: expected %q, got:\n%s", tc.env, tc.wantErr, out)
		}
	}
	// …but only on the runs that write them. A dev run installs no unit, so a
	// relative FLEET_BACKUP_DIR exported for a local `fleet backup` must not
	// refuse the whole bootstrap.
	if out, err := runScript(t, []string{"FLEET_BACKUP_DIR=backups"}, "bootstrap.sh",
		"--dry-run", "--postgres=local"); err != nil {
		t.Errorf("a run that installs no timer must not validate FLEET_BACKUP_DIR: %v\n--- output ---\n%s", err, out)
	}
}

// TestBootstrapKeepsOperatorBackupSettings — #966 review: every other key in
// this env file survives a re-run (the DSNs read themselves back out, secrets
// are generate-if-absent). The backup settings must too: resetting a relocated
// FLEET_BACKUP_DIR would silently move the dumps back onto the boot volume.
func TestBootstrapKeepsOperatorBackupSettings(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "fleet.env")
	const existing = "FLEET_BACKUP_DIR=/mnt/backup-volume\nFLEET_BACKUP_RETENTION_DAYS=14\n"
	if err := os.WriteFile(envFile, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runScript(t, []string{"FLEET_ENV_FILE=" + envFile}, "bootstrap.sh",
		"--dry-run", "--postgres=local", "--enable-service")
	if err != nil {
		t.Fatalf("bootstrap --dry-run exited non-zero: %v\n--- output ---\n%s", err, out)
	}
	for _, want := range []string{
		"daily fleet-backup.timer → /mnt/backup-volume",
		"FLEET_BACKUP_RETENTION_DAYS=14",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("re-run plan lost the operator's backup settings (missing %q)\n--- output ---\n%s", want, out)
		}
	}
	if strings.Contains(out, "/var/backups/fleet") {
		t.Errorf("re-run plan fell back to the default backup directory\n--- output ---\n%s", out)
	}
}

// TestBackupUnitsShipped — #966: the timer pair lives in deploy/ (version
// controlled, and covered by doctor's unit-drift check) rather than as fenced
// blocks in the doc. The load-bearing lines: the oneshot exits non-zero on a
// failed dump (which is what doctor's failed-run check reads), the output
// directory has an in-unit default the env file overrides, and the timer is the
// enable-able half.
func TestBackupUnitsShipped(t *testing.T) {
	root := repoRootFromTest(t)
	service, err := os.ReadFile(filepath.Join(root, "deploy", "fleet-backup.service"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Type=oneshot",
		"EnvironmentFile=/etc/fleet/fleet.env",
		"Environment=FLEET_BACKUP_DIR=/var/backups/fleet",
		"ExecStart=/usr/local/bin/fleet backup --db=all --prune",
		"UMask=0077",
	} {
		if !strings.Contains(string(service), want) {
			t.Errorf("deploy/fleet-backup.service must contain %q", want)
		}
	}
	// The service is timer-triggered: an [Install] section would invite
	// `systemctl enable fleet-backup.service`, which schedules nothing. Match
	// the section header itself, not the header comment that explains this.
	for _, line := range strings.Split(string(service), "\n") {
		if strings.TrimSpace(line) == "[Install]" {
			t.Error("deploy/fleet-backup.service must not carry an [Install] section — enable the timer")
		}
	}
	timer, err := os.ReadFile(filepath.Join(root, "deploy", "fleet-backup.timer"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"OnCalendar=*-*-* 02:00:00",
		"Persistent=true",
		"WantedBy=timers.target",
	} {
		if !strings.Contains(string(timer), want) {
			t.Errorf("deploy/fleet-backup.timer must contain %q", want)
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
	// The sandbox gate must run BEFORE the install step: its fail-closed abort
	// (missing image, failed build) has to leave the box coherent — old
	// binaries on disk, old service running. With the old order the die fired
	// AFTER the new binaries were installed, leaving new code on disk while
	// the old service kept running and the message implied nothing changed.
	sandboxAt := strings.Index(out, "Rebuilding the sandbox image")
	buildAt := strings.Index(out, "Building the fleet binary + web app")
	if sandboxAt < 0 || buildAt < 0 {
		t.Fatalf("plan missing the sandbox (%d) or build (%d) step header\n--- output ---\n%s", sandboxAt, buildAt, out)
	}
	if sandboxAt > buildAt {
		t.Errorf("sandbox gate (at %d) must run before the binary/web install step (at %d)\n--- output ---\n%s", sandboxAt, buildAt, out)
	}
}

// TestBuildSandboxImagePrintTag — update.sh's rebuild gate keys on this exact
// output (the tag a build would produce), so the query mode must resolve the
// manifest's sandbox.tag without podman and without building anything.
func TestBuildSandboxImagePrintTag(t *testing.T) {
	out, err := runScript(t, nil, "build-sandbox-image.sh", "--print-tag")
	if err != nil {
		t.Fatalf("--print-tag exited non-zero: %v\n--- output ---\n%s", err, out)
	}
	if got := strings.TrimSpace(out); got != "localhost/fleet-sandbox:latest" {
		t.Errorf("--print-tag = %q, want the generic bundle's sandbox.tag", got)
	}
}

// TestBuildSandboxImagePrintTagRenamedBundle — a bundle that renames
// sandbox.tag with an unchanged Containerfile must resolve to the NEW tag;
// that resolution is what forces update.sh to rebuild instead of leaving the
// service asking podman for an image that was never built.
func TestBuildSandboxImagePrintTagRenamedBundle(t *testing.T) {
	dir := t.TempDir()
	manifest := "sandbox:\n  containerfile: sandbox/Containerfile\n  tag: localhost/fleet-sandbox-renamed:latest\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runScript(t, []string{"FLEET_CLIENT_CONFIG_DIR=" + dir}, "build-sandbox-image.sh", "--print-tag")
	if err != nil {
		t.Fatalf("--print-tag exited non-zero: %v\n--- output ---\n%s", err, out)
	}
	if got := strings.TrimSpace(out); got != "localhost/fleet-sandbox-renamed:latest" {
		t.Errorf("--print-tag = %q, want the renamed tag", got)
	}
}

// TestBuildSandboxImageTargetsServiceStore — a root-run build must land in the
// systemd unit's User= rootless store, never root's rootful store (which the
// User=fleet unit cannot see). The real build needs podman + the unit, so this
// pins the load-bearing strings instead (the TestBootstrapDBGuardAnd-
// CaddyProtectionPresent style for un-dry-runnable paths).
func TestBuildSandboxImageTargetsServiceStore(t *testing.T) {
	root := repoRootFromTest(t)
	body, err := os.ReadFile(filepath.Join(root, "scripts", "build-sandbox-image.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	for _, want := range []string{
		`systemctl show -p User --value "${FLEET_SERVICE_NAME:-fleet}.service"`,
		`runuser -u "$BUILD_USER"`,
		`XDG_RUNTIME_DIR="/run/${BUILD_USER}"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("build-sandbox-image.sh must contain %q", want)
		}
	}
}

// TestUpdateSandboxGatePresent — the sandbox rebuild gate must key on the
// resolved image tag as well as the Containerfile hash, and must rebuild when
// the tag is missing from the service user's store (a rename with an unchanged
// Containerfile, or an image lost to a prune/pre-fix root build, otherwise
// boots clean and breaks only on the first tool call).
func TestUpdateSandboxGatePresent(t *testing.T) {
	root := repoRootFromTest(t)
	body, err := os.ReadFile(filepath.Join(root, "scripts", "update.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	for _, want := range []string{
		`build-sandbox-image.sh" --print-tag`,
		"sandbox-image.ref",
		"sandbox_podman image exists",
		"sandbox_podman image prune -f",
		`FLEET_SERVICE_NAME="$SERVICE_NAME"`,
		`runuser -u "$service_user"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("update.sh must contain %q", want)
		}
	}
}

// TestUpdatePrebuiltImageSkipsSandboxBuild — a bundle that resolves
// sandbox.image to a prebuilt ref is consumed by the service as a registry
// pull (internal/clientconfig: image WINS over tag), so update.sh must not
// key its rebuild gate on sandbox.tag and burn a multi-GB on-box build the
// service will never read. Mirrors bootstrap.sh's resolve_sandbox_image skip.
func TestUpdatePrebuiltImageSkipsSandboxBuild(t *testing.T) {
	dir := t.TempDir()
	// The bundle ships a Containerfile TOO: image must win over the
	// build-on-box path, not merely cover the no-Containerfile case.
	manifest := "sandbox:\n  containerfile: sandbox/Containerfile\n  tag: localhost/fleet-sandbox:latest\n  image: ghcr.io/example/fleet-sandbox:v7\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sandbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sandbox", "Containerfile"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertSkip := func(env []string, wantRef string) {
		t.Helper()
		out, err := runScript(t, env, "update.sh", "--dry-run", "--no-pull")
		if err != nil {
			t.Fatalf("update --dry-run exited non-zero: %v\n--- output ---\n%s", err, out)
		}
		if !strings.Contains(out, "sandbox.image="+wantRef) {
			t.Errorf("plan must skip the on-box build for the prebuilt %s\n--- output ---\n%s", wantRef, out)
		}
		if strings.Contains(out, "build-sandbox-image.sh") {
			t.Errorf("plan still schedules an on-box sandbox build\n--- output ---\n%s", out)
		}
	}
	assertSkip([]string{"FLEET_CLIENT_CONFIG_DIR=" + dir}, "ghcr.io/example/fleet-sandbox:v7")

	// The generic bundle's image key is "${FLEET_SANDBOX_IMAGE:-}". The
	// SERVICE resolves that var from its EnvironmentFile
	// (/etc/fleet/fleet.env), so update.sh must interpolate from the SAME
	// place — a value set there must produce the same skip.
	envFile := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(envFile, []byte("FLEET_SANDBOX_IMAGE=ghcr.io/example/env-file:v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertSkip([]string{"FLEET_ENV_FILE=" + envFile}, "ghcr.io/example/env-file:v1")

	// …and a var exported only in the update SHELL must NOT skip the gate:
	// the service never sees the shell's environment, so honoring it here
	// silently skipped the whole sandbox step — absence probe included —
	// recreating the boots-clean-breaks-on-first-tool-call failure the gate
	// exists to prevent. The empty env file isolates the run from any real
	// /etc/fleet/fleet.env on the host.
	emptyEnvFile := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(emptyEnvFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runScript(t, []string{
		"FLEET_ENV_FILE=" + emptyEnvFile,
		"FLEET_SANDBOX_IMAGE=ghcr.io/example/shell-only:v1",
	}, "update.sh", "--dry-run", "--no-pull")
	if err != nil {
		t.Fatalf("update --dry-run exited non-zero: %v\n--- output ---\n%s", err, out)
	}
	if strings.Contains(out, "sandbox.image=") {
		t.Errorf("a shell-only FLEET_SANDBOX_IMAGE must not skip the sandbox gate (the service cannot see it)\n--- output ---\n%s", out)
	}
	if !strings.Contains(out, "build-sandbox-image.sh") {
		t.Errorf("plan must keep the on-box sandbox build when the env file leaves the image unset\n--- output ---\n%s", out)
	}
}

// TestUpdateFailedSandboxBuildRefusesRestart — a failed sandbox build is only
// survivable while the resolved ref still exists in the service user's store
// (Containerfile changed under the same tag: the old image is stale but
// serviceable). When the ref is absent — a sandbox.tag rename plus one
// transient build failure — continuing would leave the box reporting healthy
// while every sandboxed tool call fails, so update.sh must die BEFORE the
// install step, leaving old binaries on disk and the old service running.
// The failure path needs a real failing podman build, so this pins the
// load-bearing lines the way TestUpdateSandboxGatePresent does.
func TestUpdateFailedSandboxBuildRefusesRestart(t *testing.T) {
	root := repoRootFromTest(t)
	body, err := os.ReadFile(filepath.Join(root, "scripts", "update.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	for _, want := range []string{
		// Only a verified still-present ref downgrades the failure to a warn…
		`if [[ -n "$ref_now" && "$(sandbox_image_state "$ref_now")" == "present" ]]; then`,
		// …everything else refuses to install the update, with the recovery
		// spelled out — and, because the gate runs before the install step,
		// the die's nothing-changed claim is actually true.
		`die "sandbox image build failed`,
		"refusing to install",
		"nothing was installed and the ${SERVICE_NAME} service was NOT restarted",
		`finish:  fleet update --no-pull`,
		// The store probe must not read an environmental podman failure (e.g.
		// the unit stopped and /run/<user> absent) as "image missing": the
		// runtime dir is pre-created like build-sandbox-image.sh does, and
		// only exit code 1 — podman's positive "not found" — means absent.
		`install -d -o "$service_user" -g "$service_user" -m 0700 "/run/${service_user}"`,
		`sandbox_image_state() {`,
		`absent) build_reason="${ref_now} missing from the sandbox image store"`,
		// The image ref is resolved from the service's env file (doctor.sh's
		// env_get idiom), never from the update shell's environment — the
		// unit reads EnvironmentFile=, not this shell.
		`env_get "$var" "$backend_env_file"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("update.sh must contain %q", want)
		}
	}
}

// TestUpdateAdoptsBackupUnits — the unit-adoption loop must cover the shipped
// fleet-backup AND fleet-maintenance pairs (a timer fix otherwise reaches
// provisioned boxes only via doctor, not the update path operators actually
// run on release), and its timer-unit hint must NOT say restart: the
// daemon-reload alone re-arms a rewritten timer, and restarting the backup
// oneshot would run a backup immediately — the same no-bounce rule doctor.sh
// applies. Absent units are skipped by the loop's both-files-exist check, so a
// box that declined a timer is never force-installed by the drift pass.
func TestUpdateAdoptsBackupUnits(t *testing.T) {
	root := repoRootFromTest(t)
	body, err := os.ReadFile(filepath.Join(root, "scripts", "update.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	for _, want := range []string{
		`for unit in fleet.service fleet-web.service fleet-backup.service fleet-backup.timer fleet-maintenance.service fleet-maintenance.timer; do`,
		`case "$unit" in fleet-backup.*|fleet-maintenance.*) is_timer_unit=1 ;; esac`,
		// The timer-unit adopt hint ends at daemon-reload — no restart clause.
		`warn "  adopt:  install -m 0644 $shipped $installed && systemctl daemon-reload"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("update.sh must contain %q", want)
		}
	}
}

// TestUpdateOffersMissingTimers — after the drift loop, an update must OFFER a
// fully-missing fleet-backup / fleet-maintenance pair (interactive y/N,
// default No) instead of silently leaving the box unprotected until someone
// reads doctor's output. Load-bearing rules pinned as strings (the prompt
// itself needs a TTY + a box with systemd and a missing pair, which CI is
// not): the offer is gated on --no-timers / FLEET_UPDATE_OFFER_TIMERS so a
// deliberate decline never nags, only a FULLY missing pair is offered (a
// half-installed one already got the drift loop's treatment), and a yes
// delegates to `fleet timers install` — one implementation, not a second
// inline copy of the install.
func TestUpdateOffersMissingTimers(t *testing.T) {
	root := repoRootFromTest(t)
	body, err := os.ReadFile(filepath.Join(root, "scripts", "update.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	for _, want := range []string{
		`--no-timers)      OFFER_TIMERS=0 ;;`,
		`OFFER_TIMERS="${FLEET_UPDATE_OFFER_TIMERS:-1}"`,
		`[[ "$OFFER_TIMERS" != "0" ]]`,
		`if systemctl cat "fleet-${_name}.service" >/dev/null 2>&1 || systemctl cat "fleet-${_name}.timer" >/dev/null 2>&1; then`,
		`"$fleet_bin" timers install "--${_name}" --src "$SRC_DIR"`,
		`(y/N)`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("update.sh must contain %q", want)
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
