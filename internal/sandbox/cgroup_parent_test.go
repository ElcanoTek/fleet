package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestDelegatedCgroupParentFrom(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("cgroup v2 parent resolution is unix-only")
	}
	euid := os.Geteuid()

	t.Run("delegated subgroup with empty parent procs and memory controller", func(t *testing.T) {
		root := t.TempDir()
		unitDir := filepath.Join(root, "system.slice", "fleet.service")
		subDir := filepath.Join(unitDir, "supervisor")
		if err := os.MkdirAll(subDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(unitDir, "cgroup.procs"), []byte(""), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(unitDir, "cgroup.controllers"), []byte("cpuset cpu io memory pids\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		proc := []byte("0::/system.slice/fleet.service/supervisor\n")
		got := delegatedCgroupParentFrom(proc, root, euid)
		if got != "/system.slice/fleet.service" {
			t.Fatalf("delegatedCgroupParentFrom = %q, want /system.slice/fleet.service", got)
		}
	})

	t.Run("parent still holding internal processes returns empty", func(t *testing.T) {
		root := t.TempDir()
		unitDir := filepath.Join(root, "system.slice", "fleet.service")
		if err := os.MkdirAll(filepath.Join(unitDir, "supervisor"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(unitDir, "cgroup.procs"), []byte("12345\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(unitDir, "cgroup.controllers"), []byte("cpu memory pids\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		proc := []byte("0::/system.slice/fleet.service/supervisor\n")
		if got := delegatedCgroupParentFrom(proc, root, euid); got != "" {
			t.Fatalf("expected empty parent when cgroup.procs is non-empty, got %q", got)
		}
	})

	t.Run("parent owned by another uid returns empty for non-root", func(t *testing.T) {
		root := t.TempDir()
		sliceDir := filepath.Join(root, "system.slice")
		if err := os.MkdirAll(filepath.Join(sliceDir, "fleet.service"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sliceDir, "cgroup.procs"), []byte(""), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sliceDir, "cgroup.controllers"), []byte("cpu memory pids\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		otherUID := euid + 1
		if otherUID == 0 {
			otherUID = 1
		}
		proc := []byte("0::/system.slice/fleet.service\n")
		if got := delegatedCgroupParentFrom(proc, root, otherUID); got != "" {
			t.Fatalf("expected empty parent when owned by another uid, got %q", got)
		}
	})

	t.Run("parent missing memory controller returns empty", func(t *testing.T) {
		root := t.TempDir()
		unitDir := filepath.Join(root, "system.slice", "fleet.service")
		if err := os.MkdirAll(filepath.Join(unitDir, "supervisor"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(unitDir, "cgroup.procs"), []byte(""), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(unitDir, "cgroup.controllers"), []byte("cpu pids\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		proc := []byte("0::/system.slice/fleet.service/supervisor\n")
		if got := delegatedCgroupParentFrom(proc, root, euid); got != "" {
			t.Fatalf("expected empty parent when memory controller is absent, got %q", got)
		}
	})

	t.Run("root or top-level cgroup returns empty", func(t *testing.T) {
		root := t.TempDir()
		for _, raw := range []string{"0::/\n", "0::/fleet.service\n", "1:name=systemd:/foo\n"} {
			if got := delegatedCgroupParentFrom([]byte(raw), root, euid); got != "" {
				t.Fatalf("raw=%q: got %q, want empty", raw, got)
			}
		}
	})
}

func TestContainerRunArgs_EmitsAndOmitsCgroupParent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("podman arg assembly is linux-only")
	}
	t.Setenv(seccompProfileEnv, "")

	t.Run("emits --cgroup-parent when delegated subgroup resolves", func(t *testing.T) {
		bin, logPath := fakePodman(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		sb, err := NewContainer(ctx, ContainerConfig{
			Image:            "localhost/does-not-matter:test",
			WorkspaceHostDir: t.TempDir(),
			BridgeScript:     []byte("# unused\n"),
			PodmanBinary:     bin,
			StatsInterval:    -1,
			cgroupParentFn:   func() string { return "/system.slice/fleet.service" },
		})
		if err != nil {
			t.Fatalf("NewContainer: %v", err)
		}
		defer sb.Close()

		args := recordedArgs(t, logPath)
		if !hasArg(args, "--cgroup-parent=/system.slice/fleet.service") {
			t.Fatalf("missing --cgroup-parent=/system.slice/fleet.service in podman run argv: %v", args)
		}
	})

	t.Run("omits --cgroup-parent when resolver returns empty", func(t *testing.T) {
		bin, logPath := fakePodman(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		sb, err := NewContainer(ctx, ContainerConfig{
			Image:            "localhost/does-not-matter:test",
			WorkspaceHostDir: t.TempDir(),
			BridgeScript:     []byte("# unused\n"),
			PodmanBinary:     bin,
			StatsInterval:    -1,
			cgroupParentFn:   func() string { return "" },
		})
		if err != nil {
			t.Fatalf("NewContainer: %v", err)
		}
		defer sb.Close()

		args := recordedArgs(t, logPath)
		if hasArgWithPrefix(args, "--cgroup-parent=") {
			t.Fatalf("unexpected --cgroup-parent flag when resolver returned empty: %v", args)
		}
	})
}
