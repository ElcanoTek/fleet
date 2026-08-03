// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Boot-time preflight for the allowlisted egress posture (#211 / ADR-0012).
//
// Allowlisted is the one network posture that depends on a specific rootless
// network HELPER: networkArgs emits
// `--network=slirp4netns:allow_host_loopback=true`, because the container has to
// reach the host-bound egress proxy at slirpHostGateway. Podman honors that only
// if the `slirp4netns` binary is installed.
//
// On Podman >= 5.0 the default rootless network is **pasta**, and a stock modern
// host often ships pasta WITHOUT slirp4netns. There the flag makes every
// container start fail:
//
//	Error: could not find slirp4netns, the network namespace can't be
//	configured: exec: "slirp4netns": executable file not found in $PATH
//
// That failed closed — a container that will not start cannot leak, and the
// error is a plain start failure, never ErrContainerUnavailable, so no take path
// degrades to the open warm pool — but it failed *late and repeatedly*: boot
// succeeded, the egress proxy bound, the operator saw "network mode=allowlisted
// … filtered to […]", and then every interactive turn, scheduled task, and
// approved-bash call errored at container start. This preflight moves that
// discovery to boot with an actionable message, mirroring the OCI-runtime
// preflight (PreflightRuntime).
//
// It asks PODMAN whether it has the helper rather than probing a throwaway
// container. A container probe sounds more faithful but is not: the network
// namespace is configured *before* the container's command is exec'd, so a
// bundle whose rootfs lacks whatever command the probe picked would abort boot
// while reporting a network failure — and the sandbox image is a client-bundle
// artifact free to change its base (an Alpine/busybox bundle has /bin/true, not
// /usr/bin/true). Asking podman is image-independent, cheaper, and the
// mechanism resolveRuntimePath already established.
//
// It deliberately does NOT substitute pasta. Pasta's host-loopback mapping uses
// a different gateway address than slirpHostGateway, so switching helpers means
// changing the proxy URL and the NO_PROXY exemption too — a change on a
// security-relevant path that belongs in its own reviewed PR with
// rootless-host verification. See ADR-0012's deferred note.

// networkPreflightTimeout bounds the `podman info` call. Matches
// runtimeResolveTimeout: generous for a cold podman on a loaded box, short
// enough that a wedged podman fails boot promptly rather than hanging it.
const networkPreflightTimeout = runtimeResolveTimeout

// allowlistedNetworkHelper returns the rootless network helper allowlisted mode
// requires, derived from the `--network=` flag networkArgs actually emits rather
// than restated here — so the preflight can never validate a different
// configuration than the one turns use. `slirp4netns:allow_host_loopback=true`
// yields "slirp4netns".
func allowlistedNetworkHelper() (string, error) {
	// A non-empty proxy URL selects the allowlisted branch; its value is
	// irrelevant to the network flag.
	for _, arg := range networkArgs(false, "http://preflight.invalid") {
		spec, ok := strings.CutPrefix(arg, "--network=")
		if !ok {
			continue
		}
		helper, _, _ := strings.Cut(spec, ":") // drop the ":opt=val" tail
		if helper == "" {
			return "", fmt.Errorf("networkArgs emitted an unparseable network flag %q for allowlisted mode", arg)
		}
		return helper, nil
	}
	return "", fmt.Errorf("networkArgs emitted no --network flag for allowlisted mode")
}

// PreflightAllowlistedNetwork verifies that Podman can RESOLVE the rootless
// network helper the allowlisted egress posture requires. Deliberately that and
// not more: a presence check cannot prove the binary works (a corrupt or
// non-executable slirp4netns still reports a path), so this catches the common,
// total failure — the helper is simply not installed — and nothing subtler. Called from the single production
// pool-construction path (agent.buildSandboxPool) when
// FLEET_DEFAULT_NETWORK_MODE=allowlisted, and from `fleet validate-config`.
//
// It FAILS CLOSED, in both the expected and the unexpected direction: an
// operator who asked for allowlisted egress on a host that cannot provide it
// gets a boot error naming the missing helper and the fix, and if networkArgs is
// ever changed to request a helper this function does not know how to check, it
// refuses rather than silently reporting success. Callers must not downgrade to
// open egress on error — that would hand unrestricted network to a deployment
// that explicitly asked for a filtered one.
func PreflightAllowlistedNetwork(ctx context.Context, podmanBin string) error {
	helper, err := allowlistedNetworkHelper()
	if err != nil {
		return fmt.Errorf("allowlisted egress preflight: %w", err)
	}
	return preflightNetworkHelper(ctx, podmanBin, helper)
}

// networkHelperInfoTemplate maps a rootless network helper to the `podman info`
// template that reports its resolved executable. Podman exposes each helper
// under its own field, so this mapping is per-helper by necessity — and being a
// lookup rather than an if-chain is what makes the unknown-helper case a
// fail-closed error instead of an accidental pass.
var networkHelperInfoTemplate = map[string]string{
	"slirp4netns": "{{.Host.Slirp4NetNS.Executable}}",
}

// preflightNetworkHelper asks Podman whether it has the named rootless network
// helper. Split out from PreflightAllowlistedNetwork so the unknown-helper
// fail-closed path is testable without mutating networkArgs.
func preflightNetworkHelper(ctx context.Context, podmanBin, helper string) error {
	tmpl, known := networkHelperInfoTemplate[helper]
	if !known {
		return fmt.Errorf("allowlisted egress preflight: networkArgs requests the %q network helper, "+
			"which this preflight does not know how to verify — add it to networkHelperInfoTemplate alongside that change", helper)
	}
	if podmanBin == "" {
		podmanBin = "podman"
	}
	infoCtx, cancel := context.WithTimeout(ctx, networkPreflightTimeout)
	defer cancel()
	out, err := exec.CommandContext(infoCtx, podmanBin, "info", "--format", tmpl).Output()
	if err != nil {
		return fmt.Errorf("allowlisted egress preflight: could not ask podman about the %s helper: %w%s", helper, err, stderrOf(err))
	}
	if strings.TrimSpace(string(out)) != "" {
		return nil
	}
	return fmt.Errorf("allowlisted egress requires the %s network helper, which podman does not have on this host. "+
		"Podman >= 5.0 defaults to pasta, and a stock modern host often ships pasta WITHOUT %s, which makes every "+
		"container start fail. Install it (Fedora/RHEL: `dnf install %s`; Debian/Ubuntu: `apt install %s`), or use "+
		"FLEET_DEFAULT_NETWORK_MODE=lockdown (the hard seal) or open", helper, helper, helper, helper)
}
