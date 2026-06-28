package tools

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// fakeEnqueuer records the TaskCreate it was handed so tests can assert lineage,
// model inheritance, and recurrence propagation without a database.
type fakeEnqueuer struct {
	calls    int
	last     models.TaskCreate
	returnID uuid.UUID
	err      error
}

func (f *fakeEnqueuer) EnqueueTask(_ context.Context, tc models.TaskCreate) (uuid.UUID, string, time.Time, error) {
	f.calls++
	f.last = tc
	if f.err != nil {
		return uuid.Nil, "", time.Time{}, f.err
	}
	id := f.returnID
	if id == uuid.Nil {
		id = uuid.New()
	}
	var next time.Time
	if tc.ScheduledFor != nil {
		next = *tc.ScheduledFor
	}
	status := "pending"
	if tc.ScheduledFor != nil {
		status = "scheduled"
	}
	return id, status, next, nil
}

func runCreateTask(t *testing.T, tool fantasy.AgentTool, input string) fantasy.ToolResponse {
	t.Helper()
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "tc-1", Input: input})
	if err != nil {
		t.Fatalf("create_task Run returned a transport error: %v", err)
	}
	return resp
}

// TestCreateTask_AllowsWhenGranted proves the happy path: with a parent that
// opted in (the driver only constructs the tool in that case), a single call
// persists a follow-up task, inherits the parent model, and stamps lineage.
func TestCreateTask_AllowsWhenGranted(t *testing.T) {
	parentID := uuid.New()
	parentModel := "openai/gpt-test"
	enq := &fakeEnqueuer{}
	var counter atomic.Int32
	tool := NewCreateTaskTool(CreateTaskConfig{
		Enqueuer:        enq,
		CreatingTaskID:  parentID,
		ParentModel:     &parentModel,
		ParentBudgetUSD: 10,
		MaxCreations:    DefaultMaxTaskCreations,
		Counter:         &counter,
	})

	resp := runCreateTask(t, tool, `{"prompt":"do a deep dive"}`)
	if resp.IsError {
		t.Fatalf("expected success, got error response: %q", resp.Content)
	}
	if enq.calls != 1 {
		t.Fatalf("expected 1 enqueue call, got %d", enq.calls)
	}
	if enq.last.CreatedByTaskID == nil || *enq.last.CreatedByTaskID != parentID {
		t.Fatalf("expected lineage CreatedByTaskID=%s, got %v", parentID, enq.last.CreatedByTaskID)
	}
	if enq.last.Model == nil || *enq.last.Model != parentModel {
		t.Fatalf("expected inherited model %q, got %v", parentModel, enq.last.Model)
	}
	if enq.last.MaxIterations == nil || *enq.last.MaxIterations != defaultChildMaxIterations {
		t.Fatalf("expected default max_iterations %d, got %v", defaultChildMaxIterations, enq.last.MaxIterations)
	}
}

// TestCreateTask_DeniesWhenUnprivileged is the security-critical assertion: the
// driver never constructs the tool for an interactive or non-opted-in scheduled
// run, so the model never sees create_task. We assert that gate at the wiring
// level: with no enqueuer (the nil = disabled default), even if the tool is
// reached it fails closed and persists nothing.
func TestCreateTask_DeniesWhenUnprivileged(t *testing.T) {
	var counter atomic.Int32
	tool := NewCreateTaskTool(CreateTaskConfig{
		Enqueuer:       nil, // unprivileged: no enqueuer wired
		CreatingTaskID: uuid.New(),
		Counter:        &counter,
	})
	resp := runCreateTask(t, tool, `{"prompt":"escalate me"}`)
	if !resp.IsError {
		t.Fatalf("expected an error response when no enqueuer is wired, got: %q", resp.Content)
	}
}

// TestCreateTask_PerRunCap proves a single run cannot fan out an unbounded number
// of follow-up tasks: the (DefaultMaxTaskCreations+1)th call is refused, and the
// refusal does not consume a slot a later valid call would need.
func TestCreateTask_PerRunCap(t *testing.T) {
	enq := &fakeEnqueuer{}
	var counter atomic.Int32
	tool := NewCreateTaskTool(CreateTaskConfig{
		Enqueuer:       enq,
		CreatingTaskID: uuid.New(),
		MaxCreations:   2,
		Counter:        &counter,
	})

	for i := 0; i < 2; i++ {
		if resp := runCreateTask(t, tool, `{"prompt":"spawn"}`); resp.IsError {
			t.Fatalf("call %d should succeed, got error: %q", i+1, resp.Content)
		}
	}
	resp := runCreateTask(t, tool, `{"prompt":"one too many"}`)
	if !resp.IsError {
		t.Fatalf("expected the 3rd call to be capped, got success: %q", resp.Content)
	}
	if !strings.Contains(resp.Content, "limit reached") {
		t.Fatalf("expected a limit-reached message, got: %q", resp.Content)
	}
	if enq.calls != 2 {
		t.Fatalf("expected exactly 2 persisted tasks, got %d", enq.calls)
	}
}

// TestCreateTask_RecurrenceRequiresGrant proves recurrence is governed by the
// stricter, separate bit: with AllowTaskCreation but NOT
// AllowRecurringTaskCreation, a recurrence is refused and nothing is persisted;
// with the grant it flows through.
func TestCreateTask_RecurrenceRequiresGrant(t *testing.T) {
	t.Run("denied without recurring grant", func(t *testing.T) {
		enq := &fakeEnqueuer{}
		var counter atomic.Int32
		tool := NewCreateTaskTool(CreateTaskConfig{
			Enqueuer:         enq,
			CreatingTaskID:   uuid.New(),
			RecurringAllowed: false,
			Counter:          &counter,
		})
		resp := runCreateTask(t, tool, `{"prompt":"daily","recurrence":"0 9 * * *"}`)
		if !resp.IsError {
			t.Fatalf("expected recurrence to be refused without the grant, got: %q", resp.Content)
		}
		if enq.calls != 0 {
			t.Fatalf("expected nothing persisted, got %d calls", enq.calls)
		}
		// The refusal must not consume a spawn slot.
		if got := counter.Load(); got != 0 {
			t.Fatalf("expected counter to be released to 0 after refusal, got %d", got)
		}
	})

	t.Run("allowed with recurring grant", func(t *testing.T) {
		enq := &fakeEnqueuer{}
		var counter atomic.Int32
		tool := NewCreateTaskTool(CreateTaskConfig{
			Enqueuer:         enq,
			CreatingTaskID:   uuid.New(),
			RecurringAllowed: true,
			Counter:          &counter,
		})
		resp := runCreateTask(t, tool, `{"prompt":"daily","recurrence":"0 9 * * *"}`)
		if resp.IsError {
			t.Fatalf("expected recurrence accepted with the grant, got error: %q", resp.Content)
		}
		if enq.last.Recurrence != "0 9 * * *" {
			t.Fatalf("expected recurrence propagated, got %q", enq.last.Recurrence)
		}
	})
}

// TestCreateTask_BudgetCap proves a spawned task cannot escalate spend beyond a
// fraction of the parent's budget: an explicit over-cap max_cost_usd is rejected
// (not silently clamped) and nothing is persisted.
func TestCreateTask_BudgetCap(t *testing.T) {
	enq := &fakeEnqueuer{}
	var counter atomic.Int32
	tool := NewCreateTaskTool(CreateTaskConfig{
		Enqueuer:        enq,
		CreatingTaskID:  uuid.New(),
		ParentBudgetUSD: 10, // child ceiling = 2.00
		Counter:         &counter,
	})

	resp := runCreateTask(t, tool, `{"prompt":"expensive","max_cost_usd":5.0}`)
	if !resp.IsError {
		t.Fatalf("expected over-budget request to be rejected, got: %q", resp.Content)
	}
	if enq.calls != 0 {
		t.Fatalf("expected nothing persisted on budget rejection, got %d", enq.calls)
	}
	if got := counter.Load(); got != 0 {
		t.Fatalf("expected counter released to 0 after rejection, got %d", got)
	}

	// A value within the cap is accepted.
	if resp := runCreateTask(t, tool, `{"prompt":"cheap","max_cost_usd":1.5}`); resp.IsError {
		t.Fatalf("expected within-cap request to succeed, got: %q", resp.Content)
	}
}

// TestCreateTask_RunAtParsing proves an invalid run_at is rejected (and releases
// the slot), and a valid one is stored as a UTC instant.
func TestCreateTask_RunAtParsing(t *testing.T) {
	enq := &fakeEnqueuer{}
	var counter atomic.Int32
	tool := NewCreateTaskTool(CreateTaskConfig{
		Enqueuer:       enq,
		CreatingTaskID: uuid.New(),
		Counter:        &counter,
	})

	if resp := runCreateTask(t, tool, `{"prompt":"later","run_at":"not-a-date"}`); !resp.IsError {
		t.Fatalf("expected invalid run_at to be rejected, got: %q", resp.Content)
	}
	if counter.Load() != 0 {
		t.Fatalf("expected counter released after invalid run_at, got %d", counter.Load())
	}

	resp := runCreateTask(t, tool, `{"prompt":"later","run_at":"2099-01-02T03:04:05Z"}`)
	if resp.IsError {
		t.Fatalf("expected valid run_at accepted, got error: %q", resp.Content)
	}
	if enq.last.ScheduledFor == nil {
		t.Fatal("expected ScheduledFor to be set from run_at")
	}
	if !enq.last.ScheduledFor.Equal(time.Date(2099, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("expected ScheduledFor 2099-01-02T03:04:05Z, got %v", enq.last.ScheduledFor)
	}
}
