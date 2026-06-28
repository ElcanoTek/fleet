package scheduledrun

import (
	"context"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/tools"
)

type stubEnqueuer struct{}

func (stubEnqueuer) EnqueueTask(_ context.Context, _ models.TaskCreate) (uuid.UUID, string, time.Time, error) {
	return uuid.New(), "pending", time.Time{}, nil
}

func hasCreateTaskTool(ts []fantasy.AgentTool) bool {
	for _, t := range ts {
		if t.Info().Name == tools.CreateTaskToolName {
			return true
		}
	}
	return false
}

// TestMaybeAppendCreateTaskTool_Gate is the security-critical driver-side gate
// test (#277): the create_task tool is wired ONLY for a scheduled run whose task
// opted in (allow_task_creation) AND when an enqueuer is configured. Every other
// combination — including the default (flag unset) — must leave the tool absent,
// so an interactive/unprivileged run can never self-schedule.
func TestMaybeAppendCreateTaskTool_Gate(t *testing.T) {
	base := []fantasy.AgentTool{
		fantasy.NewAgentTool("noop", "noop", func(_ context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("ok"), nil
		}),
	}

	cases := []struct {
		name      string
		enqueuer  tools.TaskEnqueuer
		allow     bool
		wantTool  bool
		wantCount int
	}{
		{name: "default (flag unset) → no tool", enqueuer: stubEnqueuer{}, allow: false, wantTool: false, wantCount: 1},
		{name: "opted in + enqueuer → tool present", enqueuer: stubEnqueuer{}, allow: true, wantTool: true, wantCount: 2},
		{name: "opted in but no enqueuer → no tool", enqueuer: nil, allow: true, wantTool: false, wantCount: 1},
		{name: "not opted in + no enqueuer → no tool", enqueuer: nil, allow: false, wantTool: false, wantCount: 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runner{cfg: &config.Config{MaxCostUSD: 10}, taskEnqueuer: tc.enqueuer}
			task := &models.Task{ID: uuid.New(), AllowTaskCreation: tc.allow}
			got := r.maybeAppendCreateTaskTool(base, task)
			if len(got) != tc.wantCount {
				t.Fatalf("expected %d tools, got %d", tc.wantCount, len(got))
			}
			if hasCreateTaskTool(got) != tc.wantTool {
				t.Fatalf("create_task present=%v, want %v", hasCreateTaskTool(got), tc.wantTool)
			}
			// The base slice must never be mutated in place (a shared turn-tool slice).
			if len(base) != 1 {
				t.Fatalf("base slice was mutated: len=%d", len(base))
			}
		})
	}
}
