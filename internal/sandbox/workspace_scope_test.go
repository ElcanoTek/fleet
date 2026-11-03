package sandbox

// Pins the scoped-workspace sandbox (#1897): a scheduled job's sandbox mounts
// only its own directory of the workspace root, so bash and run_python cannot
// list or read another job's or conversation's files. The fake-podman and
// pure pod-spec seams below need no container runtime, so these run in every
// lane; a live podman check would only re-prove that podman honours --volume.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// scopeFixture builds a workspace root shaped like production: two job
// directories, a conversation directory, the mcp-runs tree with two
// occurrences, and the shared file library.
type scopeFixture struct {
	root, job, otherJob, conv, runsBase, ownRun, otherRun, shared string
}

func newScopeFixture(t *testing.T) scopeFixture {
	t.Helper()
	root := t.TempDir()
	f := scopeFixture{
		root:     root,
		job:      filepath.Join(root, "tasks", "job-a"),
		otherJob: filepath.Join(root, "tasks", "job-b"),
		conv:     filepath.Join(root, "conv-1"),
		runsBase: filepath.Join(root, "mcp-runs"),
		ownRun:   filepath.Join(root, "mcp-runs", "task-a"),
		otherRun: filepath.Join(root, "mcp-runs", "task-b"),
		shared:   filepath.Join(root, "shared"),
	}
	for _, d := range []string{f.job, f.otherJob, f.conv, f.ownRun, f.otherRun, f.shared} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func TestResolveWorkspaceScope(t *testing.T) {
	f := newScopeFixture(t)

	t.Run("valid scope is cleaned and keeps existing read-only dirs", func(t *testing.T) {
		got, id, err := resolveWorkspaceScope(f.root, WorkspaceScope{
			Dir:      f.job + "/",
			ReadOnly: []string{f.ownRun, filepath.Join(f.runsBase, "task-missing")},
			Withhold: []string{f.runsBase},
		})
		if err != nil {
			t.Fatalf("resolveWorkspaceScope: %v", err)
		}
		if got.Dir != f.job {
			t.Errorf("Dir = %q, want %q", got.Dir, f.job)
		}
		if want := identityOf(t, f.job); id.String() != want {
			t.Errorf("identity = %s, want %s", id, want)
		}
		// A missing read-only dir is dropped, as a missing ReadOnlyMounts entry is.
		if len(got.ReadOnly) != 1 || got.ReadOnly[0] != f.ownRun {
			t.Errorf("ReadOnly = %v, want only %q", got.ReadOnly, f.ownRun)
		}
		if len(got.Withhold) != 1 || got.Withhold[0] != f.runsBase {
			t.Errorf("Withhold = %v, want %q", got.Withhold, f.runsBase)
		}
	})

	// Every refusal is fail-closed: a scope that would mount the root, or a
	// directory outside it, must never start a container.
	for name, scope := range map[string]WorkspaceScope{
		"empty dir":                  {},
		"the root itself":            {Dir: f.root},
		"outside the root":           {Dir: t.TempDir()},
		"escapes with ..":            {Dir: filepath.Join(f.root, "..", filepath.Base(f.root)+"-x")},
		"missing dir":                {Dir: filepath.Join(f.root, "tasks", "nope")},
		"read-only outside the root": {Dir: f.job, ReadOnly: []string{t.TempDir()}},
		"read-only is the root":      {Dir: f.job, ReadOnly: []string{f.root}},
		"read-only contains dir":     {Dir: f.job, ReadOnly: []string{filepath.Join(f.root, "tasks")}},
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			if got, _, err := resolveWorkspaceScope(f.root, scope); err == nil {
				t.Fatalf("resolveWorkspaceScope(%+v) = %+v, nil; want an error", scope, got)
			}
		})
	}

	t.Run("a file is not a directory", func(t *testing.T) {
		file := filepath.Join(f.root, "tasks", "file")
		if err := os.WriteFile(file, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := resolveWorkspaceScope(f.root, WorkspaceScope{Dir: file}); err == nil {
			t.Fatal("a regular file was accepted as the scoped workspace")
		}
	})

	// Everything below the root is writable by a whole-root sandbox (a chat,
	// a worktree run), and podman follows a symlinked volume source. A
	// symlink anywhere below the root — the job dir, tasks/, or a read-only
	// extra — must be refused, never followed, whether it points at another
	// job or outside the root.
	t.Run("refuses symlinks below the root", func(t *testing.T) {
		outside := t.TempDir()
		linkedJob := filepath.Join(f.root, "tasks", "job-link")
		if err := os.Symlink(f.otherJob, linkedJob); err != nil {
			t.Fatal(err)
		}
		escapeJob := filepath.Join(f.root, "tasks", "job-escape")
		if err := os.Symlink(outside, escapeJob); err != nil {
			t.Fatal(err)
		}
		linkedTasks := filepath.Join(f.root, "tasks2")
		if err := os.Symlink(filepath.Join(f.root, "tasks"), linkedTasks); err != nil {
			t.Fatal(err)
		}
		linkedRun := filepath.Join(f.runsBase, "task-link")
		if err := os.Symlink(f.otherRun, linkedRun); err != nil {
			t.Fatal(err)
		}
		for name, scope := range map[string]WorkspaceScope{
			"job dir is a symlink to another job":  {Dir: linkedJob},
			"job dir is a symlink out of the root": {Dir: escapeJob},
			"a parent component is a symlink":      {Dir: filepath.Join(linkedTasks, "job-a")},
			"a read-only extra is a symlink":       {Dir: f.job, ReadOnly: []string{linkedRun}},
		} {
			if got, _, err := resolveWorkspaceScope(f.root, scope); err == nil {
				t.Errorf("%s: resolveWorkspaceScope = %+v, nil; want a refusal", name, got)
			}
		}
	})

	// The root itself is operator configuration and may be a symlink.
	t.Run("a symlinked root is fine", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "ws")
		if err := os.Symlink(f.root, link); err != nil {
			t.Fatal(err)
		}
		if _, _, err := resolveWorkspaceScope(link, WorkspaceScope{Dir: filepath.Join(link, "tasks", "job-a")}); err != nil {
			t.Fatalf("symlinked workspace root refused: %v", err)
		}
	})
}

// identityOf is path's "dev:inode", as `stat -c %d:%i` prints it.
func identityOf(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino)
}

// scopedFakePodman is fakePodman whose `exec` answers verifyScopedMount's
// in-container stat with statOutput, so a test can play both an intact mount
// and a directory swapped while the sandbox started. Only `run` argv is
// logged.
func scopedFakePodman(t *testing.T, statOutput string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-podman")
	logPath := filepath.Join(dir, "argv.log")
	// The verb follows global flags (--cgroup-manager on linux), so find it
	// among the arguments rather than at $1.
	script := "#!/bin/sh\n" +
		"verb=\nfor a in \"$@\"; do case \"$a\" in exec|run) verb=$a; break;; esac; done\n" +
		"if [ \"$verb\" = exec ]; then printf '%s\\n' '" + statOutput + "'; exit 0; fi\n" +
		"if [ \"$verb\" = run ]; then for a in \"$@\"; do printf '%s\\n' \"$a\" >> \"" + logPath + "\"; done; fi\n" +
		"printf 'deadbeefcafe\\n'\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake podman: %v", err)
	}
	return bin, logPath
}

func TestWorkspaceMountsPlan(t *testing.T) {
	f := newScopeFixture(t)
	docs := "/opt/fleet/client/protocols"
	cfg := ContainerConfig{WorkspaceHostDir: f.root, ReadOnlyMounts: []string{docs, f.shared, f.runsBase}}

	rw, ro := cfg.workspaceMounts()
	if rw != f.root || strings.Join(ro, ",") != strings.Join(cfg.ReadOnlyMounts, ",") {
		t.Fatalf("unscoped plan = %q %v; want the root and the pool's read-only mounts unchanged", rw, ro)
	}

	cfg.Scope = &WorkspaceScope{Dir: f.job, ReadOnly: []string{f.ownRun}, Withhold: []string{f.runsBase}}
	rw, ro = cfg.workspaceMounts()
	if rw != f.job {
		t.Errorf("scoped read-write mount = %q, want the job dir %q", rw, f.job)
	}
	want := []string{docs, f.shared, f.ownRun}
	if strings.Join(ro, ",") != strings.Join(want, ",") {
		t.Errorf("scoped read-only mounts = %v, want %v (mcp-runs/ withheld, own run dir added)", ro, want)
	}
}

// TestScopedFileOpAnchorRefusesSiblings: the file tools of a scoped sandbox
// trust only what it mounts, so another job's or conversation's directory has
// no anchor at all — not even a read-only one.
func TestScopedFileOpAnchorRefusesSiblings(t *testing.T) {
	f := newScopeFixture(t)
	cfg := ContainerConfig{
		WorkspaceHostDir: f.root,
		ReadOnlyMounts:   []string{f.shared, f.runsBase},
		Scope:            &WorkspaceScope{Dir: f.job, ReadOnly: []string{f.ownRun}, Withhold: []string{f.runsBase}},
	}
	rw, ro := cfg.workspaceMounts()
	for _, root := range []string{f.otherJob, f.conv, f.otherRun, f.root} {
		if _, _, err := fileOpAnchorFor(rw, ro, root); !errors.Is(err, ErrFileOpUnsafePath) {
			t.Errorf("fileOpAnchorFor(%q) err = %v; want ErrFileOpUnsafePath", root, err)
		}
	}
	if anchor, readOnly, err := fileOpAnchorFor(rw, ro, filepath.Join(f.job, "out")); err != nil || anchor != f.job || readOnly {
		t.Errorf("own job dir: anchor=%q ro=%v err=%v; want %q, writable", anchor, readOnly, err, f.job)
	}
	if anchor, readOnly, err := fileOpAnchorFor(rw, ro, f.ownRun); err != nil || anchor != f.ownRun || !readOnly {
		t.Errorf("own MCP run dir: anchor=%q ro=%v err=%v; want %q, read-only", anchor, readOnly, err, f.ownRun)
	}
}

// TestScopedContainerRunArgs pins the podman command line of a scoped take:
// the job directory, not the root, is the read-write mount and the workdir,
// the whole mcp-runs/ tree is not mounted, and this occurrence's run dir is
// mounted read-only.
func TestScopedContainerRunArgs(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("podman arg assembly is linux-only")
	}
	t.Setenv(seccompProfileEnv, "")
	f := newScopeFixture(t)
	bin, logPath := scopedFakePodman(t, identityOf(t, f.job))
	pool := NewPool(PoolConfig{
		Mode:         ModeContainer,
		BridgeScript: []byte("# unused\n"),
		Container: ContainerConfig{
			Image:            "localhost/does-not-matter:test",
			WorkspaceHostDir: f.root,
			ReadOnlyMounts:   []string{f.shared, f.runsBase},
			PodmanBinary:     bin,
			StatsInterval:    -1,
		},
	})
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sb, cleanup, err := pool.TakeScoped(ctx, ScopedTake{
		Scope:     WorkspaceScope{Dir: f.job, ReadOnly: []string{f.ownRun}, Withhold: []string{f.runsBase}},
		NoNetwork: true,
	})
	if err != nil {
		t.Fatalf("TakeScoped: %v", err)
	}
	defer cleanup()
	if sb == nil {
		t.Fatal("TakeScoped returned a nil sandbox")
	}

	args := recordedArgs(t, logPath)
	for _, want := range []string{
		fmt.Sprintf("--volume=%s:%s:rw,z", f.job, f.job),
		fmt.Sprintf("--workdir=%s", f.job),
		fmt.Sprintf("--volume=%s:%s:ro,z", f.shared, f.shared),
		fmt.Sprintf("--volume=%s:%s:ro,z", f.ownRun, f.ownRun),
		"--network=none",
	} {
		if !hasArg(args, want) {
			t.Errorf("missing %q in scoped podman run argv", want)
		}
	}
	for _, a := range args {
		for _, banned := range []string{f.root + ":", f.runsBase + ":", f.otherJob, f.conv, f.otherRun} {
			if strings.Contains(a, "--volume="+banned) {
				t.Errorf("scoped sandbox mounts %q (arg %q): another job's or conversation's files would be visible", banned, a)
			}
		}
	}
}

// TestTakeScopedRefusesSwappedMount: if the directory podman mounted is not
// the inode the no-symlink walk checked — it was swapped while the sandbox
// started — the take tears the sandbox down and fails closed.
func TestTakeScopedRefusesSwappedMount(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("podman arg assembly is linux-only")
	}
	t.Setenv(seccompProfileEnv, "")
	f := newScopeFixture(t)
	bin, _ := scopedFakePodman(t, identityOf(t, f.otherJob))
	pool := NewPool(PoolConfig{
		Mode:         ModeContainer,
		BridgeScript: []byte("# unused\n"),
		Container:    ContainerConfig{Image: "img", WorkspaceHostDir: f.root, PodmanBinary: bin, StatsInterval: -1},
	})
	defer pool.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sb, _, err := pool.TakeScoped(ctx, ScopedTake{Scope: WorkspaceScope{Dir: f.job}, NoNetwork: true})
	if err == nil || sb != nil {
		t.Fatalf("TakeScoped = %v, %v; want a fail-closed refusal for a swapped mount", sb, err)
	}
	if !strings.Contains(err.Error(), "not the one checked") {
		t.Errorf("err = %v; want the swapped-mount refusal", err)
	}
}

func TestShellSingleQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/ws/tasks/a":  "'/ws/tasks/a'",
		"/ws/it's":     `'/ws/it'\''s'`,
		"/ws/a b;rm x": "'/ws/a b;rm x'",
	} {
		if got := shellSingleQuote(in); got != want {
			t.Errorf("shellSingleQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestTakeScopedRefusals(t *testing.T) {
	f := newScopeFixture(t)
	bin, _ := fakePodman(t)
	pool := NewPool(PoolConfig{
		Mode:         ModeContainer,
		BridgeScript: []byte("# unused\n"),
		Container:    ContainerConfig{Image: "img", WorkspaceHostDir: f.root, PodmanBinary: bin, StatsInterval: -1},
	})
	defer pool.Close()
	// Fail closed: a scope that names the root must not start an unscoped
	// container in its place.
	if _, _, err := pool.TakeScoped(context.Background(), ScopedTake{Scope: WorkspaceScope{Dir: f.root}}); err == nil {
		t.Error("TakeScoped accepted the workspace root as a scope")
	}
	// Allowlisted egress without a proxy fails closed, exactly as the
	// unscoped take does.
	if _, _, err := pool.TakeScoped(context.Background(), ScopedTake{Scope: WorkspaceScope{Dir: f.job}, Allowlisted: true}); err == nil {
		t.Error("TakeScoped granted allowlisted egress with no egress proxy configured")
	}
	// No container backend: the caller's host fallback keys on this error.
	host := NewPool(PoolConfig{Mode: ModeHost})
	defer host.Close()
	if _, _, err := host.TakeScoped(context.Background(), ScopedTake{Scope: WorkspaceScope{Dir: f.job}}); !errors.Is(err, ErrContainerUnavailable) {
		t.Errorf("host pool TakeScoped err = %v; want ErrContainerUnavailable", err)
	}
}

// TestK8sPodSpecScopedWorkspace: under the kubernetes backend the scoped
// workspace is a subPath of the claim at its own path, and only the claim's
// read-only trees that survive the scope are mounted.
func TestK8sPodSpecScopedWorkspace(t *testing.T) {
	f := newScopeFixture(t)
	cfg := testContainerConfig(t)
	cfg.WorkspaceHostDir = f.root
	cfg.ReadOnlyMounts = []string{f.shared, f.runsBase}
	cfg.Scope = &WorkspaceScope{Dir: f.job, ReadOnly: []string{f.ownRun}, Withhold: []string{f.runsBase}}
	kcfg := KubernetesConfig{Namespace: "fleet-sandbox", WorkspaceClaim: "fleet-workspace"}

	pod, err := buildSandboxPod(applyContainerDefaults(cfg), kcfg, "fleet-sandbox-scoped")
	if err != nil {
		t.Fatalf("buildSandboxPod: %v", err)
	}
	c := pod.Spec.Containers[0]
	if c.WorkingDir != f.job {
		t.Errorf("WorkingDir = %q, want the job dir %q", c.WorkingDir, f.job)
	}
	got := map[string]k8sVolumeMount{}
	for _, m := range c.VolumeMounts {
		if m.Name == "workspace" {
			got[m.MountPath] = m
		}
	}
	want := map[string]k8sVolumeMount{
		f.job:    {Name: "workspace", MountPath: f.job, SubPath: "tasks/job-a"},
		f.shared: {Name: "workspace", MountPath: f.shared, SubPath: "shared", ReadOnly: true},
		f.ownRun: {Name: "workspace", MountPath: f.ownRun, SubPath: "mcp-runs/task-a", ReadOnly: true},
	}
	if len(got) != len(want) {
		t.Errorf("workspace-claim mounts = %+v, want exactly %+v", got, want)
	}
	for path, w := range want {
		if g, ok := got[path]; !ok || g != w {
			t.Errorf("mount at %q = %+v (present=%v), want %+v", path, g, ok, w)
		}
	}

	// A scope outside the claim root is refused by the builder too.
	cfg.Scope = &WorkspaceScope{Dir: t.TempDir()}
	if _, err := buildSandboxPod(applyContainerDefaults(cfg), kcfg, "fleet-sandbox-bad"); err == nil {
		t.Error("buildSandboxPod accepted a scoped workspace outside the claim root")
	}
}
