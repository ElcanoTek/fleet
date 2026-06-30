// Command fleet-admin is the DEPRECATED entry point for the fleet operator CLI.
// The operator CLI is now unified into the single `fleet` binary (#461): use
// `fleet <verb>` (e.g. `fleet update`, `fleet status`) instead. This shim simply
// forwards to the same admin dispatch the `fleet` binary uses, after printing a
// one-line deprecation notice, so existing scripts and muscle memory keep
// working for one release. It will be removed in a future release.
package main

import (
	"fmt"
	"os"

	"github.com/ElcanoTek/fleet/internal/admincli"
)

func main() {
	fmt.Fprintln(os.Stderr, "warning: `fleet-admin` is deprecated and will be removed; use `fleet <command>` instead (e.g. `fleet update`).")
	os.Exit(admincli.Run(os.Args[1:]))
}
