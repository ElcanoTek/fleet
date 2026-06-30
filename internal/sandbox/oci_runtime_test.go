// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package sandbox

import (
	"context"
	"math"
	"os/exec"
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

func TestPreflightRuntimeNoopForSharedKernel(t *testing.T) {
	ctx := context.Background()
	for _, rt := range []string{"", "  ", "runc", "crun", "runsc"} {
		if err := PreflightRuntime(ctx, rt); err != nil {
			t.Errorf("PreflightRuntime(%q) = %v, want nil (shared-kernel runtimes need no preflight)", rt, err)
		}
	}
}

// TestPreflightRuntimeKataFailsClosedWithoutBinary asserts the no-degrade
// invariant: when the manifest asks for kata but kata-runtime is absent, the
// preflight returns an error rather than letting boot proceed (and silently
// fall back to a shared-kernel container). Guarded so a host that genuinely has
// kata-runtime installed doesn't fail the assertion.
func TestPreflightRuntimeKataFailsClosedWithoutBinary(t *testing.T) {
	if _, err := exec.LookPath("kata-runtime"); err == nil {
		t.Skip("kata-runtime present on this host; absence path not exercised")
	}
	if err := PreflightRuntime(context.Background(), "kata"); err == nil {
		t.Error("PreflightRuntime(kata) without kata-runtime = nil, want fail-closed error")
	}
}

// TestPreflightRuntimeKrunFailsClosedWithoutKVMorBinary asserts krun fails
// closed when KVM or the krun binary is unavailable.
func TestPreflightRuntimeKrunFailsClosedWithoutKVMorBinary(t *testing.T) {
	_, krunErr := exec.LookPath("krun")
	kvmErr := kvmAccessible()
	if krunErr == nil && kvmErr == nil {
		t.Skip("krun + /dev/kvm both present; failure path not exercised")
	}
	if err := PreflightRuntime(context.Background(), "libkrun"); err == nil {
		t.Error("PreflightRuntime(libkrun) without krun/KVM = nil, want fail-closed error")
	}
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
