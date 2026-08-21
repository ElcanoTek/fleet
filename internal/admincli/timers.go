// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// `fleet timers` manages the scheduled-maintenance systemd pairs fleet ships in
// deploy/: fleet-backup.{service,timer} (daily pg_dump of both databases) and
// fleet-maintenance.{service,timer} (daily podman layer + build-cache prune).
//
// Why a dedicated verb: bootstrap --enable-service installs the pairs on a NEW
// box, but a box provisioned before they shipped (or with the --no-*-timer
// opt-outs) has none, and until now the only path to them was copy-pasting a
// four-command hint out of `fleet doctor`. `fleet timers install` is that hint
// as one idempotent command: install the missing halves from the checkout's
// deploy/, daemon-reload, enable --now. It deliberately does NOT overwrite a
// unit that is already installed — reconciling drift between an installed unit
// and deploy/ stays `fleet doctor` / `fleet update`'s job, so an operator
// hand-edit can never be clobbered by an install verb.
//
// These timers are a systemd-deployment concern. On a host without systemd
// (a container platform, Kubernetes, another supervisor) the command explains
// what to schedule instead and exits non-zero rather than pretending — the
// same jobs belong to that platform's scheduler (cron, a CronJob).

// timerPair describes one shipped service+timer pair under deploy/. Fixed
// names (unlike the fleet unit, which FLEET_SERVICE_NAME can rename) because
// bootstrap installs exactly these — internal/boxdoctor and scripts/doctor.sh
// probe the same.
type timerPair struct {
	name    string // selector: --backup / --maintenance
	service string
	timer   string
	what    string // one-line purpose, printed in plans and summaries
	skip    string // the honest "you may not need this" caveat
	// The backup oneshot writes into FLEET_BACKUP_DIR; installing it without
	// the directory would make the first fire create it with pg_dump's
	// defaults instead of the 0700 root-owned posture bootstrap establishes.
	needsBackupDir bool
}

var timerPairs = []timerPair{
	{
		name:           "backup",
		service:        "fleet-backup.service",
		timer:          "fleet-backup.timer",
		what:           "daily pg_dump of the chat + sched databases (02:00)",
		skip:           "skip if you back up at the volume/hypervisor layer",
		needsBackupDir: true,
	},
	{
		name:    "maintenance",
		service: "fleet-maintenance.service",
		timer:   "fleet-maintenance.timer",
		what:    "daily podman layer + build-cache prune (03:30)",
		skip:    "skip if you prune the container store yourself",
	},
}

// timersHost is the seam between the install logic and the box, so the logic
// is table-testable without systemd or root (the admincli test convention:
// probe/mutate functions injected, real implementations in defaultTimersHost).
type timersHost struct {
	haveSystemctl func() bool
	unitInstalled func(unit string) bool         // systemctl cat <unit>
	installUnit   func(src, dst string) error    // 0644 copy into /etc/systemd/system
	daemonReload  func() error                   // systemctl daemon-reload
	enableNow     func(timer string) error       // systemctl enable --now <timer>
	ensureDir     func(dir string) (bool, error) // 0700 mkdir-if-absent; reports whether it created
	isRoot        func() bool
	out           io.Writer
	errOut        io.Writer
}

func defaultTimersHost() timersHost {
	return timersHost{
		haveSystemctl: func() bool {
			_, err := exec.LookPath("systemctl")
			return err == nil
		},
		unitInstalled: systemctlUnitInstalled,
		installUnit: func(src, dst string) error {
			body, err := os.ReadFile(src) //nolint:gosec // G304: src is <checkout>/deploy/<fixed unit name>, operator-controlled, never request input.
			if err != nil {
				return err
			}
			return os.WriteFile(dst, body, 0o644) //nolint:gosec // G306: systemd unit files are world-readable by convention (they hold no secrets; the env file they reference stays 0600).
		},
		daemonReload: func() error { return systemctlRun("daemon-reload") },
		enableNow:    func(timer string) error { return systemctlRun("enable", "--now", timer) },
		ensureDir: func(dir string) (bool, error) {
			if _, err := os.Stat(dir); err == nil {
				// Never re-mode a directory that already exists: FLEET_BACKUP_DIR
				// may point at a shared mount whose permissions are the operator's
				// call (the unit's UMask=0077 keeps the dump FILES owner-only
				// regardless). Same rule as bootstrap.sh.
				return false, nil
			}
			return true, os.MkdirAll(dir, 0o700)
		},
		isRoot: func() bool { return os.Geteuid() == 0 },
		out:    os.Stdout,
		errOut: os.Stderr,
	}
}

// systemctlRun executes one short systemctl verb, surfacing systemd's own
// message on failure (it names the actual problem — masked unit, no D-Bus —
// better than we could).
func systemctlRun(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	//nolint:gosec // G204: fixed "systemctl" binary; args are compile-time verbs + the fixed timer unit names above.
	out, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("systemctl %s: %s", strings.Join(args, " "), msg)
	}
	return nil
}

// cmdTimers dispatches the `fleet timers` group. Only "install" exists —
// state reporting already lives in `fleet status` / `fleet doctor`, and a
// third reporter would just be one more place for the verdicts to drift.
func cmdTimers(argv []string) int {
	if len(argv) == 0 {
		timersUsage(os.Stderr)
		return 1
	}
	switch argv[0] {
	case "install":
		return cmdTimersInstall(argv[1:])
	case "-h", "--help", "help":
		timersUsage(os.Stderr)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown timers subcommand %q\n\n", argv[0])
		timersUsage(os.Stderr)
		return 1
	}
}

func timersUsage(w io.Writer) {
	fmt.Fprint(w, `usage: fleet timers install [--backup] [--maintenance] [--src <dir>] [--dry-run]

Install + enable the scheduled-maintenance systemd timers fleet ships in
deploy/ (both pairs by default; --backup / --maintenance select one):

  fleet-backup.timer       daily pg_dump of the chat + sched databases (02:00)
                           (skip if you back up at the volume/hypervisor layer)
  fleet-maintenance.timer  daily podman layer + build-cache prune (03:30)
                           (skip if you prune the container store yourself)

Idempotent: already-installed units are never overwritten (drift is
'fleet doctor' / 'fleet update --adopt-units' territory); a pair that is
installed but disabled or stopped is re-enabled. Needs root except --dry-run.
On a host without systemd (Kubernetes, another supervisor) this explains what
to schedule instead and exits non-zero.
`)
}

// timersInstallOpts is the parsed flag set for `fleet timers install`.
type timersInstallOpts struct {
	backup      bool
	maintenance bool
	src         string
	dryRun      bool
}

func cmdTimersInstall(argv []string) int {
	fs := flag.NewFlagSet("timers install", flag.ContinueOnError)
	var opts timersInstallOpts
	fs.BoolVar(&opts.backup, "backup", false, "install only the fleet-backup pair")
	fs.BoolVar(&opts.maintenance, "maintenance", false, "install only the fleet-maintenance pair")
	fs.StringVar(&opts.src, "src", "", "fleet source checkout holding deploy/ (default: FLEET_ROOT or auto-detected)")
	fs.BoolVar(&opts.dryRun, "dry-run", false, "print the plan; install nothing (no root needed)")
	fs.Usage = func() { timersUsage(os.Stderr) }
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		return errf(2, "timers install: unexpected argument %q (see fleet timers --help)", fs.Arg(0))
	}
	return runTimersInstall(opts, defaultTimersHost())
}

// runTimersInstall is the whole verb behind the timersHost seam.
func runTimersInstall(opts timersInstallOpts, h timersHost) int {
	selected := selectedTimerPairs(opts)
	// errf's sibling, aimed at the host seam so tests can assert the messages.
	fail := func(format string, a ...any) int {
		fmt.Fprintf(h.errOut, "error: "+format+"\n", a...)
		return 5
	}

	if !h.haveSystemctl() {
		// Not an error in the box's config — these timers just don't apply
		// here. Say what owns the equivalent jobs instead, then exit non-zero
		// because the requested install did not (and cannot) happen.
		fmt.Fprintln(h.errOut, "no systemd on this host — the fleet timers don't apply (a container platform, Kubernetes, or another supervisor?).")
		fmt.Fprintln(h.errOut, "Schedule the equivalent jobs with your platform's scheduler instead (cron, a Kubernetes CronJob):")
		fmt.Fprintln(h.errOut, "  daily: fleet backup --db=all --prune   (database dumps; skip if you back up at the volume layer)")
		fmt.Fprintln(h.errOut, "  daily: fleet cleanup                   (podman layer + build-cache prune)")
		return 5
	}

	deployDir := findDeployDir(opts.src)
	if deployDir == "" {
		return fail("timers install: no deploy/ directory found (set FLEET_ROOT, pass --src <checkout>, or run from the repo) — the shipped units live in <checkout>/deploy")
	}
	for _, p := range selected {
		for _, unit := range []string{p.service, p.timer} {
			if _, err := os.Stat(filepath.Join(deployDir, unit)); err != nil {
				return fail("timers install: %s missing under %s — is this a fleet checkout?", unit, deployDir)
			}
		}
	}

	if !opts.dryRun && !h.isRoot() {
		return fail("timers install writes /etc/systemd/system — run as root: sudo fleet timers install (or --dry-run to preview)")
	}

	installedAny := false
	for _, p := range selected {
		for _, unit := range []string{p.service, p.timer} {
			dst := filepath.Join("/etc/systemd/system", unit)
			if h.unitInstalled(unit) {
				// Present units are left alone on purpose: overwriting here
				// would silently clobber operator hand-edits; drift against
				// deploy/ is reconciled (with consent) by doctor/update.
				fmt.Fprintf(h.out, "%s already installed — leaving it as-is (drift vs deploy/ is `fleet doctor`'s job)\n", unit)
				continue
			}
			src := filepath.Join(deployDir, unit)
			if opts.dryRun {
				fmt.Fprintf(h.out, "[dry-run] would install %s → %s\n", src, dst)
				installedAny = true
				continue
			}
			if err := h.installUnit(src, dst); err != nil {
				return fail("timers install: %s → %s: %v", src, dst, err)
			}
			fmt.Fprintf(h.out, "installed %s\n", dst)
			installedAny = true
		}
		if p.needsBackupDir {
			dir := resolveBackupDir()
			if opts.dryRun {
				fmt.Fprintf(h.out, "[dry-run] would create %s if missing (0700 root-owned — a dump holds every conversation, task and user row)\n", dir)
			} else if created, err := h.ensureDir(dir); err != nil {
				return fail("timers install: create %s: %v", dir, err)
			} else if created {
				fmt.Fprintf(h.out, "created %s (0700 root-owned — a dump holds every conversation, task and user row)\n", dir)
			}
		}
	}

	if opts.dryRun {
		for _, p := range selected {
			fmt.Fprintf(h.out, "[dry-run] would run: systemctl daemon-reload && systemctl enable --now %s (%s; %s)\n", p.timer, p.what, p.skip)
		}
		return 0
	}

	// One reload covers every install above; enable --now needs the freshly
	// written units visible. Skipping it when nothing was installed keeps the
	// re-run path (everything present, maybe disabled) from a pointless reload.
	if installedAny {
		if err := h.daemonReload(); err != nil {
			return fail("timers install: %v", err)
		}
	}
	// enable --now is idempotent AND the repair for the two other states
	// doctor advises on (installed-but-disabled, enabled-but-stopped), so it
	// runs unconditionally for each selected pair.
	for _, p := range selected {
		if err := h.enableNow(p.timer); err != nil {
			return fail("timers install: %v", err)
		}
		fmt.Fprintf(h.out, "%s enabled and started (%s)\n", p.timer, p.what)
	}
	return 0
}

// selectedTimerPairs maps the selector flags onto pairs; no selector means both
// (the common "make this box stop warning" case).
func selectedTimerPairs(opts timersInstallOpts) []timerPair {
	if !opts.backup && !opts.maintenance {
		return timerPairs
	}
	var out []timerPair
	for _, p := range timerPairs {
		if (p.name == "backup" && opts.backup) || (p.name == "maintenance" && opts.maintenance) {
			out = append(out, p)
		}
	}
	return out
}

// findDeployDir locates the checkout's deploy/ directory holding the shipped
// units. Same probe order as findScript (FLEET_ROOT, cwd, the binary's dir,
// the bootstrap layout's <install-dir>/src), keyed on fleet-backup.timer being
// present so a random dir named deploy/ never matches.
func findDeployDir(flagSrc string) string {
	candidates := []string{}
	if src := strings.TrimSpace(flagSrc); src != "" {
		candidates = append(candidates, filepath.Join(src, "deploy"))
	} else {
		if root := strings.TrimSpace(os.Getenv("FLEET_ROOT")); root != "" {
			candidates = append(candidates, filepath.Join(root, "deploy"))
		}
		candidates = append(candidates, "deploy")
		if exe, err := os.Executable(); err == nil {
			candidates = append(candidates,
				filepath.Join(filepath.Dir(exe), "deploy"),
				filepath.Join(filepath.Dir(exe), "src", "deploy"))
		}
	}
	for _, c := range candidates {
		if _, err := os.Stat(filepath.Join(c, "fleet-backup.timer")); err == nil { //nolint:gosec // G703: candidate paths are operator-controlled (--src flag, FLEET_ROOT env, the literal "deploy", the binary's own dir), never request or LLM input — same rule as findScript.
			return c
		}
	}
	return ""
}

// resolveBackupDir mirrors the unit's own resolution: FLEET_BACKUP_DIR from
// the process env, else from the server env file (which the unit reads via
// EnvironmentFile=), else the in-unit default /var/backups/fleet. A relative
// value is ignored — the unit runs with "/" as cwd, so a relative dir is a
// misconfiguration bootstrap refuses; here we just fall back to the default
// rather than creating a directory literally named "backups" under /.
func resolveBackupDir() string {
	dir := strings.TrimSpace(os.Getenv("FLEET_BACKUP_DIR"))
	if dir == "" {
		dir = envFileGet(serverEnvFile(""), "FLEET_BACKUP_DIR")
	}
	if filepath.IsAbs(dir) {
		return dir
	}
	return "/var/backups/fleet"
}

// envFileGet reads one KEY from an env file WITHOUT sourcing it (the file
// holds secrets; sourcing would execute arbitrary content on a tampered box).
// Last assignment wins, surrounding quotes stripped — the same contract as
// doctor.sh/bootstrap.sh's env_get. Unreadable file (including not-root on the
// 0600 file) reads as unset.
func envFileGet(file, key string) string {
	body, err := os.ReadFile(file) //nolint:gosec // G304: operator-config path (FLEET_ENV_FILE / the fixed /etc/fleet default), never request input.
	if err != nil {
		return ""
	}
	val := ""
	for _, line := range strings.Split(string(body), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), key+"="); ok {
			val = strings.Trim(rest, `"'`)
		}
	}
	return val
}
