package sandbox

// Pins the mounts of the workspace root's repository (its .git and the
// per-run worktree directory .fleet-worktrees): every whole-root sandbox gets
// both as their own read-only bind mounts, and a worktree run's sandbox gets,
// writable and nested in them, only what its git writes (its checkout and
// record, objects/, refs/, logs/), with objects/info read-only again. The plan
// and argument tests need no container runtime. The real thing runs in
// scheduledrun's TestWorktreeRunsLiveContainer, where fleet's own code writes
// the worktree records and does the checkout (this package cannot import
// internal/worktree).

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

type gitFixture struct {
	root, gitDir, wtDir, conv, wt, admin string
}

// gitDirsFixture lays out a whole-root workspace the way a worktree run sees
// it: .git (objects/info, refs, logs, worktrees/<id>), .fleet-worktrees/<run>
// and a conversation directory.
func gitDirsFixture(t *testing.T) gitFixture {
	t.Helper()
	root := t.TempDir()
	f := gitFixture{
		root:   root,
		gitDir: filepath.Join(root, ".git"),
		wtDir:  filepath.Join(root, ".fleet-worktrees"),
		conv:   filepath.Join(root, "conv-1"),
	}
	f.wt = filepath.Join(f.wtDir, "task-run")
	f.admin = filepath.Join(f.gitDir, "worktrees", "task-run")
	for _, d := range []string{
		filepath.Join(f.gitDir, "hooks"), filepath.Join(f.gitDir, "objects", "info"),
		filepath.Join(f.gitDir, "refs"), filepath.Join(f.gitDir, "logs"), f.admin, f.wt, f.conv,
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func mountPaths(ms []gitDirMount) (ro, rw []string) {
	for _, m := range ms {
		if m.readOnly {
			ro = append(ro, m.path)
		} else {
			rw = append(rw, m.path)
		}
	}
	return ro, rw
}

func TestWholeRootGitDirsPlan(t *testing.T) {
	f := gitDirsFixture(t)
	base := ContainerConfig{WorkspaceHostDir: f.root, WholeRootGitDirs: []string{f.gitDir, f.wtDir}}

	rw, ro := base.workspaceMounts()
	if rw != f.root || strings.Join(ro, ",") != f.gitDir+","+f.wtDir {
		t.Fatalf("whole-root plan = %q %v; want the root with .git and .fleet-worktrees read-only", rw, ro)
	}
	if anchor, readOnly, err := fileOpAnchorFor(rw, ro, filepath.Join(f.gitDir, "hooks")); err != nil || anchor != f.gitDir || !readOnly {
		t.Errorf("anchor for .git/hooks = %q ro=%v err=%v; want %q, read-only", anchor, readOnly, err, f.gitDir)
	}
	gotRO, gotRW := mountPaths(base.gitDirMounts())
	if strings.Join(gotRO, ",") != f.gitDir+","+f.wtDir || len(gotRW) != 0 {
		t.Errorf("chat gitDirMounts ro=%v rw=%v; want both read-only, nothing writable", gotRO, gotRW)
	}

	run := base
	run.Worktree = &WorktreeMount{Path: f.wt, AdminDir: f.admin}
	mounts := run.gitDirMounts()
	gotRO, gotRW = mountPaths(mounts)
	wantRW := []string{filepath.Join(f.gitDir, "objects"), filepath.Join(f.gitDir, "refs"), filepath.Join(f.gitDir, "logs"), f.admin, f.wt}
	wantRO := []string{f.gitDir, f.wtDir, filepath.Join(f.gitDir, "objects", "info")}
	slices.Sort(gotRW)
	slices.Sort(wantRW)
	slices.Sort(gotRO)
	slices.Sort(wantRO)
	if !slices.Equal(gotRW, wantRW) || !slices.Equal(gotRO, wantRO) {
		t.Errorf("worktree-run mounts ro=%v rw=%v; want ro=%v rw=%v", gotRO, gotRW, wantRO, wantRW)
	}
	for i := 1; i < len(mounts); i++ {
		if strings.Count(mounts[i].path, "/") < strings.Count(mounts[i-1].path, "/") {
			t.Errorf("mount %q comes after the deeper %q; parents must come first", mounts[i].path, mounts[i-1].path)
		}
	}
	if _, ro := run.workspaceMounts(); len(ro) != 0 {
		t.Errorf("worktree-run anchor read-only set = %v; want none (the mounts enforce)", ro)
	}
	if anchor, readOnly, err := fileOpAnchorFor(f.root, nil, f.wt); err != nil || anchor != f.root || readOnly {
		t.Errorf("worktree-run anchor = %q ro=%v err=%v; want the writable root", anchor, readOnly, err)
	}

	// A record or checkout outside its expected parent gets no writable mount.
	odd := base
	odd.Worktree = &WorktreeMount{Path: f.conv, AdminDir: filepath.Join(f.gitDir, "hooks")}
	if _, rw := mountPaths(odd.gitDirMounts()); slices.Contains(rw, f.conv) || slices.Contains(rw, filepath.Join(f.gitDir, "hooks")) {
		t.Errorf("a misplaced worktree got writable mounts: %v", rw)
	}
	if err := checkWorktreeMount(base, *odd.Worktree); err == nil {
		t.Error("checkWorktreeMount accepted a misplaced worktree")
	}
	if err := checkWorktreeMount(base, *run.Worktree); err != nil {
		t.Errorf("checkWorktreeMount: %v", err)
	}
	if err := checkWorktreeMount(ContainerConfig{WorkspaceHostDir: f.root}, *run.Worktree); err == nil {
		t.Error("checkWorktreeMount accepted a pool with no recorded repository")
	}
	// No objects/info means no read-only mount over it, and no logs means the
	// run's branch cannot be created inside the read-only .git: refused.
	for _, dir := range []string{filepath.Join(f.gitDir, "objects", "info"), filepath.Join(f.gitDir, "logs")} {
		if err := os.Rename(dir, dir+".away"); err != nil {
			t.Fatal(err)
		}
		if err := checkWorktreeMount(base, *run.Worktree); err == nil {
			t.Errorf("checkWorktreeMount accepted a repository without %s", dir)
		}
		if err := os.Rename(dir+".away", dir); err != nil {
			t.Fatal(err)
		}
	}

	scoped := base
	scoped.Scope = &WorkspaceScope{Dir: f.conv}
	if _, ro := scoped.workspaceMounts(); len(ro) != 0 {
		t.Errorf("scoped plan read-only = %v; want none (a scoped sandbox never sees the root)", ro)
	}
	if got := scoped.gitDirMounts(); len(got) != 0 {
		t.Errorf("scoped gitDirMounts = %+v; want none", got)
	}

	persistent := base
	persistent.WorkspaceHostDir = f.conv
	if _, ro := persistent.workspaceMounts(); len(ro) != 0 {
		t.Errorf("persistent plan read-only = %v; want none (outside its workspace)", ro)
	}
	if got := persistent.gitDirMounts(); len(got) != 0 {
		t.Errorf("persistent gitDirMounts = %+v; want none", got)
	}
}

// TestWholeRootGitDirsSkipWhatIsNotADirectory: an entry removed since boot,
// or replaced by a symlink, gets no mount — neither podman nor the kubelet
// is handed a path that would resolve somewhere else (or be created).
func TestWholeRootGitDirsSkipWhatIsNotADirectory(t *testing.T) {
	f := gitDirsFixture(t)
	if err := os.RemoveAll(f.gitDir); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(f.wtDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), f.wtDir); err != nil {
		t.Fatal(err)
	}
	cfg := ContainerConfig{WorkspaceHostDir: f.root, WholeRootGitDirs: []string{f.gitDir, f.wtDir}}
	if got := cfg.gitDirMounts(); len(got) != 0 {
		t.Fatalf("gitDirMounts = %+v; want none for a missing and a symlinked entry", got)
	}
}

func TestWholeRootGitDirsRunArgs(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("podman arg assembly is linux-only")
	}
	t.Setenv(seccompProfileEnv, "")
	f := gitDirsFixture(t)
	for _, tc := range []struct {
		name string
		wt   *WorktreeMount
	}{
		{"chat and warm pool", nil},
		{"worktree run", &WorktreeMount{Path: f.wt, AdminDir: f.admin}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ContainerConfig{WorkspaceHostDir: f.root, WholeRootGitDirs: []string{f.gitDir, f.wtDir}, Worktree: tc.wt}
			want := map[string]string{}
			for _, m := range cfg.gitDirMounts() {
				mode := "rw"
				if m.readOnly {
					mode = "ro"
				}
				want[m.path] = mode
			}
			if tc.wt == nil && len(want) != 2 || tc.wt != nil && len(want) != 8 {
				t.Fatalf("planned mounts = %v", want)
			}

			bin, logPath := fakePodman(t)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			run := cfg
			run.Image, run.BridgeScript, run.PodmanBinary, run.StatsInterval = "img", []byte("# unused\n"), bin, -1
			sb, err := NewContainer(ctx, run)
			if err != nil {
				t.Fatalf("NewContainer: %v", err)
			}
			defer sb.Close()
			args := recordedArgs(t, logPath)
			for path, mode := range want {
				if !hasArg(args, fmt.Sprintf("--volume=%s:%s:%s,z", path, path, mode)) {
					t.Errorf("missing %s %s mount in podman run argv", path, mode)
				}
			}

			podCfg := testContainerConfig(t)
			podCfg.WorkspaceHostDir = f.root
			podCfg.WholeRootGitDirs = cfg.WholeRootGitDirs
			podCfg.Worktree = tc.wt
			pod, err := buildSandboxPod(applyContainerDefaults(podCfg), KubernetesConfig{Namespace: "ns", WorkspaceClaim: "claim"}, "fleet-sandbox-git")
			if err != nil {
				t.Fatalf("buildSandboxPod: %v", err)
			}
			for path, mode := range want {
				n := 0
				for _, m := range pod.Spec.Containers[0].VolumeMounts {
					if m.MountPath != path {
						continue
					}
					n++
					rel, _ := filepath.Rel(f.root, path)
					if m.Name != "workspace" || m.SubPath != rel || m.ReadOnly != (mode == "ro") {
						t.Errorf("pod mount for %s = %+v; want a workspace subPath %q, read-only=%v", path, m, rel, mode == "ro")
					}
				}
				if n != 1 {
					t.Errorf("pod has %d mounts of %s, want exactly 1", n, path)
				}
			}
		})
	}

	t.Run("a workspace that is not a repository gets no git mounts", func(t *testing.T) {
		bare := t.TempDir()
		cfg := testContainerConfig(t)
		cfg.WorkspaceHostDir = bare
		cfg.WholeRootGitDirs = []string{filepath.Join(bare, ".git"), filepath.Join(bare, ".fleet-worktrees")}
		pod, err := buildSandboxPod(applyContainerDefaults(cfg), KubernetesConfig{Namespace: "ns", WorkspaceClaim: "claim"}, "fleet-sandbox-bare")
		if err != nil {
			t.Fatalf("buildSandboxPod: %v", err)
		}
		for _, m := range pod.Spec.Containers[0].VolumeMounts {
			if strings.HasPrefix(m.MountPath, cfg.WholeRootGitDirs[0]) || strings.HasPrefix(m.MountPath, cfg.WholeRootGitDirs[1]) {
				t.Errorf("pod mounts %+v; a missing entry must not reach the kubelet, which would create it", m)
			}
		}
	})
}
