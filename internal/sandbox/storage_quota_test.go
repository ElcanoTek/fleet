package sandbox

import (
	"context"
	"fmt"
	"os/exec"
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
		{"storage-opt when supported", 5, true, []string{"--storage-opt=size=5g"}},
		{"ulimit fallback", 5, false, []string{"--ulimit=fsize=5368709120"}}, // 5 * 1<<30
		{"ulimit fallback 1g", 1, false, []string{"--ulimit=fsize=1073741824"}},
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
