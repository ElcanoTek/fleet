// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package sandbox

import (
	"context"
	"os/exec"
)

// PodmanExec is the execution context for the sandbox preflights' subprocess
// probes: WHICH binary (Binary — the podman path, "" = "podman"), run as WHOM
// (Prefix — an argv hop such as runuser/env to the unit's User=), and from
// WHERE (Dir — rootless podman re-execs and chdir()s back to an inherited cwd
// the target user may not be able to enter, e.g. /root).
//
// The zero value probes as the calling process from the inherited working
// directory — exactly what the boot path wants, because the service process
// already IS the service user: agent.buildSandboxPool passes PodmanExec{}
// (modulo Binary) and preflights byte-identically to the historical
// podmanBin-only behavior. `fleet validate-config` is the other caller: run as
// root against a rootless per-user podman store, it builds the context with
// ServiceStorePodmanExec so every probe (runtime resolution, network helper,
// KVM) asks the question as the user the service will actually run as —
// otherwise a healthy box preflights red, the exact false-negative class the
// service-store resolution exists to prevent.
type PodmanExec struct {
	Binary string   // podman binary path; "" = "podman" on PATH
	Prefix []string // argv run before the binary (e.g. runuser/env); empty = as the caller
	Dir    string   // working directory; "" = inherit
}

// CommandContext builds the exec.Cmd for a probe. binary overrides Binary when
// non-empty (the KVM probe execs `test`, not podman); otherwise the configured
// Binary applies, defaulting to "podman".
//
//nolint:gosec // G204: Prefix and Binary are operator-configured (the unit's User=, the podman path), and args are fixed probe arguments built by this package — no request/LLM input reaches argv.
func (e PodmanExec) CommandContext(ctx context.Context, binary string, args ...string) *exec.Cmd {
	argv := e.argv(binary, args...)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if e.Dir != "" {
		cmd.Dir = e.Dir
	}
	return cmd
}

// argv is the pure half of CommandContext — the exact argv that gets exec'd —
// so tests pin the prefix/no-prefix shapes without running anything.
func (e PodmanExec) argv(binary string, args ...string) []string {
	b := binary
	if b == "" {
		b = e.Binary
	}
	if b == "" {
		b = "podman"
	}
	argv := make([]string, 0, len(e.Prefix)+1+len(args))
	argv = append(argv, e.Prefix...)
	argv = append(argv, b)
	return append(argv, args...)
}

// ServiceStorePodmanExec builds the PodmanExec that probes as the fleet unit's
// User=, plus a note naming whose store/context the verdict is actually about.
// Pure, so every caller's table can pin it. THIS is the single copy of the
// runuser/HOME/XDG_RUNTIME_DIR construction — admincli.ServiceStorePodmanArgv
// (status probes) and `fleet validate-config` (preflights) both delegate here
// rather than growing a second, drift-prone version.
//
// Rootless podman keeps one image store PER USER. The service runs as the
// unit's User= (fleet), so the sandbox image — and the runtime/network
// registrations in that user's containers.conf — live in THAT user's world; a
// root shell running podman inspects root's instead and reports "missing" for
// what `fleet status` just verified. As root with a non-root service user the
// command therefore goes through runuser with the unit's HOME and
// XDG_RUNTIME_DIR (the same shape scripts/build-sandbox-image.sh and
// doctor.sh use), and Dir follows the service user's home because rootless
// podman chdir()s back to an inherited cwd the service user may not be able to
// enter (e.g. /root). As anyone else it runs as the caller and the note says
// whose store that was, so nobody chases a phantom.
func ServiceStorePodmanExec(svcUser, svcHome string, asRoot bool) (PodmanExec, string) {
	switch {
	case asRoot && svcUser != "" && svcUser != "root":
		home := svcHome
		if home == "" {
			home = "/var/lib/" + svcUser
		}
		return PodmanExec{
			Prefix: []string{"runuser", "-u", svcUser, "--", "env", "HOME=" + home, "XDG_RUNTIME_DIR=/run/" + svcUser},
			Dir:    svcHome, // only when KNOWN — matches the status-probe caller, which inherits otherwise
		}, " (as " + svcUser + " — the service's image store)"
	case asRoot:
		return PodmanExec{}, " (root's store — the service runs as root too)"
	case svcUser != "" && svcUser != "root":
		return PodmanExec{}, " (YOUR store, not " + svcUser + "'s — run: sudo fleet status, or sudo fleet doctor for the authoritative check)"
	default:
		return PodmanExec{}, ""
	}
}
