// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package sandbox

// Tests for the allowlisted-egress boot preflight (#211 / ADR-0012). A fake
// podman keeps them deterministic on any host: the failure this preflight
// exists to catch (no slirp4netns helper) cannot be reproduced on a box that
// HAS the helper, and a box that lacks it cannot run the success case.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakePodmanInfoHelper writes a stub podman that ASSERTS it was asked
// `info --format {{.Host.Slirp4NetNS.Executable}}` before replying with
// helperPath on stdout (empty = helper absent) and exitCode.
//
// The argv assertion is load-bearing: a preflight that queried some OTHER field,
// or that shelled out to something other than `info`, would validate nothing —
// and without this the whole file would still be green.
//
// stderrBytes is written verbatim via a heredoc so a test can carry podman's
// REAL error output, quotes and all, rather than a doctored copy.
func fakePodmanInfoHelper(t *testing.T, helperPath, stderrBytes string, exitCode int) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fake-podman")
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("case \" $* \" in *\" info \"*) ;; *) echo \"fake-podman: expected an info command, got: $*\" >&2; exit 90;; esac\n")
	b.WriteString("case \"$*\" in *Host.Slirp4NetNS.Executable*) ;; *) echo \"fake-podman: expected the slirp4netns executable template, got: $*\" >&2; exit 91;; esac\n")
	if stderrBytes != "" {
		b.WriteString("cat >&2 <<'FAKE_PODMAN_STDERR'\n" + stderrBytes + "\nFAKE_PODMAN_STDERR\n")
	}
	if helperPath != "" {
		b.WriteString("printf '%s\\n' '" + helperPath + "'\n")
	}
	b.WriteString("exit " + strconv.Itoa(exitCode) + "\n")
	if err := os.WriteFile(bin, []byte(b.String()), 0o700); err != nil {
		t.Fatalf("write fake podman: %v", err)
	}
	return bin
}

// TestAllowlistedNetworkHelperMatchesNetworkArgs pins that the preflight checks
// the helper the turns actually request. Deriving it from networkArgs is the
// whole point — a hand-restated name could drift and silently verify a helper
// nobody uses.
func TestAllowlistedNetworkHelperMatchesNetworkArgs(t *testing.T) {
	got, err := allowlistedNetworkHelper()
	if err != nil {
		t.Fatalf("allowlistedNetworkHelper: %v", err)
	}
	var flag string
	for _, a := range networkArgs(false, "http://example.invalid") {
		if strings.HasPrefix(a, "--network=") {
			flag = a
		}
	}
	if flag == "" {
		t.Fatal("networkArgs emitted no --network flag for allowlisted mode")
	}
	if !strings.HasPrefix(strings.TrimPrefix(flag, "--network="), got) {
		t.Errorf("preflight checks helper %q but turns request %q", got, flag)
	}
	// Guard the lockdown/open branches are not what we sampled.
	if got == "none" || got == "" {
		t.Errorf("helper = %q — the preflight sampled the wrong networkArgs branch", got)
	}
}

func TestPreflightAllowlistedNetwork_HelperPresent(t *testing.T) {
	podman := fakePodmanInfoHelper(t, "/usr/bin/slirp4netns", "", 0)
	if err := PreflightAllowlistedNetwork(context.Background(), podman); err != nil {
		t.Errorf("PreflightAllowlistedNetwork with the helper present = %v, want nil", err)
	}
}

// TestPreflightAllowlistedNetwork_HelperMissing is the case this preflight
// exists for: a stock Podman >= 5.0 host defaults to pasta and often ships no
// slirp4netns, so podman reports an empty executable and every container start
// would fail. The error must name the helper and the remedy.
func TestPreflightAllowlistedNetwork_HelperMissing(t *testing.T) {
	podman := fakePodmanInfoHelper(t, "", "", 0)
	err := PreflightAllowlistedNetwork(context.Background(), podman)
	if err == nil {
		t.Fatal("PreflightAllowlistedNetwork with no helper = nil, want a fail-closed error")
	}
	for _, want := range []string{"slirp4netns", "pasta", "lockdown", "dnf install", "apt install"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must mention %q so the operator can act on it; got: %v", want, err)
		}
	}
}

// TestPreflightAllowlistedNetwork_WhitespaceOnlyIsMissing: podman printing only
// a newline must not be read as "helper present".
func TestPreflightAllowlistedNetwork_WhitespaceOnlyIsMissing(t *testing.T) {
	podman := fakePodmanInfoHelper(t, "   ", "", 0)
	if err := PreflightAllowlistedNetwork(context.Background(), podman); err == nil {
		t.Fatal("a whitespace-only helper path = nil, want fail-closed")
	}
}

// TestPreflightAllowlistedNetwork_PodmanFailureFailsClosed: if podman itself
// cannot answer, the posture is unverifiable and boot must not proceed. Carries
// podman's real stderr bytes (double quotes included) through a heredoc.
func TestPreflightAllowlistedNetwork_PodmanFailureFailsClosed(t *testing.T) {
	const realErr = `Error: cannot re-exec process to join the existing user namespace`
	podman := fakePodmanInfoHelper(t, "", realErr, 125)
	err := PreflightAllowlistedNetwork(context.Background(), podman)
	if err == nil {
		t.Fatal("a failing podman info = nil, want fail-closed")
	}
	if !strings.Contains(err.Error(), "could not ask podman") {
		t.Errorf("error should say podman could not be consulted; got: %v", err)
	}
	if !strings.Contains(err.Error(), "user namespace") {
		t.Errorf("error should carry podman's own stderr so the real cause is visible; got: %v", err)
	}
}

// TestPreflightAllowlistedNetwork_DefaultsPodmanBinary covers the branch EVERY
// production boot takes: buildSandboxPool passes
// poolCfg.Container.PodmanBinary, which is structurally "" there (the field is
// filled only by applyContainerDefaults, inside NewContainer). Dropping the
// ""→"podman" fallback would break every allowlisted boot while leaving the
// rest of this file green.
func TestPreflightAllowlistedNetwork_DefaultsPodmanBinary(t *testing.T) {
	dir := t.TempDir()
	stub := fakePodmanInfoHelper(t, "/usr/bin/slirp4netns", "", 0)
	data, err := os.ReadFile(stub)
	if err != nil {
		t.Fatalf("read stub: %v", err)
	}
	onPath := filepath.Join(dir, "podman")
	if err := os.WriteFile(onPath, data, 0o700); err != nil {
		t.Fatalf("write PATH podman: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := PreflightAllowlistedNetwork(context.Background(), ""); err != nil {
		t.Errorf("PreflightAllowlistedNetwork with an empty podman binary = %v, want the PATH default to be used", err)
	}
}

// TestPreflightNetworkHelper_UnknownHelperFailsClosed covers the drift guard: if
// networkArgs is ever changed to request a helper this preflight cannot verify
// (pasta, say — the deferred work in ADR-0012), the preflight must REFUSE rather
// than silently report success against the wrong helper. A fake podman that
// rejects every invocation proves it never even asks.
//
// COVERAGE NOTE, stated plainly because a reviewer will look for it: replacing
// PreflightAllowlistedNetwork's `allowlistedNetworkHelper()` call with a
// hardcoded "slirp4netns" is an EQUIVALENT mutation today and no unit test can
// kill it, because that is exactly the value networkArgs currently yields. What
// actually protects against drift is this guard plus the fact that changing
// networkArgs' signature breaks compilation at the derivation site. The
// derivation is still the right construction — it makes the hardcode
// unnecessary rather than merely redundant.
func TestPreflightNetworkHelper_UnknownHelperFailsClosed(t *testing.T) {
	podman := fakePodmanInfoHelper(t, "/usr/bin/anything", "", 0)
	for _, helper := range []string{"pasta", "", "bridge"} {
		err := preflightNetworkHelper(context.Background(), podman, helper)
		if err == nil {
			t.Fatalf("preflightNetworkHelper(%q) = nil, want a fail-closed error", helper)
		}
		if !strings.Contains(err.Error(), "does not know how to verify") {
			t.Errorf("helper %q: err = %v, want the unknown-helper refusal", helper, err)
		}
	}
}

// TestNetworkHelperInfoTemplateCoversWhatNetworkArgsRequests ties the lookup
// table to the live posture: whatever helper networkArgs requests for
// allowlisted mode must have an entry, or every allowlisted boot fails closed on
// the guard above. This is the test that would go red if someone changed
// networkArgs without extending the table.
func TestNetworkHelperInfoTemplateCoversWhatNetworkArgsRequests(t *testing.T) {
	helper, err := allowlistedNetworkHelper()
	if err != nil {
		t.Fatalf("allowlistedNetworkHelper: %v", err)
	}
	if _, ok := networkHelperInfoTemplate[helper]; !ok {
		t.Fatalf("networkArgs requests the %q helper but networkHelperInfoTemplate has no entry for it — every allowlisted boot would fail closed", helper)
	}
}
