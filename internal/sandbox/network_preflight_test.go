// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package sandbox

// Tests for the allowlisted-egress boot preflight (#211 / ADR-0012). A fake
// podman keeps them deterministic on any host: the failure this preflight
// exists to catch (no slirp4netns helper) cannot otherwise be reproduced on a
// box that HAS the helper, and the box that lacks it cannot run the success
// case.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakePodmanRun writes a stub podman that ASSERTS it was asked to run a
// container with the allowlisted network flag, then replies with exitCode and
// combinedOutput. The argv assertion matters: a preflight that probed some
// OTHER network configuration would validate nothing, and this file would still
// be green.
func fakePodmanRun(t *testing.T, wantNetArg string, combinedOutput string, exitCode int) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fake-podman")
	script := "#!/bin/sh\n" +
		"case \" $* \" in *\" run \"*) ;; *) echo \"fake-podman: expected a run command, got: $*\" >&2; exit 90;; esac\n" +
		"case \" $* \" in *\" " + wantNetArg + " \"*) ;; *) echo \"fake-podman: expected " + wantNetArg + ", got: $*\" >&2; exit 91;; esac\n"
	if combinedOutput != "" {
		script += "printf '%s\\n' \"" + combinedOutput + "\" >&2\n"
	}
	script += "exit " + itoa(exitCode) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake podman: %v", err)
	}
	return bin
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

// TestAllowlistedNetworkArgMatchesNetworkArgs pins that the preflight probes
// the SAME network flag the turns use. Deriving it from networkArgs is the whole
// point — a hand-restated flag could drift and silently validate a posture
// nobody runs.
func TestAllowlistedNetworkArgMatchesNetworkArgs(t *testing.T) {
	got, err := allowlistedNetworkArg()
	if err != nil {
		t.Fatalf("allowlistedNetworkArg: %v", err)
	}
	var want string
	for _, a := range networkArgs(false, "http://example.invalid") {
		if strings.HasPrefix(a, "--network=") {
			want = a
		}
	}
	if want == "" {
		t.Fatal("networkArgs emitted no --network flag for allowlisted mode")
	}
	if got != want {
		t.Errorf("preflight probes %q but turns use %q — the preflight would validate a posture nobody runs", got, want)
	}
}

func TestPreflightAllowlistedNetwork_OK(t *testing.T) {
	netArg, err := allowlistedNetworkArg()
	if err != nil {
		t.Fatalf("allowlistedNetworkArg: %v", err)
	}
	podman := fakePodmanRun(t, netArg, "", 0)
	if err := PreflightAllowlistedNetwork(context.Background(), podman, "localhost/img:test"); err != nil {
		t.Errorf("PreflightAllowlistedNetwork on a healthy host = %v, want nil", err)
	}
}

// TestPreflightAllowlistedNetwork_MissingSlirp4netns is the case this preflight
// exists for: a stock Podman >= 5.0 host defaults to pasta and often ships no
// slirp4netns, so every container start fails. The error must name the helper
// and the fix, not surface a raw podman message.
func TestPreflightAllowlistedNetwork_MissingSlirp4netns(t *testing.T) {
	netArg, err := allowlistedNetworkArg()
	if err != nil {
		t.Fatalf("allowlistedNetworkArg: %v", err)
	}
	const podmanErr = "Error: could not find slirp4netns, the network namespace cant be configured: exec: slirp4netns: executable file not found in PATH"
	podman := fakePodmanRun(t, netArg, podmanErr, 125)

	err = PreflightAllowlistedNetwork(context.Background(), podman, "localhost/img:test")
	if err == nil {
		t.Fatal("PreflightAllowlistedNetwork on a host without slirp4netns = nil, want a fail-closed error")
	}
	msg := err.Error()
	for _, want := range []string{"slirp4netns", "pasta", "lockdown"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error must mention %q so the operator can act on it; got: %v", want, err)
		}
	}
}

// TestPreflightAllowlistedNetwork_OtherFailureFailsClosed: any other reason a
// container cannot start with the allowlisted network is still fail-closed, and
// still names the flag that was tried.
func TestPreflightAllowlistedNetwork_OtherFailureFailsClosed(t *testing.T) {
	netArg, err := allowlistedNetworkArg()
	if err != nil {
		t.Fatalf("allowlistedNetworkArg: %v", err)
	}
	podman := fakePodmanRun(t, netArg, "Error: something else entirely went wrong", 125)
	err = PreflightAllowlistedNetwork(context.Background(), podman, "localhost/img:test")
	if err == nil {
		t.Fatal("a failing probe = nil, want fail-closed")
	}
	if !strings.Contains(err.Error(), netArg) {
		t.Errorf("error should name the network flag that was tried; got: %v", err)
	}
}

// TestPreflightAllowlistedNetwork_NoImageIsNoop: an empty image means the pool
// has no container backend to probe (the test/dev host-executor path), so the
// preflight must not fail boot. A fake podman that would reject any invocation
// proves it is never called.
func TestPreflightAllowlistedNetwork_NoImageIsNoop(t *testing.T) {
	podman := fakePodmanRun(t, "--network=never-matches", "", 99)
	for _, image := range []string{"", "   "} {
		if err := PreflightAllowlistedNetwork(context.Background(), podman, image); err != nil {
			t.Errorf("PreflightAllowlistedNetwork(image=%q) = %v, want nil (nothing to probe)", image, err)
		}
	}
}
