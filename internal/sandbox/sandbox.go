// Package sandbox is the per-turn execution boundary for bash and run_python.
//
// One Sandbox = one Linux environment that survives for a single agent
// turn. Every bash invocation and every run_python call within that turn
// dispatches into the same Sandbox; the workspace bind / chdir is the
// same one the rest of the chat server uses, so files written here are
// the files the LLM, the host-side fs tools, and the user all see.
//
// Two backends:
//
//   - "container" — rootless Podman container, --read-only rootfs,
//     dropped caps, capped memory/cpu/pids. The default in production.
//     Network egress is governed by ContainerConfig.NoNetwork — lockdown
//     turns seal the namespace (no DNS, no routes); non-lockdown turns
//     get the rootless slirp4netns default so `pip install` and outbound
//     HTTP from bash/python both work.
//   - "host" — the legacy in-process model: bash via os/exec, python via
//     a long-lived python3 subprocess holding the IPython kernel. Used
//     for tests and dev environments without Podman, and as the
//     fallback when Podman is unavailable. Same protocol, same JSON
//     request/response shape on the wire, so callers don't care which
//     backend is in use.
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
	"sync"
	"time"
)

// BashWaitDelay is how long cmd.Wait may keep waiting on the
// stdout/stderr pipes after the command's context is cancelled or the
// process exits. Without it, a grandchild process inheriting the pipes
// (e.g. `server &`) keeps them open and cmd.Run blocks forever — past
// the tool's own timeout — wedging the agent. Folded in from cutlass's
// direct-exec bash path during the P3 sandbox/tools merge.
const BashWaitDelay = 10 * time.Second

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
type cappedBuffer struct {
	buf       bytes.Buffer
	cap       int
	discarded int64
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
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
}

// PythonResult is the parsed pythonResponse the bridge writes to stdout.
type PythonResult struct {
	Status           string
	Output           string
	Stdout           string
	Stderr           string
	Result           string
	Vars             map[string]any
	Error            string
	BridgeTruncation map[string]BridgeCaptureInfo
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
}

// impl is the backend interface. Two concrete implementations live in
// host.go and container.go.
type impl interface {
	runBash(ctx context.Context, req BashRequest) (BashResult, error)
	runPython(ctx context.Context, req PythonRequest) (PythonResult, error)
	close()
}

// Mode reports the backend in use. Useful for tests and for log lines
// that want to disambiguate which path the turn ran through.
func (s *Sandbox) ModeName() string {
	switch s.mode {
	case ModeHost:
		return "host"
	case ModeContainer:
		return "container"
	default:
		return "unknown"
	}
}

// RunBash dispatches one bash invocation through the active backend.
// Returns ErrClosed if the sandbox has already been torn down.
func (s *Sandbox) RunBash(ctx context.Context, req BashRequest) (BashResult, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return BashResult{}, ErrClosed
	}
	s.mu.Unlock()
	return s.impl.runBash(ctx, req)
}

// RunPython dispatches one run_python invocation through the active
// backend. Bridge stdin/stdout serialization is the backend's problem,
// not the caller's.
func (s *Sandbox) RunPython(ctx context.Context, req PythonRequest) (PythonResult, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return PythonResult{}, ErrClosed
	}
	s.mu.Unlock()
	return s.impl.runPython(ctx, req)
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
	s.mu.Unlock()
	s.impl.close()
}

// ErrClosed is returned from RunBash / RunPython after Close has run.
var ErrClosed = errors.New("sandbox is closed")

// ErrContainerUnavailable is returned by Pool.TakeContainer when the
// pool was constructed without a container image.
var ErrContainerUnavailable = errors.New("sandbox: container backend not configured")
