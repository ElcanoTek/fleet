// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Boot-time preflight for the allowlisted egress posture (#211 / ADR-0012).
//
// Allowlisted mode is the one network posture that depends on a specific
// rootless network HELPER: networkArgs emits
// `--network=slirp4netns:allow_host_loopback=true`, because the container has to
// reach the host-bound egress proxy at slirpHostGateway. Podman only honors that
// if the `slirp4netns` binary is installed.
//
// On Podman >= 5.0 the default rootless network is **pasta**, and a stock
// modern host (Fedora 40+, for instance) ships pasta WITHOUT slirp4netns. There
// the flag makes every container start fail:
//
//	Error: could not find slirp4netns, the network namespace can't be
//	configured: exec: "slirp4netns": executable file not found in $PATH
//
// That failed closed — a container that will not start cannot leak — but it
// failed *late and repeatedly*: boot succeeded, the egress proxy bound, the
// operator saw "network mode=allowlisted … filtered to […]", and then every
// interactive turn, scheduled task, and approved-bash call errored at container
// start. This preflight moves that discovery to boot, with an actionable
// message, in the same spirit as the OCI-runtime preflight (PreflightRuntime)
// and the storage-quota probe (ProbeStorageOptSupport).
//
// It deliberately does NOT try to substitute pasta. Pasta's host-loopback
// mapping uses a different gateway address than slirpHostGateway, so switching
// helpers means changing the proxy URL and the NO_PROXY exemption too — a
// change to a security-relevant path that belongs in its own reviewed PR, not
// smuggled into a preflight. See ADR-0012's "Deferred" note.

// networkPreflightTimeout bounds the throwaway probe container. Generous for a
// cold podman on a loaded box; short enough that a wedged podman fails boot
// promptly rather than hanging it.
const networkPreflightTimeout = 60 * time.Second

// allowlistedNetworkArg returns the `--network=…` flag networkArgs emits for
// allowlisted mode. Derived from networkArgs itself rather than restated, so the
// preflight can never validate a different network configuration than the one
// the turns actually use.
func allowlistedNetworkArg() (string, error) {
	// A non-empty proxy URL selects the allowlisted branch; its value is
	// irrelevant to the network flag.
	for _, arg := range networkArgs(false, "http://preflight.invalid") {
		if strings.HasPrefix(arg, "--network=") {
			return arg, nil
		}
	}
	return "", fmt.Errorf("networkArgs emitted no --network flag for allowlisted mode")
}

// PreflightAllowlistedNetwork verifies that a container can actually START with
// the network configuration allowlisted mode requires, by running a throwaway
// `--rm` container off the sandbox image that does nothing but exit 0. Called
// from the single production pool-construction path (agent.buildSandboxPool)
// when FLEET_DEFAULT_NETWORK_MODE=allowlisted.
//
// It FAILS CLOSED: an operator who asked for allowlisted egress on a host that
// cannot provide it gets a boot error naming the missing helper, not a
// deployment whose every tool call fails. Callers must not downgrade to open
// egress on error — that would hand unrestricted network to a deployment that
// explicitly asked for a filtered one.
//
// image is the resolved sandbox image (the same ref the pool will use). An empty
// image means the pool has no container backend at all — nothing to probe, so
// this is a no-op rather than a boot failure, matching how the other probes
// treat that case.
func PreflightAllowlistedNetwork(ctx context.Context, podmanBin, image string) error {
	if strings.TrimSpace(image) == "" {
		return nil
	}
	if podmanBin == "" {
		podmanBin = "podman"
	}
	netArg, err := allowlistedNetworkArg()
	if err != nil {
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, networkPreflightTimeout)
	defer cancel()
	//nolint:gosec // podmanBin, the derived network flag, and image are all operator-set config, not request input
	out, err := exec.CommandContext(probeCtx, podmanBin, "run", "--rm", netArg, image, "/usr/bin/true").CombinedOutput()
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(string(out))
	if len(detail) > 512 {
		detail = detail[len(detail)-512:]
	}
	// Recognize the specific, common cause so the operator gets the fix rather
	// than a raw podman error.
	if strings.Contains(strings.ToLower(detail), "slirp4netns") {
		return fmt.Errorf("allowlisted egress requires the slirp4netns network helper, which this host does not have: %w (%s)\n"+
			"Podman >= 5.0 defaults to pasta, and a stock modern host often ships pasta WITHOUT slirp4netns. "+
			"Install it (Fedora/RHEL: `dnf install slirp4netns`; Debian/Ubuntu: `apt install slirp4netns`), "+
			"or use FLEET_DEFAULT_NETWORK_MODE=lockdown (the hard seal) or open", err, detail)
	}
	return fmt.Errorf("allowlisted egress preflight: a container could not start with %s: %w (%s)", netArg, err, detail)
}
