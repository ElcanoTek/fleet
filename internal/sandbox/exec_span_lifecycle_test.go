//go:build fleet_host_executor

package sandbox

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/codes"

	"github.com/ElcanoTek/fleet/internal/telemetry/spantest"
)

// The run_python span records the timeout the cell actually runs under: the
// caller's, clamped to the host's per-cell ceiling (FLEET_PYTHON_CELL_TIMEOUT)
// — not the unclamped request (#2052).
func TestRunPythonSpanRecordsClampedTimeout(t *testing.T) {
	sr := spantest.Install(t)
	sb := NewHost(nil)
	defer sb.Close()
	sb.pythonCellTimeout = 2 * time.Second

	_, _ = sb.RunPython(context.Background(), PythonRequest{Code: "1", Timeout: time.Minute})

	exec := sr.EndedNamed(t, "sandbox.exec")
	for _, kv := range exec.Attributes() {
		if kv.Key == attrSandboxTimeout {
			if got := kv.Value.AsInt64(); got != 2000 {
				t.Fatalf("fleet.sandbox.timeout_ms = %d, want the clamped 2000", got)
			}
			sr.AssertAllEnded(t)
			return
		}
	}
	t.Fatal("no fleet.sandbox.timeout_ms attribute")
}

// panicImpl is a backend whose dispatch panics.
type panicImpl struct{ impl }

func (panicImpl) poisoned() bool { return false }
func (panicImpl) runBash(context.Context, BashRequest) (BashResult, error) {
	panic("backend exploded")
}

// A backend panic ends the sandbox.exec span as a "panic" failure and the
// panic propagates unchanged (#2052).
func TestExecSpanEndsOnBackendPanic(t *testing.T) {
	sr := spantest.Install(t)
	sb := &Sandbox{mode: ModeHost, impl: panicImpl{}}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the backend panic did not propagate")
			}
		}()
		_, _ = sb.RunBash(context.Background(), BashRequest{Command: "true"})
	}()
	sr.AssertAllEnded(t)
	spantest.AssertStatus(t, sr.EndedNamed(t, "sandbox.exec"), codes.Error, "panic")
}
