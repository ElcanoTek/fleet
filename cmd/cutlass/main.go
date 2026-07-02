// Command cutlass is the DEPRECATED name for fleet's local one-shot task
// harness. It was folded into the unified CLI as `fleet task run` (mirroring
// the fleet-admin consolidation, ADR-0012); this shim forwards for one
// deprecation release and is then removed.
package main

import (
	"fmt"
	"os"

	"github.com/ElcanoTek/fleet/internal/taskrun"
)

func main() {
	fmt.Fprintln(os.Stderr, "cutlass: DEPRECATED — use `fleet task run <task.yaml>` (this shim forwards for one release)")
	os.Exit(taskrun.Run(os.Args[1:], "cutlass"))
}
