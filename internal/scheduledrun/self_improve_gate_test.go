package scheduledrun

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// stubTaskMemory is a non-nil TaskMemoryStore placeholder for the gate test.
type stubTaskMemory struct{}

func (stubTaskMemory) UpsertTaskMemory(context.Context, uuid.UUID, string, string, int, int) error {
	return nil
}
func (stubTaskMemory) GetTaskMemory(context.Context, uuid.UUID, string) (string, error) {
	return "", nil
}
func (stubTaskMemory) ListTaskMemories(context.Context, uuid.UUID) ([]tools.TaskMemory, error) {
	return nil, nil
}

// TestSelfImproveTaskMemory_Gate is the trust-critical Captain's Log gate (#285,
// #322): persistent task memory is handed to a run ONLY when its task set
// instruction_self_improve AND the runner was built with the seam. Every other
// combination — including the default (flag unset) — yields nil, so a task that
// did not opt in behaves exactly as before (no remember/recall, no injection).
func TestSelfImproveTaskMemory_Gate(t *testing.T) {
	var tm tools.TaskMemoryStore = stubTaskMemory{}

	cases := []struct {
		name     string
		runnerTM tools.TaskMemoryStore
		optIn    bool
		want     bool
	}{
		{name: "default (flag unset) → nothing wired", runnerTM: tm, optIn: false, want: false},
		{name: "opted in + seam present → wired", runnerTM: tm, optIn: true, want: true},
		{name: "opted in but runner has no seam → nothing", runnerTM: nil, optIn: true, want: false},
		{name: "not opted in + seam present → nothing", runnerTM: tm, optIn: false, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runner{taskMemory: tc.runnerTM}
			task := &models.Task{ID: uuid.New(), InstructionSelfImprove: tc.optIn}
			got := r.selfImproveTaskMemory(task)
			if (got != nil) != tc.want {
				t.Errorf("task memory wired=%v, want %v", got != nil, tc.want)
			}
		})
	}

	// A nil task must not panic and wires nothing.
	if got := (&Runner{taskMemory: tm}).selfImproveTaskMemory(nil); got != nil {
		t.Fatal("nil task must yield nil")
	}
}
