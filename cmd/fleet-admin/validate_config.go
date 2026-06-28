package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
)

// cmdValidateConfig is the `fleet-admin validate-config` preflight verb. It runs
// the read-only configuration checks an operator wants BEFORE `systemctl start
// fleet` (or as a CI gate), and exits non-zero if any blocking check fails — so a
// misconfigured bundle, a missing credential env var, or a malformed MCP catalog
// is caught at validation time instead of as a cryptic runtime error minutes
// after startup.
//
// It deliberately REUSES the doctor (`status`) check helpers — checkBundle,
// checkEnv, checkDB, checkSandbox — so there is one source of truth for "is this
// deployment healthy". On top of those it surfaces the manifest/MCP-catalog
// shape problems that clientconfig.Load only LOGS as warnings (unresolved stdio
// script-path args and malformed Agent Skills) as explicit ✗ failures, since at
// validation time those are exactly the defects an operator wants to block on.
//
// Network-touching checks (the two DB pings, the podman sandbox run) are gated by
// flags so the verb is usable in a CI runner with no Postgres / no podman /no
// outbound access: pass --skip-network-checks to drop the DB pings and
// --no-sandbox to drop the podman run. With both set the run is fully offline and
// validates only the bundle shape + env-var presence.
//
// Exit codes: 0 valid · 6 invalid (one or more required checks failed) · 1 usage.
func cmdValidateConfig(argv []string) int {
	fs := flag.NewFlagSet("validate-config", flag.ContinueOnError)
	bundleDir := fs.String("bundle-path", "", "client bundle dir (default FLEET_CLIENT_CONFIG_DIR / config/default)")
	chatURL := fs.String("chat-database-url", "", "chat Postgres DSN (default FLEET_CHAT_DATABASE_URL / DATABASE_URL)")
	schedURL := fs.String("sched-database-url", "", "sched Postgres DSN (default FLEET_SCHED_DATABASE_URL / DATABASE_URL)")
	skipNetwork := fs.Bool("skip-network-checks", false, "skip the database pings (usable in CI with no Postgres / no outbound access)")
	skipSandbox := fs.Bool("no-sandbox", false, "skip the podman sandbox run check")
	if err := fs.Parse(argv); err != nil {
		return 1
	}

	r := &report{}
	fmt.Fprintln(os.Stderr, "fleet-admin validate-config — preflight configuration checks")

	// 1. client bundle loads + validates (manifest parse, MCP-catalog shape,
	//    sandbox descriptor). clientconfig.Load runs the structural validate()
	//    internally, so a malformed catalog already fails here.
	bundle := checkBundle(r, strings.TrimSpace(*bundleDir))

	// 2. manifest/MCP-catalog shape problems clientconfig.Load only WARNS about:
	//    stdio script-path args that do not resolve under the bundle, and
	//    malformed Agent Skills. At validation time these are blocking — a
	//    connector that cannot launch or a skill that silently drops out of the
	//    roster is a defect an operator wants to fix before boot.
	checkBundleShape(r, bundle)

	// 3. required env vars present (read-only; never prints values).
	checkEnv(r)

	// 4. both databases reachable (network I/O — gated for offline CI runs).
	if *skipNetwork {
		r.skip("chat database", "skipped (--skip-network-checks)")
		r.skip("sched database", "skipped (--skip-network-checks)")
	} else {
		checkDB(r, "chat database", *chatURL, chatDSN)
		checkDB(r, "sched database", *schedURL, schedDSN)
	}

	// 5. sandbox image present + runnable (network/podman I/O — gated likewise).
	if *skipSandbox {
		r.skip("sandbox image", "skipped (--no-sandbox)")
	} else {
		checkSandbox(r, bundle)
	}

	return r.finish()
}

// checkBundleShape surfaces the bundle's non-fatal load-time warnings as explicit
// pass/fail report lines. clientconfig.Load logs unresolved stdio script-path
// args (ValidateMCPArgPaths) and malformed Agent Skills (ValidateSkills) as
// warnings rather than failing the load, so a server can boot with a broken
// connector; validate-config promotes them to blocking failures so the defect is
// caught before startup. It is a no-op pass when the bundle did not load (the
// load failure is already reported by checkBundle).
func checkBundleShape(r *report, bundle *clientconfig.Bundle) {
	if bundle == nil {
		return
	}
	problems := bundle.ValidateMCPArgPaths()
	problems = append(problems, bundle.ValidateSkills()...)
	if len(problems) == 0 {
		r.pass("manifest shape", fmt.Sprintf("%d MCP server(s), %d skill(s): all paths resolve", len(bundle.MCPCatalog), len(bundle.Skills())))
		return
	}
	for _, p := range problems {
		r.fail("manifest shape", p)
	}
}
