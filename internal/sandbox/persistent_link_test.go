package sandbox

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A persistent conversation sandbox mounts <root>/<conversation id> read-write,
// and podman follows a symbolic link in a volume source. A conversation
// directory that a whole-root sandbox replaced with a link is refused before
// anything starts: podman is never invoked with it, and the directory the
// link points at is neither mounted nor changed.
func TestPersistentSandboxRefusesLinkedConversationDir(t *testing.T) {
	stubDir := t.TempDir()
	calls := filepath.Join(stubDir, "calls")
	stub := "#!/bin/sh\necho \"$@\" >> " + shellSingleQuote(calls) + "\nexit 1\n"
	if err := os.WriteFile(filepath.Join(stubDir, "podman"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Chmod(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	const convID = "conv-linked"
	if err := os.Symlink(outside, filepath.Join(root, convID)); err != nil {
		t.Fatal(err)
	}
	p := &Pool{cfg: PoolConfig{
		Mode:         ModeContainer,
		BridgeScript: []byte("# bridge\n"),
		Container:    ContainerConfig{Image: "fleet-sandbox:test", WorkspaceHostDir: root, BridgeDir: t.TempDir()},
	}}

	sb, cleanup, err := p.newPersistentSandbox(context.Background(), convID)
	if cleanup != nil {
		cleanup()
	}
	if sb != nil {
		sb.Close()
	}
	if b, rerr := os.ReadFile(calls); rerr == nil && len(b) > 0 {
		t.Errorf("podman was invoked for a conversation directory that is a symbolic link:\n%s", b)
	}
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("err = %v, want the symbolic-link refusal", err)
	}
	if info, err := os.Stat(outside); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("the directory the link points at changed: %v %v", info, err)
	}
	if info, err := os.Lstat(filepath.Join(root, convID)); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link was replaced: %v %v", info, err)
	}
}

// persistentFakePodman is a fake podman for the persistent mount check. Every
// verb is logged; `exec … bash -c CMD` runs CMD on the host (the workspace is
// mounted at the same path inside a real container, so the marker check sees
// what the container would), and with swap set, `run` replaces the
// conversation directory with a fresh empty one first — the directory podman
// would mount if the name were swapped while the sandbox started.
func persistentFakePodman(t *testing.T, conv string, swap bool) (bin, logPath string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "fake-podman")
	logPath = filepath.Join(dir, "verbs.log")
	swapCmd := ":"
	if swap {
		swapCmd = "mv " + shellSingleQuote(conv) + " " + shellSingleQuote(conv+".swapped") + " && mkdir " + shellSingleQuote(conv)
	}
	script := "#!/bin/sh\n" +
		"verb=\nfor a in \"$@\"; do case \"$a\" in exec|run|kill|info) verb=$a; break;; esac; done\n" +
		"printf '%s\\n' \"$verb\" >> " + shellSingleQuote(logPath) + "\n" +
		"if [ \"$verb\" = run ]; then " + swapCmd + "; fi\n" +
		"if [ \"$verb\" = exec ]; then\n" +
		"  prev=; for a in \"$@\"; do if [ \"$prev\" = -c ]; then sh -c \"$a\"; exit $?; fi; prev=$a; done\n" +
		"  exit 0\nfi\n" +
		"printf 'deadbeefcafe\\n'\nexit 0\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, logPath
}

func persistentPool(t *testing.T, root, bin string) *Pool {
	t.Helper()
	pool := NewPool(PoolConfig{
		Mode:         ModeContainer,
		BridgeScript: []byte("# unused\n"),
		Container: ContainerConfig{
			Image:            "localhost/does-not-matter:test",
			WorkspaceHostDir: root,
			PodmanBinary:     bin,
			StatsInterval:    -1,
			BridgeDir:        t.TempDir(),
		},
	})
	t.Cleanup(func() { pool.Close() })
	return pool
}

func markersIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), mountMarkerPrefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

// The started persistent container must have mounted the very directory that
// was prepared: a directory swapped in while it started (the marker placed
// through the pinned descriptor is not there) closes the sandbox and fails
// the take; the intact directory starts. Either way the marker is removed.
func TestPersistentSandboxVerifiesTheMountedDirectory(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("podman arg assembly is linux-only")
	}
	t.Setenv(seccompProfileEnv, "")
	const convID = "conv-persistent"

	t.Run("the prepared directory is mounted", func(t *testing.T) {
		root := t.TempDir()
		conv := filepath.Join(root, convID)
		bin, logPath := persistentFakePodman(t, conv, false)
		pool := persistentPool(t, root, bin)
		sb, cleanup, err := pool.newPersistentSandbox(context.Background(), convID)
		if err != nil {
			t.Fatalf("newPersistentSandbox: %v", err)
		}
		if sb == nil {
			t.Fatal("no sandbox returned")
		}
		if got := markersIn(t, conv); len(got) != 0 {
			t.Errorf("marker left behind: %v", got)
		}
		if b, _ := os.ReadFile(logPath); strings.Contains(string(b), "kill") {
			t.Errorf("the sandbox was closed although its mount is intact:\n%s", b)
		}
		cleanup()
	})

	t.Run("a read-only conversation directory still starts", func(t *testing.T) {
		// User code can `chmod a-w .` its own directory; the first turn of a
		// persistent sandbox has not re-asserted the mode yet.
		root := t.TempDir()
		conv := filepath.Join(root, convID)
		if err := os.Mkdir(conv, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(conv, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(conv, 0o755) })
		bin, _ := persistentFakePodman(t, conv, false)
		pool := persistentPool(t, root, bin)
		sb, cleanup, err := pool.newPersistentSandbox(context.Background(), convID)
		if err != nil {
			t.Fatalf("newPersistentSandbox over a 0o555 directory: %v", err)
		}
		if sb == nil {
			t.Fatal("no sandbox returned")
		}
		cleanup()
		if info, err := os.Stat(conv); err != nil || info.Mode().Perm() != 0o755 {
			t.Errorf("directory mode = %v, %v; want 0755 restored", info.Mode().Perm(), err)
		}
	})

	t.Run("a marker a killed process left behind is removed", func(t *testing.T) {
		root := t.TempDir()
		conv := filepath.Join(root, convID)
		if err := os.Mkdir(conv, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(conv, mountMarkerPrefix+"stale"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		bin, _ := persistentFakePodman(t, conv, false)
		pool := persistentPool(t, root, bin)
		_, cleanup, err := pool.newPersistentSandbox(context.Background(), convID)
		if err != nil {
			t.Fatalf("newPersistentSandbox: %v", err)
		}
		cleanup()
		if got := markersIn(t, conv); len(got) != 0 {
			t.Errorf("markers left: %v", got)
		}
	})

	t.Run("a directory swapped in while the sandbox started", func(t *testing.T) {
		root := t.TempDir()
		conv := filepath.Join(root, convID)
		bin, logPath := persistentFakePodman(t, conv, true)
		pool := persistentPool(t, root, bin)
		sb, cleanup, err := pool.newPersistentSandbox(context.Background(), convID)
		if cleanup != nil {
			cleanup()
		}
		if err == nil || !strings.Contains(err.Error(), "not the one prepared") {
			t.Fatalf("err = %v, want the mount check to fail", err)
		}
		if sb != nil {
			t.Error("a sandbox was returned for a swapped mount")
		}
		if b, _ := os.ReadFile(logPath); !strings.Contains(string(b), "kill") {
			t.Errorf("the sandbox with the swapped mount was not closed:\n%s", b)
		}
		if got := markersIn(t, conv+".swapped"); len(got) != 0 {
			t.Errorf("marker left behind in the prepared directory: %v", got)
		}
	})
}
