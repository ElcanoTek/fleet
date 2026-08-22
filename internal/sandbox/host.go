//go:build fleet_host_executor

// host.go is the UNSANDBOXED host executor — bash via os/exec, python via a
// host python3 subprocess. It is fenced behind the `fleet_host_executor` build
// tag (#159) so the unsandboxed-execution code is NOT compiled into a release
// binary (`go build ./...`): the documented guarantee is that the host executor
// "cannot ship enabled in a production build". The test suite opts in
// (`go test -tags fleet_host_executor`; `make test` does this); when the tag is
// absent host_disabled.go provides a stub and MockMode fails closed at boot.

package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/ElcanoTek/fleet/internal/safe"
)

// hostExecutorCompiledIn reports that the host executor is present in this
// build. Read it via HostExecutorCompiledIn(); host_disabled.go sets it false.
const hostExecutorCompiledIn = true

// newHostSandbox builds a host-backed Sandbox. pool.go routes ModeHost here so
// the untagged build (no host executor) returns an error instead of failing to
// compile against NewHost.
func newHostSandbox(bridgeScript []byte) (*Sandbox, error) {
	return NewHost(bridgeScript), nil
}

// hostImpl is the legacy in-process backend: bash via os/exec, python
// via a long-lived python3 subprocess holding the IPython kernel.
//
// This backend exists so unit tests and dev environments without Podman
// can run the agent end-to-end. Production should always pick the
// container backend; nothing in the host backend defends against a
// rogue agent shelling out to the host filesystem beyond the systemd
// profile.
type hostImpl struct {
	bridgeScript []byte // python_bridge.py contents

	mu sync.Mutex // serializes bridge stdin/stdout

	// bridge process state — created lazily on first runPython call so
	// that a sandbox dedicated to bash work doesn't pay the python boot
	// cost.
	bridgeCmd        *exec.Cmd
	bridgeStdin      io.WriteCloser
	bridgeStdout     *bufio.Reader
	bridgeStarted    bool
	bridgeScriptPath string // temp file the script was extracted to
}

// NewHost constructs a host-mode sandbox. bridgeScript is the embedded
// python_bridge.py contents (passed in by the tools package via
// //go:embed). Pass nil to disable run_python — RunPython will return an
// error in that mode, useful for "bash-only" tests.
func NewHost(bridgeScript []byte) *Sandbox {
	return &Sandbox{
		mode: ModeHost,
		impl: &hostImpl{bridgeScript: bridgeScript},
	}
}

func (h *hostImpl) runBash(ctx context.Context, req BashRequest) (BashResult, error) {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Running the caller's shell IS this component's contract: it is the
	// unsandboxed TEST/DEV-ONLY executor behind the fleet_host_executor build
	// tag (#159) — a release build gets the fail-closed stub in
	// host_disabled.go, so this sink cannot ship (ADR-0002 enforcement). The
	// CodeQL go/command-injection finding here is waived in
	// .github/codeql-accepted-findings.json for the same reason (the CodeQL
	// workflow deliberately compiles this file into the database so it is
	// scanned rather than being a coverage hole; in-source codeql[]
	// suppressions do not work with that pipeline — see codeql.yml).
	//nolint:gosec // shell execution is the purpose of this tool
	cmd := exec.CommandContext(cmdCtx, "bash", "-c", req.Command)
	if req.WorkingDir != "" {
		cmd.Dir = req.WorkingDir
	}
	// Run bash as its own process-group leader and SIGKILL the WHOLE group on
	// cancel/timeout (#796): Go's default CommandContext kill signals only the
	// direct child, so backgrounded grandchildren survived a cancelled call.
	// The container backend holds the same invariant via its in-container
	// killer; here we have direct process control.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return os.ErrProcessDone // group-killed above; nothing more to signal
	}
	// Without WaitDelay, cmd.Run blocks until the stdout/stderr pipes
	// close — a background grandchild (e.g. `server &`) holding them
	// open would hang the agent forever, even after the timeout killed
	// bash itself.
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
	if cmdCtx.Err() != nil {
		// The cmd.Cancel above SIGKILLed the whole process group, so the
		// common case (foreground shell + its group) is dead. The host
		// backend has NO PID namespace to tear down, so a process that
		// called setsid to leave the group can still survive — but this
		// backend is the test/dev-only unsandboxed executor (build tag
		// fleet_host_executor) and is explicitly NOT a security boundary
		// (see the type doc), so it makes the best-effort group kill and
		// does not attempt the container backend's namespace-level
		// guarantee. Production always runs the container backend (#796).
		res.TimedOut = errors.Is(cmdCtx.Err(), context.DeadlineExceeded)
		res.Cancelled = !res.TimedOut
		res.CleanupConfirmed = true
		return res, nil
	}
	if execErr != nil && cmd.ProcessState == nil {
		// Failed to start at all (binary missing, etc) — surface as error.
		return res, execErr
	}
	return res, nil
}

// poisoned always reports false for the host backend (#796): there is no
// container to retire, and this test/dev-only executor is not a hardened
// boundary. Production uses the container backend, which does poison+retire.
func (h *hostImpl) poisoned() bool { return false }

// runFileOp runs the SAME embedded fileops.py the container backend uses, but
// as a plain host python3 subprocess (#784) — no container. Running the one
// script in both backends guarantees byte-identical read/write/edit semantics,
// and lets the fileop tests exercise the real executor on a dev box without
// Podman. python3 is a hard dependency of the run_python bridge, so it is
// present wherever this test/dev-only backend is used.
func (h *hostImpl) runFileOp(ctx context.Context, req FileOpRequest) (FileOpResult, error) {
	return h.executeFileOp(ctx, req)
}

func (h *hostImpl) bindFileOpRoot(ctx context.Context, root string) (FileOpRootIdentity, error) {
	res, err := h.executeFileOp(ctx, FileOpRequest{Op: fileOpBindRoot, Path: root, Root: root})
	if err != nil {
		return FileOpRootIdentity{}, err
	}
	return res.rootIdentity, nil
}

func (h *hostImpl) executeFileOp(ctx context.Context, req FileOpRequest) (FileOpResult, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	wire := map[string]any{
		"op":     string(req.Op),
		"path":   req.Path,
		"root":   req.Root,
		"anchor": req.Root,
	}
	switch req.Op {
	case FileOpRead:
		wire["offset"] = req.Offset
		wire["limit"] = req.Limit
	case FileOpWrite:
		wire["data_b64"] = base64.StdEncoding.EncodeToString(req.Data)
	case FileOpEdit:
		wire["old_b64"] = base64.StdEncoding.EncodeToString([]byte(req.OldText))
		wire["new_b64"] = base64.StdEncoding.EncodeToString([]byte(req.NewText))
		wire["replace_all"] = req.ReplaceAll
		if req.ExpectedSHA256 != "" {
			wire["expected_sha256"] = req.ExpectedSHA256
		}
	case fileOpBindRoot:
	default:
		return FileOpResult{}, fmt.Errorf("unknown fileop %q", req.Op)
	}
	if req.testPause > 0 {
		wire["test_pause_ms"] = req.testPause.Milliseconds()
		wire["test_ready_name"] = req.testReadyName
	}
	if req.rootBound {
		wire["expected_dev"] = req.expectedDev
		wire["expected_ino"] = req.expectedIno
	}
	reqJSON, err := json.Marshal(wire)
	if err != nil {
		return FileOpResult{}, fmt.Errorf("marshal fileop: %w", err)
	}

	//nolint:gosec // fixed embedded script; the host backend is the test/dev executor
	cmd := exec.CommandContext(cmdCtx, "python3", "-c", string(fileOpsScript))
	cmd.Stdin = bytes.NewReader(reqJSON)
	out, err := cmd.Output()
	if err != nil {
		if cmdCtx.Err() != nil {
			return FileOpResult{}, fmt.Errorf("fileop %s timed out: %w", req.Op, cmdCtx.Err())
		}
		return FileOpResult{}, fmt.Errorf("fileop %s: %w", req.Op, err)
	}
	return decodeFileOpResponse(out)
}

func (h *hostImpl) runPython(ctx context.Context, req PythonRequest) (PythonResult, error) {
	if h.bridgeScript == nil {
		return PythonResult{}, fmt.Errorf("run_python disabled: no bridge script")
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}

	if err := h.ensureBridge(); err != nil {
		return PythonResult{}, fmt.Errorf("start python bridge: %w", err)
	}

	wireReq := bridgeRequest{
		Code:           req.Code,
		ReturnVars:     req.ReturnVars,
		TimeoutSeconds: int(timeout.Seconds()),
		WorkspaceDir:   req.WorkspaceDir,
		ResetKernel:    req.ResetKernel,
	}
	reqBytes, err := json.Marshal(wireReq)
	if err != nil {
		return PythonResult{}, fmt.Errorf("marshal bridge request: %w", err)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if err := h.sendLocked(reqBytes); err != nil {
		return PythonResult{}, err
	}
	respBytes, err := h.readLocked(ctx, timeout)
	if err != nil {
		return PythonResult{}, err
	}
	return parseBridgeResponse(respBytes)
}

func (h *hostImpl) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.terminateBridgeLocked()
	if h.bridgeScriptPath != "" {
		_ = os.Remove(h.bridgeScriptPath)
		h.bridgeScriptPath = ""
	}
}

// resourceUsage reports no telemetry: the host backend runs in-process with no
// container to sample (#263). The container backend is the only one that polls
// `podman stats`.
func (h *hostImpl) resourceUsage() (ResourceUsageSummary, bool) {
	return ResourceUsageSummary{}, false
}

// ensureBridge spawns the python bridge subprocess if it hasn't been
// started. Idempotent and lazy — the first runPython pays the boot
// cost, subsequent calls reuse the running process.
func (h *hostImpl) ensureBridge() error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.bridgeStarted && h.bridgeCmd != nil && h.bridgeCmd.ProcessState == nil {
		return nil
	}

	if h.bridgeScriptPath == "" {
		f, err := os.CreateTemp("", "chat-bridge-*.py")
		if err != nil {
			return fmt.Errorf("temp file: %w", err)
		}
		if _, err := f.Write(h.bridgeScript); err != nil {
			_ = f.Close()
			return fmt.Errorf("write bridge: %w", err)
		}
		_ = f.Close()
		h.bridgeScriptPath = f.Name()
	}

	//nolint:gosec,noctx // long-running bridge process; lifetime tied to sandbox
	cmd := exec.Command("python3", h.bridgeScriptPath)
	cmd.Stderr = os.Stderr

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
		return fmt.Errorf("start: %w", err)
	}

	h.bridgeCmd = cmd
	h.bridgeStdin = stdin
	h.bridgeStdout = bufio.NewReader(stdout)
	h.bridgeStarted = true

	// Tiny grace period so the bridge has time to set up its stdin reader
	// before we shove a request at it. Matches legacy timing.
	time.Sleep(100 * time.Millisecond)
	return nil
}

func (h *hostImpl) sendLocked(reqBytes []byte) error {
	if _, err := fmt.Fprintf(h.bridgeStdin, "%s\n", reqBytes); err != nil {
		// Pipe may have closed — try a single restart.
		h.terminateBridgeLocked()
		h.mu.Unlock()
		startErr := h.ensureBridge()
		h.mu.Lock()
		if startErr != nil {
			return fmt.Errorf("send (after restart attempt): %w", err)
		}
		if _, err := fmt.Fprintf(h.bridgeStdin, "%s\n", reqBytes); err != nil {
			return fmt.Errorf("send (after restart): %w", err)
		}
	}
	return nil
}

func (h *hostImpl) readLocked(ctx context.Context, timeout time.Duration) ([]byte, error) {
	type readResult struct {
		data []byte
		err  error
	}
	ch := make(chan readResult, 1)
	// Snapshot the reader under h.mu (held by the caller) BEFORE launching
	// the goroutine: the cancel/timeout arms below call
	// terminateBridgeLocked, which nils h.bridgeStdout, so re-reading the
	// FIELD inside the goroutine is a data race — and in the
	// ctx-already-cancelled ordering the goroutine can observe nil and
	// panic (#583). Mirrors containerImpl.runPython.
	stdout := h.bridgeStdout
	go func() {
		defer safe.Recover("sandbox.host.bridge_read", func(any) {
			ch <- readResult{err: fmt.Errorf("bridge reader panicked")}
		})
		data, err := stdout.ReadBytes('\n')
		ch <- readResult{data: data, err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		h.terminateBridgeLocked()
		return nil, fmt.Errorf("python execution cancelled: %w", ctx.Err())
	case <-timer.C:
		h.terminateBridgeLocked()
		return nil, fmt.Errorf("python execution timed out after %v", timeout)
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("bridge closed unexpectedly: %w", r.err)
		}
		return r.data, nil
	}
}

func (h *hostImpl) terminateBridgeLocked() {
	if h.bridgeCmd != nil && h.bridgeCmd.Process != nil {
		_ = h.bridgeCmd.Process.Signal(os.Interrupt)
		// Brief grace period; force-kill if still alive.
		done := make(chan struct{})
		go func() {
			_ = h.bridgeCmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(200 * time.Millisecond):
			_ = h.bridgeCmd.Process.Kill()
			<-done
		}
	}
	if h.bridgeStdin != nil {
		_ = h.bridgeStdin.Close()
	}
	h.bridgeCmd = nil
	h.bridgeStdin = nil
	h.bridgeStdout = nil
	h.bridgeStarted = false
}
