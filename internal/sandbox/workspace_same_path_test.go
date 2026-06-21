package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestContainerWorkspaceSamePathCoherence pins the contract that
// powers MCP↔sandbox path coherence: the workspace is bind-mounted at
// the SAME absolute path inside the container as on the host. So
// `/opt/chat/workspace/<convID>/foo.csv` written by a host-side MCP
// (e.g. mcp_email_download_attachment) opens at the same absolute
// path from inside the container's python kernel — no translation
// layer, no LLM coherence problems when an absolute path crosses the
// boundary.
//
// Regression for two real bugs reported in lockdown chats:
//   - bridge `kernel chdir failed: FileNotFoundError` because the host
//     path didn't exist inside the container,
//   - per-conversation isolation broken: writes leaked into the shared
//     workspace root when chdir silently failed.
func TestContainerWorkspaceSamePathCoherence(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("container backend tested on linux only")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available")
	}
	if os.Geteuid() == 0 {
		// Production runs chat-server as the unprivileged `chat` user
		// (so podman is rootless and `--userns=keep-id` actually maps
		// the host caller to the container's sandbox uid). Under root,
		// rootful podman ignores keep-id mapping — the in-container
		// uid stays disconnected from the bind-mount source's owner —
		// and the test would fail on a permission gap that doesn't
		// exist in production. Skip rather than mask with a chmod that
		// would also hide the real perms regression.
		t.Skip("rootful podman doesn't match production userns mapping; run as a non-root user")
	}
	bridge, err := os.ReadFile("../tools/python_bridge.py")
	if err != nil {
		t.Skipf("python_bridge.py not readable: %v", err)
	}

	wsHost := t.TempDir()
	// Match production perms exactly. t.TempDir() is 0o700 by default,
	// which the in-container sandbox uid 1000 can't traverse — but
	// production's workspace ROOT is 0o755 (see agent.go MkdirAll on
	// boot) and per-conv subdirs are 0o755 (see tools.EnsureWorkspaceDir).
	// Earlier revisions of this test chmod'd everything to 0o777, which
	// hid the lockdown perms bug where production used to make per-conv
	// dirs 0o750 and they showed up as root:root 0o750 inside the
	// container, breaking every ls/chdir for the sandbox user. Mirror
	// production exactly so the test would fail if that regression
	// returned.
	if err := os.Chmod(wsHost, 0o755); err != nil {
		t.Fatalf("chmod wsHost: %v", err)
	}
	convDir := filepath.Join(wsHost, "conv-abc")
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatalf("mkdir convDir: %v", err)
	}

	// Drop a file from the HOST side first — this is the "MCP just
	// downloaded an attachment" simulation. If same-path mounting is
	// broken, the python kernel below won't be able to open this
	// absolute path.
	mcpFile := filepath.Join(convDir, "from_mcp.txt")
	if err := os.WriteFile(mcpFile, []byte("payload-from-host-mcp"), 0o644); err != nil {
		t.Fatalf("write mcpFile: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	sb, err := NewContainer(ctx, ContainerConfig{
		Image:            "ghcr.io/elcanotek/sandbox:latest",
		WorkspaceHostDir: wsHost,
		BridgeScript:     bridge,
	})
	if err != nil {
		t.Fatalf("NewContainer: %v", err)
	}
	defer sb.Close()

	// Production flow: tools/python_repl.go passes the host abs path
	// as WorkspaceDir, the bridge chdirs into it, and an absolute path
	// (such as one an MCP returned) opens cleanly.
	res, err := sb.RunPython(ctx, PythonRequest{
		Code: "import os\n" +
			// Read the file the host-side MCP wrote, by its full host path.
			"with open('" + mcpFile + "') as f: payload = f.read()\n" +
			// Write a marker through the cwd (relative path).
			"with open('marker.txt', 'w') as f: f.write('here')\n" +
			"print('cwd=', os.getcwd())\n" +
			"print('payload=', payload)\n",
		Timeout:      60 * time.Second,
		WorkspaceDir: convDir,
	})
	if err != nil {
		t.Fatalf("RunPython: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("status=%q stderr=%q error=%q", res.Status, res.Stderr, res.Error)
	}

	// cwd inside the container must equal the HOST path verbatim.
	wantCwd := "cwd= " + convDir
	if !strings.Contains(res.Stdout, wantCwd) {
		t.Errorf("kernel cwd not same-path; want %q in stdout=%q", wantCwd, res.Stdout)
	}
	// MCP-style absolute path resolved inside the container.
	if !strings.Contains(res.Stdout, "payload= payload-from-host-mcp") {
		t.Errorf("absolute host path not readable from container kernel; stdout=%q", res.Stdout)
	}
	// Per-conv isolation: the relative-write landed in convDir, not the shared root.
	if _, err := os.Stat(filepath.Join(convDir, "marker.txt")); err != nil {
		t.Errorf("marker not in scoped convDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wsHost, "marker.txt")); err == nil {
		t.Errorf("marker leaked into shared workspace root — isolation broken")
	}
}

// TestContainerReadOnlyMountsSamePath pins the contract that powers
// agent supporting-doc access from inside the lockdown sandbox: each
// path passed via ContainerConfig.ReadOnlyMounts is bind-mounted at the
// SAME absolute path inside the container, read-only.
//
// Regression for the lockdown bug where bash inside the container saw
// dangling symlinks for `personas/` / `protocols/` / `system_prompts/`
// because tools/workspace.go points the per-conversation symlinks at
// the host path of those dirs (e.g. /opt/chat/server/personas) — and
// without this same-path mount, that absolute path doesn't exist inside
// the container. Host-side view_file works (pathsec follows the
// symlink); container-side bash/run_python doesn't.
func TestContainerReadOnlyMountsSamePath(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("container backend tested on linux only")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available")
	}
	if os.Geteuid() == 0 {
		// Same reason as TestContainerWorkspaceSamePathCoherence —
		// rootful podman ignores keep-id.
		t.Skip("rootful podman doesn't match production userns mapping; run as a non-root user")
	}

	// Stand up a docs dir that mimics /opt/chat/server/personas: a few
	// YAML files the agent would `cat` from bash. We deliberately put it
	// somewhere under t.TempDir() *outside* wsHost so the only way the
	// container can see it is via the ReadOnlyMounts entry.
	docsHost := t.TempDir()
	if err := os.Chmod(docsHost, 0o755); err != nil {
		t.Fatalf("chmod docsHost: %v", err)
	}
	personaPath := filepath.Join(docsHost, "victoria.yaml")
	if err := os.WriteFile(personaPath, []byte("name: Victoria\nrole: marketing\n"), 0o644); err != nil {
		t.Fatalf("write persona: %v", err)
	}

	wsHost := t.TempDir()
	if err := os.Chmod(wsHost, 0o755); err != nil {
		t.Fatalf("chmod wsHost: %v", err)
	}
	convDir := filepath.Join(wsHost, "conv-docs")
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatalf("mkdir convDir: %v", err)
	}
	// Drop a symlink the way EnsureWorkspaceDir does in production. We
	// want bash inside the container to follow this symlink and find
	// the persona file at the same absolute host path.
	convPersonas := filepath.Join(convDir, "personas")
	if err := os.Symlink(docsHost, convPersonas); err != nil {
		t.Fatalf("symlink personas: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sb, err := NewContainer(ctx, ContainerConfig{
		Image:            "ghcr.io/elcanotek/sandbox:latest",
		WorkspaceHostDir: wsHost,
		BridgeScript:     []byte("# unused for bash test\n"),
		ReadOnlyMounts:   []string{docsHost},
	})
	if err != nil {
		t.Fatalf("NewContainer: %v", err)
	}
	defer sb.Close()

	// Read via the symlink — the actual production path. If
	// ReadOnlyMounts isn't honored, the symlink target (an absolute
	// host path under docsHost) doesn't exist inside the container and
	// `cat` fails with ENOENT.
	res, err := sb.RunBash(ctx, BashRequest{
		Command:    "cat personas/victoria.yaml",
		WorkingDir: convDir,
	})
	if err != nil {
		t.Fatalf("RunBash via symlink: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("symlink cat exit=%d stderr=%q (regression: supporting-doc bind mount missing — lockdown chats see dangling personas/ symlink)", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(string(res.Stdout), "name: Victoria") {
		t.Errorf("symlink cat stdout=%q, want persona contents", res.Stdout)
	}

	// Read-only contract: a write into the mount must fail. The agent
	// should never be writing back to /opt/chat/server/personas — the
	// docs are operator-edited at the host layer.
	resWrite, err := sb.RunBash(ctx, BashRequest{
		Command:    "echo regression > personas/should-not-exist.yaml",
		WorkingDir: convDir,
	})
	if err != nil {
		t.Fatalf("RunBash write: %v", err)
	}
	if resWrite.ExitCode == 0 {
		t.Errorf("write into ReadOnlyMounts succeeded (exit=0) — should be blocked by :ro bind")
	}
}

// TestContainerWorkspaceSurvivesConcurrentMount pins the warm-pool
// regression that was reported in production logs: a non-lockdown chat
// got "Permission denied" on its own per-conversation workspace dir
// (`ls: cannot access '.': Permission denied`) and `Read-only file
// system` from a write attempt — even though the same dir worked fine in
// a sibling lockdown chat.
//
// Cause: the workspace bind used `--volume=...:rw,Z` (capital Z = a
// per-container *private* SELinux MCS label). Pool.fill spawns warm
// containers serially. Container A mounts → podman relabels the host dir
// with MCS-A. A is parked in the warm pool. Pool.fill then spawns B →
// podman relabels with MCS-B, overwriting MCS-A. When the user's next
// turn pulls A out of the pool to handle the request, A's process labels
// no longer match the dir's MCS-B, so every read fails with EACCES and
// every write fails with EROFS. Lockdown's TakeContainer always
// cold-starts so it gets the latest MCS — that's why only non-lockdown
// turns saw the bug.
//
// Fix: `:z` (lowercase) — shared label that every container shares, so
// concurrent mounts don't trample each other's labels.
//
// Test plan: stand up two containers concurrently against the same
// workspace root, then assert the FIRST container (still running)
// can still read + write its workspace AFTER the second container has
// mounted. Without the fix this fails with EACCES on read and EROFS
// on write — exactly the symptoms in the user's logs.
//
// NOTE: this assertion is only enforceable on SELinux-Enforcing hosts
// (production Fedora). On Permissive hosts (most dev laptops + this CI
// runner today) podman still emits the `:Z` chcon but the kernel logs
// denials instead of blocking, so both containers continue to work and
// the test passes regardless. Run on a Fedora host with `setenforce 1`
// to verify the regression is actually caught — that's the env the bug
// reproduces in.
func TestContainerWorkspaceSurvivesConcurrentMount(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("container backend tested on linux only")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available")
	}
	if os.Geteuid() == 0 {
		t.Skip("rootful podman doesn't match production userns mapping; run as a non-root user")
	}

	wsHost := t.TempDir()
	if err := os.Chmod(wsHost, 0o755); err != nil {
		t.Fatalf("chmod wsHost: %v", err)
	}
	convA := filepath.Join(wsHost, "conv-a")
	convB := filepath.Join(wsHost, "conv-b")
	for _, d := range []string{convA, convB} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cfg := ContainerConfig{
		Image:            "ghcr.io/elcanotek/sandbox:latest",
		WorkspaceHostDir: wsHost,
		BridgeScript:     []byte("# unused for bash test\n"),
	}

	// Container A — analogue of a warm-pool slot.
	sbA, err := NewContainer(ctx, cfg)
	if err != nil {
		t.Fatalf("NewContainer A: %v", err)
	}
	defer sbA.Close()

	// Sanity: A can write to its workspace right after spawn.
	res, err := sbA.RunBash(ctx, BashRequest{
		Command:    "touch marker_a && ls -l marker_a",
		WorkingDir: convA,
	})
	if err != nil {
		t.Fatalf("A initial bash: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("A initial bash exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}

	// Container B — analogue of the next Pool.fill spawning while A is
	// still parked in the pool. With `:Z`, this is the relabel that
	// breaks A. With `:z`, it's a no-op.
	sbB, err := NewContainer(ctx, cfg)
	if err != nil {
		t.Fatalf("NewContainer B: %v", err)
	}
	defer sbB.Close()

	// The decisive check: A can still read + write after B mounted.
	// Reading via `ls -ld` on the cwd (the exact failure shape from
	// the user logs: "ls: cannot access '.': Permission denied").
	res, err = sbA.RunBash(ctx, BashRequest{
		Command:    "ls -ld . && touch marker_a_after && cat marker_a",
		WorkingDir: convA,
	})
	if err != nil {
		t.Fatalf("A post-B bash: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("A post-B bash exit=%d stderr=%q stdout=%q (regression: warm-pool container lost workspace access when a sibling container mounted with :Z private label)",
			res.ExitCode, res.Stderr, res.Stdout)
	}

	// And B can still see its own workspace too — the symmetric case,
	// just to make sure we didn't accidentally over-restrict.
	res, err = sbB.RunBash(ctx, BashRequest{
		Command:    "touch marker_b && ls -l marker_b",
		WorkingDir: convB,
	})
	if err != nil {
		t.Fatalf("B bash: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("B bash exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
}

// TestContainerBashSamePath covers the same property for bash:
// passing the host abs path as WorkingDir means the container cwd
// matches that path verbatim.
func TestContainerBashSamePath(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("container backend tested on linux only")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available")
	}
	if os.Geteuid() == 0 {
		// See note on TestContainerWorkspaceSamePathCoherence — rootful
		// podman ignores keep-id, masking the production-mode userns
		// behavior we care about.
		t.Skip("rootful podman doesn't match production userns mapping; run as a non-root user")
	}

	wsHost := t.TempDir()
	if err := os.Chmod(wsHost, 0o755); err != nil {
		t.Fatalf("chmod wsHost: %v", err)
	}
	convDir := filepath.Join(wsHost, "conv-bash")
	// 0o755 — matches tools.EnsureWorkspaceDir's production mode. See
	// the longer note on TestContainerWorkspaceSamePathCoherence.
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatalf("mkdir convDir: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sb, err := NewContainer(ctx, ContainerConfig{
		Image:            "ghcr.io/elcanotek/sandbox:latest",
		WorkspaceHostDir: wsHost,
		BridgeScript:     []byte("# unused for bash test\n"),
	})
	if err != nil {
		t.Fatalf("NewContainer: %v", err)
	}
	defer sb.Close()

	res, err := sb.RunBash(ctx, BashRequest{
		Command:    "pwd",
		WorkingDir: convDir,
	})
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != convDir {
		t.Errorf("pwd=%q, want host path %q (same-path mount broken?)", got, convDir)
	}
}
