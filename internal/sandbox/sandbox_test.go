package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestHostBashEcho exercises the host backend end-to-end: a real
// bash subprocess runs and we get stdout back.
func TestHostBashEcho(t *testing.T) {
	sb := NewHost(nil)
	defer sb.Close()

	res, err := sb.RunBash(context.Background(), BashRequest{
		Command: "echo hello",
	})
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "hello" {
		t.Errorf("Stdout = %q, want %q", got, "hello")
	}
}

func TestHostBashExitCode(t *testing.T) {
	sb := NewHost(nil)
	defer sb.Close()

	res, err := sb.RunBash(context.Background(), BashRequest{
		Command: "exit 42",
	})
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	if res.ExitCode != 42 {
		t.Errorf("ExitCode = %d, want 42", res.ExitCode)
	}
}

func TestHostBashTimeout(t *testing.T) {
	sb := NewHost(nil)
	defer sb.Close()

	start := time.Now()
	res, err := sb.RunBash(context.Background(), BashRequest{
		Command: "sleep 10",
		Timeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	if !res.TimedOut {
		t.Errorf("expected TimedOut=true, got %+v", res)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("timeout took too long: %v", elapsed)
	}
}

func TestHostBashWorkingDir(t *testing.T) {
	tmp := t.TempDir()
	sb := NewHost(nil)
	defer sb.Close()

	res, err := sb.RunBash(context.Background(), BashRequest{
		Command:    "pwd",
		WorkingDir: tmp,
	})
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != tmp {
		// Some platforms (macOS) symlink /tmp → /private/tmp; tolerate
		// either form by checking the suffix.
		if !strings.HasSuffix(got, tmp) {
			t.Errorf("Stdout = %q, want %q", got, tmp)
		}
	}
}

func TestSandboxClosedReturnsErrClosed(t *testing.T) {
	sb := NewHost(nil)
	sb.Close()

	if _, err := sb.RunBash(context.Background(), BashRequest{Command: "echo x"}); !errors.Is(err, ErrClosed) {
		t.Errorf("RunBash after Close: err = %v, want ErrClosed", err)
	}
}

func TestSandboxModeName(t *testing.T) {
	sb := NewHost(nil)
	defer sb.Close()
	if got := sb.ModeName(); got != "host" {
		t.Errorf("ModeName = %q, want %q", got, "host")
	}
}

func TestPoolHostMode(t *testing.T) {
	p := NewPool(PoolConfig{
		Size: 2,
		Mode: ModeHost,
	})
	defer p.Close()

	// Host-mode sandboxes are cheap to construct, so even without
	// waiting for the warm goroutine we should always get one.
	sb1, cleanup1, err := p.Take()
	if err != nil {
		t.Fatalf("Take 1: %v", err)
	}
	defer cleanup1()

	sb2, cleanup2, err := p.Take()
	if err != nil {
		t.Fatalf("Take 2: %v", err)
	}
	defer cleanup2()

	if sb1 == sb2 {
		t.Errorf("Pool returned the same sandbox twice — per-turn isolation broken")
	}

	res, err := sb1.RunBash(context.Background(), BashRequest{Command: "echo pool"})
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "pool" {
		t.Errorf("Stdout = %q, want %q", got, "pool")
	}
}

func TestPoolDisabledFallsThroughToColdStart(t *testing.T) {
	p := NewPool(PoolConfig{Size: 0, Mode: ModeHost})
	defer p.Close()

	sb, cleanup, err := p.Take()
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	defer cleanup()

	if sb == nil {
		t.Fatal("Take with Size=0 returned nil sandbox")
	}
}

// defaultTestImage is the image our container integration tests use
// when FLEET_SANDBOX_TEST_IMAGE (and the CHAT_SANDBOX_TEST_IMAGE
// back-compat alias) are unset. It's the generic build-on-box artifact
// the default client bundle produces — build it first with
// `scripts/build-sandbox-image.sh` (config/default/sandbox/Containerfile,
// a vanilla fedora-minimal base). fleet's core ships no vendor-published
// sandbox image, so the default here is local-only (no implicit registry
// pull); point FLEET_SANDBOX_TEST_IMAGE at another ref to override.
const defaultTestImage = "localhost/fleet-sandbox:latest"

// sandboxEnv reads the canonical FLEET_SANDBOX_* variable, falling back
// to the legacy CHAT_SANDBOX_* name for back-compat.
func sandboxEnv(name string) string {
	if v := os.Getenv("FLEET_SANDBOX_" + name); v != "" {
		return v
	}
	return os.Getenv("CHAT_SANDBOX_" + name)
}

func testImage() string {
	if img := sandboxEnv("TEST_IMAGE"); img != "" {
		return img
	}
	return defaultTestImage
}

// TestContainerBashEcho is a podman-gated integration test. Skipped
// when the platform is not linux or `podman` is not on PATH. Image
// defaults to the locally-built default-bundle sandbox
// (localhost/fleet-sandbox:latest); override with
// $FLEET_SANDBOX_TEST_IMAGE when iterating locally.
func TestContainerBashEcho(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("container backend tested on linux only")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available")
	}
	image := testImage()

	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sb, err := NewContainer(ctx, ContainerConfig{
		Image:            image,
		WorkspaceHostDir: tmp,
		BridgeScript:     []byte("# unused for bash-only test\n"),
	})
	if err != nil {
		t.Fatalf("NewContainer: %v", err)
	}
	defer sb.Close()

	res, err := sb.RunBash(context.Background(), BashRequest{
		Command: "echo hello-from-container",
	})
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(string(res.Stdout), "hello-from-container") {
		t.Errorf("Stdout = %q, missing greeting", res.Stdout)
	}
}

// TestContainerBridgeDirHonored is a regression for a production
// outage: PrivateTmp=true in chat-server.service combined with
// rootless-podman OCI helpers (which can leave the unit's mount
// namespace via logind reparenting) made `--volume=/tmp/bridge.py:...`
// fail with `statfs ... no such file or directory`. The fix is to
// route the bridge tempfile out of /tmp via ContainerConfig.BridgeDir.
// This test pins that contract so the path can't silently regress
// back to /tmp.
func TestContainerBridgeDirHonored(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("container backend tested on linux only")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available")
	}

	workspaceDir := t.TempDir()
	bridgeDir := filepath.Join(t.TempDir(), "sandbox-bridge")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sb, err := NewContainer(ctx, ContainerConfig{
		Image:            testImage(),
		WorkspaceHostDir: workspaceDir,
		BridgeScript:     []byte("# bridge-dir-honored test\n"),
		BridgeDir:        bridgeDir,
	})
	if err != nil {
		t.Fatalf("NewContainer: %v", err)
	}
	defer sb.Close()

	impl, ok := sb.impl.(*containerImpl)
	if !ok {
		t.Fatalf("sandbox impl is not *containerImpl")
	}
	if !strings.HasPrefix(impl.bridgeScriptPath, bridgeDir+string(os.PathSeparator)) {
		t.Errorf("bridgeScriptPath = %q, want a child of BridgeDir %q (regression: PrivateTmp+rootless-podman would hide a /tmp-resident bridge file from the OCI helpers)", impl.bridgeScriptPath, bridgeDir)
	}
}

// TestPoolContainerFailureSurfacesToCaller pins the failure-surface
// contract that replaced the old graceful-degradation behavior. A
// misconfigured container backend (here: a known-bogus image ref) must
// return errors to every Take() — there is no host-mode fallback in
// production, because letting agent-emitted code escape the container
// is the whole risk we're guarding against. The agent surfaces the
// error as a tool-call failure ("sandbox unavailable") and the
// operator sees the underlying podman error in chat-server logs.
func TestPoolContainerFailureSurfacesToCaller(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("container backend tested on linux only")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available")
	}
	tmp := t.TempDir()

	p := NewPool(PoolConfig{
		Size:         0, // disable warming so Take cold-starts each call
		Mode:         ModeContainer,
		BridgeScript: []byte("# unused\n"),
		Container: ContainerConfig{
			Image:            "localhost/sandbox-this-image-does-not-exist-on-purpose:nope",
			WorkspaceHostDir: tmp,
			StartTimeout:     5 * time.Second,
		},
	})
	defer p.Close()

	// Every Take must return an error — no degradation, no fallback.
	for i := 0; i < 5; i++ {
		_, _, err := p.Take()
		if err == nil {
			t.Fatalf("Take attempt %d: expected container error, got nil — host-mode fallback was removed and should not return", i+1)
		}
	}
}

// TestPoolContainerMode exercises the full production path: a warm
// pool of container-backed sandboxes, Take()→RunBash and Take()→RunPython,
// per-turn isolation between two consecutive Takes.
func TestPoolContainerMode(t *testing.T) {
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

	p := NewPool(PoolConfig{
		Size:         1,
		Mode:         ModeContainer,
		BridgeScript: bridge,
		Container: ContainerConfig{
			Image:            testImage(),
			WorkspaceHostDir: tmp,
		},
	})
	defer p.Close()

	// First Take: stash kernel state in this turn's sandbox.
	sb1, cleanup1, err := p.Take()
	if err != nil {
		t.Fatalf("Take 1: %v", err)
	}
	res, err := sb1.RunPython(context.Background(), PythonRequest{
		Code:    "df_marker = 'turn-1'\nprint('hello from', df_marker)",
		Timeout: 60 * time.Second,
	})
	cleanup1() // fire teardown immediately so the next Take can't share
	if err != nil {
		t.Fatalf("RunPython turn 1: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("turn 1 status=%q stderr=%q error=%q", res.Status, res.Stderr, res.Error)
	}
	if !strings.Contains(res.Stdout, "turn-1") {
		t.Errorf("turn 1 stdout=%q", res.Stdout)
	}

	// Second Take: a fresh sandbox. df_marker from turn 1 must NOT be
	// in scope — that's the per-turn isolation guarantee. The OpenAI
	// 2024 cross-conversation file leak was exactly this property
	// breaking; we test for it explicitly.
	sb2, cleanup2, err := p.Take()
	if err != nil {
		t.Fatalf("Take 2: %v", err)
	}
	defer cleanup2()
	if sb1 == sb2 {
		t.Errorf("Take returned the same sandbox twice — per-turn isolation broken")
	}
	res2, err := sb2.RunPython(context.Background(), PythonRequest{
		Code:    "try:\n    print('LEAKED:', df_marker)\nexcept NameError:\n    print('OK: df_marker not defined')",
		Timeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunPython turn 2: %v", err)
	}
	if !strings.Contains(res2.Stdout, "OK: df_marker not defined") {
		t.Errorf("turn 2 saw turn 1's state — stdout=%q", res2.Stdout)
	}
}

// TestContainerPythonBridge runs the actual python_bridge.py inside the
// container and exercises the IPython kernel through it: import pandas,
// build a DataFrame, verify state persists across two RunPython calls
// in the same sandbox.
//
// Reads the bridge from server/internal/tools/python_bridge.py via a
// small relative read (we don't have the //go:embed in this package).
// Skipped if the bridge file isn't where we expect.
func TestContainerPythonBridge(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("container backend tested on linux only")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available")
	}
	image := testImage()

	bridge, err := os.ReadFile("../tools/python_bridge.py")
	if err != nil {
		t.Skipf("python_bridge.py not readable: %v", err)
	}

	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	sb, err := NewContainer(ctx, ContainerConfig{
		Image:            image,
		WorkspaceHostDir: tmp,
		BridgeScript:     bridge,
	})
	if err != nil {
		t.Fatalf("NewContainer: %v", err)
	}
	defer sb.Close()

	// First call: import pandas and stash a DataFrame in the kernel.
	res1, err := sb.RunPython(context.Background(), PythonRequest{
		Code: "import pandas as pd\n" +
			"df = pd.DataFrame({'a':[1,2,3]})\n" +
			"print(len(df))",
		Timeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunPython 1: %v", err)
	}
	if res1.Status != "success" {
		t.Fatalf("RunPython 1 status=%q stderr=%q error=%q", res1.Status, res1.Stderr, res1.Error)
	}
	if !strings.Contains(res1.Stdout, "3") {
		t.Errorf("RunPython 1 stdout=%q, want '3'", res1.Stdout)
	}

	// Second call in the same sandbox: confirm `df` is still defined.
	// This proves the IPython kernel persists across RunPython calls
	// within one sandbox lifetime — the whole reason we keep the bridge
	// alive for the turn rather than spawning per-call.
	res2, err := sb.RunPython(context.Background(), PythonRequest{
		Code:       "print(df['a'].sum())",
		ReturnVars: []string{"df"},
		Timeout:    60 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunPython 2: %v", err)
	}
	if res2.Status != "success" {
		t.Fatalf("RunPython 2 status=%q stderr=%q error=%q", res2.Status, res2.Stderr, res2.Error)
	}
	if !strings.Contains(res2.Stdout, "6") {
		t.Errorf("RunPython 2 stdout=%q, want sum '6' (kernel state lost?)", res2.Stdout)
	}
}

// TestContainerImageHasExpectedPackages imports each of the libraries
// the agent's tool-description promises are pre-installed. If a future
// image bump removes one without updating the description, this test
// catches it before the LLM does.
func TestContainerImageHasExpectedPackages(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
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

	// One import per package the run_python tool description claims is
	// pre-installed. Any failure here means the LLM will hit an
	// ImportError at runtime — block the change.
	res, err := sb.RunPython(context.Background(), PythonRequest{
		Code: `
import importlib
mods = [
    "pandas", "numpy", "scipy", "pyarrow",
    "matplotlib", "seaborn",
    "sklearn",
    "openpyxl", "xlsxwriter", "pypdf", "reportlab",
    "PIL",
    "bs4", "lxml", "yaml", "requests", "tabulate",
]
missing = []
for m in mods:
    try:
        importlib.import_module(m)
    except Exception as e:
        missing.append(f"{m}: {e}")
if missing:
    raise SystemExit("MISSING:\n" + "\n".join(missing))
print("all_imports_ok")
`,
		Timeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatalf("RunPython: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("status=%q stdout=%q stderr=%q error=%q", res.Status, res.Stdout, res.Stderr, res.Error)
	}
	if !strings.Contains(res.Stdout, "all_imports_ok") {
		t.Errorf("expected all imports ok; stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
}

// TestContainerBashHasExpectedTools mirrors the import test for the CLI
// tools the bash tool description promises.
func TestContainerBashHasExpectedTools(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("container backend tested on linux only")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available")
	}
	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	sb, err := NewContainer(ctx, ContainerConfig{
		Image:            testImage(),
		WorkspaceHostDir: tmp,
		BridgeScript:     []byte("# unused for bash-only test\n"),
	})
	if err != nil {
		t.Fatalf("NewContainer: %v", err)
	}
	defer sb.Close()

	tools := []string{
		"bash", "ls", "cat", "grep", "sed", "find",
		"git", "jq", "less", "rg",
		"pandoc", "convert", "identify",
		"python3",
	}
	cmd := "for t in " + strings.Join(tools, " ") + "; do command -v \"$t\" >/dev/null || echo MISSING:$t; done; echo done"
	res, err := sb.RunBash(context.Background(), BashRequest{Command: cmd, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	if strings.Contains(string(res.Stdout), "MISSING:") {
		t.Errorf("missing CLI tool(s); stdout=%q", res.Stdout)
	}
	if !strings.Contains(string(res.Stdout), "done") {
		t.Errorf("loop did not complete; stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
}

// TestContainerNetworkNone proves NoNetwork=true (the lockdown path)
// blocks egress. The default — and what non-lockdown chats get — is
// slirp4netns with outbound HTTP allowed (so `pip install` and curl
// work in routine analysis flows). Skipped under the same conditions
// as TestContainer.
func TestContainerNetworkNone(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("container backend tested on linux only")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available")
	}
	image := testImage()

	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sb, err := NewContainer(ctx, ContainerConfig{
		Image:            image,
		WorkspaceHostDir: tmp,
		BridgeScript:     []byte("# unused\n"),
		NoNetwork:        true, // lockdown contract
	})
	if err != nil {
		t.Fatalf("NewContainer: %v", err)
	}
	defer sb.Close()

	// Try a DNS lookup; with --network=none even loopback DNS fails.
	// `getent hosts` exits non-zero and prints nothing on failure;
	// we accept any non-zero exit as proof the network namespace is
	// empty.
	res, err := sb.RunBash(context.Background(), BashRequest{
		Command: "getent hosts example.com || echo NO_NETWORK",
	})
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	if !strings.Contains(string(res.Stdout), "NO_NETWORK") {
		t.Errorf("expected DNS lookup to fail with NoNetwork=true, got stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
}

// TestContainerNetworkDefaultAllowsEgress is the symmetric assertion:
// without NoNetwork (i.e. the non-lockdown path), DNS resolution and
// outbound HTTP via curl reach the public internet. Pinned so a future
// hardening pass that re-adds `--network=none` to the default path
// fails this test instead of silently breaking `pip install` for every
// data-analysis chat.
func TestContainerNetworkDefaultAllowsEgress(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("container backend tested on linux only")
	}
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not available")
	}
	if testing.Short() {
		t.Skip("hits the public internet; skip under -short")
	}
	image := testImage()

	tmp := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sb, err := NewContainer(ctx, ContainerConfig{
		Image:            image,
		WorkspaceHostDir: tmp,
		BridgeScript:     []byte("# unused\n"),
		// NoNetwork defaults to false — slirp4netns default applies.
	})
	if err != nil {
		t.Fatalf("NewContainer: %v", err)
	}
	defer sb.Close()

	// example.com is the canonical IETF reserved test host. Avoid
	// flakiness: a 5s connect timeout and -sS (silent except errors)
	// so the test exits fast on a sandboxed CI runner without egress.
	// Probe egress from the HOST first. If the host itself can't reach the
	// target, skip (environmental — e.g. a no-internet CI runner). If the host
	// CAN reach it but the SANDBOX cannot, that is exactly the regression this
	// test guards (the default sandbox must allow egress) — so fail, don't skip.
	if out, herr := exec.Command("curl", "-sS", "--max-time", "5", "-o", "/dev/null", "-w", "%{http_code}", "https://example.com").Output(); herr != nil || strings.TrimSpace(string(out)) != "200" {
		t.Skipf("host has no egress to example.com (code=%q err=%v) — cannot distinguish from a sandbox block", strings.TrimSpace(string(out)), herr)
	}

	res, err := sb.RunBash(context.Background(), BashRequest{
		Command: "curl -sS --max-time 5 -o /dev/null -w '%{http_code}\\n' https://example.com",
	})
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	got := strings.TrimSpace(string(res.Stdout))
	if got != "200" {
		t.Errorf("default sandbox blocked egress (curl returned %q stderr=%q) but the host has egress — the default sandbox must allow it", got, res.Stderr)
	}
}
