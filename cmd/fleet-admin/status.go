package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/ElcanoTek/fleet/internal/clientconfig"
)

// cmdStatus is the `fleet-admin status` (a.k.a. doctor) health report. It runs a
// set of read-only checks against the local deployment and prints a ✓/✗ line per
// check, exiting non-zero if ANY required check failed. It is safe to run any
// time: it pings the databases (no migrations, no writes), runs a throwaway
// `podman run ... true` for the sandbox, loads + validates the client bundle,
// reports required env-var presence, and reports the systemd unit state when a
// unit is installed.
//
// Exit codes: 0 healthy · 6 unhealthy (one or more required checks failed) ·
// 1 usage.
func cmdStatus(argv []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	chatURL := fs.String("chat-database-url", "", "chat Postgres DSN (default FLEET_CHAT_DATABASE_URL / DATABASE_URL)")
	schedURL := fs.String("sched-database-url", "", "sched Postgres DSN (default FLEET_SCHED_DATABASE_URL / DATABASE_URL)")
	bundleDir := fs.String("client-config", "", "client bundle dir (default FLEET_CLIENT_CONFIG_DIR / config/default)")
	service := fs.String("service", "", "systemd unit to inspect (default FLEET_SERVICE_NAME / fleet)")
	skipSandbox := fs.Bool("no-sandbox", false, "skip the podman sandbox run check")
	if err := fs.Parse(argv); err != nil {
		return 1
	}

	r := &report{}
	r.head()

	// 1. client bundle loads + validates.
	bundle := checkBundle(r, strings.TrimSpace(*bundleDir))

	// 2. required env vars.
	checkEnv(r)

	// 3. both databases reachable.
	checkDB(r, "chat database", *chatURL, chatDSN)
	checkDB(r, "sched database", *schedURL, schedDSN)

	// 4. sandbox image present + runnable.
	if *skipSandbox {
		r.skip("sandbox image", "skipped (--no-sandbox)")
	} else {
		checkSandbox(r, bundle)
	}

	// 5. systemd unit state (informational unless the unit is installed + failed).
	checkService(r, serviceName(*service))

	return r.finish()
}

// ── check helpers ──────────────────────────────────────────────────────────

func checkBundle(r *report, dir string) *clientconfig.Bundle {
	b, err := clientconfig.Load(dir)
	if err != nil {
		r.fail("client bundle", fmt.Sprintf("load failed: %v", err))
		return nil
	}
	r.pass("client bundle", fmt.Sprintf("loaded %s (app=%q, %d MCP server(s))", b.Dir, b.Branding.AppName, len(b.MCPCatalog)))
	return b
}

// requiredEnv enumerates the env vars a running fleet deployment needs. The two
// DSNs are checked separately (they can also come from --flags), so here we only
// flag the credentials the env file must carry. OPENROUTER_API_KEY is treated as
// required unless mock mode is on (matching config.Validate).
func checkEnv(r *report) {
	mock := truthy(os.Getenv("FLEET_MOCK_MODE")) || truthy(os.Getenv("CHAT_MOCK_MODE"))
	type envCheck struct {
		name     string
		required bool
	}
	checks := []envCheck{
		{"OPENROUTER_API_KEY", !mock},
		{"FLEET_CLIENT_CONFIG_DIR", false}, // optional: defaults to config/default
	}
	for _, c := range checks {
		val := strings.TrimSpace(os.Getenv(c.name))
		switch {
		case val != "":
			r.pass("env "+c.name, "set")
		case c.required:
			r.fail("env "+c.name, "unset (required — add it to the 0600 env file)")
		default:
			r.warnLine("env "+c.name, "unset (using default)")
		}
	}
	// At least one DSN source must resolve; the per-DB checks below report the
	// detail, so here we only note the env file the deployment reads.
	if ef := strings.TrimSpace(os.Getenv("FLEET_ENV_FILE")); ef != "" {
		r.pass("env FLEET_ENV_FILE", ef)
	} else {
		r.warnLine("env FLEET_ENV_FILE", "unset (config.Load reads .env.local by default)")
	}
}

// checkDB pings a database with a short timeout. It opens a bare pgx connection
// (NOT store.Open / storage.Initialize, which run migrations) so the check is
// read-only and fast.
func checkDB(r *report, label, flagURL string, resolve func(string) (string, error)) {
	dsn, err := resolve(flagURL)
	if err != nil {
		r.fail(label, "DSN unresolved: "+err.Error())
		return
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		r.fail(label, "open: "+err.Error())
		return
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		r.fail(label, "ping failed: "+err.Error())
		return
	}
	var one int
	if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		r.fail(label, "SELECT 1 failed: "+errString(err))
		return
	}
	r.pass(label, "reachable ("+redactDSN(dsn)+")")
}

// checkSandbox resolves the image ref the running process would consume
// (FLEET_SANDBOX_IMAGE / CHAT_SANDBOX_IMAGE env wins, else the bundle's
// ResolvedImageRef) and runs a throwaway `podman run --rm <ref> true` to confirm
// the image is present + the runtime can launch it.
func checkSandbox(r *report, bundle *clientconfig.Bundle) {
	ref := strings.TrimSpace(os.Getenv("FLEET_SANDBOX_IMAGE"))
	if ref == "" {
		ref = strings.TrimSpace(os.Getenv("CHAT_SANDBOX_IMAGE"))
	}
	if ref == "" && bundle != nil {
		ref = strings.TrimSpace(bundle.Sandbox().ResolvedImageRef())
	}
	if ref == "" {
		r.fail("sandbox image", "no image ref resolved (set FLEET_SANDBOX_IMAGE or the bundle's sandbox.tag/image)")
		return
	}
	if _, err := exec.LookPath("podman"); err != nil {
		r.warnLine("sandbox image", fmt.Sprintf("podman not on PATH — cannot verify %s (install podman)", ref))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	//nolint:gosec // G204: fixed "podman" binary; ref is the operator's resolved bundle image, not request/LLM input.
	cmd := exec.CommandContext(ctx, "podman", "run", "--rm", ref, "true")
	if out, err := cmd.CombinedOutput(); err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}
		r.fail("sandbox image", fmt.Sprintf("%s not runnable: %s (build it: scripts/build-sandbox-image.sh)", ref, firstLine(detail)))
		return
	}
	r.pass("sandbox image", ref+" present + runnable")
}

// checkService reports the systemd unit state. An installed-but-failed unit is a
// failure; an absent unit is informational (a box may run fleet under another
// supervisor or directly), and "active" is a pass.
func checkService(r *report, name string) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		r.skip("systemd unit", "systemctl not on PATH (no systemd)")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	//nolint:gosec // G204: fixed "systemctl" binary; name is the operator-supplied unit name.
	if err := exec.CommandContext(ctx, "systemctl", "cat", name+".service").Run(); err != nil {
		r.skip("systemd unit", fmt.Sprintf("%s.service not installed (run scripts/bootstrap.sh --enable-service to install it)", name))
		return
	}
	//nolint:gosec // G204: fixed "systemctl" binary; name is the operator-supplied unit name.
	out, _ := exec.CommandContext(ctx, "systemctl", "is-active", name+".service").Output()
	state := strings.TrimSpace(string(out))
	switch state {
	case "active":
		r.pass("systemd unit", name+".service is active")
	case "activating", "reloading":
		r.warnLine("systemd unit", name+".service is "+state)
	default:
		r.fail("systemd unit", fmt.Sprintf("%s.service is %q — journalctl -u %s -n 50", name, state, name))
	}
}

// ── small report accumulator ────────────────────────────────────────────────

type report struct {
	failed int
}

func (r *report) head() {
	fmt.Fprintln(os.Stderr, "fleet-admin status — deployment health")
}

func (r *report) pass(label, detail string)     { fmt.Printf("✓ %-22s %s\n", label, detail) }
func (r *report) skip(label, detail string)     { fmt.Printf("– %-22s %s\n", label, detail) }
func (r *report) warnLine(label, detail string) { fmt.Printf("✓ %-22s %s\n", label, detail) }
func (r *report) fail(label, detail string) {
	r.failed++
	fmt.Printf("✗ %-22s %s\n", label, detail)
}

func (r *report) finish() int {
	fmt.Println()
	if r.failed == 0 {
		fmt.Fprintln(os.Stderr, "✓ healthy — all required checks passed")
		return 0
	}
	fmt.Fprintf(os.Stderr, "✗ unhealthy — %d check(s) failed\n", r.failed)
	return 6
}

// ── tiny utilities ──────────────────────────────────────────────────────────

func serviceName(flag string) string {
	if v := strings.TrimSpace(flag); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("FLEET_SERVICE_NAME")); v != "" {
		return v
	}
	return "fleet"
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func errString(err error) string {
	if err == nil {
		return "unexpected value"
	}
	return err.Error()
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// redactDSN strips userinfo (user:password@) from a DSN so the status report
// never echoes a password.
func redactDSN(dsn string) string {
	if i := strings.Index(dsn, "://"); i >= 0 {
		rest := dsn[i+3:]
		if at := strings.IndexByte(rest, '@'); at >= 0 {
			return dsn[:i+3] + "***@" + rest[at+1:]
		}
	}
	return dsn
}
