// Package sandbox is the per-turn execution boundary for bash and run_python.
//
// One Sandbox = one Linux environment that survives for a single agent
// turn. Every bash invocation, run_python call, and general file operation
// within that turn dispatches into the same Sandbox; the workspace bind / chdir
// is the same one the rest of Fleet uses, so files written here are the files
// later sandbox calls, governed host brokers, and the user see.
//
// Two backends:
//
//   - "container" — rootless Podman container, --read-only rootfs,
//     dropped caps, capped memory/cpu/pids. The default in production.
//     Network egress is governed by ContainerConfig.NoNetwork — lockdown
//     turns seal the namespace (no DNS, no routes); non-lockdown turns
//     get the rootless slirp4netns default so `pip install` and outbound
//     HTTP from bash/python both work.
//   - "host" — a test-only fixture: bash via os/exec, Python via a long-lived
//     python3 subprocess, and file operations via the same fixed helper
//     protocol. It is compiled only with the fleet_host_executor build tag. A
//     release build contains a fail-closed stub and never falls back to host
//     execution when Podman is unavailable.
//
// Lifecycle: NewHost / NewContainer to construct, RunBash / RunPython to
// dispatch, Close to tear down. The Pool wraps NewContainer so the
// per-turn cold-start (container spin + python boot + pandas import) is
// hidden behind a warm queue, exactly the way the legacy KernelPool
// hid bare python boot.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/telemetry"
)

// BashWaitDelay is how long cmd.Wait may keep waiting on the
// stdout/stderr pipes after the command's context is cancelled or the
// process exits. Without it, a grandchild process inheriting the pipes
// (e.g. `server &`) keeps them open and cmd.Run blocks forever — past
// the tool's own timeout — wedging the agent. Folded in from cutlass's
// direct-exec bash path during the P3 sandbox/tools merge.
//
// A var rather than a const only so a test binary can shorten it: the tests
// that prove the WaitDelay backstop fires (a backgrounded child holding the
// pipes) otherwise sit through the full 10s each.
var BashWaitDelay = 10 * time.Second

// BashOutputCaptureCap bounds how many bytes of stdout/stderr are held
// in memory per stream. A command like `yes` or a verbose build can
// otherwise buffer gigabytes before the truncation logic ever runs and
// OOM the agent. 64 MB comfortably covers real build/test logs while
// keeping worst-case memory bounded; bytes beyond the cap are counted
// and discarded. Folded in from cutlass's direct-exec bash path during
// the P3 sandbox/tools merge.
const BashOutputCaptureCap = 64 * 1024 * 1024

// cappedBuffer is an io.Writer that stores at most cap bytes and counts
// the rest, so unbounded command output can't exhaust memory. It always
// reports a full-length write so exec's pipe copier never errors.
//
// Mutex-guarded, and read via snapshot(), because writer quiescence is no
// longer guaranteed on every path: client-go's exec executor returns from a
// cancelled StreamWithContext WITHOUT joining its stdout/stderr copy
// goroutines, so the kubernetes backend can be assembling a cancellation
// result while an abandoned copier is still writing (a reproduced -race
// finding on PR #1285). Locking here makes every sink safe regardless of
// which backend feeds it; snapshot() returns copies so no caller ever holds
// a live reference into the buffer.
type cappedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	cap       int
	discarded int64
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	remaining := c.cap - c.buf.Len()
	if remaining <= 0 {
		c.discarded += int64(len(p))
		return len(p), nil
	}
	if len(p) > remaining {
		c.discarded += int64(len(p) - remaining)
		c.buf.Write(p[:remaining])
		return len(p), nil
	}
	return c.buf.Write(p)
}

// snapshot returns a copy of the captured bytes and the discarded count,
// consistent with each other. A copy, not the live slice: a late write from
// an abandoned copier must never mutate a result already handed out.
func (c *cappedBuffer) snapshot() ([]byte, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]byte, c.buf.Len())
	copy(out, c.buf.Bytes())
	return out, c.discarded
}

// Mode picks a backend at construction time. Callers don't usually pick
// directly — the Pool factory inspects the environment and chooses.
type Mode int

const (
	// ModeHost runs bash directly via os/exec and the python bridge
	// directly as a subprocess. Matches legacy behavior.
	ModeHost Mode = iota

	// ModeContainer runs bash and the python bridge inside a rootless
	// Podman container with --read-only / dropped caps. Network egress
	// is per-turn (ContainerConfig.NoNetwork) — see container.go.
	ModeContainer

	// ModeKubernetes runs bash and the python bridge inside an ephemeral
	// Kubernetes Pod, exec'd over the apiserver (#989) — the enterprise
	// split-control-plane backend. Selected by FLEET_SANDBOX_BACKEND;
	// see k8s_backend.go.
	ModeKubernetes
)

// BashRequest is the per-call input the sandbox sees for a bash
// invocation. The application-level safety check (denylist, sensitive
// path matching, destructive-pattern matching) runs at the tool layer
// before this struct is constructed; the sandbox treats Command as
// already-vetted user-supplied shell.
type BashRequest struct {
	Command    string
	WorkingDir string
	Timeout    time.Duration
}

// BashResult is the raw process result. The tool layer formats this into
// the JSON wire shape the LLM sees.
type BashResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
	// TimedOut means the context deadline (Timeout) fired before the
	// process exited. ExitCode is undefined in that case.
	TimedOut bool
	// Cancelled means the CALLER's context was cancelled (user Stop, turn
	// teardown) before the process exited — distinct from TimedOut so the
	// tool layer can tell the model which one happened. ExitCode is
	// undefined in that case.
	Cancelled bool
	// CleanupConfirmed is meaningful only when TimedOut or Cancelled is set:
	// true when the backend PROVED this invocation's process tree is dead
	// (#796). False means processes may still be running; the sandbox is
	// poisoned and refuses further work until retired.
	CleanupConfirmed bool
	// SandboxRetired reports that cancellation/timeout invalidated the whole
	// sandbox, including persistent Python kernel state. Production container
	// sandboxes set this after tearing down the PID namespace; the test-only
	// host executor does not have a container to retire.
	SandboxRetired bool
	// StdoutDiscarded / StderrDiscarded count bytes dropped because the
	// stream exceeded BashOutputCaptureCap. The tool layer surfaces a
	// note when either is non-zero.
	StdoutDiscarded int64
	StderrDiscarded int64
}

// PythonRequest is the per-call input for a run_python call. Mirrors the
// pythonRequest JSON shape the embedded bridge.py expects on stdin.
type PythonRequest struct {
	Code         string
	ReturnVars   []string
	Timeout      time.Duration
	WorkspaceDir string
	// ResetKernel discards the current IPython kernel (all variables/imports)
	// and starts a fresh one before running Code. Only meaningful in persistent
	// REPL mode (#213).
	ResetKernel bool
}

// PythonResult is the parsed pythonResponse the bridge writes to stdout.
//
// NOTE: parseBridgeResponse (bridge_protocol.go) converts bridgeResponse to this
// type directly, so the field order/types here MUST stay in lockstep with
// bridgeResponse.
type PythonResult struct {
	Status           string
	Output           string
	Stdout           string
	Stderr           string
	Result           string
	Vars             map[string]any
	Error            string
	BridgeTruncation map[string]BridgeCaptureInfo
	// ImageFiles are workspace-relative paths the bridge saved for each
	// image/png the kernel produced (#213).
	ImageFiles []string
}

// BridgeCaptureInfo mirrors the bridge_truncation map values from
// bridge.py. Forwarded as-is; the tool layer translates field names.
type BridgeCaptureInfo struct {
	Truncated     bool `json:"truncated"`
	CapturedBytes int  `json:"captured_bytes"`
	TotalBytes    int  `json:"total_bytes"`
}

// Sandbox is the per-turn execution handle. Construct with NewHost or
// NewContainer; usually obtained from a Pool. Methods are goroutine-safe
// — the underlying backend serializes calls so the bridge stdin/stdout
// stays coherent.
type Sandbox struct {
	mode Mode
	impl impl

	mu     sync.Mutex
	closed bool
	// defaultWorkingDir, when set, is the cwd applied to a RunBash /
	// RunPython call that does not specify its own WorkingDir / WorkspaceDir.
	// It is one of the two seams git-worktree isolation (#180) uses to scope a
	// scheduled run into its per-run worktree (a subdirectory of the bind-mounted
	// workspace root). This seam covers a call that arrives with an empty
	// WorkingDir: this default fills it. The in-process tool layer is scoped
	// separately via tools.WithForcedWorkingDir, because its resolvers pre-fill a
	// non-empty cwd (the process cwd) that this empty-only default would not
	// override.
	//
	// Safe without a reset because a Sandbox is single-use: the pool hands each
	// one out once and Close()s it after the turn (it is never returned to the
	// pool and reused). A future pre-warm change that RECYCLES a taken sandbox
	// MUST clear this on release, or one task's worktree would leak to the next.
	//
	// The single exception is persistent REPL mode (#213): there a sandbox is
	// deliberately reused across turns WITHIN ONE conversation (never across
	// conversations). defaultWorkingDir stays empty there — persistent mode is
	// interactive chat only, which passes WorkspaceDir per call; the worktree
	// seam is a scheduled-run concern and the two never combine.
	defaultWorkingDir string

	// fileOpRoot/fileOpRootIdentity are the writable filesystem capability
	// bound before a turn exposes tools to the model (#784). The identity is
	// captured inside the selected runtime so it works with runc, Kata and krun.
	fileOpRoot         string
	fileOpRootIdentity FileOpRootIdentity

	// pythonCellTimeout is the host-operator ceiling on a single run_python cell
	// (FLEET_PYTHON_CELL_TIMEOUT, #213). Zero disables it. When > 0, RunPython
	// clamps the per-call Timeout to min(Timeout, pythonCellTimeout) before
	// dispatching. Enforced here (in the shared RunPython) rather than in the
	// bridge because the bridge runs inside the container with no --env
	// forwarding, so a host-side env value can't reach it directly.
	pythonCellTimeout time.Duration

	// replace builds a fresh sandbox equivalent to this one — same image,
	// mounts, network posture and resource limits, with every check the take
	// path ran on the original. The pool that handed this sandbox out sets it;
	// nil (a sandbox constructed directly, or the test-only host backend) means
	// a poisoned backend stays poisoned. See backend.
	replace func(ctx context.Context) (*Sandbox, error)
	// replaceMu serializes backend replacement so parallel tool calls that
	// find the same poisoned backend start one container, not one each.
	replaceMu sync.Mutex
	// replacements counts the replacements this sandbox has attempted in the
	// current run, failed ones included; see maxBackendReplacements and
	// startRun.
	replacements int
}

// maxBackendReplacements bounds how many times one sandbox tries to replace a
// poisoned backend within one run (a turn, or a scheduled run). Each attempt is a container start that can wait out the
// whole start timeout, so a failed attempt counts like a successful one: a
// run whose commands keep timing out, or whose box cannot start a container
// at all, stops paying for them. Past the bound the sandbox stays poisoned
// and is retired by its owner, as before.
const maxBackendReplacements = 3

// SetPythonCellTimeout sets the per-cell run_python ceiling (#213). The pool
// calls this at construction from the resolved FLEET_PYTHON_CELL_TIMEOUT. Zero
// disables the ceiling. Goroutine-safe.
func (s *Sandbox) SetPythonCellTimeout(d time.Duration) {
	s.mu.Lock()
	s.pythonCellTimeout = d
	s.mu.Unlock()
}

// SetDefaultWorkingDir sets the cwd applied to bash/python calls that don't
// carry their own WorkingDir/WorkspaceDir. Used by the scheduled runner to bind
// a per-run git worktree (#180). Pass "" to clear. Goroutine-safe.
func (s *Sandbox) SetDefaultWorkingDir(dir string) {
	s.mu.Lock()
	s.defaultWorkingDir = dir
	s.mu.Unlock()
}

// impl is the backend interface. Three concrete implementations live in
// container.go (rootless Podman, the single-box production backend),
// k8s_backend.go (Kubernetes pods, the enterprise split backend, #989), and
// host.go (the test-only fixture behind the fleet_host_executor build tag).
type impl interface {
	runBash(ctx context.Context, req BashRequest) (BashResult, error)
	runPython(ctx context.Context, req PythonRequest) (PythonResult, error)
	// runFileOp executes a view/write/edit file operation inside the backend
	// (#784) so model-authored filesystem mutations run in the sandbox, not the
	// host process. See fileops.go.
	runFileOp(ctx context.Context, req FileOpRequest) (FileOpResult, error)
	bindFileOpRoot(ctx context.Context, root string) (FileOpRootIdentity, error)
	// poisoned reports whether a cancelled/timed-out invocation may have left
	// processes running in the backend (#796: cleanup could not be proved).
	// A poisoned backend refuses further work — the owner must retire it
	// (close kills the container and with it anything still running).
	poisoned() bool
	close()
}

// ErrPoisoned is returned by RunBash/RunPython on a sandbox whose earlier
// cancelled/timed-out invocation could not be proved dead (#796) and whose
// backend could not be replaced (see backend). The sandbox must be retired;
// the persistent pool does so automatically at the next borrow/release
// boundary.
var ErrPoisoned = errors.New("sandbox poisoned: a cancelled command may still be running; the sandbox is being retired")

// HostExecutorCompiledIn reports whether the unsandboxed host executor (ModeHost,
// host.go) was compiled into this binary — true only with the
// `fleet_host_executor` build tag (tests/dev). A release build returns false, so
// callers (e.g. the MockMode path) can fail closed instead of running agent tool
// calls unsandboxed on the host. See host.go / host_disabled.go (#159).
func HostExecutorCompiledIn() bool { return hostExecutorCompiledIn }

// ModeName reports the backend in use ("host", "container", "kubernetes").
func (s *Sandbox) ModeName() string {
	switch s.mode {
	case ModeHost:
		return "host"
	case ModeContainer:
		return "container"
	case ModeKubernetes:
		return "kubernetes"
	default:
		return "unknown"
	}
}

// RunBash dispatches one bash invocation through the active backend.
// Returns ErrClosed if the sandbox has already been torn down, and
// ErrPoisoned if an earlier cancelled/timed-out invocation could not be
// proved dead (fail closed: the suspect container must be retired, not
// reused, so a stopped turn's stragglers can't overlap fresh work).
func (s *Sandbox) RunBash(ctx context.Context, req BashRequest) (res BashResult, err error) {
	ctx, span := s.startExecSpan(ctx, "bash", req.Timeout, len(req.Command))
	defer func() {
		if r := recover(); r != nil {
			telemetry.EndPanicked(span)
			panic(r)
		}
		endBashSpan(span, res, err)
	}()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return BashResult{}, ErrClosed
	}
	if req.WorkingDir == "" {
		req.WorkingDir = s.defaultWorkingDir
	}
	s.mu.Unlock()
	be, err := s.backend(ctx)
	if err != nil {
		return BashResult{}, err
	}
	return be.runBash(ctx, req)
}

// Poisoned reports whether a cancelled/timed-out invocation may have left
// processes running in this sandbox (#796). The persistent pool retires a
// poisoned sandbox instead of lending it to another turn.
func (s *Sandbox) Poisoned() bool {
	return s.currentImpl().poisoned()
}

// currentImpl returns the backend this sandbox runs on now. Read under mu:
// backend swaps it when it replaces a poisoned one.
func (s *Sandbox) currentImpl() impl {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.impl
}

// setReplace records how to build a fresh sandbox equivalent to this one. The
// pool calls it on every sandbox it hands out, before the caller sees it.
func (s *Sandbox) setReplace(build func(ctx context.Context) (*Sandbox, error)) {
	s.mu.Lock()
	s.replace = build
	s.mu.Unlock()
}

// startRun opens a fresh replacement budget (maxBackendReplacements). The
// persistent pool calls it each time it lends a conversation's sandbox to a
// turn: the bound is per run, and that sandbox outlives many.
func (s *Sandbox) startRun() {
	s.mu.Lock()
	s.replacements = 0
	s.mu.Unlock()
}

// checkReplacement adds a check every replacement must pass before it is
// adopted — the same one the take path ran on the original (a scoped mount's
// identity, for one). A replacement that fails it is closed and refused.
func (s *Sandbox) checkReplacement(check func(ctx context.Context, fresh *Sandbox) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	build := s.replace
	if build == nil {
		return
	}
	s.replace = func(ctx context.Context) (*Sandbox, error) {
		fresh, err := build(ctx)
		if err != nil {
			return nil, err
		}
		if err := check(ctx, fresh); err != nil {
			fresh.Close()
			return nil, err
		}
		return fresh, nil
	}
}

// backend returns the backend a new call runs on. A healthy backend is
// returned as it is. A poisoned one — an earlier call timed out or was
// cancelled and its processes could not be proved dead (#796) — is never
// reused: it is closed, which kills the container and with it anything still
// running, and a fresh one built the way the pool built the original takes
// its place, bound to the same file root.
//
// Without this a single timed-out command cost the rest of the run: the turn
// holds this one *Sandbox until it ends, the pool only swaps a poisoned
// sandbox at the next borrow, and so every later bash, Python and file call
// of that turn failed with ErrPoisoned. A scheduled patrol that spent an hour
// collecting data and then hit the run_python ceiling could do nothing with
// it. What does not survive the swap is in-container state: the Python
// kernel's variables and anything written outside the mounted workspace.
//
// It fails closed. With no way to build a replacement, past
// maxBackendReplacements, when the caller's context is already done, or when
// the replacement cannot be started, cannot pass the take path's checks or
// cannot bind the file root, the call gets ErrPoisoned exactly as before.
func (s *Sandbox) backend(ctx context.Context) (impl, error) {
	cur := s.currentImpl()
	if !cur.poisoned() {
		return cur, nil
	}

	s.replaceMu.Lock()
	defer s.replaceMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrClosed
	}
	cur = s.impl
	build := s.replace
	replaced := s.replacements
	root := s.fileOpRoot
	s.mu.Unlock()
	if !cur.poisoned() {
		// A parallel call replaced it while this one waited on replaceMu.
		return cur, nil
	}
	if build == nil || replaced >= maxBackendReplacements || ctx.Err() != nil {
		return nil, ErrPoisoned
	}
	s.mu.Lock()
	s.replacements++
	s.mu.Unlock()

	// The old container dies before the new one mounts the same workspace, so
	// nothing the cancelled command left running can write under fresh work.
	cur.close()
	fresh, err := build(ctx)
	if err != nil {
		logging.From(ctx).Warn("sandbox: could not replace a poisoned sandbox; it stays retired", "err", err)
		return nil, fmt.Errorf("%w; a replacement could not be started", ErrPoisoned)
	}
	next := fresh.impl
	// The file root is bound again inside the replacement, exactly as it was
	// bound inside the original, and the identity it reports is the one file
	// calls are checked against from here on. It is not compared with the
	// original's: the identity is whatever the runtime's own kernel reports,
	// and a replacement pod on another node, or a fresh VM guest (kata,
	// libkrun), numbers the same directory differently. What makes the
	// directory the right one is what made it right the first time — the
	// take path's mount checks, which build has just run again.
	var identity FileOpRootIdentity
	if root != "" {
		got, err := next.bindFileOpRoot(ctx, root)
		if err != nil || (got.Dev == 0 && got.Ino == 0) {
			next.close()
			logging.From(ctx).Warn("sandbox: a replacement sandbox could not bind the file root; refusing it", "err", err)
			return nil, fmt.Errorf("%w; a replacement could not be bound to the workspace", ErrPoisoned)
		}
		identity = got
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		next.close()
		return nil, ErrClosed
	}
	s.impl = next
	if root != "" {
		s.fileOpRootIdentity = identity
	}
	s.mu.Unlock()
	logging.From(ctx).Info("sandbox: replaced a poisoned sandbox within the run", "replacement", replaced+1)
	return next, nil
}

// ReapDetachedProcesses terminates any background processes inside a persistent
// sandbox that are not PID 1 or the persistent Python bridge process when a
// turn releases its borrow. Returns false (and poisons the sandbox) if cleanup
// could not be confirmed.
func (s *Sandbox) ReapDetachedProcesses() bool {
	if s == nil {
		return true
	}
	cur := s.currentImpl()
	if cur == nil {
		return true
	}
	if r, ok := cur.(interface{ reapDetachedProcesses() bool }); ok {
		return r.reapDetachedProcesses()
	}
	return true
}

// RunPython dispatches one run_python invocation through the active
// backend. Bridge stdin/stdout serialization is the backend's problem,
// not the caller's.
func (s *Sandbox) RunPython(ctx context.Context, req PythonRequest) (res PythonResult, err error) {
	ctx, span := s.startExecSpan(ctx, "python", req.Timeout, len(req.Code))
	defer func() {
		if r := recover(); r != nil {
			telemetry.EndPanicked(span)
			panic(r)
		}
		endPythonSpan(span, res, err)
	}()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return PythonResult{}, ErrClosed
	}
	if req.WorkspaceDir == "" {
		req.WorkspaceDir = s.defaultWorkingDir
	}
	// Clamp to the host-operator per-cell ceiling when one is set. min of the
	// two, treating a non-positive incoming Timeout as "no caller limit".
	if s.pythonCellTimeout > 0 && (req.Timeout <= 0 || s.pythonCellTimeout < req.Timeout) {
		req.Timeout = s.pythonCellTimeout
	}
	s.mu.Unlock()
	// The span records the timeout the cell runs under, after the clamp.
	setExecTimeout(span, req.Timeout)
	be, err := s.backend(ctx)
	if err != nil {
		return PythonResult{}, err
	}
	return be.runPython(ctx, req)
}

// Close tears down the backend. Safe to call multiple times. After
// Close, RunBash / RunPython return ErrClosed.
func (s *Sandbox) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	cur := s.impl
	s.mu.Unlock()
	cur.close()
}

// ErrClosed is returned from RunBash / RunPython after Close has run.
var ErrClosed = errors.New("sandbox is closed")

// ErrContainerUnavailable is returned by Pool.TakeContainer when the
// pool was constructed without a container image.
var ErrContainerUnavailable = errors.New("sandbox: container backend not configured")
