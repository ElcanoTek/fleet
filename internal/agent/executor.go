package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/sandbox"
)

// sandboxExecutor implements agentcore.Executor over the SINGLE sandbox backend
// (P3a's *sandbox.Sandbox). Both modes drive their bash/run_python through the
// same Sandbox; this adapter is the agentcore.Executor seam over that backend,
// held on Deps so the loop / finalize hook can surface sandboxed execution.
//
// The native bash/run_python TOOLS the drivers register are built directly with
// tools.NewBashTool(sb) / tools.NewRunPythonTool(sb) over the same *Sandbox —
// this adapter exposes the same execution surface behind the agentcore.Executor
// interface for the loop and any policy/finalize code that needs it without the
// fantasy tool wrapper.
type sandboxExecutor struct {
	sb *sandbox.Sandbox
}

// NewSandboxExecutor wraps a per-run *sandbox.Sandbox as an agentcore.Executor.
// A nil sandbox is permitted at construction; RunBash/RunPython then return an
// error (there is no host-mode escape hatch — the sandbox boundary is the
// point).
func NewSandboxExecutor(sb *sandbox.Sandbox) agentcore.Executor {
	return &sandboxExecutor{sb: sb}
}

const executorDefaultTimeout = 5 * time.Minute

// RunBash executes a bash command in the run's sandbox and returns combined
// stdout+stderr (the executor's "output" is the same combined view the tool
// layer renders, minus the JSON envelope).
func (e *sandboxExecutor) RunBash(ctx context.Context, command string) (string, error) {
	if e.sb == nil {
		return "", fmt.Errorf("executor requires a sandbox; pool.Take returned nil or was bypassed")
	}
	res, err := e.sb.RunBash(ctx, sandbox.BashRequest{
		Command: command,
		Timeout: executorDefaultTimeout,
	})
	if err != nil {
		return "", err
	}
	if res.TimedOut {
		return e.combine(res.Stdout, res.Stderr), fmt.Errorf("bash command timed out after %s", executorDefaultTimeout)
	}
	out := e.combine(res.Stdout, res.Stderr)
	if res.ExitCode != 0 {
		return out, fmt.Errorf("bash exited with code %d", res.ExitCode)
	}
	return out, nil
}

// RunPython executes a Python snippet in the run's sandbox kernel and returns
// the combined output (stdout then stderr, then the bridge's formatted output).
func (e *sandboxExecutor) RunPython(ctx context.Context, code string) (string, error) {
	if e.sb == nil {
		return "", fmt.Errorf("executor requires a sandbox; pool.Take returned nil or was bypassed")
	}
	res, err := e.sb.RunPython(ctx, sandbox.PythonRequest{
		Code:    code,
		Timeout: executorDefaultTimeout,
	})
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	if res.Stdout != "" {
		sb.WriteString(res.Stdout)
	}
	if res.Stderr != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(res.Stderr)
	}
	if res.Output != "" && res.Output != res.Stdout {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(res.Output)
	}
	if res.Error != "" {
		return sb.String(), fmt.Errorf("python error: %s", res.Error)
	}
	return sb.String(), nil
}

func (e *sandboxExecutor) combine(stdout, stderr []byte) string {
	var sb strings.Builder
	if len(stdout) > 0 {
		sb.Write(stdout)
	}
	if len(stderr) > 0 {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.Write(stderr)
	}
	return sb.String()
}
