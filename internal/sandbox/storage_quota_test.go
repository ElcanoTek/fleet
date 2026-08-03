package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDiskQuotaArgs(t *testing.T) {
	cases := []struct {
		name      string
		gb        int
		storeOpt  bool
		wantFlags []string
	}{
		// The per-file ulimit is ALWAYS emitted: it is the only mechanism that
		// reaches the bind-mounted workspace, which --storage-opt (a
		// writable-LAYER quota) cannot see. storage-opt is added on top when
		// the driver supports it.
		{"both when storage-opt is supported", 5, true, []string{"--ulimit=fsize=5368709120", "--storage-opt=size=5g"}},
		{"ulimit only when storage-opt is unsupported", 5, false, []string{"--ulimit=fsize=5368709120"}}, // 5 * 1<<30
		{"ulimit only 1g", 1, false, []string{"--ulimit=fsize=1073741824"}},
		{"disabled at zero", 0, true, nil},
		{"disabled when negative", -1, true, nil},
		{"disabled when negative (ulimit path)", -1, false, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := diskQuotaArgs(c.gb, c.storeOpt)
			if fmt.Sprint(got) != fmt.Sprint(c.wantFlags) {
				t.Errorf("diskQuotaArgs(%d, %v) = %v, want %v", c.gb, c.storeOpt, got, c.wantFlags)
			}
		})
	}
}

func TestEffectiveDiskGB(t *testing.T) {
	cases := map[int]int{0: defaultDiskLimitGB, 5: 5, 10: 10, -1: -1}
	for in, want := range cases {
		if got := effectiveDiskGB(in); got != want {
			t.Errorf("effectiveDiskGB(%d) = %d, want %d", in, got, want)
		}
	}
}

// TestApplyContainerDefaults_DiskLimit pins that an unset (0) DiskLimitGB picks up
// the default, while an explicit negative (disabled) is preserved.
func TestApplyContainerDefaults_DiskLimit(t *testing.T) {
	if got := applyContainerDefaults(ContainerConfig{}).DiskLimitGB; got != defaultDiskLimitGB {
		t.Errorf("default DiskLimitGB = %d, want %d", got, defaultDiskLimitGB)
	}
	if got := applyContainerDefaults(ContainerConfig{DiskLimitGB: 12}).DiskLimitGB; got != 12 {
		t.Errorf("explicit DiskLimitGB = %d, want 12", got)
	}
	if got := applyContainerDefaults(ContainerConfig{DiskLimitGB: -1}).DiskLimitGB; got != -1 {
		t.Errorf("disabled DiskLimitGB = %d, want -1 (preserved)", got)
	}
}

// TestProbeStorageOptSupport_NoImage returns false (safe fallback) without an
// image — no podman invocation, so it runs anywhere.
func TestProbeStorageOptSupport_NoImage(t *testing.T) {
	if ProbeStorageOptSupport(context.Background(), "podman", "") {
		t.Error("probe with empty image should report false (use the ulimit fallback)")
	}
}

// ── podman-gated integration tests (skipped off linux / without podman) ──

// TestContainerDiskQuotaSetsRLimit verifies the ulimit fallback actually reaches
// the container: with StorageOptSupported=false and a 1 GiB cap, RLIMIT_FSIZE
// inside the sandbox is 1 GiB (reported by `ulimit -f` in 512-byte blocks). This
// is the kernel-level proof of the quota without a multi-GiB write.
func TestContainerDiskQuotaSetsRLimit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("container backend tested on linux only")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available")
	}
	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	sb, err := NewContainer(ctx, ContainerConfig{
		Image:               testImage(),
		WorkspaceHostDir:    tmp,
		BridgeScript:        []byte("# unused\n"),
		DiskLimitGB:         1,
		StorageOptSupported: false, // force the always-available ulimit fsize path
	})
	if err != nil {
		t.Fatalf("NewContainer: %v", err)
	}
	defer sb.Close()

	res, err := sb.RunBash(context.Background(), BashRequest{Command: "ulimit -f"})
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ulimit -f exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	// RLIMIT_FSIZE must be a bounded positive value (NOT "unlimited"): the quota
	// flag reached the kernel. We don't pin the exact number — `ulimit -f` reports
	// 512-byte blocks while podman takes bytes, so the precise figure depends on
	// that conversion; the dd test below proves the cap is actually ~1 GiB.
	got := strings.TrimSpace(string(res.Stdout))
	if got == "unlimited" {
		t.Fatalf("ulimit -f = unlimited; RLIMIT_FSIZE was not applied")
	}
	if n, perr := strconv.Atoi(got); perr != nil || n <= 0 {
		t.Errorf("ulimit -f = %q, want a bounded positive block count", got)
	}
}

// TestContainerDiskQuotaBlocksOversizeFile is the acceptance scenario from #216:
// a single-file dd past the cap is killed (non-zero exit) before it can exhaust
// the host disk.
func TestContainerDiskQuotaBlocksOversizeFile(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("container backend tested on linux only")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available")
	}
	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	sb, err := NewContainer(ctx, ContainerConfig{
		Image:               testImage(),
		WorkspaceHostDir:    tmp,
		BridgeScript:        []byte("# unused\n"),
		DiskLimitGB:         1,
		StorageOptSupported: false,
	})
	if err != nil {
		t.Fatalf("NewContainer: %v", err)
	}
	defer sb.Close()

	// Write past the 1 GiB RLIMIT_FSIZE to a single workspace file. The kernel
	// raises SIGXFSZ at the limit, so dd is killed and the command exits non-zero.
	res, err := sb.RunBash(context.Background(), BashRequest{
		Command: "dd if=/dev/zero of=big bs=1M count=1100",
	})
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("dd past the quota succeeded (exit 0); expected it to be killed. stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
}

// TestContainerDiskQuotaCapsWorkspaceOnStorageOptHosts is the regression guard
// for the fail-open this test file previously could not see: every
// dd-past-the-cap case above ran with StorageOptSupported=false, so nothing
// covered the *better* storage drivers — where the old either/or
// diskQuotaArgs emitted ONLY --storage-opt. That flag caps the container's
// writable LAYER, which under --read-only is essentially unwritable, and does
// not apply to bind mounts; the workspace (also the default workdir) was
// therefore completely uncapped on exactly those hosts.
//
// StorageOptSupported is forced true here regardless of what this host's
// driver actually supports, because the assertion is about which FLAGS the
// config produces, not about the driver: RLIMIT_FSIZE must still reach the
// container and still bound a workspace write.
func TestContainerDiskQuotaCapsWorkspaceOnStorageOptHosts(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("container backend tested on linux only")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available")
	}
	if !ProbeStorageOptSupport(context.Background(), "", testImage()) {
		// --storage-opt=size would make `podman run` itself fail on a driver
		// that does not support it, so this case can only run where it does.
		t.Skip("storage driver does not support --storage-opt size")
	}
	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	sb, err := NewContainer(ctx, ContainerConfig{
		Image:               testImage(),
		WorkspaceHostDir:    tmp,
		BridgeScript:        []byte("# unused\n"),
		DiskLimitGB:         1,
		StorageOptSupported: true,
	})
	if err != nil {
		t.Fatalf("NewContainer: %v", err)
	}
	defer sb.Close()

	res, err := sb.RunBash(context.Background(), BashRequest{Command: "ulimit -f"})
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	if res.ExitCode != 0 {
		// Without this the next assertion is vacuous: a failed exec yields
		// empty stdout, which is != "unlimited" and would pass.
		t.Fatalf("ulimit -f exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got == "unlimited" {
		t.Fatal("ulimit -f = unlimited on a storage-opt host — the workspace bind mount has no cap at all")
	}

	// The workspace is a bind mount, outside any storage-driver quota. Only
	// RLIMIT_FSIZE can stop this write. (The workspace is already the default
	// --workdir, so a bare relative name lands there.)
	res, err = sb.RunBash(context.Background(), BashRequest{
		Command: "dd if=/dev/zero of=big bs=1M count=1100",
	})
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("dd past the quota into the WORKSPACE succeeded (exit 0) on a storage-opt host — the bind mount is uncapped. stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
}

// TestProbeCommandUnavailable pins the storage probe's error classification.
//
// The probe runs `--entrypoint=/usr/bin/true`, which is an assumption about the
// rootfs of a CLIENT-BUNDLE artifact — a busybox-based bundle has /bin/true. But
// podman validates --storage-opt at container-CREATE time and only then hands
// off to the runtime to exec, so a failure to exec proves the quota was
// ACCEPTED. Verified empirically: on an ext4 host, `--storage-opt=size=1g` with
// a nonexistent entrypoint reports the QUOTA error, not the exec error.
//
// The classification must stay narrow in the fail-closed direction: a false
// positive means reporting quota-capable on a driver that is not, after which
// `--storage-opt=size` is passed to every real container and every start fails —
// worse than losing the writable-layer quota.
func TestProbeCommandUnavailable(t *testing.T) {
	// Real podman/crun output, captured on this host.
	execFailures := []string{
		"Error: crun: executable file `/usr/bin/true` not found: No such file or directory: OCI runtime attempted to invoke a command that was not found",
		`Error: unable to start container: executable file not found in $PATH`,
	}
	for _, s := range execFailures {
		if !probeCommandUnavailable(s) {
			t.Errorf("probeCommandUnavailable(%.60q…) = false, want true — the quota was accepted, only the probe command was missing", s)
		}
	}

	// Must NOT be classified as an exec failure: these are real reasons to fall
	// back to the per-file-only quota, and misreading them breaks every container.
	quotaAndOtherFailures := []string{
		"Error: configure storage: storage option overlay.size and overlay.inodes only supported for backingFS XFS. Found extfs",
		`Error: short-name resolution enforced but cannot prompt without a TTY`,
		"Error: cannot connect to the podman socket: no such file or directory",
		"Error: initializing source docker://img: reading manifest: manifest unknown",
		"",
	}
	for _, s := range quotaAndOtherFailures {
		if probeCommandUnavailable(s) {
			t.Errorf("probeCommandUnavailable(%.60q…) = true, want false — this must NOT be read as quota-accepted", s)
		}
	}
}

// fakePodmanStorageProbe writes a stub podman that ASSERTS it was asked to run a
// container with the 1g storage quota, then fails with the given stderr. The argv
// assertion keeps the stub honest: a probe that stopped passing --storage-opt
// would be testing nothing.
func fakePodmanStorageProbe(t *testing.T, stderrBytes string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fake-podman")
	script := "#!/bin/sh\n" +
		"case \" $* \" in *\" --storage-opt=size=1g \"*) ;; *) echo \"fake-podman: expected --storage-opt=size=1g, got: $*\" >&2; exit 90;; esac\n" +
		"cat >&2 <<'FAKE_STDERR'\n" + stderrBytes + "\nFAKE_STDERR\n" +
		"exit 125\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake podman: %v", err)
	}
	return bin
}

// TestProbeStorageOptSupport_UsesTheClassification pins the WIRING, not just the
// classifier: ProbeStorageOptSupport must actually consult
// probeCommandUnavailable. Without this, reverting the carve-out (so an exec
// failure falls through to `return false`) leaves the classifier's own tests
// green while a busybox-based bundle silently loses the writable-layer quota
// again — the very bug this change fixes.
func TestProbeStorageOptSupport_UsesTheClassification(t *testing.T) {
	t.Run("exec failure means the quota was accepted", func(t *testing.T) {
		podman := fakePodmanStorageProbe(t,
			"Error: crun: executable file `/usr/bin/true` not found: No such file or directory: OCI runtime attempted to invoke a command that was not found")
		if !ProbeStorageOptSupport(context.Background(), podman, "localhost/busybox-bundle:test") {
			t.Error("a probe whose COMMAND was missing reported no quota support — podman validates --storage-opt before the exec, so the quota was accepted")
		}
	})
	t.Run("a real quota rejection still reports no support", func(t *testing.T) {
		podman := fakePodmanStorageProbe(t,
			"Error: configure storage: storage option overlay.size and overlay.inodes only supported for backingFS XFS. Found extfs")
		if ProbeStorageOptSupport(context.Background(), podman, "localhost/img:test") {
			t.Error("a driver that rejected the quota was reported as quota-capable — every real container start would then fail")
		}
	})
}
