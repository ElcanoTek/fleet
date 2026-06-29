package sandbox

import (
	"context"
	"testing"
	"time"
)

// recordingImpl is a backend stub that captures the PythonRequest it receives,
// so we can assert the per-cell timeout clamp Sandbox.RunPython applies before
// dispatch — without spinning a container or a kernel.
type recordingImpl struct {
	lastPython PythonRequest
}

func (r *recordingImpl) runBash(context.Context, BashRequest) (BashResult, error) {
	return BashResult{}, nil
}
func (r *recordingImpl) runPython(_ context.Context, req PythonRequest) (PythonResult, error) {
	r.lastPython = req
	return PythonResult{Status: "success"}, nil
}
func (r *recordingImpl) resourceUsage() (ResourceUsageSummary, bool) {
	return ResourceUsageSummary{}, false
}
func (r *recordingImpl) close() {}

func TestRunPython_CellTimeoutClamp(t *testing.T) {
	cases := []struct {
		name        string
		cellTimeout time.Duration
		callTimeout time.Duration
		want        time.Duration
	}{
		{"no ceiling passes call timeout through", 0, 300 * time.Second, 300 * time.Second},
		{"ceiling lower than call clamps to ceiling", 120 * time.Second, 300 * time.Second, 120 * time.Second},
		{"ceiling higher than call keeps call (min)", 600 * time.Second, 300 * time.Second, 300 * time.Second},
		{"ceiling with no call limit applies ceiling", 120 * time.Second, 0, 120 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingImpl{}
			sb := &Sandbox{mode: ModeHost, impl: rec}
			sb.SetPythonCellTimeout(tc.cellTimeout)
			if _, err := sb.RunPython(context.Background(), PythonRequest{Code: "x=1", Timeout: tc.callTimeout}); err != nil {
				t.Fatalf("RunPython: %v", err)
			}
			if rec.lastPython.Timeout != tc.want {
				t.Errorf("dispatched timeout = %v, want %v", rec.lastPython.Timeout, tc.want)
			}
		})
	}
}

// TestRunPython_ResetKernelForwarded proves the reset_kernel flag reaches the
// backend unchanged (the bridge acts on it).
func TestRunPython_ResetKernelForwarded(t *testing.T) {
	rec := &recordingImpl{}
	sb := &Sandbox{mode: ModeHost, impl: rec}
	if _, err := sb.RunPython(context.Background(), PythonRequest{Code: "x=1", ResetKernel: true}); err != nil {
		t.Fatalf("RunPython: %v", err)
	}
	if !rec.lastPython.ResetKernel {
		t.Error("ResetKernel must be forwarded to the backend")
	}
}
