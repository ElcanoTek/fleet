package sandbox

// Pins the security-relevant flags that must reach `podman run`. Everything
// else in this package tests the arg BUILDERS (diskQuotaArgs, networkArgs,
// resolveSeccompArg) as pure functions, or asserts effects from inside a live
// container — so deleting an `args = append(...)` line in start() slips
// through both: the pure tests still pass, and the container tests are
// podman-gated (masked in the Go CI job, and the e2e-live lane runs only a
// fixed --run list). This test closes that gap by substituting a fake podman
// that records its argv, so it needs no podman, no image, and runs everywhere
// in milliseconds.
//
// It deliberately asserts flag PRESENCE, not order or exact composition: the
// point is that a hardening flag cannot silently disappear, not to freeze the
// command line.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakePodman writes a shell stub that appends every argument it receives to a
// log file and exits 0, and returns (binary path, log path). `podman run`
// prints a container id on success; the stub prints one so start()'s parse is
// satisfied.
func fakePodman(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-podman")
	logPath := filepath.Join(dir, "argv.log")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> \"" + logPath + "\"; done\nprintf 'deadbeefcafe\\n'\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake podman: %v", err)
	}
	return bin, logPath
}

func recordedArgs(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// hasArgWithPrefix covers flags whose value we don't want to freeze (paths,
// generated names) but whose presence is the point.
func hasArgWithPrefix(args []string, prefix string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

func TestContainerRunArgs_PinsHardeningAndQuotaFlags(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("podman arg assembly is linux-only")
	}
	bin, logPath := fakePodman(t)
	workspace := t.TempDir()

	// StatsInterval < 0 disables the telemetry poller, which would otherwise
	// invoke the fake binary again and interleave `podman stats` argv into the
	// log. NoNetwork pins the sealed posture so --network=none is assertable.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sb, err := NewContainer(ctx, ContainerConfig{
		Image:               "localhost/does-not-matter:test",
		WorkspaceHostDir:    workspace,
		BridgeScript:        []byte("# unused\n"),
		PodmanBinary:        bin,
		DiskLimitGB:         5,
		StorageOptSupported: true,
		NoNetwork:           true,
		StatsInterval:       -1,
	})
	if err != nil {
		t.Fatalf("NewContainer with a fake podman: %v", err)
	}
	defer sb.Close()

	args := recordedArgs(t, logPath)

	// Disk quota (#216). The per-file cap is what reaches the workspace bind
	// mount; the layer cap is added on top when the driver supports it.
	// Regression guard for the either/or that left the workspace uncapped on
	// quota-capable hosts.
	for _, want := range []string{
		fmt.Sprintf("--ulimit=fsize=%d", int64(5)<<30),
		"--storage-opt=size=5g",
	} {
		if !hasArg(args, want) {
			t.Errorf("missing disk-quota flag %q in podman run argv", want)
		}
	}

	// Container hardening (ADR-0002). Each of these is a documented layer of
	// the sandbox boundary; losing one silently is exactly the failure this
	// test exists to catch.
	for _, want := range []string{
		"--read-only",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		"--network=none",
	} {
		if !hasArg(args, want) {
			t.Errorf("missing hardening flag %q in podman run argv", want)
		}
	}

	// The workspace must be bind-mounted at the SAME absolute path on both
	// sides (ADR-0002) and be the default workdir; the userns mapping is what
	// makes the rootless container's uid 1000 line up with the host owner.
	for _, want := range []string{
		fmt.Sprintf("--volume=%s:%s:rw,z", workspace, workspace),
		fmt.Sprintf("--workdir=%s", workspace),
		"--userns=keep-id:uid=1000,gid=1000",
	} {
		if !hasArg(args, want) {
			t.Errorf("missing mount/identity flag %q in podman run argv", want)
		}
	}

	// Every tmpfs must carry a size=. They are writable surface that neither
	// disk-quota flag reaches, and diskQuotaArgs' reasoning explicitly leans
	// on them being bounded — an unbounded tmpfs is a host-memory fill.
	sawTmpfs := false
	for _, a := range args {
		if !strings.HasPrefix(a, "--tmpfs=") {
			continue
		}
		sawTmpfs = true
		if !strings.Contains(a, "size=") {
			t.Errorf("tmpfs mount %q has no size= — unbounded tmpfs is writable surface outside both disk-quota flags", a)
		}
	}
	if !sawTmpfs {
		t.Error("no --tmpfs mounts in podman run argv — the read-only rootfs needs bounded scratch space")
	}

	// --memory-swap must EQUAL --memory: that equality is the control (it
	// disables the swap escape, where a container over its memory cap simply
	// swaps to disk). Asserting only that the flag exists would pass with
	// --memory-swap=-1, i.e. unlimited swap.
	memory, okMem := argValue(args, "--memory=")
	swap, okSwap := argValue(args, "--memory-swap=")
	switch {
	case !okMem:
		t.Error("missing --memory= in podman run argv")
	case !okSwap:
		t.Error("missing --memory-swap= in podman run argv")
	case memory != swap:
		t.Errorf("--memory-swap=%s != --memory=%s — the swap escape is open", swap, memory)
	}

	for _, prefix := range []string{"--pids-limit=", "--cpus="} {
		if !hasArgWithPrefix(args, prefix) {
			t.Errorf("missing flag %s… in podman run argv", prefix)
		}
	}

	// The seccomp profile is passed as a separate value argument
	// ("--security-opt", "seccomp=<value>"). Matching only the "seccomp="
	// prefix is NOT enough: resolveSeccompArg returns the literal
	// "unconfined" when FLEET_SANDBOX_SECCOMP_PROFILE disables the filter, so
	// a prefix check passes with the syscall filter entirely off. Require a
	// real profile path.
	profile, ok := argValue(args, "seccomp=")
	if !ok {
		t.Fatal("missing seccomp profile in podman run argv — the syscall filter layer would be off")
	}
	if profile == "unconfined" || profile == "none" {
		t.Errorf("seccomp=%s — the syscall filter is DISABLED; this test must pin a real profile", profile)
	}
	if !strings.HasSuffix(profile, ".json") {
		t.Errorf("seccomp=%q, want a path to the materialized profile", profile)
	}
}

// argValue returns the text after prefix for the first matching argument.
func argValue(args []string, prefix string) (string, bool) {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return strings.TrimPrefix(a, prefix), true
		}
	}
	return "", false
}

// TestContainerRunArgs_QuotaOmitsLayerCapWithoutDriverSupport is the companion
// case: without storage-opt support the per-file cap must STILL be emitted (it
// is the one that bounds the workspace), and the layer cap must be absent
// (passing it would make `podman run` itself fail on such a driver).
func TestContainerRunArgs_QuotaOmitsLayerCapWithoutDriverSupport(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("podman arg assembly is linux-only")
	}
	bin, logPath := fakePodman(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sb, err := NewContainer(ctx, ContainerConfig{
		Image:               "localhost/does-not-matter:test",
		WorkspaceHostDir:    t.TempDir(),
		BridgeScript:        []byte("# unused\n"),
		PodmanBinary:        bin,
		DiskLimitGB:         5,
		StorageOptSupported: false,
		StatsInterval:       -1,
	})
	if err != nil {
		t.Fatalf("NewContainer with a fake podman: %v", err)
	}
	defer sb.Close()

	args := recordedArgs(t, logPath)
	if !hasArg(args, fmt.Sprintf("--ulimit=fsize=%d", int64(5)<<30)) {
		t.Error("the per-file quota must be emitted regardless of storage-driver support — it is the only cap on the workspace bind mount")
	}
	if hasArgWithPrefix(args, "--storage-opt=") {
		t.Error("the writable-layer quota must be omitted without driver support — podman run would fail outright")
	}
}
