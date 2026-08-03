// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package sandbox

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNormalizeRuntime(t *testing.T) {
	cases := []struct {
		in          string
		want        string
		wantChanged bool
	}{
		{"", "", false},
		{"  ", "", false},
		{"runc", "runc", false},
		{"crun", "crun", false},
		{"kata", "kata", false},
		{"runsc", "runsc", false},
		{"krun", "krun", false},
		// "libkrun" is the product name; podman's runtime name is "krun".
		{"libkrun", "krun", true},
		{"LIBKRUN", "krun", true},
		// Bare names are lower-cased so the flag/preflight/binary all agree.
		{"Kata", "kata", true},
		{" kata ", "kata", false},
		// An explicit path is passed through verbatim — never rewritten.
		{"/usr/bin/kata-runtime", "/usr/bin/kata-runtime", false},
		{"/opt/libkrun/bin/krun", "/opt/libkrun/bin/krun", false},
	}
	for _, c := range cases {
		got, changed := NormalizeRuntime(c.in)
		if got != c.want || changed != c.wantChanged {
			t.Errorf("NormalizeRuntime(%q) = (%q, %v), want (%q, %v)", c.in, got, changed, c.want, c.wantChanged)
		}
	}
}

func TestRuntimeKind(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"runc", ""},
		{"crun", ""},
		{"runsc", ""},
		{"kata", "kata"},
		{"Kata", "kata"},
		{"krun", "krun"},
		{"libkrun", "krun"}, // normalized to krun first
		// Path forms classify by basename so the preflight + overhead still fire.
		{"/usr/bin/kata-runtime", "kata"},
		{"/opt/kata/bin/kata-qemu", "kata"},
		{"/usr/local/bin/krun", "krun"},
		{"/opt/libkrun/bin/libkrun", "krun"},
		{"/usr/bin/runc", ""},
	}
	for _, c := range cases {
		if got := runtimeKind(c.in); got != c.want {
			t.Errorf("runtimeKind(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestResolveRuntime(t *testing.T) {
	cases := []struct {
		env, bundle, want string
	}{
		{"", "", ""},
		{"kata", "", "kata"},
		{"", "kata", "kata"},     // bundle fills when env empty
		{"runc", "kata", "runc"}, // env wins over bundle
		{"libkrun", "", "krun"},  // normalized
		{"", "libkrun", "krun"},  // bundle value normalized too
		{"  kata  ", "", "kata"}, // trimmed
		{"", "  ", ""},           // whitespace bundle → empty
	}
	for _, c := range cases {
		if got := ResolveRuntime(c.env, c.bundle); got != c.want {
			t.Errorf("ResolveRuntime(%q,%q) = %q, want %q", c.env, c.bundle, got, c.want)
		}
	}
}

func TestRuntimeBinary(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"runc", "runc"},
		{"crun", "crun"},
		{"runsc", "runsc"},
		{"kata", "kata-runtime"},
		{"Kata", "kata-runtime"},
		{"krun", "krun"},
		{"libkrun", "krun"}, // normalized first, then mapped
		{"/usr/local/bin/kata-runtime", "/usr/local/bin/kata-runtime"},
	}
	for _, c := range cases {
		if got := RuntimeBinary(c.in); got != c.want {
			t.Errorf("RuntimeBinary(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseMemoryToBytes(t *testing.T) {
	const mib = int64(1) << 20
	ok := []struct {
		in   string
		want int64
	}{
		{"512m", 512 * mib},
		{"2g", 2 * 1024 * mib},
		{"2048m", 2048 * mib},
		{"1k", 1024},
		{"1b", 1},
		// A BARE number is BYTES (podman convention) — 512 MiB, not 512 MiB*MiB.
		{"536870912", 512 * mib},
		{"1024", 1024},
		// Suffix case-insensitivity.
		{"2G", 2 * 1024 * mib},
		{"  512M  ", 512 * mib},
	}
	for _, c := range ok {
		got, err := parseMemoryToBytes(c.in)
		if err != nil {
			t.Errorf("parseMemoryToBytes(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseMemoryToBytes(%q) = %d, want %d", c.in, got, c.want)
		}
	}
	bad := []string{"", "   ", "garbage", "1.5g", "-5m", "0", "0m", "12x", "g", "m"}
	for _, in := range bad {
		if _, err := parseMemoryToBytes(in); err == nil {
			t.Errorf("parseMemoryToBytes(%q) = nil error, want an error (fail-closed)", in)
		}
	}
}

func TestAddKataMemoryOverhead(t *testing.T) {
	cases := []struct {
		limit    string
		overhead int
		want     string
	}{
		{"512m", 512, "1024m"},
		{"2g", 512, "2560m"},
		{"2048m", 512, "2560m"},
		{"536870912", 512, "1024m"}, // bare bytes (512 MiB) + 512
		{"512m", 256, "768m"},
		// Sub-MiB base ceils UP to 1 MiB so we never under-provision.
		{"1024", 512, "513m"},
	}
	for _, c := range cases {
		got, err := addKataMemoryOverhead(c.limit, c.overhead)
		if err != nil {
			t.Errorf("addKataMemoryOverhead(%q, %d) error: %v", c.limit, c.overhead, err)
			continue
		}
		if got != c.want {
			t.Errorf("addKataMemoryOverhead(%q, %d) = %q, want %q", c.limit, c.overhead, got, c.want)
		}
	}
	// Unparseable base fails closed.
	if _, err := addKataMemoryOverhead("garbage", 512); err == nil {
		t.Error("addKataMemoryOverhead(garbage) = nil error, want fail-closed error")
	}
	// Overflow fails closed instead of emitting a negative --memory.
	if got, err := addKataMemoryOverhead("9223372036854775807", math.MaxInt64); err == nil {
		t.Errorf("addKataMemoryOverhead(huge, MaxInt64) = %q, want overflow error", got)
	}
	// A near-MaxInt64 bare-byte base must not wrap negative in the ceil step.
	if got, err := addKataMemoryOverhead("9223372036854775807", 512); err != nil {
		t.Errorf("addKataMemoryOverhead(near-max bytes, 512) errored: %v", err)
	} else if strings.HasPrefix(got, "-") {
		t.Errorf("addKataMemoryOverhead(near-max bytes) = %q, produced a negative limit (ceil overflow)", got)
	}
}

func TestApplyKataMemoryOverhead(t *testing.T) {
	// Non-kata runtimes are untouched.
	for _, rt := range []string{"", "runc", "crun", "runsc", "krun"} {
		cfg := ContainerConfig{Runtime: rt, MemoryLimit: "512m"}
		if err := applyKataMemoryOverhead(&cfg); err != nil {
			t.Fatalf("applyKataMemoryOverhead(runtime=%q) error: %v", rt, err)
		}
		if cfg.MemoryLimit != "512m" {
			t.Errorf("runtime=%q bumped memory to %q, want unchanged 512m", rt, cfg.MemoryLimit)
		}
	}

	// Kata bumps the limit by the default overhead.
	t.Setenv("FLEET_SANDBOX_KATA_OVERHEAD_MB", "")
	cfg := ContainerConfig{Runtime: "kata", MemoryLimit: "512m"}
	if err := applyKataMemoryOverhead(&cfg); err != nil {
		t.Fatalf("applyKataMemoryOverhead(kata): %v", err)
	}
	if cfg.MemoryLimit != "1024m" {
		t.Errorf("kata default overhead: memory = %q, want 1024m", cfg.MemoryLimit)
	}

	// The env override is honored.
	t.Setenv("FLEET_SANDBOX_KATA_OVERHEAD_MB", "256")
	cfg = ContainerConfig{Runtime: "kata", MemoryLimit: "2g"}
	if err := applyKataMemoryOverhead(&cfg); err != nil {
		t.Fatalf("applyKataMemoryOverhead(kata, override): %v", err)
	}
	if cfg.MemoryLimit != "2304m" { // 2048 + 256
		t.Errorf("kata env overhead: memory = %q, want 2304m", cfg.MemoryLimit)
	}

	// A PATH-form kata runtime is classified by basename, so it still gets the
	// overhead (the bypass the review flagged).
	t.Setenv("FLEET_SANDBOX_KATA_OVERHEAD_MB", "")
	cfg = ContainerConfig{Runtime: "/usr/bin/kata-runtime", MemoryLimit: "512m"}
	if err := applyKataMemoryOverhead(&cfg); err != nil {
		t.Fatalf("applyKataMemoryOverhead(path-form kata): %v", err)
	}
	if cfg.MemoryLimit != "1024m" {
		t.Errorf("path-form kata overhead: memory = %q, want 1024m", cfg.MemoryLimit)
	}

	// An unparseable limit fails closed for kata (not silently passed through).
	cfg = ContainerConfig{Runtime: "kata", MemoryLimit: "garbage"}
	if err := applyKataMemoryOverhead(&cfg); err == nil {
		t.Error("applyKataMemoryOverhead(kata, garbage) = nil, want fail-closed error")
	}
}

func TestKataOverheadMB(t *testing.T) {
	t.Setenv("FLEET_SANDBOX_KATA_OVERHEAD_MB", "")
	if got := kataOverheadMB(); got != DefaultKataOverheadMB {
		t.Errorf("kataOverheadMB() default = %d, want %d", got, DefaultKataOverheadMB)
	}
	t.Setenv("FLEET_SANDBOX_KATA_OVERHEAD_MB", "768")
	if got := kataOverheadMB(); got != 768 {
		t.Errorf("kataOverheadMB() = %d, want 768", got)
	}
	// Invalid / non-positive values fall back to the default (lenient knob).
	for _, v := range []string{"abc", "-1", "0"} {
		t.Setenv("FLEET_SANDBOX_KATA_OVERHEAD_MB", v)
		if got := kataOverheadMB(); got != DefaultKataOverheadMB {
			t.Errorf("kataOverheadMB() with %q = %d, want default %d", v, got, DefaultKataOverheadMB)
		}
	}
}

// ── preflight: podman-authoritative runtime resolution ──
//
// PreflightRuntime asks podman which binary it will exec for --runtime=<name>
// instead of guessing from the name, so these tests substitute a fake podman.
// That makes the fail-closed paths deterministic on any host — previously the
// kata/krun failure cases could only be observed by NOT having those runtimes
// installed, and the +LIBKRUN hard-fail had no coverage at all.

// fakePodmanResolving writes a stub podman that ASSERTS its argv before
// answering. The assertion is the point: an argv-blind stub cannot tell
// whether the preflight passes `--runtime=<name>` at all, so deleting that
// flag — the entire mechanism this file exists to pin — would leave every test
// green. wantRuntime is the normalized name the preflight must ask podman
// about; the stub exits 2 if it is absent or if the format template is not the
// runtime path.
//
// resolvePath is what the stub reports: a path (exit 0), "" for podman's real
// "not found" failure (exit 1), or the sentinel "<empty>" for the
// exit-0-with-no-output case, which must also fail closed.
func fakePodmanResolving(t *testing.T, wantRuntime, resolvePath string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fake-podman")
	var reply string
	switch resolvePath {
	case "":
		reply = "echo 'Error: default OCI runtime not found: invalid argument' >&2\nexit 1\n"
	case "<empty>":
		reply = "exit 0\n"
	default:
		// A stderr line alongside the exit-0 answer: real podman emits warnings
		// this way (e.g. "User-selected graph driver ... overwritten") while
		// still succeeding. resolveRuntimePath must read STDOUT ONLY — with
		// CombinedOutput the warning would be prepended to the path and every
		// named-runtime boot on such a host would false-abort.
		reply = "echo 'time=\"...\" level=warning msg=\"a benign podman warning\"' >&2\n" +
			"printf '%s\\n' \"" + resolvePath + "\"\nexit 0\n"
	}
	script := "#!/bin/sh\n" +
		"case \" $* \" in *\" --runtime=" + wantRuntime + " \"*) ;; *) echo \"fake-podman: expected --runtime=" + wantRuntime + ", got: $*\" >&2; exit 2;; esac\n" +
		"case \"$*\" in *OCIRuntime.Path*) ;; *) echo \"fake-podman: expected the runtime-path format template, got: $*\" >&2; exit 2;; esac\n" +
		reply
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake podman: %v", err)
	}
	return bin
}

// fakeRuntimeBinary writes a stub OCI runtime whose --version banner is
// versionBanner. Used to exercise the +LIBKRUN check without a real crun.
func fakeRuntimeBinary(t *testing.T, name, versionBanner string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), name)
	script := "#!/bin/sh\nprintf '%s\\n' \"" + versionBanner + "\"\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake runtime: %v", err)
	}
	return bin
}

// TestPreflightRuntimeNoopForPodmanDefault: only the EMPTY runtime skips the
// preflight entirely. A named shared-kernel runtime is no longer a no-op — it
// must at least be resolvable by podman, which catches the runtime that is
// installed but never registered in containers.conf (previously it passed a
// PATH lookup, then failed at every container creation).
func TestPreflightRuntimeNoopForPodmanDefault(t *testing.T) {
	// An unresolvable fake podman proves the empty cases never call it.
	podman := fakePodmanResolving(t, "never-called", "")
	for _, rt := range []string{"", "  "} {
		if err := PreflightRuntime(context.Background(), podman, rt); err != nil {
			t.Errorf("PreflightRuntime(%q) = %v, want nil (podman default needs no preflight)", rt, err)
		}
	}
}

func TestPreflightRuntimeSharedKernelRequiresResolution(t *testing.T) {
	ctx := context.Background()
	for _, rt := range []string{"runc", "crun", "runsc"} {
		t.Run(rt+" resolvable", func(t *testing.T) {
			podman := fakePodmanResolving(t, rt, "/usr/bin/"+rt)
			if err := PreflightRuntime(ctx, podman, rt); err != nil {
				t.Errorf("PreflightRuntime(%q) with a resolvable runtime = %v, want nil", rt, err)
			}
		})
		t.Run(rt+" unresolvable fails closed", func(t *testing.T) {
			podman := fakePodmanResolving(t, rt, "")
			err := PreflightRuntime(ctx, podman, rt)
			if err == nil {
				t.Fatalf("PreflightRuntime(%q) with an unregistered runtime = nil, want a fail-closed error", rt)
			}
			if !strings.Contains(err.Error(), "could not resolve") {
				t.Errorf("err = %v, want it to name the resolution failure", err)
			}
		})
	}
}

// TestPreflightRuntimeProbesTheResolvedBinary is the core regression guard: a
// containers.conf remap must make the preflight probe the RESOLVED binary, not
// whatever same-named binary happens to be first on PATH.
//
// The scenario is synthetic — real podman execs `<bin> --version` during
// `info`, so it would not return success for a nonexistent path — but that is
// exactly what makes it a clean discriminator: the error can only name this
// path if the preflight used podman's answer instead of its own guess.
func TestPreflightRuntimeProbesTheResolvedBinary(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "definitely-not-installed")
	podman := fakePodmanResolving(t, "krun", missing)
	err := PreflightRuntime(context.Background(), podman, "krun")
	if err == nil {
		t.Fatal("PreflightRuntime(krun) = nil when podman resolves it to a missing binary, want fail-closed")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("err = %v, want it to name the RESOLVED binary %q — probing the PATH guess instead is the bug this guards", err, missing)
	}
}

// TestPreflightRuntimeEmptyResolutionFailsClosed: podman exiting 0 but naming
// no binary must NOT be read as success. Without this, dropping the empty-path
// guard in resolveRuntimePath ships green and the preflight goes on to probe
// "".
func TestPreflightRuntimeEmptyResolutionFailsClosed(t *testing.T) {
	podman := fakePodmanResolving(t, "crun", "<empty>")
	err := PreflightRuntime(context.Background(), podman, "crun")
	if err == nil {
		t.Fatal("PreflightRuntime with an empty resolved path = nil, want fail-closed")
	}
	if !strings.Contains(err.Error(), "empty binary path") {
		t.Errorf("err = %v, want it to name the empty resolution", err)
	}
}

// TestPreflightRuntimeAsksPodmanForTheNormalizedName: the operator-facing name
// is "libkrun" but podman's registered runtime is "krun", so the preflight must
// ask podman about the NORMALIZED name. Passing the raw value would make
// podman reject a perfectly good libkrun host. The fake podman asserts the argv,
// so this fails if normalization is dropped.
func TestPreflightRuntimeAsksPodmanForTheNormalizedName(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent-krun")
	podman := fakePodmanResolving(t, "krun", missing)
	err := PreflightRuntime(context.Background(), podman, "libkrun")
	if err == nil {
		t.Fatal("PreflightRuntime(libkrun) = nil, want the missing-binary failure")
	}
	// A wrong-argv stub exits 2 with "expected --runtime=krun"; reaching the
	// binary probe instead proves the normalized name was used.
	if strings.Contains(err.Error(), "expected --runtime=") {
		t.Fatalf("preflight asked podman for the RAW name, not the normalized one: %v", err)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("err = %v, want the resolved binary %q named", err, missing)
	}
}

// TestPreflightRuntimeKataFailsClosedWithoutKVM: kata needs usable KVM. On a
// host that has it, the binary check still has to pass first, so assert the
// error names one of the two gates rather than skipping.
func TestPreflightRuntimeKataFailsClosedWithoutKVM(t *testing.T) {
	if kvmAccessible() == nil {
		t.Skip("/dev/kvm is usable on this host; the KVM failure path cannot be exercised")
	}
	kata := fakeRuntimeBinary(t, "kata-runtime", "kata-runtime : 3.2.0")
	podman := fakePodmanResolving(t, "kata", kata)
	err := PreflightRuntime(context.Background(), podman, "kata")
	if err == nil {
		t.Fatal("PreflightRuntime(kata) without KVM = nil, want fail-closed")
	}
	if !strings.Contains(err.Error(), "/dev/kvm") {
		t.Errorf("err = %v, want it to name /dev/kvm as the gate", err)
	}
}

// TestPreflightRuntimeKrunFailsClosedWithoutKVM is the krun twin of the kata
// KVM test. Both other krun cases resolve to a MISSING binary and so fail at
// the binary lookup before ever reaching the KVM gate — leaving the gate the
// ADR names untested for krun. Here the resolved binary exists and reports
// +LIBKRUN, so KVM is the only thing left to fail on.
func TestPreflightRuntimeKrunFailsClosedWithoutKVM(t *testing.T) {
	if kvmAccessible() == nil {
		t.Skip("/dev/kvm is usable on this host; the KVM failure path cannot be exercised")
	}
	krun := fakeRuntimeBinary(t, "krun", "crun version 1.14 +LIBKRUN")
	podman := fakePodmanResolving(t, "krun", krun)
	err := PreflightRuntime(context.Background(), podman, "krun")
	if err == nil {
		t.Fatal("PreflightRuntime(krun) without KVM = nil, want fail-closed")
	}
	if !strings.Contains(err.Error(), "/dev/kvm") {
		t.Errorf("err = %v, want it to name /dev/kvm as the gate", err)
	}
}

// TestResolveRuntimePathDefaultsPodmanBinary covers the branch EVERY production
// boot takes: agent.buildSandboxPool passes poolCfg.Container.PodmanBinary,
// which is structurally "" there (the field is only filled in by
// applyContainerDefaults, inside NewContainer). Dropping the ""→"podman"
// fallback would break every named-runtime boot while leaving the rest of this
// file green, because the other tests all pass an explicit stub path.
func TestResolveRuntimePathDefaultsPodmanBinary(t *testing.T) {
	dir := t.TempDir()
	// A "podman" on PATH that answers like the real one.
	stub := filepath.Join(dir, "podman")
	script := "#!/bin/sh\ncase \" $* \" in *\" --runtime=crun \"*) ;; *) exit 2;; esac\nprintf '%s\\n' /usr/bin/crun\nexit 0\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatalf("write stub podman: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := resolveRuntimePath(context.Background(), "", "crun")
	if err != nil {
		t.Fatalf("resolveRuntimePath with an empty podman binary = %v, want the PATH default to be used", err)
	}
	if got != "/usr/bin/crun" {
		t.Errorf("resolved path = %q, want /usr/bin/crun", got)
	}
}

// TestVerifyKrunLibkrun covers the check that a crun renamed to `krun` cannot
// masquerade as libkrun — the SILENT loss of VM isolation ADR-0010 forbids.
// Tested directly rather than through PreflightRuntime because the KVM gate
// runs first and would mask it on every CI host.
func TestVerifyKrunLibkrun(t *testing.T) {
	ctx := context.Background()
	t.Run("plain crun is rejected", func(t *testing.T) {
		bin := fakeRuntimeBinary(t, "krun", "crun version 1.14\ncommit: abc\nspec: 1.0.0\n+SYSTEMD +SELINUX +CAP +SECCOMP")
		err := verifyKrunLibkrun(ctx, bin)
		if err == nil {
			t.Fatal("a crun build WITHOUT +LIBKRUN was accepted — it would run as a shared-kernel container")
		}
		if !strings.Contains(err.Error(), "+LIBKRUN") {
			t.Errorf("err = %v, want it to name the missing +LIBKRUN feature", err)
		}
	})
	t.Run("a real libkrun build is accepted", func(t *testing.T) {
		bin := fakeRuntimeBinary(t, "krun", "crun version 1.14\n+SYSTEMD +SELINUX +CAP +SECCOMP +LIBKRUN +WASM")
		if err := verifyKrunLibkrun(ctx, bin); err != nil {
			t.Errorf("verifyKrunLibkrun with +LIBKRUN = %v, want nil", err)
		}
	})
	t.Run("banner case does not matter", func(t *testing.T) {
		bin := fakeRuntimeBinary(t, "krun", "crun version 1.14 +libkrun")
		if err := verifyKrunLibkrun(ctx, bin); err != nil {
			t.Errorf("verifyKrunLibkrun with a lowercase banner = %v, want nil (the check is case-insensitive)", err)
		}
	})
	t.Run("an unrunnable binary fails closed", func(t *testing.T) {
		if err := verifyKrunLibkrun(ctx, filepath.Join(t.TempDir(), "nope")); err == nil {
			t.Error("verifyKrunLibkrun on a missing binary = nil, want an error")
		}
	})
}

// TestContainerKataRuntime is a hypervisor-gated integration test (acceptance
// criterion for #217): spin up a sandbox under the kata runtime, run a bash
// command, and assert it exits cleanly. Skipped unless the host can actually run
// Kata — linux, podman + kata-runtime on PATH, and /dev/kvm openable.
func TestContainerKataRuntime(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("kata runtime tested on linux only")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available")
	}
	if _, err := exec.LookPath("kata-runtime"); err != nil {
		t.Skip("kata-runtime not available")
	}
	if err := kvmAccessible(); err != nil {
		t.Skipf("/dev/kvm not accessible: %v", err)
	}
	image := testImage()

	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	sb, err := NewContainer(ctx, ContainerConfig{
		Image:            image,
		WorkspaceHostDir: tmp,
		BridgeScript:     []byte("# unused for bash-only test\n"),
		Runtime:          "kata",
		MemoryLimit:      "1024m", // generous so the +512 overhead leaves room to boot
	})
	if err != nil {
		t.Fatalf("NewContainer(kata): %v", err)
	}
	defer sb.Close()

	res, err := sb.RunBash(context.Background(), BashRequest{Command: "echo hello-from-kata"})
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(string(res.Stdout), "hello-from-kata") {
		t.Errorf("Stdout = %q, missing greeting", res.Stdout)
	}
}
