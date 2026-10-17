// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
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
		"would install fleet",
		// A bare --enable-service install stages the in-repo default bundle
		// where the service can relabel it (#1655).
		"would stage",
		"/bundle (owned by fleet",
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
		// The marker itself moved to scripts/lib/caddyfile.sh (the one
		// renderer, shared with update.sh + doctor.sh); bootstrap must source
		// it rather than carry a second copy — see TestCaddyfileMarkerParity.
		`. "$SCRIPT_DIR/lib/caddyfile.sh"`,
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
		"would install fleet", // the install-to-ExecStart step (#71)
		"would remove a leftover fleet-admin shim", // the retired shim's eviction (ADR-0060)
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

// TestUpdateSandboxFreshnessBackstop — the rebuild gate must ALSO fire on age:
// a bundle whose Containerfile and tag never change used to mean the deployed
// image was never rebuilt at all, serving weeks-old base layers and unpatched
// package CVEs on boxes that ran `fleet update` regularly (the Grype CI gate
// scans a fresh build — only a rebuild brings a deployed box up to what CI
// vouched for). The age path needs a real image in a real store, so this pins
// the load-bearing lines plus the dry-run plan text.
func TestUpdateSandboxFreshnessBackstop(t *testing.T) {
	root := repoRootFromTest(t)
	body, err := os.ReadFile(filepath.Join(root, "scripts", "update.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	for _, want := range []string{
		// The knob with its default, and the age probe (creation time read from
		// the SERVICE user's store via sandbox_podman, like every other probe).
		`SANDBOX_MAX_AGE_DAYS="${FLEET_SANDBOX_MAX_AGE_DAYS:-7}"`,
		`sandbox_image_age_days() {`,
		`sandbox_podman image inspect --format '{{.Created.Unix}}'`,
		// Only a readable creation time triggers — empty means unknown, and an
		// unanswerable probe must not burn a multi-GB rebuild.
		`[[ -n "$image_age_days" ]] && (( image_age_days >= SANDBOX_MAX_AGE_DAYS ))`,
		// An age-triggered rebuild must bypass the layer cache: a cached build
		// against an unmoved base reproduces the SAME image with the same old
		// creation date, so the backstop would re-fire forever refreshing nothing.
		`sandbox_build_no_cache=1`,
		`FLEET_SANDBOX_BUILD_NO_CACHE="$sandbox_build_no_cache"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("update.sh must contain %q", want)
		}
	}

	// Every build (any trigger) must re-check the registry for a fresher base:
	// without --pull=newer, podman reuses whatever stale base sits in the local
	// store and the generic Containerfile's "base tracks latest for security
	// patches" intent never actually happens.
	builder, err := os.ReadFile(filepath.Join(root, "scripts", "build-sandbox-image.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`BUILD_ARGS=(--pull=newer)`,
		`NO_CACHE="${FLEET_SANDBOX_BUILD_NO_CACHE:-0}"`,
		`BUILD_ARGS+=(--no-cache)`,
	} {
		if !strings.Contains(string(builder), want) {
			t.Errorf("build-sandbox-image.sh must contain %q", want)
		}
	}

	// The dry-run plan must surface the backstop with the effective threshold —
	// --sandbox-max-age overrides the default.
	out := runScriptDryRun(t, "update.sh", "--dry-run", "--no-pull", "--sandbox-max-age", "3")
	if !strings.Contains(out, "installed image is 3+ days old") {
		t.Errorf("update --dry-run plan missing the freshness backstop with the flag's threshold\n--- output ---\n%s", out)
	}

	// A non-numeric threshold gates a multi-GB rebuild, so it must die up front
	// rather than silently disabling the backstop.
	out, err = runScript(t, nil, "update.sh", "--dry-run", "--no-pull", "--sandbox-max-age", "weekly")
	if err == nil {
		t.Fatalf("update accepted a non-numeric --sandbox-max-age\n--- output ---\n%s", out)
	}
	if !strings.Contains(out, "must be a non-negative integer") {
		t.Errorf("expected the max-age validation error, got:\n%s", out)
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

// sliceBetween returns the part of s between the first occurrence of start and
// the first occurrence of end after it, failing the test when either marker is
// missing — a moved marker must surface as a loud test failure rather than a
// silently empty region that asserts nothing.
func sliceBetween(t *testing.T, s, start, end string) string {
	t.Helper()
	i := strings.Index(s, start)
	if i < 0 {
		t.Fatalf("marker %q not found", start)
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		t.Fatalf("marker %q not found after %q", end, start)
	}
	return rest[:j]
}

// TestUpdateReexecForwardsFlagState — the self-update re-exec (update.sh
// re-running the copy the pull just installed) passes NO argv, so every
// setting a command-line flag can change must be restated as its env
// equivalent on the `exec env` line. A flag missed there is silently
// downgraded to its default on exactly the run that pulled the fix — the
// hardest run to notice it on, and one the operator only gets once. That is
// how --sandbox-max-age, --adopt-units and --no-timers were all dropped.
//
// So this derives the flag-settable variables from the arg parser itself
// rather than hardcoding a list: a NEW flag fails this test until it is either
// forwarded or given a documented reason not to be.
func TestUpdateReexecForwardsFlagState(t *testing.T) {
	root := repoRootFromTest(t)
	body, err := os.ReadFile(filepath.Join(root, "scripts", "update.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)

	// Deliberate non-forwards, each with the reason it is safe to omit.
	exempt := map[string]string{
		"SRC_DIR":    "re-derives from the script path the exec names",
		"NO_PULL":    "forwarding it would skip the client-bundle pull the re-exec exists to preserve",
		"DRY_RUN":    "a dry run never fast-forwards, so it never reaches the re-exec",
		"ASSUME_YES": "hardcoded to 1 on the exec line: the operator already confirmed this commit range",
	}

	parser := sliceBetween(t, script, "while [[ $# -gt 0 ]]; do", "\ndone\n")
	reexec := sliceBetween(t, script, "exec env FLEET_UPDATE_REEXEC=1", `bash "$SRC_DIR/scripts/update.sh"`)

	assigned := regexp.MustCompile(`\b([A-Z][A-Z0-9_]{2,})=(?:"|[0-9])`)
	seen := map[string]bool{}
	for _, m := range assigned.FindAllStringSubmatch(parser, -1) {
		v := m[1]
		if seen[v] {
			continue
		}
		seen[v] = true
		if _, ok := exempt[v]; ok {
			continue
		}
		if !strings.Contains(reexec, `="$`+v+`"`) {
			t.Errorf("--flag sets %s but the self-update re-exec does not forward it: add FLEET_…=%q to the `exec env` line, or add %s to the exempt map with the reason it is safe to drop", v, "$"+v, v)
		}
	}
	if len(seen) < 8 {
		t.Fatalf("only %d flag-settable variables found in the arg parser — the parser markers likely moved and this test is asserting nothing", len(seen))
	}
	// The three that were missing, pinned by name so a future rewrite of the
	// exec line cannot quietly drop them again.
	for _, want := range []string{
		`FLEET_SANDBOX_MAX_AGE_DAYS="$SANDBOX_MAX_AGE_DAYS"`,
		`FLEET_UPDATE_ADOPT_UNITS="$ADOPT_UNITS"`,
		`FLEET_UPDATE_OFFER_TIMERS="$OFFER_TIMERS"`,
	} {
		if !strings.Contains(reexec, want) {
			t.Errorf("the self-update re-exec must forward %s", want)
		}
	}
}

// TestUpdateUnresolvedSandboxTagIsNotUpToDate — when the resolved sandbox tag
// comes back empty, BOTH store-aware gates go blind: the presence probe and the
// max-age freshness backstop each need a ref. That case used to fall out of the
// gate's if/elif chain with no build reason set and print
// `sandbox image up to date (…, tag unresolved)` — a clean bill of health for
// an image nothing had looked at, which is how a weeks-old sandbox sits on a
// box that updates cleanly every day. It must warn instead, and say what was
// skipped.
func TestUpdateUnresolvedSandboxTagIsNotUpToDate(t *testing.T) {
	root := repoRootFromTest(t)
	body, err := os.ReadFile(filepath.Join(root, "scripts", "update.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	for _, want := range []string{
		// The blind-spot flag, and the two ways a gate fails to reach an answer.
		`sandbox_unverified=""`,
		`sandbox_unverified="the image tag could not be resolved`,
		`sandbox_unverified="podman could not read the image store for ${ref_now}`,
		// Reported as a warning, never as "up to date", with what was skipped
		// and the by-hand recovery.
		`warn "sandbox image NOT verified and NOT rebuilt`,
		"neither the store-presence check nor the ${SANDBOX_MAX_AGE_DAYS}-day freshness backstop reached an answer",
		`diagnose the tag: FLEET_CLIENT_CONFIG_DIR=${CLIENT_DIR} ${SCRIPT_DIR}/build-sandbox-image.sh --print-tag`,
		// The resolver's stderr is kept, so the warning can name the cause
		// (--print-tag always prints a name:tag when it runs at all, so an
		// empty answer means the script itself could not run).
		`--print-tag 2>"$ref_err_file"`,
		`ref_err="$(head -n 1 "$ref_err_file"`,
		// The success line only fires with a ref in hand.
		"${cf_now:0:12}, ${ref_now}) — skipping the image build.",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("update.sh must contain %q", want)
		}
	}
	// The old reassuring fallback must be gone, not merely supplemented.
	if strings.Contains(script, "tag unresolved") {
		t.Error(`update.sh still reports "tag unresolved" as an up-to-date sandbox image`)
	}
}

// ── the node handoff ────────────────────────────────────────────────────────

// nodeStubDir writes a `node` shim reporting the given version and returns its
// directory, for prepending to PATH. fleet_resolve_node_bin prefers a versioned
// /usr/bin/node-NN and falls back to `node` on PATH, so a stub claiming a very
// high major forces the "resolved" branch on ANY box — including one that has no
// qualifying interpreter at all. That is what makes the pass-path assertions
// below deterministic rather than a function of the runner's node version.
func nodeStubDir(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	// A very high major so it outranks anything the runner already has, in the
	// PATH-fallback branch of fleet_resolve_node_bin.
	shim := "#!/bin/sh\n[ \"$1\" = \"-v\" ] && { echo " + version + "; exit 0; }\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "node"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// fakeSrcDir builds the minimum a script needs to accept a --src/SRC_DIR: a
// go.mod (so the "is this a fleet checkout?" guard passes) and a web/.nvmrc
// declaring the node major. It is the only deterministic lever on the *refusal*
// path — a runner that already has node 24 cannot be made to lack it, but it can
// be pointed at a checkout demanding node 99.
func fakeSrcDir(t *testing.T, nvmrc string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"go.mod":           "module example.test\n",
		"web/.nvmrc":       nvmrc + "\n",
		"web/package.json": "{}\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// hermeticNodeEnv points doctor.sh at a throwaway checkout AND a nonexistent
// fleet-web.env, so a --node run depends on nothing but the node interpreters
// actually installed. Without the second half these tests read this box's real
// /etc/fleet/fleet-web.env: a stale stamp there (pointing at an interpreter that
// has since been removed) fails them for a reason unrelated to the code, which
// is exactly what happened while this suite was being written.
func hermeticNodeEnv(t *testing.T, nvmrc string) []string {
	t.Helper()
	return []string{
		"SRC_DIR=" + fakeSrcDir(t, nvmrc),
		"FLEET_WEB_ENV_FILE=" + filepath.Join(t.TempDir(), "absent-fleet-web.env"),
	}
}

// TestUpdateDryRunResolvesNodeRatherThanClaimingTo — `update --dry-run` is the
// command an operator runs to ask "will this work on my box?", and the node gate
// is what aborts the real run. The plan used to print
// "would resolve node >= web/.nvmrc" WITHOUT ever calling the resolver, so on a
// box the real run refuses it printed a clean checklist, a green
// "fleet rebuilt at <sha>" banner, and exit 0.
//
// The pass path is forced with a node stub so this holds on any runner.
func TestUpdateDryRunResolvesNodeRatherThanClaimingTo(t *testing.T) {
	env := []string{"PATH=" + nodeStubDir(t, "v999.0.0") + string(os.PathListSeparator) + os.Getenv("PATH")}
	out, err := runScript(t, env, "update.sh", "--dry-run", "--no-pull")
	if err != nil {
		t.Fatalf("update --dry-run exited non-zero: %v\n--- output ---\n%s", err, out)
	}
	if !strings.Contains(out, "node gate PASSES") {
		t.Fatalf("a v999 node on PATH must satisfy the gate\n--- output ---\n%s", out)
	}
	// The resolved-value rule: the pass must name what it resolved, not what it
	// wanted, and must not fall back to the old un-backed phrasing.
	// Deliberately NOT asserting "v999.0.0": fleet_resolve_node_bin prefers a
	// versioned /usr/bin/node-NN over `node` on PATH, so on a runner that has one
	// the stub is outranked. The stub's job is only to guarantee that SOMETHING
	// qualifies; what is asserted is that the pass names what it resolved.
	if !strings.Contains(out, "resolved on this box, not assumed") {
		t.Errorf("a passing node gate must report the RESOLVED interpreter\n--- output ---\n%s", out)
	}
	if strings.Contains(out, "would resolve node") {
		t.Errorf("the plan still claims it \"would resolve node\" instead of resolving it\n--- output ---\n%s", out)
	}
	// A dry run built nothing, so it must not sign off with the real run's banner.
	for _, bad := range []string{"fleet rebuilt at", "fleet updated "} {
		if strings.Contains(out, bad) {
			t.Errorf("dry-run banner claims %q — nothing was built\n--- output ---\n%s", bad, out)
		}
	}
	if !strings.Contains(out, "dry run:") {
		t.Errorf("dry-run must close with a banner saying so\n--- output ---\n%s", out)
	}
}

// TestUpdateNodeGatePrecedesSandboxRebuild — the gate has to run before the first
// expensive, destructive step. It used to sit inside step 4, i.e. AFTER step 3
// could spend 2-3 minutes rebuilding the sandbox image and then prune the
// superseded layers — while still telling the operator nothing had been built.
// It cannot move above step 1 either: the floor is declared in the CHECKOUT's
// web/.nvmrc, so a pre-pull gate would read the OLD major.
func TestUpdateNodeGatePrecedesSandboxRebuild(t *testing.T) {
	out := runScriptDryRun(t, "update.sh", "--dry-run", "--no-pull")
	gateAt := strings.Index(out, "Node gate")
	sandboxAt := strings.Index(out, "Rebuilding the sandbox image")
	if gateAt < 0 || sandboxAt < 0 {
		t.Fatalf("plan missing the node gate (%d) or sandbox (%d) step header\n--- output ---\n%s", gateAt, sandboxAt, out)
	}
	if gateAt > sandboxAt {
		t.Errorf("the node gate (at %d) must run before the sandbox rebuild (at %d) — an abort after a multi-GB build is not a gate\n--- output ---\n%s", gateAt, sandboxAt, out)
	}
}

// TestDoctorNodeOnlyIsScoped — `doctor.sh --node` exists so update.sh can hand a
// node shortfall to the ONE implementation of the node install without invoking
// a full doctor pass. A full pass adopts drifted units, a write `fleet update`
// only performs behind explicit consent (--adopt-units) — so --node reaching the
// other steps would launder a consent-gated write through the node repair.
//
// Both the plan AND a real (--check) run are asserted: the dry-run branch exits
// early, so testing it alone would leave the scoping of the executing path
// unguarded.
func TestDoctorNodeOnlyIsScoped(t *testing.T) {
	outOfScope := []string{"Rootless podman", "Sandbox smoke", "functional drift", "Package currency", "Source freshness"}

	plan := runScriptDryRun(t, "doctor.sh", "--node", "--dry-run")
	for _, want := range []string{"nodejs", "FLEET_NODE_BIN"} {
		if !strings.Contains(plan, want) {
			t.Errorf("--node plan must name %q\n--- output ---\n%s", want, plan)
		}
	}
	for _, bad := range outOfScope {
		if strings.Contains(plan, bad) {
			t.Errorf("--node plan reaches out-of-scope step %q\n--- output ---\n%s", bad, plan)
		}
	}

	// The executing path. --check so it installs nothing; SRC_DIR points at a
	// checkout demanding node 1, so it passes on any runner and the run reaches
	// its scoped exit rather than dying early.
	run, err := runScript(t, hermeticNodeEnv(t, "1"), "doctor.sh", "--node", "--check")
	if err != nil {
		t.Fatalf("--node --check against a node-1 floor must succeed: %v\n--- output ---\n%s", err, run)
	}
	for _, bad := range outOfScope {
		if strings.Contains(run, bad) {
			t.Errorf("--node --check executed out-of-scope step %q — it must exit after the node blocks\n--- output ---\n%s", bad, run)
		}
	}
}

// TestDoctorNodeCheckIsUnprivilegedAndDecisive — `fleet update --check` shells out
// to exactly this, and that command is documented as a dev-box probe needing no
// root. It must run without the root refusal, and its exit code must track
// whether a qualifying interpreter actually resolves — not merely whether the
// script reached the end. Both outcomes are forced, so this holds on a runner
// with node 24 and on one without.
func TestDoctorNodeCheckIsUnprivilegedAndDecisive(t *testing.T) {
	for _, tc := range []struct {
		name    string
		nvmrc   string
		wantErr bool
	}{
		{"floor this box always meets", "1", false},
		{"floor no box can meet", "99", true},
	} {
		out, err := runScript(t, hermeticNodeEnv(t, tc.nvmrc), "doctor.sh", "--node", "--check")
		// Note: this arm is only meaningful when the suite runs unprivileged (the
		// usual case; CI runners are not root). Under uid 0 the root gate cannot
		// fire either way, so the decisive-exit assertion below is what carries
		// this test there.
		if strings.Contains(out, "run as root") {
			t.Fatalf("%s: --node --check must not demand root\n--- output ---\n%s", tc.name, out)
		}
		if !strings.Contains(out, "node only") {
			t.Fatalf("%s: --node --check did not run the scoped toolchain step\n--- output ---\n%s", tc.name, out)
		}
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: wantErr=%v, got err=%v\n--- output ---\n%s", tc.name, tc.wantErr, err, out)
		}
	}
}

// TestScriptHelpPrintsTheWholeHeaderAndNoShell — bootstrap/update/fleet-upgrade
// all rendered --help with a HARDCODED `sed -n '2,Np'` range, which rots the
// moment the header grows: update.sh silently dropped its last paragraph and
// fleet-upgrade.sh printed raw shell out of its own help. FEATURE-NOTES records
// fixing this once already. The help is now derived from the header block, and
// this pins both ends of the derivation.
func TestScriptHelpPrintsTheWholeHeaderAndNoShell(t *testing.T) {
	root := repoRootFromTest(t)
	for _, script := range []string{"bootstrap.sh", "update.sh", "fleet-upgrade.sh"} {
		out, err := runScript(t, nil, script, "--help")
		if err != nil {
			t.Fatalf("%s --help exited non-zero: %v\n--- output ---\n%s", script, err, out)
		}
		// No shell may leak past the end of the header comment block.
		for _, shell := range []string{"set -euo pipefail", "SCRIPT_DIR=", "#!/usr/bin/env"} {
			if strings.Contains(out, shell) {
				t.Errorf("%s --help leaks script body (%q) — the header range over-reaches\n--- output ---\n%s", script, shell, out)
			}
		}
		// And the whole block must be there: the LAST header line must appear.
		body, readErr := os.ReadFile(filepath.Join(root, "scripts", script))
		if readErr != nil {
			t.Fatal(readErr)
		}
		var last string
		for _, line := range strings.Split(string(body), "\n")[1:] {
			if !strings.HasPrefix(line, "#") {
				break
			}
			if trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "#"), " ")); trimmed != "" {
				last = trimmed
			}
		}
		if last == "" {
			t.Fatalf("%s has no header comment block to render", script)
		}
		if !strings.Contains(out, last) {
			t.Errorf("%s --help is truncated: it omits the final header line %q\n--- output ---\n%s", script, last, out)
		}
	}
}

// TestFleetUpgradeRestartsTheWebTier — deploy/fleet-web.service carries
// BindsTo=fleet.service, so `systemctl restart fleet` STOPS the web tier and
// systemd does not bring it back. update.sh has restarted it explicitly for
// exactly this reason; fleet-upgrade.sh did not, and then printed
// "fleet upgraded + healthy" with the web tier down.
func TestFleetUpgradeRestartsTheWebTier(t *testing.T) {
	out := runScriptDryRun(t, "fleet-upgrade.sh", "--dry-run")
	if !strings.Contains(out, "restart fleet-web") {
		t.Errorf("fleet-upgrade plan never restarts the BindsTo'd web tier\n--- output ---\n%s", out)
	}
	if !strings.Contains(out, "is-active") {
		t.Errorf("the fleet-web restart must be backed by a resolved-state read, not the restart's exit code\n--- output ---\n%s", out)
	}

	// The plan text above is one literal info line; deleting the actual call
	// would leave it green. There is no systemd here to exercise the real path,
	// so pin the invocation statically instead — on BOTH paths, because a
	// rollback restart stops the BindsTo'd web tier just as the upgrade one does.
	root := repoRootFromTest(t)
	body, err := os.ReadFile(filepath.Join(root, "scripts", "fleet-upgrade.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	if n := strings.Count(script, "\n    restart_web_tier"); n < 2 {
		t.Errorf("restart_web_tier is invoked on %d path(s); expected both the readiness-gate and rollback paths", n)
	}
	// And the banner must not claim health the run never measured.
	if !strings.Contains(script, `HEALTH_VERIFIED="skipped"`) || !strings.Contains(script, "health NOT verified") {
		t.Errorf("fleet-upgrade must track whether the readiness gate actually ran, and say so when it did not")
	}
}

// bundleLib runs one scripts/lib/bundle.sh function with the given PATH (so a
// test can take rsync away and exercise the rename fallback) and returns its
// combined output and exit status.
func bundleLib(t *testing.T, path, fn string, args ...string) (string, error) {
	t.Helper()
	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "bundle.sh")
	cmd := exec.Command("bash", append([]string{"-c", ". " + lib + " && " + fn + ` "$@"`, "bundle"}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+path)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// toolsOnlyPath builds a PATH holding only the named tools (symlinked from
// wherever they live), so a function can be exercised with a tool missing.
func toolsOnlyPath(t *testing.T, tools ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, tool := range tools {
		resolved, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s not available", tool)
		}
		if err := os.Symlink(resolved, filepath.Join(dir, tool)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestStageDefaultBundle exercises scripts/lib/bundle.sh's staging as the
// current user: the copy is complete and carries the marker naming its
// source; a re-stage drops a file gone upstream; a marker replaced by a
// symlink is replaced, never written through; a destination holding a bundle
// that is not a staged copy is refused and left intact; a source that is not
// a bundle, a relative destination and a symlinked destination are refused;
// and a box without rsync is refused rather than swapped by rename (which
// would empty the mounts of running sandboxes).
func TestStageDefaultBundle(t *testing.T) {
	me, err := exec.Command("id", "-un").Output()
	if err != nil {
		t.Skip("id -un unavailable")
	}
	owner := strings.TrimSpace(string(me))
	base := []string{"bash", "tar", "mkdir", "rm", "mktemp", "ls", "basename", "dirname", "readlink", "grep", "id", "cat", "printf", "cp", "find", "chmod", "mv", "dd"}
	t.Run("no rsync is refused", func(t *testing.T) {
		root := t.TempDir()
		src := filepath.Join(root, "src")
		if err := os.MkdirAll(src, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, "manifest.yaml"), []byte("name: t\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(root, "stage")
		if out, err := bundleLib(t, toolsOnlyPath(t, base...), "stage_default_bundle", src, dst, owner); err == nil || !strings.Contains(out, "rsync is required") {
			t.Fatalf("staging without rsync: err=%v (want a refusal naming rsync)\n%s", err, out)
		}
		if _, err := os.Stat(dst); !os.IsNotExist(err) {
			t.Fatal("a refused staging still created its destination")
		}
	})
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync not installed; the staging path needs it")
	}
	for name, path := range map[string]string{"rsync in place": toolsOnlyPath(t, append(base, "rsync")...)} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			src := filepath.Join(root, "src")
			dst := filepath.Join(root, "stage")
			if err := os.MkdirAll(filepath.Join(src, "personas"), 0o755); err != nil {
				t.Fatal(err)
			}
			for f, body := range map[string]string{"manifest.yaml": "name: t\n", "personas/a.yaml": "a\n", "stale.txt": "old\n"} {
				if err := os.WriteFile(filepath.Join(src, f), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if out, err := bundleLib(t, path, "stage_default_bundle", src, dst, owner); err != nil {
				t.Fatalf("stage: %v\n%s", err, out)
			}
			if got, _ := os.ReadFile(filepath.Join(dst, "personas", "a.yaml")); string(got) != "a\n" {
				t.Fatalf("copy incomplete: personas/a.yaml = %q", got)
			}
			if got, _ := os.ReadFile(filepath.Join(dst, ".fleet-staged-from")); strings.TrimSpace(string(got)) != src {
				t.Fatalf("marker = %q, want %q", got, src)
			}
			// Upstream drops a file; a re-stage must not keep it.
			if err := os.Remove(filepath.Join(src, "stale.txt")); err != nil {
				t.Fatal(err)
			}
			if out, err := bundleLib(t, path, "stage_default_bundle", src, dst, owner); err != nil {
				t.Fatalf("re-stage: %v\n%s", err, out)
			}
			if _, err := os.Stat(filepath.Join(dst, "stale.txt")); !os.IsNotExist(err) {
				t.Fatal("a file removed upstream lingered in the staged copy")
			}
			assertRelativeSourceMarkedAbsolute(t, path, root, dst, owner)
			assertMarkerSymlinkNotFollowed(t, path, src, dst, owner, root)
			assertPartialSourceRefused(t, path, src, dst, owner)
			assertFailedSyncRestores(t, base, src, dst, owner)
			// A hand-placed bundle at the destination (no marker) is never deleted.
			hand := filepath.Join(root, "hand")
			if err := os.MkdirAll(hand, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(hand, "manifest.yaml"), []byte("name: theirs\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := bundleLib(t, path, "stage_default_bundle", src, hand, owner)
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 2 {
				t.Fatalf("staging over a hand-placed bundle: err=%v (want exit 2)\n%s", err, out)
			}
			if got, _ := os.ReadFile(filepath.Join(hand, "manifest.yaml")); string(got) != "name: theirs\n" {
				t.Fatal("a hand-placed bundle was overwritten")
			}
			// Refused inputs, nothing written.
			for _, c := range [][3]string{{root, filepath.Join(root, "never"), owner}, {src, "relative/stage", owner}} {
				if out, err := bundleLib(t, path, "stage_default_bundle", c[0], c[1], c[2]); err == nil {
					t.Fatalf("staging %v succeeded:\n%s", c, out)
				}
			}
			if _, err := os.Stat(filepath.Join(root, "never")); !os.IsNotExist(err) {
				t.Fatal("a refused staging still created its destination")
			}
			link := filepath.Join(root, "link")
			if err := os.Symlink(dst, link); err != nil {
				t.Fatal(err)
			}
			if out, err := bundleLib(t, path, "stage_default_bundle", src, link, owner); err == nil {
				t.Fatalf("staging onto a symlink succeeded:\n%s", out)
			}
		})
	}
}

// assertMarkerSymlinkNotFollowed: the service user owns the staged tree, so
// it can swap the marker for a symlink to a file it wants overwritten; a
// re-stage replaces the link and leaves the target alone.
func assertMarkerSymlinkNotFollowed(t *testing.T, path, src, dst, owner, root string) {
	t.Helper()
	victim := filepath.Join(root, "victim")
	if err := os.WriteFile(victim, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(dst, ".fleet-staged-from")
	if err := os.Remove(markerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, markerPath); err != nil {
		t.Fatal(err)
	}
	if out, err := bundleLib(t, path, "stage_default_bundle", src, dst, owner); err != nil {
		t.Fatalf("re-stage over a symlinked marker: %v\n%s", err, out)
	}
	if got, _ := os.ReadFile(victim); string(got) != "keep\n" {
		t.Fatalf("staging wrote through a symlinked marker: victim = %q", got)
	}
	if fi, err := os.Lstat(markerPath); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("the marker is still a symlink after a re-stage (err=%v)", err)
	}
}

// assertFailedSyncRestores: an rsync that fails part-way has already
// replaced and deleted files, so staging must put the previous copy back.
// A stub rsync does the real sync and then reports failure, once — the shape
// of a disk filling up mid-transfer — and the copy must read as it did before.
func assertFailedSyncRestores(t *testing.T, base []string, src, dst, owner string) {
	t.Helper()
	realRsync, err := exec.LookPath("rsync")
	if err != nil {
		return
	}
	path := toolsOnlyPath(t, base...)
	stubDir := t.TempDir()
	once := filepath.Join(stubDir, "failed-once")
	stub := "#!/usr/bin/env bash\n\"" + realRsync + "\" \"$@\"\nif [[ ! -e " + once + " ]]; then : > " + once + "; exit 23; fi\n"
	if err := os.WriteFile(filepath.Join(stubDir, "rsync"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dst, "personas", "a.yaml"))
	if err := os.WriteFile(filepath.Join(src, "personas", "a.yaml"), []byte("changed upstream\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.WriteFile(filepath.Join(src, "personas", "a.yaml"), before, 0o644) }()
	out, err := bundleLib(t, stubDir+string(os.PathListSeparator)+path, "stage_default_bundle", src, dst, owner)
	if err == nil || !strings.Contains(out, "restoring the previous copy") {
		t.Fatalf("a failed sync was not reported and restored: err=%v\n%s", err, out)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "personas", "a.yaml")); string(got) != string(before) {
		t.Fatalf("after a failed sync personas/a.yaml = %q, want the previous %q", got, before)
	}
	if left, _ := filepath.Glob(dst + ".prev.*"); len(left) != 0 {
		t.Fatalf("the kept copy was left behind: %v\n%s", left, out)
	}
	assertFailedRollbackKeepsBackup(t, realRsync, path, src, dst, owner)
}

// assertRelativeSourceMarkedAbsolute: update.sh --src . hands a relative
// source in; the marker's readers accept only an absolute path, so staging
// must record the resolved one or no later run would recognise the copy.
func assertRelativeSourceMarkedAbsolute(t *testing.T, path, root, dst, owner string) {
	t.Helper()
	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "bundle.sh")
	cmd := exec.Command("bash", "-c", ". "+lib+` && stage_default_bundle "$@"`, "bundle", "./src", dst, owner)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "PATH="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("stage from a relative source: %v\n%s", err, out)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(root, "src"))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, ".fleet-staged-from")); strings.TrimSpace(string(got)) != want {
		t.Fatalf("marker after a relative source = %q, want the absolute %q", got, want)
	}
	if out, err := bundleLib(t, path, "bundle_marker_source", dst, owner); err != nil || strings.TrimSpace(out) != want {
		t.Fatalf("the marker written from a relative source is not readable back: out=%q err=%v", out, err)
	}
}

// assertFailedRollbackKeepsBackup: when the rollback sync fails as well (the
// same full disk), the kept copy is the only good one left — it must survive,
// and the exit status (3) must tell the caller to stop.
func assertFailedRollbackKeepsBackup(t *testing.T, realRsync, path, src, dst, owner string) {
	t.Helper()
	stubDir := t.TempDir()
	stub := "#!/usr/bin/env bash\n\"" + realRsync + "\" \"$@\"\nexit 23\n"
	if err := os.WriteFile(filepath.Join(stubDir, "rsync"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dst, "personas", "a.yaml"))
	if err := os.WriteFile(filepath.Join(src, "personas", "a.yaml"), []byte("changed upstream\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.WriteFile(filepath.Join(src, "personas", "a.yaml"), before, 0o644) }()
	out, err := bundleLib(t, stubDir+string(os.PathListSeparator)+path, "stage_default_bundle", src, dst, owner)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 3 || !strings.Contains(out, "previous copy is kept at") {
		t.Fatalf("a failed rollback did not exit 3 naming the kept copy: err=%v\n%s", err, out)
	}
	kept, _ := filepath.Glob(dst + ".prev.*")
	if len(kept) != 1 {
		t.Fatalf("want exactly one kept copy after a failed rollback, got %v", kept)
	}
	if got, _ := os.ReadFile(filepath.Join(kept[0], "personas", "a.yaml")); string(got) != string(before) {
		t.Fatalf("the kept copy holds %q, want the previous %q", got, before)
	}
	if left, _ := filepath.Glob(dst + ".new.*"); len(left) != 0 {
		t.Fatalf("the scratch unpack was left behind: %v", left)
	}
	_ = os.RemoveAll(kept[0])
}

// assertPartialSourceRefused: a source that cannot be read in full is refused
// before the owner sees anything, and the existing copy keeps its files — a
// partial archive must never reach the --delete sync. Root reads past a 000
// mode, so this needs an unprivileged test run, as CI's.
func assertPartialSourceRefused(t *testing.T, path, src, dst, owner string) {
	t.Helper()
	if os.Geteuid() == 0 {
		return
	}
	locked := filepath.Join(src, "locked.yaml")
	if err := os.WriteFile(locked, []byte("x\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(locked)
	if out, err := bundleLib(t, path, "stage_default_bundle", src, dst, owner); err == nil {
		t.Fatalf("staging from a partly unreadable source succeeded:\n%s", out)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "personas", "a.yaml")); string(got) != "a\n" {
		t.Fatalf("a failed stage disturbed the existing copy: personas/a.yaml = %q", got)
	}
}

// TestBundlePathPredicates pins the two "where is this bundle" questions the
// scripts ask: only REPO_ROOT/config/default is the generic bundle; anything
// under the checkout is in it; and doctor's checkout-independent test keys on
// the fleet module two levels up.
func TestBundlePathPredicates(t *testing.T) {
	root := repoRootFromTest(t)
	path := os.Getenv("PATH")
	def := filepath.Join(root, "config", "default")
	for _, c := range []struct {
		fn, dir, repo string
		want          bool
	}{
		{"bundle_is_default_in_checkout", def, root, true},
		{"bundle_is_default_in_checkout", def + "/", root, true},
		{"bundle_is_default_in_checkout", filepath.Join(root, "config"), root, false},
		{"bundle_is_default_in_checkout", "/var/lib/fleet/bundle", root, false},
		{"bundle_is_default_in_checkout", def, "", false},
		{"bundle_is_in_checkout", filepath.Join(root, "client"), root, true},
		{"bundle_is_in_checkout", "/opt/fleet/client", root, false},
		{"bundle_is_in_checkout", "/opt/fleet/client", "", false},
		{"bundle_looks_like_fleet_default", def, "", true},
		{"bundle_looks_like_fleet_default", "/var/lib/fleet/bundle", "", false},
	} {
		_, err := bundleLib(t, path, c.fn, c.dir, c.repo)
		if got := err == nil; got != c.want {
			t.Errorf("%s(%q, %q) = %v, want %v", c.fn, c.dir, c.repo, got, c.want)
		}
	}
	// A unit systemd does not know reports no ReadWritePaths, so the shipped
	// unit's set applies: the state dir and /opt/fleet/client are writable,
	// a service-owned path elsewhere is still read-only to the service.
	for dir, want := range map[string]bool{
		"/var/lib/fleet/bundle":    true,
		"/opt/fleet/client":        true,
		"/opt/fleet/client/sub":    true,
		"/srv/fleet-bundle":        false,
		"/opt/fleet/src/config":    false,
		"/opt/fleet/client-backup": false,
	} {
		_, err := bundleLib(t, path, "bundle_writable_in_unit", dir, "fleet-test-no-such-unit", "/var/lib/fleet")
		if got := err == nil; got != want {
			t.Errorf("bundle_writable_in_unit(%q) = %v, want %v", dir, got, want)
		}
	}
}

// TestBundleMarkerSourceNeverFollowsALink: the marker sits in the service
// user's tree, so a root read through it could print a root-only file. A
// regular marker yields its first line; a symlinked one yields nothing.
func TestBundleMarkerSourceNeverFollowsALink(t *testing.T) {
	me, err := exec.Command("id", "-un").Output()
	if err != nil {
		t.Skip("id -un unavailable")
	}
	owner := strings.TrimSpace(string(me))
	path := os.Getenv("PATH")
	dir := t.TempDir()
	marker := filepath.Join(dir, ".fleet-staged-from")
	if err := os.WriteFile(marker, []byte("/opt/fleet/src/config/default\nsecond line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := bundleLib(t, path, "bundle_marker_source", dir, owner); err != nil || strings.TrimSpace(out) != "/opt/fleet/src/config/default" {
		t.Fatalf("regular marker: out=%q err=%v, want its first line", out, err)
	}
	secret := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(secret, []byte("ROOT_ONLY=do-not-print\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, marker); err != nil {
		t.Fatal(err)
	}
	out, err := bundleLib(t, path, "bundle_marker_source", dir, owner)
	if err == nil || strings.Contains(out, "do-not-print") {
		t.Fatalf("a symlinked marker was read through: out=%q err=%v", out, err)
	}
	// A first line carrying a terminal escape (or not an absolute path) is
	// refused: root-run update and doctor print what this returns.
	for _, bad := range []string{"/opt/fleet/src\x1b[2K\rall good\n", "config/default\n"} {
		if err := os.Remove(marker); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(marker, []byte(bad), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := bundleLib(t, path, "bundle_marker_source", dir, owner); err == nil || strings.ContainsRune(out, 0x1b) {
			t.Fatalf("marker %q was accepted: out=%q err=%v", bad, out, err)
		}
	}
	// A FIFO in the marker's place must be refused, not block the read
	// forever (and update.sh or doctor.sh with it).
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(marker, 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "bundle.sh")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", ". "+lib+` && bundle_marker_source "$@"`, "bundle", dir, owner)
	cmd.Env = append(os.Environ(), "PATH="+path)
	err = cmd.Run()
	if ctx.Err() != nil {
		t.Fatal("bundle_marker_source blocked on a FIFO marker")
	}
	if err == nil {
		t.Fatal("a FIFO marker was taken for a staged copy")
	}
}

// TestUpdateDryRunRestagesAMissingStagedCopy: the env file still points the
// service at the staging path but the copy is gone. update must plan to
// recreate it rather than restart the service onto a bundle that does not
// exist.
func TestUpdateDryRunRestagesAMissingStagedCopy(t *testing.T) {
	const stage = "/var/lib/fleet/bundle"
	if _, err := os.Stat(stage); err == nil {
		t.Skip(stage + " exists on this box; the case needs it absent")
	}
	envFile := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(envFile, []byte("FLEET_CLIENT_CONFIG_DIR="+stage+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runScript(t, []string{"FLEET_ENV_FILE=" + envFile, "FLEET_CLIENT_CONFIG_EXPLICIT=0"}, "update.sh", "--dry-run", "--no-pull")
	if err != nil {
		t.Fatalf("update --dry-run exited non-zero: %v\n--- output ---\n%s", err, out)
	}
	if !strings.Contains(out, "would stage") || !strings.Contains(out, stage) {
		t.Fatalf("update did not plan to recreate the missing staged copy\n--- output ---\n%s", out)
	}
}

// TestUpdateDryRunPlansBundleStagingForAPreStagingBox: a box installed before
// #1655 has an env file pointing the service at the checkout's generic bundle;
// `fleet update` must plan to stage it, and never for an explicit
// --client-config.
func TestUpdateDryRunPlansBundleStagingForAPreStagingBox(t *testing.T) {
	root := repoRootFromTest(t)
	envFile := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(envFile, []byte("FLEET_CLIENT_CONFIG_DIR="+filepath.Join(root, "config", "default")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// runScript sets FLEET_CLIENT_CONFIG_DIR in the environment, which update.sh
	// reads as an explicit operator choice; FLEET_CLIENT_CONFIG_EXPLICIT=0 is how
	// the script's own re-exec says "that came from a fallback", and it is what
	// an operator's plain `fleet update` amounts to.
	out, err := runScript(t, []string{"FLEET_ENV_FILE=" + envFile, "FLEET_CLIENT_CONFIG_EXPLICIT=0"}, "update.sh", "--dry-run", "--no-pull")
	if err != nil {
		t.Fatalf("update --dry-run exited non-zero: %v\n--- output ---\n%s", err, out)
	}
	if !strings.Contains(out, "would stage") || !strings.Contains(out, "/bundle (owned by") {
		t.Fatalf("update plan does not stage the generic bundle for a pre-staging box\n--- output ---\n%s", out)
	}
	out, err = runScript(t, []string{"FLEET_ENV_FILE=" + envFile}, "update.sh", "--dry-run", "--no-pull", "--client-config", filepath.Join(root, "config", "default"))
	if err != nil {
		t.Fatalf("update --dry-run --client-config exited non-zero: %v\n--- output ---\n%s", err, out)
	}
	if strings.Contains(out, "would stage") {
		t.Fatalf("update planned to stage over an explicit --client-config\n--- output ---\n%s", out)
	}
}

// TestUpdateDryRunDoesNotClaimAStaleStagedCopyWasRefreshed: a staged copy
// whose marker names another checkout (fleet re-cloned at a new path) is not
// restaged, so update must report it as not advanced rather than print
// "refreshed … above" over a copy it never touched.
func TestUpdateDryRunDoesNotClaimAStaleStagedCopyWasRefreshed(t *testing.T) {
	stage := t.TempDir()
	if err := os.WriteFile(filepath.Join(stage, ".fleet-staged-from"), []byte("/old/fleet/config/default\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(t.TempDir(), "fleet.env")
	if err := os.WriteFile(envFile, []byte("FLEET_CLIENT_CONFIG_DIR="+stage+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runScript(t, []string{"FLEET_ENV_FILE=" + envFile, "FLEET_CLIENT_CONFIG_EXPLICIT=0"}, "update.sh", "--dry-run", "--no-pull")
	if err != nil {
		t.Fatalf("update --dry-run exited non-zero: %v\n--- output ---\n%s", err, out)
	}
	if strings.Contains(out, "refreshed from") {
		t.Fatalf("update claimed a refresh of a staged copy it did not restage\n--- output ---\n%s", out)
	}
	if !strings.Contains(out, "NOT refreshed by this update") {
		t.Fatalf("update did not report the staged copy as not refreshed\n--- output ---\n%s", out)
	}
}
