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

// figureCode emits one image/png via display_data using matplotlib(Agg) +
// IPython.display, so the figure is deterministic without relying on an
// inline-backend being auto-configured in the kernel.
const figureCode = `
import io
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
from IPython.display import Image, display
fig, ax = plt.subplots()
ax.plot([1, 2, 3], [1, 4, 9])
buf = io.BytesIO()
fig.savefig(buf, format="png")
display(Image(data=buf.getvalue(), format="png"))
print("FIGURE_DONE")
`

// requireHostKernel returns a host-backed sandbox with a working IPython
// kernel, or skips the test when the host python lacks jupyter_client /
// ipykernel / matplotlib (e.g. a minimal CI runner). Host mode sidesteps the
// rootless-userns workspace-write barrier that gates the container variants, so
// these run wherever the python data stack is installed.
func requireHostKernel(t *testing.T) *Sandbox {
	t.Helper()
	bridge, err := os.ReadFile("../tools/python_bridge.py")
	if err != nil {
		t.Skipf("python_bridge.py not readable: %v", err)
	}
	sb := NewHost(bridge)
	res, err := sb.RunPython(context.Background(), PythonRequest{Code: "print('ok')", Timeout: 30 * time.Second})
	if err != nil || res.Status != "success" {
		sb.Close()
		t.Skipf("host python kernel unavailable (need jupyter_client+ipykernel): err=%v status=%q", err, res.Status)
	}
	return sb
}

func TestHostPython_InlineFigure(t *testing.T) {
	sb := requireHostKernel(t)
	defer sb.Close()
	tmp := t.TempDir()

	res, err := sb.RunPython(context.Background(), PythonRequest{
		Code: figureCode, Timeout: 60 * time.Second, WorkspaceDir: tmp,
	})
	if err != nil {
		t.Fatalf("RunPython: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("status=%q stderr=%q error=%q", res.Status, res.Stderr, res.Error)
	}
	if len(res.ImageFiles) != 1 {
		t.Fatalf("ImageFiles = %v, want exactly 1 captured figure", res.ImageFiles)
	}
	rel := res.ImageFiles[0]
	if !strings.HasPrefix(rel, "figures/") || !strings.HasSuffix(rel, ".png") {
		t.Errorf("ImageFiles[0] = %q, want a figures/*.png workspace-relative path", rel)
	}
	if strings.Contains(res.Stdout, "iVBOR") || strings.Contains(res.Output, "iVBOR") {
		t.Error("base64 PNG data leaked into the model-visible tool result")
	}
	assertSavedPNG(t, filepath.Join(tmp, rel))
}

func TestHostPython_ResetKernel(t *testing.T) {
	sb := requireHostKernel(t)
	defer sb.Close()

	if _, err := sb.RunPython(context.Background(), PythonRequest{
		Code: "kept = 12345\nprint('set')", Timeout: 30 * time.Second,
	}); err != nil {
		t.Fatalf("RunPython set: %v", err)
	}
	res, err := sb.RunPython(context.Background(), PythonRequest{
		Code: "print(kept)", ResetKernel: true, Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunPython after reset: %v", err)
	}
	if res.Status != "error" {
		t.Errorf("status=%q, want error after reset wiped `kept` (stdout=%q)", res.Status, res.Stdout)
	}
	if !strings.Contains(res.Error, "kept") && !strings.Contains(res.Error, "NameError") {
		t.Errorf("error=%q, want a NameError mentioning `kept`", res.Error)
	}
	res2, err := sb.RunPython(context.Background(), PythonRequest{
		Code: "print(2 + 2)", Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunPython post-reset: %v", err)
	}
	if res2.Status != "success" || !strings.Contains(res2.Stdout, "4") {
		t.Errorf("post-reset run status=%q stdout=%q, want success with '4'", res2.Status, res2.Stdout)
	}
}

// TestContainerPython_InlineFigure is the rootless-container counterpart of the
// host figure test. It needs the in-container sandbox uid to own the bind-mount,
// which only holds under rootless podman + keep-id (production), so it skips
// under rootful podman like the other workspace-write tests in this package.
func TestContainerPython_InlineFigure(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("container backend tested on linux only")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available")
	}
	if os.Geteuid() == 0 {
		t.Skip("rootful podman ignores keep-id, so the sandbox uid can't write the bind-mounted workspace; run as a non-root user")
	}
	bridge, err := os.ReadFile("../tools/python_bridge.py")
	if err != nil {
		t.Skipf("python_bridge.py not readable: %v", err)
	}
	wsHost := t.TempDir()
	// t.TempDir() is 0o700; match production's 0o755 so the keep-id-mapped
	// sandbox uid can traverse + write the workspace.
	if err := os.Chmod(wsHost, 0o755); err != nil {
		t.Fatalf("chmod wsHost: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	sb, err := NewContainer(ctx, ContainerConfig{
		Image:            testImage(),
		StartTimeout:     90 * time.Second,
		WorkspaceHostDir: wsHost,
		BridgeScript:     bridge,
	})
	if err != nil {
		t.Fatalf("NewContainer: %v", err)
	}
	defer sb.Close()

	res, err := sb.RunPython(ctx, PythonRequest{
		Code: figureCode, Timeout: 90 * time.Second, WorkspaceDir: wsHost,
	})
	if err != nil {
		t.Fatalf("RunPython: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("status=%q stderr=%q error=%q", res.Status, res.Stderr, res.Error)
	}
	if len(res.ImageFiles) != 1 {
		t.Fatalf("ImageFiles = %v, want exactly 1 captured figure", res.ImageFiles)
	}
	assertSavedPNG(t, filepath.Join(wsHost, res.ImageFiles[0]))
}

// TestContainerPython_ResetKernel proves reset_kernel=true wipes kernel state
// before running the supplied code, then a fresh kernel works. It writes no
// workspace files, so it runs under rootful podman too.
func TestContainerPython_ResetKernel(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("container backend tested on linux only")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available")
	}
	bridge, err := os.ReadFile("../tools/python_bridge.py")
	if err != nil {
		t.Skipf("python_bridge.py not readable: %v", err)
	}
	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	sb, err := NewContainer(ctx, ContainerConfig{
		Image:            testImage(),
		WorkspaceHostDir: tmp,
		BridgeScript:     bridge,
	})
	if err != nil {
		t.Fatalf("NewContainer: %v", err)
	}
	defer sb.Close()

	if _, err := sb.RunPython(context.Background(), PythonRequest{
		Code: "kept = 12345\nprint('set')", Timeout: 60 * time.Second,
	}); err != nil {
		t.Fatalf("RunPython set: %v", err)
	}
	res, err := sb.RunPython(context.Background(), PythonRequest{
		Code: "print(kept)", ResetKernel: true, Timeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunPython after reset: %v", err)
	}
	if res.Status != "error" {
		t.Errorf("status=%q, want error after reset wiped `kept` (stdout=%q)", res.Status, res.Stdout)
	}
	if !strings.Contains(res.Error, "kept") && !strings.Contains(res.Error, "NameError") {
		t.Errorf("error=%q, want a NameError mentioning `kept`", res.Error)
	}
	res2, err := sb.RunPython(context.Background(), PythonRequest{
		Code: "print(2 + 2)", Timeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunPython post-reset: %v", err)
	}
	if res2.Status != "success" || !strings.Contains(res2.Stdout, "4") {
		t.Errorf("post-reset run status=%q stdout=%q, want success with '4'", res2.Status, res2.Stdout)
	}
}

// countKernelsCode walks /proc and counts LIVE ipykernel processes (a SIGKILLed
// but not-yet-reaped orphan has an empty cmdline and is not counted).
const countKernelsCode = `
import glob
n = 0
for p in glob.glob('/proc/[0-9]*/cmdline'):
    try:
        with open(p, 'rb') as f:
            if b'ipykernel_launcher' in f.read():
                n += 1
    except OSError:
        pass
print('KERNELS', n)
`

// TestContainerPython_ReapsOrphanedKernelOnCancel proves the central persistent-
// mode lifecycle fix (#213): a cancelled cell tears down the bridge and orphans
// its kernel inside the SURVIVING container, but the next bridge start sweeps
// the orphan (reap_stale_kernels), so live kernels never accumulate. Without the
// reap this would observe 2 live kernels (the orphan + the new one).
func TestContainerPython_ReapsOrphanedKernelOnCancel(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("container backend tested on linux only")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available")
	}
	bridge, err := os.ReadFile("../tools/python_bridge.py")
	if err != nil {
		t.Skipf("python_bridge.py not readable: %v", err)
	}
	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	sb, err := NewContainer(ctx, ContainerConfig{
		Image:            testImage(),
		WorkspaceHostDir: tmp,
		BridgeScript:     bridge,
	})
	if err != nil {
		t.Fatalf("NewContainer: %v", err)
	}
	defer sb.Close()

	// Start the first kernel.
	if _, err := sb.RunPython(context.Background(), PythonRequest{Code: "print('k1')", Timeout: 60 * time.Second}); err != nil {
		t.Fatalf("RunPython k1: %v", err)
	}

	// Cancel a long-running cell mid-flight (simulates registerTurn preempting a
	// turn): the per-call context fires while req.Timeout is far away, so the
	// ctx.Done branch tears the bridge down and orphans the sleeping kernel.
	cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Second)
	_, _ = sb.RunPython(cctx, PythonRequest{Code: "import time; time.sleep(30)", Timeout: 60 * time.Second})
	ccancel()

	// Next call starts a fresh bridge, which reaps the orphan before its own
	// kernel boots. Exactly one live kernel should remain.
	res, err := sb.RunPython(context.Background(), PythonRequest{Code: countKernelsCode, Timeout: 60 * time.Second})
	if err != nil {
		t.Fatalf("RunPython count: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("count status=%q stderr=%q error=%q", res.Status, res.Stderr, res.Error)
	}
	if !strings.Contains(res.Stdout, "KERNELS 1") {
		t.Errorf("expected exactly 1 live kernel after a cancelled cell (orphan reaped), got stdout=%q", res.Stdout)
	}
}

func assertSavedPNG(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read saved figure %s: %v", path, err)
	}
	if len(data) < 8 || string(data[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Errorf("saved figure %s is not a PNG (first bytes %x)", path, data[:min(8, len(data))])
	}
}
