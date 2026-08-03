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
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> " + logPath + "; done\nprintf 'deadbeefcafe\\n'\nexit 0\n"
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

	// Value-bearing flags: assert the flag is present without freezing its
	// value.
	for _, prefix := range []string{"--memory=", "--memory-swap=", "--pids-limit=", "--cpus=", "--workdir="} {
		if !hasArgWithPrefix(args, prefix) {
			t.Errorf("missing flag %s… in podman run argv", prefix)
		}
	}

	// The seccomp profile is passed as a separate value argument
	// ("--security-opt", "seccomp=<path>"), so match the pair loosely.
	if !hasArgWithPrefix(args, "seccomp=") {
		t.Error("missing seccomp profile in podman run argv — the syscall filter layer would be off")
	}
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
