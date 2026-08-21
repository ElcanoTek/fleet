// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

// Package boxdoctor runs READ-ONLY box-level health checks from inside the
// fleet process, for the Settings → Admin → Doctor panel (/admin/doctor).
//
// It is the in-process, unprivileged sibling of scripts/doctor.sh: the script
// runs as root on the host and REPAIRS what it finds; this package only
// diagnoses — the fleet service user cannot (and must not) rewrite subuid
// ranges, reinstall systemd units, or upgrade packages. Every failing check
// therefore carries a Fix hint, almost always "run `sudo fleet doctor` on the
// box", so the admin UI can show not just what is wrong but the one command
// that repairs it.
//
// Scope: checks that are meaningful FROM the service process's own vantage
// point (its user, HOME, env, network). Anything that needs root visibility
// (package currency, env-file permissions, unit drift vs deploy/) stays in
// doctor.sh — reporting a degraded guess here would be worse than saying
// nothing. Checks degrade to StatusSkip when their probe isn't available
// (no systemctl, no podman) rather than failing the box for what we cannot
// see; the process's systemd hardening (ProtectSystem=strict) never blocks
// the reads used here (/etc/subuid, statfs, systemctl is-active over D-Bus).
package boxdoctor

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/ElcanoTek/fleet/internal/diskguard"
)

// Status is a check verdict. warn does not fail the box; fail does; skip
// means the probe wasn't available from this process.
type Status string

const (
	StatusOK   Status = "ok"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
	StatusSkip Status = "skip"
)

// Check is one named probe result. Fix, when set, is the operator remediation
// (a command to run on the box) the UI renders alongside a warn/fail.
type Check struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

// Summary is the per-status tally the UI badges.
type Summary struct {
	OK   int `json:"ok"`
	Warn int `json:"warn"`
	Fail int `json:"fail"`
	Skip int `json:"skip"`
}

// Report is the full doctor result. Healthy means zero StatusFail checks.
type Report struct {
	GeneratedAt time.Time `json:"generated_at"`
	DurationMS  int64     `json:"duration_ms"`
	Deep        bool      `json:"deep"`
	Healthy     bool      `json:"healthy"`
	Summary     Summary   `json:"summary"`
	Checks      []Check   `json:"checks"`
}

// Options wires the process-held dependencies in. Everything is optional;
// an unset field turns its check into a skip (never a false fail).
type Options struct {
	// ChatPing pings the chat DB through the server's own pool (the store),
	// so the check reflects the connection the app actually uses.
	ChatPing func(context.Context) error
	// SchedDSN is the orchestrator DB DSN; empty resolves from
	// FLEET_SCHED_DATABASE_URL / DATABASE_URL like the rest of the process.
	SchedDSN string
	// SandboxImage is the resolved sandbox image ref (config/bundle); empty
	// falls back to the FLEET_SANDBOX_IMAGE / CHAT_SANDBOX_IMAGE env vars.
	SandboxImage string
	// ServiceName is the systemd unit basename (default FLEET_SERVICE_NAME
	// or "fleet").
	ServiceName string
	// DataDir is the writable root whose disk headroom is checked (default
	// the process working directory — /var/lib/fleet under the shipped unit).
	DataDir string
	// Deep additionally launches a throwaway sandbox container
	// (`podman run --rm --network=none <image> true`) — the definitive smoke,
	// but seconds-slow, so the UI requests it explicitly.
	Deep bool
}

const sudoDoctorFix = "run on the box: sudo fleet doctor"

// Run executes every check and returns the report. All probes are read-only
// and per-check time-bounded; ctx cancellation aborts the remainder (the
// checks already gathered are still returned).
func Run(ctx context.Context, opts Options) *Report {
	start := time.Now()
	r := &Report{GeneratedAt: start.UTC(), Deep: opts.Deep}

	add := func(c Check) {
		if ctx.Err() != nil && c.Status == StatusSkip {
			c.Detail = "aborted: " + ctx.Err().Error()
		}
		r.Checks = append(r.Checks, c)
	}

	add(checkChatDB(ctx, opts.ChatPing))
	add(checkSchedDB(ctx, opts.SchedDSN))
	add(checkModelKey())
	add(checkDisk("disk: data dir", dataDir(opts.DataDir)))
	add(checkDisk("disk: podman image store", podmanStoreDir()))
	add(checkSubIDs("subuid", "/etc/subuid"))
	add(checkSubIDs("subgid", "/etc/subgid"))
	add(checkPodman(ctx))
	add(checkSandboxImage(ctx, opts.SandboxImage, opts.Deep))
	for _, c := range checkUnits(ctx, serviceName(opts.ServiceName)) {
		add(c)
	}
	for _, c := range checkRestartChurn(ctx, serviceName(opts.ServiceName)) {
		add(c)
	}
	add(checkWebStopPolicy(ctx))
	add(checkBackups(ctx))

	for _, c := range r.Checks {
		switch c.Status {
		case StatusOK:
			r.Summary.OK++
		case StatusWarn:
			r.Summary.Warn++
		case StatusFail:
			r.Summary.Fail++
		case StatusSkip:
			r.Summary.Skip++
		}
	}
	r.Healthy = r.Summary.Fail == 0
	r.DurationMS = time.Since(start).Milliseconds()
	return r
}

// ── individual checks ───────────────────────────────────────────────────────

func checkChatDB(ctx context.Context, ping func(context.Context) error) Check {
	c := Check{Name: "chat database"}
	if ping == nil {
		c.Status, c.Detail = StatusSkip, "no store wired"
		return c
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := ping(pctx); err != nil {
		c.Status, c.Detail, c.Fix = StatusFail, "ping failed: "+err.Error(), sudoDoctorFix+" (checks postgresql + DSNs)"
		return c
	}
	c.Status, c.Detail = StatusOK, "reachable via the server pool"
	return c
}

func checkSchedDB(ctx context.Context, dsn string) Check {
	c := Check{Name: "sched database"}
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("FLEET_SCHED_DATABASE_URL"))
	}
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("DATABASE_URL"))
	}
	if dsn == "" {
		c.Status, c.Detail, c.Fix = StatusFail, "no DSN resolved (FLEET_SCHED_DATABASE_URL unset)", sudoDoctorFix
		return c
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		c.Status, c.Detail, c.Fix = StatusFail, "open: "+err.Error(), sudoDoctorFix
		return c
	}
	defer func() { _ = db.Close() }()
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pctx); err != nil {
		c.Status, c.Detail, c.Fix = StatusFail, "ping failed: "+err.Error(), sudoDoctorFix+" (checks postgresql + DSNs)"
		return c
	}
	c.Status, c.Detail = StatusOK, "reachable"
	return c
}

// checkModelKey mirrors config.Validate's rule: the OpenRouter key is required
// unless mock mode is on. Presence only — the value is never echoed.
func checkModelKey() Check {
	c := Check{Name: "model API key"}
	mock := truthy(os.Getenv("FLEET_MOCK_MODE")) || truthy(os.Getenv("CHAT_MOCK_MODE"))
	switch {
	case strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY")) != "":
		c.Status, c.Detail = StatusOK, "OPENROUTER_API_KEY set"
	case mock:
		c.Status, c.Detail = StatusWarn, "OPENROUTER_API_KEY unset but mock mode is on"
	default:
		c.Status, c.Detail, c.Fix = StatusFail, "OPENROUTER_API_KEY unset", "run on the box: sudo fleet config set-openrouter-key"
	}
	return c
}

// checkDisk fails at ≥95% used and warns at ≥85% — full disks are the top
// cause of confusing sandbox/DB failures (image builds and WAL both die).
func checkDisk(name, path string) Check {
	c := Check{Name: name}
	if path == "" {
		c.Status, c.Detail = StatusSkip, "path unresolved"
		return c
	}
	// One statfs implementation for the whole process (internal/diskguard):
	// this check, the Prometheus gauges, the admin storage panel and the
	// backpressure decision all measure the same way, so a box can never be
	// "95% used" here and "fine" there.
	total, avail, err := diskguard.Usage(path)
	if err != nil {
		c.Status, c.Detail = StatusSkip, fmt.Sprintf("statfs %s: %v", path, err)
		return c
	}
	if total == 0 {
		c.Status, c.Detail = StatusSkip, "statfs reported zero size"
		return c
	}
	usedPct := 100 - float64(avail)*100/float64(total)
	detail := fmt.Sprintf("%s: %.1f%% used, %s free", path, usedPct, humanBytes(avail))
	switch {
	case usedPct >= 95:
		c.Status, c.Detail, c.Fix = StatusFail, detail, "run on the box: sudo fleet cleanup (reclaims dangling podman layers + build caches); check `systemctl status fleet-maintenance.timer` — the hourly in-process sweep and this daily timer should be keeping ahead of this"
	case usedPct >= 85:
		c.Status, c.Detail, c.Fix = StatusWarn, detail, "consider: sudo fleet cleanup (and confirm fleet-maintenance.timer is enabled)"
	default:
		c.Status, c.Detail = StatusOK, detail
	}
	return c
}

// checkSubIDs verifies the process user has a rootless-podman ID range —
// the single most common cause of "sandbox worked yesterday" breakage after
// user or image-store surgery.
func checkSubIDs(name, file string) Check {
	c := Check{Name: name + " range"}
	u, err := user.Current()
	if err != nil {
		c.Status, c.Detail = StatusSkip, "current user unresolved: "+err.Error()
		return c
	}
	body, err := os.ReadFile(file) //nolint:gosec // fixed /etc path
	if err != nil {
		c.Status, c.Detail = StatusSkip, fmt.Sprintf("cannot read %s: %v", file, err)
		return c
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, u.Username+":") || strings.HasPrefix(line, u.Uid+":") {
			c.Status, c.Detail = StatusOK, fmt.Sprintf("%s has a %s range", file, u.Username)
			return c
		}
	}
	c.Status = StatusFail
	c.Detail = fmt.Sprintf("%s has no range for %s — rootless podman cannot map the userns", file, u.Username)
	c.Fix = sudoDoctorFix
	return c
}

func checkPodman(ctx context.Context) Check {
	c := Check{Name: "rootless podman"}
	if _, err := exec.LookPath("podman"); err != nil {
		c.Status, c.Detail, c.Fix = StatusFail, "podman not on PATH", sudoDoctorFix
		return c
	}
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(pctx, "podman", "info", "--format", "{{.Version.Version}}").Output(); err == nil {
		c.Status, c.Detail = StatusOK, "podman "+strings.TrimSpace(string(out))+" functional for this user"
		return c
	}
	c.Status, c.Detail, c.Fix = StatusFail, "`podman info` fails for the service user", sudoDoctorFix+" (repairs the store dirs, containers.conf, and stale pause namespaces)"
	return c
}

func checkSandboxImage(ctx context.Context, ref string, deep bool) Check {
	c := Check{Name: "sandbox image"}
	ref = resolveImageRef(ref)
	if ref == "" {
		c.Status, c.Detail, c.Fix = StatusFail, "no image ref resolved (FLEET_SANDBOX_IMAGE / bundle sandbox.tag)", sudoDoctorFix
		return c
	}
	if _, err := exec.LookPath("podman"); err != nil {
		c.Status, c.Detail = StatusSkip, "podman not on PATH — cannot verify "+ref
		return c
	}
	ectx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	//nolint:gosec // G204: fixed "podman" binary; ref is the operator's resolved bundle image, not request input.
	if err := exec.CommandContext(ectx, "podman", "image", "exists", ref).Run(); err != nil {
		c.Status, c.Detail, c.Fix = StatusFail, ref+" missing from this user's rootless store", "run on the box: sudo fleet update (rebuilds the bundle's sandbox image)"
		return c
	}
	if !deep {
		c.Status, c.Detail = StatusOK, ref+" present (deep run skipped)"
		return c
	}
	rctx, cancel2 := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel2()
	//nolint:gosec // G204: fixed "podman" binary; ref is the operator's resolved bundle image, not request input.
	cmd := exec.CommandContext(rctx, "podman", "run", "--rm", "--network=none", ref, "true")
	if out, err := cmd.CombinedOutput(); err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}
		c.Status, c.Detail, c.Fix = StatusFail, ref+" not runnable: "+firstLine(detail), sudoDoctorFix
		return c
	}
	c.Status, c.Detail = StatusOK, ref+" present + runnable (deep smoke passed)"
	return c
}

// checkUnits reports the sibling systemd units' state. The fleet unit itself is
// included for completeness (under systemd it is trivially active while we're
// answering, but a non-unit deployment shows "not installed" honestly);
// fleet-web/postgresql/caddy are the tiers whose silent death users report as
// "fleet is broken". Absent optional units are warns/skips, never fails.
func checkUnits(ctx context.Context, service string) []Check {
	units := []struct {
		unit     string
		optional bool
	}{
		{service + ".service", false},
		{"fleet-web.service", true},
		{"postgresql.service", true},
		{"caddy.service", true},
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return []Check{{Name: "systemd units", Status: StatusSkip, Detail: "systemctl not on PATH (no systemd)"}}
	}
	out := make([]Check, 0, len(units))
	for _, u := range units {
		c := Check{Name: "unit " + strings.TrimSuffix(u.unit, ".service")}
		uctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		//nolint:gosec // G204: fixed "systemctl" binary; unit names are compile-time constants + the configured service name.
		if err := exec.CommandContext(uctx, "systemctl", "cat", u.unit).Run(); err != nil {
			if u.optional {
				c.Status, c.Detail = StatusSkip, u.unit+" not installed (optional tier)"
			} else {
				c.Status, c.Detail, c.Fix = StatusWarn, u.unit+" not installed (another supervisor?)", "install it: scripts/bootstrap.sh --enable-service"
			}
			cancel()
			out = append(out, c)
			continue
		}
		//nolint:gosec // G204: fixed "systemctl" binary; unit names are compile-time constants + the configured service name.
		state, _ := exec.CommandContext(uctx, "systemctl", "is-active", u.unit).Output()
		cancel()
		switch s := strings.TrimSpace(string(state)); s {
		case "active":
			c.Status, c.Detail = StatusOK, u.unit+" active"
		case "activating", "reloading":
			c.Status, c.Detail = StatusWarn, u.unit+" is "+s
		default:
			c.Status, c.Detail, c.Fix = StatusFail, fmt.Sprintf("%s is %q", u.unit, s), fmt.Sprintf("run on the box: sudo systemctl start %s (then journalctl -u %s -n 50 if it fails)", u.unit, strings.TrimSuffix(u.unit, ".service"))
		}
		out = append(out, c)
	}
	return out
}

// The scheduled-backup pair shipped in deploy/ and installed by
// scripts/bootstrap.sh --enable-service. Fixed names (unlike the fleet unit,
// which FLEET_SERVICE_NAME can rename) because bootstrap installs exactly these.
const (
	backupTimerUnit   = "fleet-backup.timer"
	backupServiceUnit = "fleet-backup.service"
)

// checkBackups reports whether this box takes scheduled database dumps. It
// probes the same units scripts/doctor.sh does, and reaches the same verdicts.
func checkBackups(ctx context.Context) Check {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return Check{Name: "scheduled backups", Status: StatusSkip, Detail: "systemctl not on PATH (no systemd)"}
	}
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// Both halves must be present: a timer whose service unit is missing fires
	// into nothing, and reporting that as "backups are configured" would be the
	// same false comfort this check exists to remove.
	installed := exec.CommandContext(sctx, "systemctl", "cat", backupTimerUnit).Run() == nil &&
		exec.CommandContext(sctx, "systemctl", "cat", backupServiceUnit).Run() == nil
	enabled := false
	if installed {
		enabled = exec.CommandContext(sctx, "systemctl", "is-enabled", "--quiet", backupTimerUnit).Run() == nil
	}
	// is-enabled only reads the install symlink, so an enabled timer that is not
	// RUNNING still has to be probed separately — see backupVerdict.
	active := false
	if enabled {
		active = exec.CommandContext(sctx, "systemctl", "is-active", "--quiet", backupTimerUnit).Run() == nil
	}
	lastResult := ""
	if active {
		out, _ := exec.CommandContext(sctx, "systemctl", "show", "-p", "Result", "--value", backupServiceUnit).Output()
		lastResult = strings.TrimSpace(string(out))
	}
	return backupVerdict(installed, enabled, active, lastResult)
}

// backupVerdict maps the probed timer state onto a verdict; split out so the
// matrix is testable on a host without systemd.
//
// A MISSING timer is an advisory, never a failure: a same-host pg_dump protects
// against logical loss, but an operator who snapshots the volume or the
// hypervisor has a stronger answer and is not misconfigured. An ENABLED BUT
// INACTIVE timer never fires either, and its service's Result stays "success",
// so it would otherwise read as a clean box. A timer whose LAST RUN FAILED is a
// genuine fault — the oneshot exits non-zero when a dump fails its integrity
// check, and a timer that has been failing for a week is worse than no timer,
// because the box looks covered.
func backupVerdict(installed, enabled, active bool, lastResult string) Check {
	c := Check{Name: "scheduled backups"}
	switch {
	case !installed:
		c.Status = StatusWarn
		c.Detail = "no " + backupTimerUnit + " + " + backupServiceUnit + " pair installed — nothing on this box dumps the databases"
		// Installing just this pair, not a re-bootstrap: on a provisioned box
		// bootstrap also rebuilds binaries and re-provisions Postgres. `fleet
		// timers install` is that one-pair install as a single idempotent verb.
		c.Fix = "run on the box: sudo fleet timers install --backup (skip if you back up at the volume/hypervisor layer)"
	case !enabled:
		c.Status = StatusWarn
		c.Detail = backupTimerUnit + " installed but not enabled — it will never fire"
		c.Fix = "run on the box: sudo systemctl enable --now " + backupTimerUnit
	case !active:
		c.Status = StatusWarn
		c.Detail = backupTimerUnit + " is enabled but not active — it will not fire until it is started"
		c.Fix = "run on the box: sudo systemctl start " + backupTimerUnit
	case lastResult != "" && lastResult != "success":
		c.Status = StatusFail
		c.Detail = fmt.Sprintf("%s last run failed (Result=%s) — no dump is being written", backupServiceUnit, lastResult)
		c.Fix = "run on the box: journalctl -u fleet-backup -n 50 (then retry: sudo systemctl start " + backupServiceUnit + ")"
	default:
		c.Status = StatusOK
		c.Detail = backupTimerUnit + " enabled and active, no failed run recorded"
	}
	return c
}

// The app units whose churn is worth reporting. Timers, Postgres and Caddy are
// deliberately excluded: a oneshot's outcome is already covered by
// checkBackups, and the other two are not ours to diagnose beyond is-active.
func churnUnits(service string) []string {
	return []string{service + ".service", "fleet-web.service"}
}

// checkRestartChurn reports units systemd has been RESTARTING BY ITSELF.
//
// This closes a real blind spot in checkUnits: both app units run
// Restart=always, so a unit that dies is active again ~5s later and is-active
// reports "active" the whole time. A unit can therefore crash-loop
// indefinitely while every existing check here stays green — the operator's
// only hint is load, or disk filling with core dumps.
//
// NRestarts is the right property for this and not merely a convenient one: it
// counts only restarts driven by the Restart= policy, and a MANUAL
// `systemctl restart` resets it to zero. So an ordinary deploy leaves no trace
// and cannot produce a false alarm, while a unit failing on its own
// accumulates a count that only a human intervention clears.
//
// Being precise about what this does NOT catch, since it is the neighbouring
// bug: a process that segfaults during a manual stop (the fleet-web teardown
// crash — see docs/WEB-TIER-SHUTDOWN.md) restarts by operator action, so
// NRestarts stays 0. That fault is a configuration matter, which
// checkWebStopPolicy asserts instead.
func checkRestartChurn(ctx context.Context, service string) []Check {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return []Check{{Name: "restart churn", Status: StatusSkip, Detail: "systemctl not on PATH (no systemd)"}}
	}
	out := make([]Check, 0, 2)
	for _, unit := range churnUnits(service) {
		short := strings.TrimSuffix(unit, ".service")
		uctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		//nolint:gosec // G204: fixed "systemctl" binary; unit names are the configured service name + a compile-time constant.
		if err := exec.CommandContext(uctx, "systemctl", "cat", unit).Run(); err != nil {
			cancel()
			out = append(out, Check{
				Name:   "restarts: " + short,
				Status: StatusSkip,
				Detail: unit + " not installed",
			})
			continue
		}
		//nolint:gosec // G204: fixed "systemctl" binary; unit names as above.
		nRaw, _ := exec.CommandContext(uctx, "systemctl", "show", "-p", "NRestarts", "--value", unit).Output()
		//nolint:gosec // G204: fixed "systemctl" binary; unit names as above.
		rRaw, _ := exec.CommandContext(uctx, "systemctl", "show", "-p", "Result", "--value", unit).Output()
		cancel()
		out = append(out, restartChurnVerdict(short, strings.TrimSpace(string(nRaw)), strings.TrimSpace(string(rRaw))))
	}
	return out
}

// crashLoopThreshold is where "it restarted a couple of times" becomes "this is
// looping". Deliberately low: NRestarts only counts self-inflicted restarts and
// is cleared by any manual restart, so even a handful is abnormal.
const crashLoopThreshold = 5

// restartChurnVerdict maps one unit's NRestarts + Result onto a verdict; split
// out so the matrix is testable on a host without systemd.
//
// A non-numeric or empty NRestarts means the property was unavailable (an old
// systemd, a unit systemd does not know) — reported as a skip rather than a
// guess. Result enriches the detail but does not drive the verdict on its own:
// it describes the LAST run, which on a healthy unit that was restarted after
// an incident still names the old failure, and nagging about a resolved event
// forever is how a check gets ignored.
func restartChurnVerdict(short, nRestarts, result string) Check {
	c := Check{Name: "restarts: " + short}
	n, err := strconv.Atoi(nRestarts)
	if nRestarts == "" || err != nil {
		c.Status, c.Detail = StatusSkip, "systemd did not report NRestarts for this unit"
		return c
	}
	lastRun := ""
	if result != "" && result != "success" {
		lastRun = fmt.Sprintf("; last run ended with Result=%s", result)
	}
	switch {
	case n == 0:
		c.Status = StatusOK
		c.Detail = "no automatic restarts since the last manual start" + lastRun
	case n >= crashLoopThreshold:
		c.Status = StatusFail
		c.Detail = fmt.Sprintf("%s has restarted itself %d times — it is crash-looping, not serving steadily%s", short, n, lastRun)
		c.Fix = fmt.Sprintf("run on the box: journalctl -u %s -n 100 (the restarts are self-inflicted; is-active stays \"active\" throughout)", short)
	default:
		c.Status = StatusWarn
		c.Detail = fmt.Sprintf("%s has restarted itself %d time(s) since the last manual start%s", short, n, lastRun)
		c.Fix = fmt.Sprintf("run on the box: journalctl -u %s -n 100", short)
	}
	return c
}

// checkWebStopPolicy asserts the value systemd RESOLVED for fleet-web's
// TimeoutStopFailureMode, mirroring the same assertion in scripts/doctor.sh.
//
// Why a config check earns a place among health checks: this exact directive
// shipped in the unit body and did nothing for a full release, because a
// distro-global /usr/lib/systemd/system/service.d/ drop-in overrides a unit
// body (Fedora sets abort there). Nothing observed that — the unit file said
// the right thing, so every file-comparing check passed. Only the resolved
// value can tell the difference, and it is exactly the kind of silent drift a
// doctor exists to surface.
//
// Read-only and unprivileged: `systemctl show` answers property queries over
// D-Bus without privilege, the same way checkBackups reads Result.
func checkWebStopPolicy(ctx context.Context) Check {
	c := Check{Name: "fleet-web stop policy"}
	if _, err := exec.LookPath("systemctl"); err != nil {
		c.Status, c.Detail = StatusSkip, "systemctl not on PATH (no systemd)"
		return c
	}
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(sctx, "systemctl", "cat", "fleet-web.service").Run(); err != nil {
		c.Status, c.Detail = StatusSkip, "fleet-web.service not installed (optional tier)"
		return c
	}
	out, _ := exec.CommandContext(sctx, "systemctl", "show", "-p", "TimeoutStopFailureMode", "--value", "fleet-web.service").Output()
	return webStopPolicyVerdict(strings.TrimSpace(string(out)))
}

// webStopPolicyVerdict maps the resolved TimeoutStopFailureMode onto a verdict;
// split out so it is testable without systemd.
func webStopPolicyVerdict(resolved string) Check {
	c := Check{Name: "fleet-web stop policy"}
	switch resolved {
	case "kill":
		c.Status = StatusOK
		c.Detail = "TimeoutStopFailureMode resolves to kill — an overrun stop is SIGKILLed"
	case "":
		// Pre-246 systemd has no such property. Nothing to assert, and no fix.
		c.Status = StatusSkip
		c.Detail = "this systemd does not expose TimeoutStopFailureMode"
	default:
		// Name the resolved value rather than its consequence: abort dumps a
		// full memory image, terminate just re-sends SIGTERM and can leave a
		// wedged process. Neither is what the unit ships.
		// Warn, not fail — and deliberately a weaker verdict than the same
		// assertion in scripts/doctor.sh, which fails. The difference is what
		// each one can claim: doctor.sh INSTALLS the drop-in and then checks,
		// so a wrong value there means a repair was attempted and did not
		// hold. This check cannot repair anything, and the consequence is
		// hygiene rather than downtime — an overrun stop dies the wrong way;
		// LimitCORE=0 already keeps the memory image off disk either way. Do
		// not "harmonise" these two into fail without that changing.
		c.Status = StatusWarn
		c.Detail = fmt.Sprintf("TimeoutStopFailureMode resolves to %q, not kill — the shipped drop-in is not winning", resolved)
		c.Fix = "run on the box: sudo fleet doctor (installs deploy/fleet-web.service.d/10-timeout-kill.conf); inspect with: systemctl cat fleet-web"
	}
	return c
}

// ── small helpers ───────────────────────────────────────────────────────────

func serviceName(v string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	if v = strings.TrimSpace(os.Getenv("FLEET_SERVICE_NAME")); v != "" {
		return v
	}
	return "fleet"
}

func resolveImageRef(ref string) string {
	if ref = strings.TrimSpace(ref); ref != "" {
		return ref
	}
	if v := strings.TrimSpace(os.Getenv("FLEET_SANDBOX_IMAGE")); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv("CHAT_SANDBOX_IMAGE"))
}

func dataDir(v string) string {
	if v = strings.TrimSpace(v); v != "" {
		return v
	}
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}

// podmanStoreDir is the rootless image store — the disk that actually fills
// up (each sandbox rebuild strands ~1.3 GB until pruned).
func podmanStoreDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home + "/.local/share/containers"
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
