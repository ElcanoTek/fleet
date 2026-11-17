package tools

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaybeAddEmptyOutputHint(t *testing.T) {
	cases := []struct {
		name     string
		resp     pythonResponse
		wantHint bool
	}{
		{
			name:     "all empty success gets hint",
			resp:     pythonResponse{Status: "success"},
			wantHint: true,
		},
		{
			name:     "whitespace-only stdout still counts as empty",
			resp:     pythonResponse{Status: "success", Output: "\n", Stdout: "\n"},
			wantHint: true,
		},
		{
			name:     "stdout present",
			resp:     pythonResponse{Status: "success", Output: "42", Stdout: "42"},
			wantHint: false,
		},
		{
			name:     "vars returned",
			resp:     pythonResponse{Status: "success", Vars: map[string]any{"payload": "abc"}},
			wantHint: false,
		},
		{
			name:     "stderr carries its own signal",
			resp:     pythonResponse{Status: "success", Stderr: "warning: deprecated"},
			wantHint: false,
		},
		{
			name:     "error carries its own signal",
			resp:     pythonResponse{Status: "error", Error: "NameError: x"},
			wantHint: false,
		},
		{
			name:     "rendered figure counts as a trace",
			resp:     pythonResponse{Status: "success", ImageFiles: []string{"figures/fig-abc.png"}},
			wantHint: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			maybeAddEmptyOutputHint(&tc.resp)
			if got := tc.resp.Hint != ""; got != tc.wantHint {
				t.Errorf("hint set = %v, want %v (hint=%q)", got, tc.wantHint, tc.resp.Hint)
			}
			if tc.wantHint && !strings.Contains(tc.resp.Hint, "NO output") {
				t.Errorf("hint text unexpected: %q", tc.resp.Hint)
			}
		})
	}
}

func TestPythonBridgeKernelBootstrapBlocksGitPush(t *testing.T) {
	pyBin, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed on host")
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed on host")
	}

	tmpDir := t.TempDir()
	bridgePath := filepath.Join(tmpDir, "python_bridge.py")
	if err := os.WriteFile(bridgePath, PythonBridgeScript(), 0o600); err != nil {
		t.Fatalf("write python_bridge.py: %v", err)
	}

	stubDir := filepath.Join(tmpDir, "stub_pkg")
	if err := os.MkdirAll(stubDir, 0o700); err != nil {
		t.Fatalf("mkdir stub_pkg: %v", err)
	}
	stubLauncher := `import ctypes
import os
import shutil
import subprocess
import sys

conn_file = os.environ["TEST_CONN_FILE"]
work = os.environ["TEST_WORK_REPO"]
remote = os.environ["TEST_REMOTE_REPO"]
git_bin = shutil.which("git")

if sys.argv != ["ipykernel_launcher", "-f", conn_file]:
    raise SystemExit(f"unexpected sys.argv: {sys.argv!r}")

if os.path.exists("/proc/self/cmdline"):
    with open("/proc/self/cmdline", "rb") as f:
        if b"ipykernel_launcher" not in f.read():
            raise SystemExit("cmdline missing ipykernel_launcher marker")

def expect_permission_error(label, fn):
    try:
        fn()
    except PermissionError as exc:
        if "REFUSED" not in str(exc):
            raise SystemExit(f"{label}: unexpected PermissionError: {exc}")
        return
    raise SystemExit(f"{label}: expected PermissionError, succeeded")

expect_permission_error(
    "direct subprocess git push",
    lambda: subprocess.run(["git", "-C", work, "push", "origin", "HEAD"], check=True),
)
expect_permission_error(
    "concatenated subprocess git push",
    lambda: subprocess.run(["git", "-C", work, "pu" + "sh", "origin", "HEAD"], check=True),
)
expect_permission_error(
    "spoofed argv0 executable git",
    lambda: subprocess.Popen(["harmless", "-C", work, "push", "origin", "HEAD"], executable=git_bin),
)
expect_permission_error(
    "os.system git push",
    lambda: os.system(f"git -C {work} push origin HEAD"),
)
expect_permission_error(
    "stripped child env",
    lambda: subprocess.run(["echo", "stripped"], env={"PATH": os.environ.get("PATH", "")}, check=True),
)
expect_permission_error(
    "delete GIT_CONFIG_COUNT",
    lambda: os.environ.pop("GIT_CONFIG_COUNT"),
)
expect_permission_error(
    "ctypes libc system",
    lambda: getattr(ctypes.CDLL(None), "system"),
)

expect_permission_error(
    "shell variable assignment git push",
    lambda: subprocess.run(["sh", "-c", f'g=git; p=push; "$g" -C "{work}" "$p" origin HEAD'], check=True),
)

# Even if shell quote-splitting hides both "git" and "push" words from argv
# inspection, the inherited GIT_CONFIG_* pushInsteadOf transport block still
# rejects pushing inside Git itself.
obfuscated = subprocess.run(
    ["sh", "-c", f'g=gi""t; p=pu""sh; "$g" -C "{work}" "$p" origin HEAD'],
    capture_output=True,
    text=True,
)
if obfuscated.returncode == 0 or "fleet-approval-required" not in obfuscated.stderr:
    raise SystemExit(f"obfuscated shell push unexpectedly succeeded: rc={obfuscated.returncode} stderr={obfuscated.stderr!r}")

# Benign Git read/local operations and env extensions continue to work.
for benign in (
    ["git", "-C", work, "status", "--short"],
    ["git", "-C", work, "log", "-n", "1"],
    ["git", "-C", work, "diff"],
    ["git", "-C", work, "fetch", "origin"],
    ["git", "-C", work, "commit", "--allow-empty", "-m", "push button fix"],
):
    subprocess.run(benign, check=True, capture_output=True, text=True)

subprocess.run(
    ["echo", "ok"],
    env={**os.environ, "EXTRA_ALLOWED_VAR": "1"},
    check=True,
    capture_output=True,
    text=True,
)
print("KERNEL_GUARD_VERIFIED")
`
	if err := os.WriteFile(filepath.Join(stubDir, "ipykernel_launcher.py"), []byte(stubLauncher), 0o600); err != nil {
		t.Fatalf("write stub ipykernel_launcher.py: %v", err)
	}

	remoteRepo := filepath.Join(tmpDir, "remote.git")
	workRepo := filepath.Join(tmpDir, "work")
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), gitBin, args...)
		out, runErr := cmd.CombinedOutput()
		if runErr != nil {
			t.Fatalf("git %v failed: %v\n%s", args, runErr, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "--bare", remoteRepo)
	runGit("clone", remoteRepo, workRepo)
	runGit("-C", workRepo, "config", "user.email", "test@example.com")
	runGit("-C", workRepo, "config", "user.name", "Test")
	runGit("-C", workRepo, "commit", "--allow-empty", "-m", "initial")
	runGit("-C", workRepo, "push", "origin", "HEAD")
	initialRemoteHead := runGit("--git-dir", remoteRepo, "rev-parse", "HEAD")
	runGit("-C", workRepo, "commit", "--allow-empty", "-m", "unpushed-commit")

	connFile := filepath.Join(tmpDir, "kernel-test.json")
	harness := `import os, subprocess, sys
sys.path.insert(0, os.environ["TEST_BRIDGE_DIR"])
import python_bridge

base_env = dict(os.environ)
base_env["PYTHONPATH"] = os.environ["TEST_STUB_DIR"] + os.pathsep + base_env.get("PYTHONPATH", "")
cmd, env = python_bridge.build_kernel_launch_spec(os.environ["TEST_CONN_FILE"], base_env=base_env)
proc = subprocess.run(cmd, env=env, capture_output=True, text=True)
if proc.returncode != 0:
    sys.stderr.write(proc.stdout + "\n" + proc.stderr)
    raise SystemExit(proc.returncode)
print(proc.stdout.strip())
`
	cmd := exec.CommandContext(t.Context(), pyBin, "-c", harness)
	cmd.Env = append(os.Environ(),
		"TEST_BRIDGE_DIR="+tmpDir,
		"TEST_STUB_DIR="+stubDir,
		"TEST_CONN_FILE="+connFile,
		"TEST_WORK_REPO="+workRepo,
		"TEST_REMOTE_REPO="+remoteRepo,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kernel bootstrap guard harness failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "KERNEL_GUARD_VERIFIED") {
		t.Fatalf("expected KERNEL_GUARD_VERIFIED, got: %s", out)
	}

	afterRemoteHead := runGit("--git-dir", remoteRepo, "rev-parse", "HEAD")
	if afterRemoteHead != initialRemoteHead {
		t.Fatalf("remote HEAD advanced from %s to %s despite run_python git push guard", initialRemoteHead, afterRemoteHead)
	}
}
