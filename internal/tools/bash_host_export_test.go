package tools

import (
	"context"

	"github.com/ElcanoTek/fleet/internal/sandbox"
)

// runBash runs the bash tool against the HOST backend, constructing a fresh host
// sandbox per call. It is a TEST-ONLY convenience: host execution is forbidden in
// production (every prod call passes a pool-issued container sandbox to
// runBashWithSandbox), so this host shortcut lives in a _test.go file and never
// ships in the production binary.
func runBash(ctx context.Context, params BashParams) (string, error) {
	return runBashWithSandbox(ctx, sandbox.NewHost(nil), params)
}
