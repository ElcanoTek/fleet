// Command fleet-admin is the DEPRECATED entry point for the fleet operator CLI.
// The operator CLI is now unified into the single `fleet` binary (#461): use
// `fleet <verb>` (e.g. `fleet update`, `fleet status`) instead. This shim simply
// forwards to the same admin dispatch the `fleet` binary uses, after printing a
// one-line deprecation notice, so existing scripts and muscle memory keep
// working. It is removed in the first release on or after 2026-12-01 — the
// dated trigger docs/adr/0012-unified-fleet-cli.md records (dated because
// releases are date-based and no 1.0.0 will ever be cut; ADR-0059). Until then
// it stays, and the
// build/upgrade scripts (Makefile bins/install, scripts/update.sh,
// scripts/fleet-upgrade.sh) still emit and install it.
package main

import (
	"fmt"
	"os"

	"github.com/ElcanoTek/fleet/internal/admincli"
)

func main() {
	fmt.Fprintln(os.Stderr, "warning: `fleet-admin` is deprecated and is removed in the first release on or after 2026-12-01; use `fleet <command>` instead (e.g. `fleet update`).")
	os.Exit(admincli.Run(os.Args[1:]))
}
