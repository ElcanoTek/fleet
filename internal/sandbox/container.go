package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// containerImpl is the production backend: rootless Podman container,
// --read-only rootfs, dropped caps, capped memory/cpu/pids. Both the
// bash tool and the run_python bridge run inside the container; the
// workspace root is bind-mounted in. Network egress is controlled by
// ContainerConfig.NoNetwork — lockdown turns set it (sealed namespace),
// non-lockdown turns leave it off so `pip install` / outbound HTTP work.
//
// Lifecycle: start one container in `podman run -d sleep infinity` mode;
// the bridge is launched lazily on the first runPython call via
// `podman exec -i`, and that exec session's stdin/stdout is held for
// subsequent runPython calls in the turn. bash calls use one-shot
// `podman exec` per invocation. Close kills the container, which reaps
// every exec'd process inside.
type containerImpl struct {
	cfg ContainerConfig

	// containerID is the random ULID-ish name we assigned at start.
	// Using a name (not the SHA) keeps `podman ps` readable.
	containerID string

	// bridge state — lazily initialized on first runPython call.
	mu               sync.Mutex
	bridgeCmd        *exec.Cmd
	bridgeStdin      io.WriteCloser
	bridgeStdout     *bufio.Reader
	bridgeStderr     *syncBuffer // captured `podman exec` stderr — used to surface the real reason on broken-pipe write failures
	bridgeStarted    bool
	bridgeScriptPath string // host-side temp file (bind-mounted into container)
}

// syncBuffer is a goroutine-safe wrapper around bytes.Buffer. We need
// it because `podman exec`'s stderr is read in the background and the
// runPython path reads the captured bytes when surfacing an error —
// without the lock, that's a data race the race detector trips on.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) Snapshot() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// ContainerConfig configures one Sandbox container. Most fields have
// sensible defaults; only Image and WorkspaceHostDir are required.
type ContainerConfig struct {
	// Image is the container image reference, e.g.
	// "elcanotek/chat-sandbox:v1" or "localhost/chat-sandbox:dev".
	Image string

	// WorkspaceHostDir is the host path to the workspace ROOT (e.g.
	// /opt/chat/workspace). It is bind-mounted at the SAME absolute
	// path inside the container so absolute paths mean the same thing
	// on both sides — that's what lets MCP-returned paths (host-side
	// subprocesses) resolve cleanly inside bash and run_python without
	// a translation layer. Per-conversation subdirs are addressed via
	// WorkingDir / WorkspaceDir on individual calls.
	WorkspaceHostDir string

	// BridgeScript is the contents of python_bridge.py (embedded in the
	// Go binary). Written to a host temp file at sandbox start and
	// bind-mounted read-only at /opt/bridge/bridge.py.
	BridgeScript []byte

	// BridgeDir is the host directory where the bridge script tempfile
	// lives before being bind-mounted into the container. Defaults to
	// os.TempDir(). Override to escape systemd's PrivateTmp= namespace
	// (the rootless-podman OCI helpers can leave the unit's mount
	// namespace and lose visibility of the unit's private /tmp).
	BridgeDir string

	// PodmanBinary overrides the executable used (default "podman").
	PodmanBinary string

	// MemoryLimit defaults to "512m" if empty.
	MemoryLimit string

	// CPULimit defaults to "1.0" if empty.
	CPULimit string

	// PidsLimit defaults to 128 if zero.
	PidsLimit int

	// Runtime overrides the default OCI runtime — e.g. "runsc" for
	// gVisor. Empty means use Podman's configured default (crun/runc).
	Runtime string

	// ExtraRunArgs are appended to the `podman run` invocation just
	// before the image name. Useful for tests or one-off knobs.
	ExtraRunArgs []string

	// NoNetwork forces `--network=none` so the container has an empty
	// network namespace — no loopback, no DNS, no route to anywhere.
	// Used by the lockdown path (TakeContainer) where the security model
	// requires that an LLM-driven prompt injection cannot exfiltrate to
	// an external host. Non-lockdown chats default to false (slirp4netns,
	// outbound network on) so `pip install` and `curl` work in routine
	// data-analysis flows. The host-side MCP servers and the model
	// provider always have full network access — this flag only governs
	// what happens *inside* bash/run_python.
	NoNetwork bool

	// ReadOnlyMounts are absolute host directories that should appear
	// inside the container at the SAME absolute path, read-only.
	//
	// Used for the agent's supporting docs (personas/, protocols/,
	// system_prompts/). chat-server's tools/workspace.go drops symlinks
	// into the per-conversation workspace pointing at the host paths of
	// these dirs. Without this same-path remount, those symlinks dangle
	// inside the container and `cat personas/assistant.yaml` from bash
	// (or `open(...)` from run_python) fails — even though the *host*
	// view via view_file works fine.
	//
	// Empty / nonexistent paths are silently skipped — operators running
	// without supporting docs (e.g. CHAT_MOCK_MODE=1 tests) shouldn't
	// have to populate this list.
	ReadOnlyMounts []string

	// StartTimeout caps how long we wait for `podman run` to return a
	// container ID. Defaults to 30s.
	StartTimeout time.Duration
}

// NewContainer starts a fresh sandbox container and returns a Sandbox
// handle wrapping it. The container is detached; its bridge is NOT yet
// running — it lazy-starts on the first RunPython call.
//
// On error the container is best-effort cleaned up.
func NewContainer(ctx context.Context, cfg ContainerConfig) (*Sandbox, error) {
	if cfg.Image == "" {
		return nil, fmt.Errorf("sandbox: ContainerConfig.Image required")
	}
	if cfg.WorkspaceHostDir == "" {
		return nil, fmt.Errorf("sandbox: ContainerConfig.WorkspaceHostDir required")
	}
	if cfg.BridgeScript == nil {
		return nil, fmt.Errorf("sandbox: ContainerConfig.BridgeScript required")
	}
	c := &containerImpl{cfg: applyContainerDefaults(cfg)}
	if err := c.start(ctx); err != nil {
		c.close()
		return nil, err
	}
	return &Sandbox{mode: ModeContainer, impl: c}, nil
}

func applyContainerDefaults(cfg ContainerConfig) ContainerConfig {
	if cfg.PodmanBinary == "" {
		cfg.PodmanBinary = "podman"
	}
	if cfg.MemoryLimit == "" {
		cfg.MemoryLimit = "512m"
	}
	if cfg.CPULimit == "" {
		cfg.CPULimit = "1.0"
	}
	if cfg.PidsLimit == 0 {
		cfg.PidsLimit = 128
	}
	if cfg.StartTimeout == 0 {
		cfg.StartTimeout = defaultContainerStartTimeout
	}
	return cfg
}

// defaultContainerStartTimeout is exposed so callers (Pool.newSandbox,
// Pool.TakeContainer) can compute their outer context timeout against
// the same number NewContainer applies internally — otherwise an
// unset StartTimeout means the outer context is `0 + 5s = 5s` and
// cancels the inner before the first-run idmapped-layer chown
// finishes (which takes ~12s on a freshly-pulled image).
const defaultContainerStartTimeout = 30 * time.Second

// resolveStartTimeout returns the StartTimeout NewContainer will apply
// internally given the supplied config — i.e. the caller's value if
// non-zero, else the package default. Pool's outer context timeout
// derives from this so cold-start container creation isn't cancelled
// from above before the inner timeout has a chance to expire.
func resolveStartTimeout(cfg ContainerConfig) time.Duration {
	if cfg.StartTimeout > 0 {
		return cfg.StartTimeout
	}
	return defaultContainerStartTimeout
}

func (c *containerImpl) start(ctx context.Context) error {
	// Write the bridge script to a host temp file so we can bind-mount
	// it into the container. We bind-mount the file (not a directory)
	// so the rest of /opt inside the container stays untouched.
	//
	// BridgeDir defaults to os.TempDir(); production overrides it to a
	// path under WorkspaceHostDir's parent (e.g. /opt/chat/data) so
	// the file remains visible regardless of unit-level PrivateTmp= or
	// rootless-podman OCI helper reparenting.
	bridgeDir := c.cfg.BridgeDir
	if bridgeDir != "" {
		if err := os.MkdirAll(bridgeDir, 0o755); err != nil { //nolint:gosec // bridge dir must be readable by the rootless-podman user
			return fmt.Errorf("ensure bridge dir: %w", err)
		}
	}
	scriptF, err := os.CreateTemp(bridgeDir, "chat-sandbox-bridge-*.py")
	if err != nil {
		return fmt.Errorf("temp bridge file: %w", err)
	}
	if _, err := scriptF.Write(c.cfg.BridgeScript); err != nil {
		_ = scriptF.Close()
		_ = os.Remove(scriptF.Name())
		return fmt.Errorf("write bridge: %w", err)
	}
	if err := scriptF.Close(); err != nil {
		_ = os.Remove(scriptF.Name())
		return fmt.Errorf("close bridge: %w", err)
	}
	// Make the file world-readable so the unprivileged user inside the
	// container can read it through the bind mount. The temp file is in
	// /tmp; the contents are not secret (it's the embedded bridge that
	// already ships in our Go binary).
	if err := os.Chmod(scriptF.Name(), 0o644); err != nil { //nolint:gosec // script is non-secret embedded code, must be world-readable for the container user
		_ = os.Remove(scriptF.Name())
		return fmt.Errorf("chmod bridge: %w", err)
	}
	c.bridgeScriptPath = scriptF.Name()

	c.containerID = generateContainerName()

	args := []string{
		"run",
		"--detach",
		"--rm",
		"--name", c.containerID,
		// Hardening defaults: --read-only rootfs, no caps,
		// no-new-privileges. The workspace bind below is the only
		// persistent writable surface; tmpfs covers IPython /
		// matplotlib / /tmp scratch. Network egress is controlled
		// separately (NoNetwork) — lockdown turns seal it off, normal
		// turns leave the rootless slirp4netns default in place so
		// `pip install` + outbound HTTP work in routine analysis flows.
		"--read-only",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		// Map the container's running user (uid 1000 / sandbox, set by
		// the image's USER directive) to the HOST chat user. Without
		// this, rootless podman maps host-chat to container-root and
		// the in-container sandbox uid 1000 falls into the subuid range
		// (~100999 on host). Files in the bind-mounted workspace,
		// created by host-chat, then appear as root:root inside the
		// container — and the sandbox user can neither chdir into the
		// per-conversation workspace nor write `marker.txt` next to a
		// host-MCP-downloaded attachment. Lockdown chats hit this on
		// every turn (see TakeContainer in pool.go); non-lockdown
		// chats hid behind Pool.Take()'s degrade-to-host fallback.
		//
		// keep-id:uid=N,gid=N tells podman: inside the container, the
		// host user appears at uid=N. Picking N=1000 lines up with the
		// image's `USER sandbox` so the running user owns the
		// bind-mounted workspace from both sides — host-chat to its
		// host filesystem, container-sandbox to its container view —
		// and 0o755 dirs work without `:U` (which chowns the host side
		// to a subuid the chat user can't write to next turn).
		"--userns=keep-id:uid=1000,gid=1000",
		fmt.Sprintf("--memory=%s", c.cfg.MemoryLimit),
		fmt.Sprintf("--cpus=%s", c.cfg.CPULimit),
		fmt.Sprintf("--pids-limit=%d", c.cfg.PidsLimit),
		// Tmpfs for the directories Python / IPython / matplotlib
		// expect to write to. Without these, kernel start fails or
		// degrades silently on a --read-only rootfs. Ownership is
		// inherited from the container's running user (set by the
		// image's USER directive) — uid/gid mount options aren't
		// supported in the short --tmpfs form.
		//
		// .config covers matplotlib's default MPLCONFIGDIR
		// (~/.config/matplotlib). Without it, the first plt.savefig in
		// every turn prints a noisy "mkdir failed: read-only file
		// system" warning and falls back to /tmp/matplotlib-XXXX —
		// works, but the warning leaks into stderr the model sees and
		// the pre-warmed font cache from the image build is bypassed.
		"--tmpfs=/tmp:rw,size=128m",
		"--tmpfs=/home/sandbox/.ipython:rw,size=32m",
		"--tmpfs=/home/sandbox/.cache:rw,size=32m",
		"--tmpfs=/home/sandbox/.config:rw,size=8m",
		// Workspace root — readable + writable, the only persistent
		// writable surface. Mounted at the SAME absolute path inside the
		// container as on the host so an absolute path means the same
		// thing on both sides — that's what keeps MCP-returned paths
		// (host-side subprocesses) usable inside bash/run_python (per-
		// turn container) without a translation layer. Without same-
		// path mounting, the LLM gets handed `/opt/chat/workspace/<id>`
		// by the email MCP and then `pd.read_csv` of that path fails
		// because the container only mounts the workspace at /workspace.
		//
		// `:z` (lowercase) — SHARED SELinux label. Every container in the
		// warm pool bind-mounts the same workspace root; each one isolates
		// to its own per-conversation subdir but the ROOT is shared. With
		// `:Z` (private per-container), the warm pool's second `Pool.fill`
		// races the first: podman relabels the dir with a new MCS for B,
		// container A — already warm and waiting to be Take()'d — now has
		// the wrong MCS and bash/run_python inside it hit "Permission
		// denied" on the dir it's chdir'd to, and writes return "Read-only
		// file system". Lockdown's TakeContainer always cold-starts so the
		// latest container always has the matching MCS — that's why this
		// only surfaces in non-lockdown chats whose turn drew a stale warm
		// container. Same fix as the supporting-doc mounts below.
		fmt.Sprintf("--volume=%s:%s:rw,z", c.cfg.WorkspaceHostDir, c.cfg.WorkspaceHostDir),
		// Bridge script — read-only bind of the host temp file.
		fmt.Sprintf("--volume=%s:/opt/bridge/bridge.py:ro,Z", c.bridgeScriptPath),
		fmt.Sprintf("--workdir=%s", c.cfg.WorkspaceHostDir),
	}
	// Supporting-doc bind mounts — same-path so the personas/protocols/
	// system_prompts symlinks tools/workspace.go drops into the per-
	// conversation workspace resolve inside the container too. Read-only:
	// the agent should never write to these from a turn. See the field's
	// doc on ReadOnlyMounts for the lockdown bug this closes.
	for _, dir := range c.cfg.ReadOnlyMounts {
		if dir == "" {
			continue
		}
		if _, err := os.Stat(dir); err != nil {
			// Skip silently — operators in dev/test setups may not
			// have every supporting-doc dir provisioned. The agent
			// still handles "personas/foo.yaml" via view_file (host
			// path resolution), so a missing mount degrades the
			// in-container bash/python view but doesn't break the
			// turn.
			continue
		}
		// `:z` (lowercase) — shared SELinux label. Each container in the
		// warm pool plus every lockdown turn bind-mounts the same host
		// dirs read-only, so concurrent `:Z` (private per-container)
		// relabels race on the same path: the loser's container ends up
		// with an MCS that no longer matches the dir and `cat
		// personas/foo.yaml` fails inside it. Shared label is also what
		// podman docs explicitly recommend for read-only volumes.
		args = append(args, fmt.Sprintf("--volume=%s:%s:ro,z", dir, dir))
	}
	if c.cfg.NoNetwork {
		// Empty network namespace — no loopback, no DNS, no routes. Used
		// by lockdown turns where exfiltration to an external host is part
		// of the threat model. Non-lockdown turns omit this and inherit
		// rootless podman's slirp4netns default (outbound only, no
		// inbound listeners reachable from the host).
		args = append(args, "--network=none")
	}
	if c.cfg.Runtime != "" {
		args = append(args, fmt.Sprintf("--runtime=%s", c.cfg.Runtime))
	}
	args = append(args, c.cfg.ExtraRunArgs...)
	args = append(args, c.cfg.Image)
	// PID 1 inside the container: a do-nothing process so the
	// namespace stays alive. We exec into it for actual work.
	args = append(args, "sleep", "infinity")

	startCtx, cancel := context.WithTimeout(ctx, c.cfg.StartTimeout)
	defer cancel()
	cmd := exec.CommandContext(startCtx, c.cfg.PodmanBinary, c.podmanArgs(args)...) //nolint:gosec // podman binary + args are operator-configured, not user input
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("podman run: %w (stderr: %s)", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}

// podmanArgs prepends the global flags every podman invocation needs.
//
// --cgroup-manager=cgroupfs: write cgroups directly to cgroupfs instead
// of asking the user systemd manager (at /run/user/$UID) to create a
// transient scope. chat-server runs as a system unit (User=chat under
// system.slice/chat-server.service) so its process tree lives in
// system.slice. Rootless podman's default systemd cgroup driver places
// container scopes under user-987.slice/user@987.service/user.slice/,
// a different cgroup subtree. Migrating a container's init pid across
// that LCA boundary requires write access on the common ancestor cgroup
// (effectively /), which the chat user does NOT have, and crun fails
// with `write to .../cgroup.procs: Permission denied: OCI permission
// denied` on every podman exec — i.e. every bash invocation and every
// run_python bridge call. The cgroupfs driver places the container
// cgroup under the unit's OWN delegated subtree (Delegate=yes), where
// the chat user can write.
//
// The flag has to go on every invocation: `podman exec` doesn't inherit
// the cgroup driver chosen at `podman run` time — it re-resolves from
// the global default (which on a stock Fedora install is "systemd").
func (c *containerImpl) podmanArgs(rest []string) []string {
	out := make([]string, 0, len(rest)+1)
	// --cgroup-manager=cgroupfs is needed on Linux where chat-server runs as a
	// systemd unit (system.slice) but rootless podman defaults to the systemd
	// cgroup driver, causing cgroup migration permission errors on every exec.
	// On macOS (Podman Machine) the flag doesn't exist — skip it.
	if runtime.GOOS == "linux" {
		out = append(out, "--cgroup-manager=cgroupfs")
	}
	out = append(out, rest...)
	return out
}

func (c *containerImpl) runBash(ctx context.Context, req BashRequest) (BashResult, error) {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	args := []string{"exec"}
	if req.WorkingDir != "" {
		args = append(args, "--workdir", req.WorkingDir)
	}
	args = append(args, c.containerID, "bash", "-c", req.Command)

	cmd := exec.CommandContext(cmdCtx, c.cfg.PodmanBinary, c.podmanArgs(args)...) //nolint:gosec // bash command runs inside an isolated rootless container by design
	// Without WaitDelay, cmd.Run blocks until the stdout/stderr pipes
	// close — a background grandchild holding them open would hang the
	// agent past its own timeout.
	cmd.WaitDelay = BashWaitDelay

	// Capture stdout/stderr separately, bounded so runaway output can't
	// exhaust memory before the truncation logic runs.
	stdoutBuf := &cappedBuffer{cap: BashOutputCaptureCap}
	stderrBuf := &cappedBuffer{cap: BashOutputCaptureCap}
	cmd.Stdout = stdoutBuf
	cmd.Stderr = stderrBuf

	execErr := cmd.Run()

	res := BashResult{
		Stdout:          stdoutBuf.buf.Bytes(),
		Stderr:          stderrBuf.buf.Bytes(),
		StdoutDiscarded: stdoutBuf.discarded,
		StderrDiscarded: stderrBuf.discarded,
	}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	} else {
		res.ExitCode = -1
	}
	if cmdCtx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		return res, nil
	}
	if execErr != nil && cmd.ProcessState == nil {
		return res, fmt.Errorf("podman exec: %w", execErr)
	}
	return res, nil
}

func (c *containerImpl) runPython(ctx context.Context, req PythonRequest) (PythonResult, error) {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	if err := c.ensureBridge(); err != nil {
		return PythonResult{}, fmt.Errorf("start python bridge in container: %w", err)
	}

	wireReq := bridgeRequest{
		Code:           req.Code,
		ReturnVars:     req.ReturnVars,
		TimeoutSeconds: int(timeout.Seconds()),
		WorkspaceDir:   req.WorkspaceDir,
	}
	reqBytes, err := json.Marshal(wireReq)
	if err != nil {
		return PythonResult{}, fmt.Errorf("marshal bridge request: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if _, err := fmt.Fprintf(c.bridgeStdin, "%s\n", reqBytes); err != nil {
		return PythonResult{}, fmt.Errorf("send bridge request: %w%s", err, c.bridgeStderrSuffix())
	}

	type readResult struct {
		data []byte
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		data, err := c.bridgeStdout.ReadBytes('\n')
		ch <- readResult{data: data, err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		// Tear the bridge down (mirrors hostImpl.readLocked): the
		// orphaned reader goroutine above still owns c.bridgeStdout,
		// and the bridge may still write the late response. Reusing
		// the session would make the next run_python race that reader
		// and consume an off-by-one response stream for the rest of
		// the turn. ensureBridge starts a fresh session next call.
		c.terminateBridgeLocked()
		return PythonResult{}, fmt.Errorf("python execution cancelled: %w", ctx.Err())
	case <-timer.C:
		c.terminateBridgeLocked()
		return PythonResult{}, fmt.Errorf("python execution timed out after %v", timeout)
	case r := <-ch:
		if r.err != nil {
			return PythonResult{}, fmt.Errorf("bridge closed unexpectedly: %w%s", r.err, c.bridgeStderrSuffix())
		}
		return parseBridgeResponse(r.data)
	}
}

// terminateBridgeLocked kills the bridge exec session and clears the
// bridge state so the next ensureBridge starts fresh. Called after a
// timeout/cancel left a reader goroutine holding bridgeStdout — the
// session's response stream can no longer be trusted. The python
// process inside the container is reaped with the per-turn container
// itself. Caller must hold c.mu.
func (c *containerImpl) terminateBridgeLocked() {
	if cmd := c.bridgeCmd; cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(os.Interrupt)
		// Brief grace period; force-kill if still alive.
		done := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(200 * time.Millisecond):
			_ = cmd.Process.Kill()
			<-done
		}
	}
	if c.bridgeStdin != nil {
		_ = c.bridgeStdin.Close()
	}
	c.bridgeCmd = nil
	c.bridgeStdin = nil
	c.bridgeStdout = nil
	c.bridgeStarted = false
}

// bridgeStderrSuffix returns " (bridge stderr: ...)" when the captured
// stderr buffer has content, or "" otherwise. Suffix form so callers
// can append unconditionally with %s. Trims whitespace so we don't pad
// the error with the trailing newlines podman/crun emit.
func (c *containerImpl) bridgeStderrSuffix() string {
	if c.bridgeStderr == nil {
		return ""
	}
	stderr := strings.TrimSpace(c.bridgeStderr.Snapshot())
	if stderr == "" {
		return ""
	}
	const maxLen = 1024
	if len(stderr) > maxLen {
		stderr = stderr[len(stderr)-maxLen:]
	}
	return " (bridge stderr: " + stderr + ")"
}

// ensureBridge starts the bridge inside the container on first call.
// Subsequent calls are a no-op as long as the exec session is still
// healthy.
func (c *containerImpl) ensureBridge() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.bridgeStarted && c.bridgeCmd != nil && c.bridgeCmd.ProcessState == nil {
		return nil
	}

	// `podman exec -i` keeps stdin open; the bridge reads JSON-per-line
	// from there and writes JSON-per-line to stdout. Tee stderr into
	// both the parent's stderr (so operators see it in the journal) and
	// a buffer (so the runPython path can include it when surfacing a
	// broken-pipe write failure — otherwise the error is opaque).
	args := []string{"exec", "-i", c.containerID, "python3", "/opt/bridge/bridge.py"}
	// The bridge is intentionally not bound to the caller's ctx: it outlives
	// any single request and is torn down in close() via podman kill.
	cmd := exec.Command(c.cfg.PodmanBinary, c.podmanArgs(args)...) //nolint:gosec,noctx // G204: fixed operator-configured podman binary + our own args (no shell). noctx: the bridge intentionally outlives any single request ctx and is torn down in close() via podman kill.
	stderrBuf := &syncBuffer{}
	cmd.Stderr = io.MultiWriter(os.Stderr, stderrBuf)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return fmt.Errorf("start bridge exec: %w", err)
	}
	c.bridgeCmd = cmd
	c.bridgeStdin = stdin
	c.bridgeStdout = bufio.NewReader(stdout)
	c.bridgeStderr = stderrBuf
	c.bridgeStarted = true
	// Match legacy timing — the bridge sets up its kernel asynchronously
	// after we send the first request, but the stdin reader has to be
	// up before we can write. 100ms covers the common case; if the
	// bridge isn't ready, the first ReadBytes will just wait.
	time.Sleep(100 * time.Millisecond)
	return nil
}

func (c *containerImpl) close() {
	c.mu.Lock()
	if c.bridgeStdin != nil {
		_ = c.bridgeStdin.Close()
	}
	c.bridgeCmd = nil
	c.bridgeStdin = nil
	c.bridgeStdout = nil
	c.bridgeStderr = nil
	c.bridgeStarted = false
	containerID := c.containerID
	scriptPath := c.bridgeScriptPath
	c.containerID = ""
	c.bridgeScriptPath = ""
	c.mu.Unlock()

	if containerID != "" {
		// Best-effort kill. --rm in `podman run` means the container is
		// removed automatically once the root process exits, so killing
		// is enough.
		killCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		stop := exec.CommandContext(killCtx, c.cfg.PodmanBinary, c.podmanArgs([]string{"kill", containerID})...) //nolint:gosec // containerID is our generated UUID, not user input
		_ = stop.Run()
	}
	if scriptPath != "" {
		_ = os.Remove(scriptPath)
	}
}

// containerNamePrefix is the shared prefix for every sandbox container name. It
// is the handle the boot-time orphan sweep (PruneOrphanedContainers) filters on.
const containerNamePrefix = "chat-sandbox-"

// generateContainerName returns "chat-sandbox-<16 hex chars>". Random
// suffix avoids collisions when multiple sandboxes are spawned in
// parallel by the warm pool.
func generateContainerName() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return containerNamePrefix + hex.EncodeToString(b[:])
}
