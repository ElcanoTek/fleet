package sandbox

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func writeTestCgroupFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fakeKernelCgroupMover(root *os.Root, relSelf, subRel, pid string) error {
	selfProcsPath := path.Join(relSelf, "cgroup.procs")
	subProcsPath := path.Join(subRel, "cgroup.procs")

	cur, err := root.ReadFile(selfProcsPath)
	if err != nil {
		return err
	}
	var kept []string
	for p := range strings.FieldsSeq(string(cur)) {
		if p != pid {
			kept = append(kept, p)
		}
	}
	var updatedSelf string
	if len(kept) > 0 {
		updatedSelf = strings.Join(kept, "\n") + "\n"
	}
	if err := root.WriteFile(selfProcsPath, []byte(updatedSelf), 0o644); err != nil {
		return err
	}

	existingSub, _ := root.ReadFile(subProcsPath)
	subPIDs := strings.Fields(string(existingSub))
	if !slices.Contains(subPIDs, pid) {
		subPIDs = append(subPIDs, pid)
	}
	return root.WriteFile(subProcsPath, []byte(strings.Join(subPIDs, "\n")+"\n"), 0o644)
}

func TestDelegatedCgroupParentFrom(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("cgroup v2 parent resolution is unix-only")
	}
	euid := os.Geteuid()
	if euid == 0 {
		t.Skip("positive delegation ownership tests run as non-root")
	}

	t.Run("delegated subgroup on systemd >= 254 with empty parent procs", func(t *testing.T) {
		root := t.TempDir()
		unitDir := filepath.Join(root, "system.slice", "fleet.service")
		if err := os.MkdirAll(filepath.Join(unitDir, "supervisor"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestCgroupFile(t, filepath.Join(unitDir, "cgroup.procs"), "")
		writeTestCgroupFile(t, filepath.Join(unitDir, "cgroup.controllers"), "cpuset cpu io memory pids\n")
		proc := []byte("0::/system.slice/fleet.service/supervisor\n")
		if got := delegatedCgroupParentFrom(proc, root, euid, 4242); got != "/system.slice/fleet.service" {
			t.Fatalf("delegatedCgroupParentFrom = %q, want /system.slice/fleet.service", got)
		}
	})

	t.Run("parent still holding internal processes without self-evacuable leaf returns empty", func(t *testing.T) {
		root := t.TempDir()
		unitDir := filepath.Join(root, "system.slice", "fleet.service")
		if err := os.MkdirAll(filepath.Join(unitDir, "supervisor"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestCgroupFile(t, filepath.Join(unitDir, "cgroup.procs"), "99999\n")
		writeTestCgroupFile(t, filepath.Join(unitDir, "cgroup.controllers"), "cpu memory pids\n")
		proc := []byte("0::/system.slice/fleet.service/supervisor\n")
		if got := delegatedCgroupParentWithMover(proc, root, euid, 4242, fakeKernelCgroupMover); got != "" {
			t.Fatalf("expected empty parent when cgroup.procs is non-empty, got %q", got)
		}
	})

	t.Run("missing memory controller returns empty", func(t *testing.T) {
		root := t.TempDir()
		unitDir := filepath.Join(root, "system.slice", "fleet.service")
		if err := os.MkdirAll(filepath.Join(unitDir, "supervisor"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestCgroupFile(t, filepath.Join(unitDir, "cgroup.procs"), "")
		writeTestCgroupFile(t, filepath.Join(unitDir, "cgroup.controllers"), "cpu pids\n")
		proc := []byte("0::/system.slice/fleet.service/supervisor\n")
		if got := delegatedCgroupParentFrom(proc, root, euid, 4242); got != "" {
			t.Fatalf("expected empty parent when memory controller is absent, got %q", got)
		}
	})

	t.Run("root, top-level, or user.slice cgroup returns empty", func(t *testing.T) {
		root := t.TempDir()
		userApp := filepath.Join(root, "user.slice", "user-1000.slice", "app.slice", "term.scope")
		if err := os.MkdirAll(userApp, 0o755); err != nil {
			t.Fatal(err)
		}
		appSlice := filepath.Dir(userApp)
		writeTestCgroupFile(t, filepath.Join(appSlice, "cgroup.procs"), "")
		writeTestCgroupFile(t, filepath.Join(appSlice, "cgroup.controllers"), "cpu memory pids\n")
		for _, raw := range []string{
			"0::/\n",
			"0::/fleet.service\n",
			"0::/user.slice/user-1000.slice/app.slice/term.scope\n",
			"1:name=systemd:/foo\n",
		} {
			if got := delegatedCgroupParentWithMover([]byte(raw), root, euid, 4242, fakeKernelCgroupMover); got != "" {
				t.Fatalf("raw=%q: got %q, want empty", raw, got)
			}
		}
	})
}

func TestDelegatedCgroupParentSelfEvacuatePre254(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("cgroup v2 parent resolution is unix-only")
	}
	euid := os.Geteuid()
	if euid == 0 {
		t.Skip("positive delegation ownership tests run as non-root")
	}

	t.Run("pre-254 systemd unit root self-evacuates pids into supervisor subgroup", func(t *testing.T) {
		root := t.TempDir()
		sliceDir := filepath.Join(root, "system.slice")
		unitDir := filepath.Join(sliceDir, "fleet.service")
		writeTestCgroupFile(t, filepath.Join(sliceDir, "cgroup.procs"), "1\n")
		writeTestCgroupFile(t, filepath.Join(unitDir, "cgroup.procs"), "4242\n4243\n")
		writeTestCgroupFile(t, filepath.Join(unitDir, "cgroup.controllers"), "cpu io memory pids\n")

		proc := []byte("0::/system.slice/fleet.service\n")
		if got := delegatedCgroupParentWithMover(proc, root, euid, 4242, fakeKernelCgroupMover); got != "/system.slice/fleet.service" {
			t.Fatalf("delegatedCgroupParentWithMover = %q, want /system.slice/fleet.service", got)
		}
		unitProcs, err := os.ReadFile(filepath.Join(unitDir, "cgroup.procs"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(unitProcs)) != "" {
			t.Fatalf("expected fleet.service/cgroup.procs to be drained, got %q", string(unitProcs))
		}
		subProcs, err := os.ReadFile(filepath.Join(unitDir, "supervisor", "cgroup.procs"))
		if err != nil {
			t.Fatal(err)
		}
		moved := strings.Fields(string(subProcs))
		if !slices.Contains(moved, "4242") || !slices.Contains(moved, "4243") {
			t.Fatalf("expected both 4242 and 4243 moved to supervisor/cgroup.procs, got %v", moved)
		}
	})

	t.Run("unit owned by another uid returns empty for non-root", func(t *testing.T) {
		root := t.TempDir()
		sliceDir := filepath.Join(root, "system.slice")
		unitDir := filepath.Join(sliceDir, "fleet.service")
		writeTestCgroupFile(t, filepath.Join(sliceDir, "cgroup.procs"), "")
		writeTestCgroupFile(t, filepath.Join(sliceDir, "cgroup.controllers"), "cpu memory pids\n")
		writeTestCgroupFile(t, filepath.Join(unitDir, "cgroup.procs"), "4242\n")
		writeTestCgroupFile(t, filepath.Join(unitDir, "cgroup.controllers"), "cpu memory pids\n")
		otherUID := euid + 1
		if otherUID == 0 {
			otherUID = 1
		}
		proc := []byte("0::/system.slice/fleet.service\n")
		if got := delegatedCgroupParentWithMover(proc, root, otherUID, 4242, fakeKernelCgroupMover); got != "" {
			t.Fatalf("expected empty parent when owned by another uid, got %q", got)
		}
		if got := delegatedCgroupParentWithMover(proc, root, 0, 4242, fakeKernelCgroupMover); got != "" {
			t.Fatalf("expected empty parent for root (euid=0), got %q", got)
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
