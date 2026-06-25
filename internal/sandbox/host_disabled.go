//go:build !fleet_host_executor

// host_disabled.go is the release-build counterpart to host.go: when the
// `fleet_host_executor` tag is absent, the unsandboxed host executor is NOT
// compiled in (#159). newHostSandbox fails instead of running agent tool calls
// directly on the host, and MockMode is rejected at boot (see manager.go). This
// is what makes "the host executor cannot ship enabled in a production build" a
// property of the artifact, not just a runtime flag.

package sandbox

import "errors"

// hostExecutorCompiledIn is false in a release build. See HostExecutorCompiledIn.
const hostExecutorCompiledIn = false

// errHostExecutorDisabled is returned when something tries to construct a
// host-mode sandbox in a build that did not opt into the host executor.
var errHostExecutorDisabled = errors.New(
	"host (unsandboxed) executor is not compiled into this binary; " +
		"rebuild with -tags fleet_host_executor (tests/dev only) to enable ModeHost")

// newHostSandbox always errors in a release build — there is no host executor.
func newHostSandbox(_ []byte) (*Sandbox, error) {
	return nil, errHostExecutorDisabled
}
