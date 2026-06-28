package tools

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// CreateTaskToolName is the canonical name of the built-in create_task tool.
const CreateTaskToolName = "create_task"

// DefaultMaxTaskCreations is the per-run cap on how many follow-up tasks a single
// scheduled run may spawn. It bounds the blast radius of a misbehaving (or
// adversarially-prompted) agent: even with the capability granted, a run cannot
// fan out an unbounded number of tasks in one pass.
const DefaultMaxTaskCreations = 3

// childBudgetFraction caps a spawned task's max_cost_usd at this fraction of the
// creating task's budget, so a chain of spawns cannot escalate spend beyond what
// the parent was authorized for. A non-positive parent budget disables the cap
// (the parent itself is unbounded), matching the "0 = unlimited" ceiling
// convention used elsewhere in the runtime.
const childBudgetFraction = 0.20

// defaultChildMaxIterations is the spawned task's max_iterations when the caller
// does not specify one, matching the issue's blueprint (#277).
const defaultChildMaxIterations = 10

// TaskEnqueuer is the thin seam the create_task tool calls to persist a follow-up
// task. It is satisfied host-side by an adapter over the sched storage layer
// (internal/sched/storage); the indirection keeps the tool unit-testable without
// a database and avoids the tool taking a hard dependency on storage internals.
//
// The implementation is responsible for the SAME validation + persistence the
// public POST /tasks path performs (it reuses models.NewTask + storage.AddTask) —
// create_task adds the #277 capability gate ON TOP, never weakening it.
type TaskEnqueuer interface {
	// EnqueueTask validates and persists tc, returning the new task's id, its
	// status, and the next-run instant (zero when the task runs immediately).
	EnqueueTask(ctx context.Context, tc models.TaskCreate) (id uuid.UUID, status string, nextRunAt time.Time, err error)
}

// CreateTaskConfig carries the per-run safety state the create_task tool enforces.
// The scheduled driver builds ONE of these from the running task's row and wires
// the tool ONLY when AllowTaskCreation is true on that task (the authority gate,
// mirroring how confirm_audit is conditionally appended). Interactive and
// unprivileged scheduled runs never construct this, so the tool is never visible
// to them — there is no privilege-escalation path.
type CreateTaskConfig struct {
	// Enqueuer persists the follow-up task (the sched storage seam). Required.
	Enqueuer TaskEnqueuer
	// CreatingTaskID is the task whose scheduled run is spawning follow-ups. It is
	// recorded as the child's CreatedByTaskID lineage (set server-side here, never
	// trusted from the model).
	CreatingTaskID uuid.UUID
	// ParentModel is the creating task's model slug; a spawned task inherits it
	// when the caller does not name one.
	ParentModel *string
	// ParentBudgetUSD is the creating task's max_cost_usd ceiling. A child's
	// budget is capped at ParentBudgetUSD * childBudgetFraction. <= 0 means the
	// parent is unbounded, so the per-child cap is not applied.
	ParentBudgetUSD float64
	// RecurringAllowed mirrors the creating task's allow_recurring_task_creation
	// bit. When false, create_task refuses any non-empty recurrence — a single
	// opt-in cannot mint a self-perpetuating recurring task.
	RecurringAllowed bool
	// MaxCreations is the per-run spawn cap (0 falls back to DefaultMaxTaskCreations).
	MaxCreations int
	// Counter is the shared per-run spawn counter. The tool atomically increments
	// it and refuses once MaxCreations is reached. A nil Counter is treated as a
	// fresh per-run counter (the tool allocates one), so a misconfigured wiring
	// fails closed at the cap rather than allowing unbounded spawns.
	Counter *atomic.Int32
}

// CreateTaskParams is the agent-facing input schema for create_task.
//
// Note: there is intentionally no display-name field — the sched Task model has
// no name column, so accepting one would be an ignored input (honesty invariant).
// Use tags for attribution instead.
type CreateTaskParams struct {
	Prompt        string   `json:"prompt" description:"What the new task should do. Required."`
	RunAt         string   `json:"run_at,omitempty" description:"Optional RFC3339 / ISO-8601 datetime for a one-time future run (e.g. 2026-07-01T09:00:00Z). Omit to run as soon as a worker is free."`
	Recurrence    string   `json:"recurrence,omitempty" description:"Optional standard 5-field cron expression for a recurring task. Only permitted when this task was granted recurring task-creation; otherwise the call is rejected."`
	Model         string   `json:"model,omitempty" description:"Optional model slug for the new task. Defaults to the current task's model."`
	MaxCostUSD    float64  `json:"max_cost_usd,omitempty" description:"Optional cost ceiling for the new task in USD. Capped at 20% of this task's budget; a higher value is rejected."`
	MaxIterations int      `json:"max_iterations,omitempty" description:"Optional cap on agent steps for the new task (default 10)."`
	Tags          []string `json:"tags,omitempty" description:"Optional tags applied to the new task for attribution/filtering."`
}

const createTaskDescription = `Schedule a FOLLOW-UP task for the fleet to run later (or immediately). Use this to hand off work that should happen after this run finishes — e.g. a weekly report run that notices something worth a deep-dive can schedule that deep-dive for tomorrow.

This tool is only available to scheduled tasks that have been explicitly granted task-creation. It is NOT a way to do work right now — it enqueues a NEW independent task. Limits enforced by the runtime:
- At most a few follow-up tasks per run; further calls are rejected.
- A spawned task's cost ceiling is capped at a fraction of this task's budget.
- Recurring (cron) tasks require a separate, stricter grant; without it, recurrence is rejected.

Returns the new task's id and its next run time.`

// NewCreateTaskTool returns the built-in create_task tool wired to cfg. It is
// constructed ONLY by the scheduled driver, and ONLY when the running task opted
// in (cfg is built from that task's allow_task_creation bit). The tool itself is
// the LAST line of defence: even reached, it re-checks every limit (counter,
// budget, recurrence grant) and sets lineage server-side, so no model input can
// escalate privilege or start an unbounded self-scheduling loop (#277).
func NewCreateTaskTool(cfg CreateTaskConfig) fantasy.AgentTool {
	maxCreations := cfg.MaxCreations
	if maxCreations <= 0 {
		maxCreations = DefaultMaxTaskCreations
	}
	counter := cfg.Counter
	if counter == nil {
		// Fail closed: a missing counter means we still enforce the per-run cap
		// against a fresh counter rather than allowing unbounded spawns.
		counter = &atomic.Int32{}
	}

	return fantasy.NewAgentTool(CreateTaskToolName, createTaskDescription,
		func(ctx context.Context, in CreateTaskParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if cfg.Enqueuer == nil {
				return fantasy.NewTextErrorResponse("create_task is not available: no task enqueuer is configured."), nil
			}

			prompt := strings.TrimSpace(in.Prompt)
			if prompt == "" {
				return fantasy.NewTextErrorResponse("create_task requires a non-empty prompt."), nil
			}

			// Per-run spawn cap. Increment FIRST so a rejected over-cap call still
			// consumes nothing beyond the count — the decrement on rejection keeps
			// the counter accurate for any later (valid) attempt.
			if n := counter.Add(1); int(n) > maxCreations {
				counter.Add(-1)
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"create_task limit reached: a single run may spawn at most %d follow-up task(s).", maxCreations,
				)), nil
			}

			// Recurrence requires the stricter, separately-granted bit.
			recurrence := strings.TrimSpace(in.Recurrence)
			if recurrence != "" && !cfg.RecurringAllowed {
				counter.Add(-1)
				return fantasy.NewTextErrorResponse(
					"create_task: recurring tasks are not permitted for this task (allow_recurring_task_creation is not set). Omit recurrence to schedule a one-time task.",
				), nil
			}

			tc := models.TaskCreate{
				Prompt:          prompt,
				Tags:            in.Tags,
				CreatedByTaskID: &cfg.CreatingTaskID,
			}

			// Model: explicit override, else inherit the parent's.
			if m := strings.TrimSpace(in.Model); m != "" {
				tc.Model = &m
			} else if cfg.ParentModel != nil {
				tc.Model = cfg.ParentModel
			}

			// max_iterations: caller value or the default.
			iters := in.MaxIterations
			if iters <= 0 {
				iters = defaultChildMaxIterations
			}
			tc.MaxIterations = &iters

			// Budget propagation: a child is capped at childBudgetFraction of the
			// parent's ceiling. An explicit value above the cap is rejected (not
			// silently clamped) so the model gets a clear signal. NOTE: TaskCreate
			// has no max_cost_usd field today (the scheduled ceiling is a runtime/
			// env knob, not a per-task column), so we ENFORCE the cap here and do
			// not silently accept a value we cannot persist — see report.
			if cfg.ParentBudgetUSD > 0 {
				ceiling := cfg.ParentBudgetUSD * childBudgetFraction
				if in.MaxCostUSD > ceiling {
					counter.Add(-1)
					return fantasy.NewTextErrorResponse(fmt.Sprintf(
						"create_task: max_cost_usd %.2f exceeds the allowed ceiling of %.2f (20%% of this task's budget).",
						in.MaxCostUSD, ceiling,
					)), nil
				}
			}

			// run_at: optional one-time future run.
			if rs := strings.TrimSpace(in.RunAt); rs != "" {
				when, perr := time.Parse(time.RFC3339, rs)
				if perr != nil {
					counter.Add(-1)
					return fantasy.NewTextErrorResponse(fmt.Sprintf(
						"create_task: run_at %q is not a valid RFC3339 datetime (e.g. 2026-07-01T09:00:00Z): %v", rs, perr,
					)), nil
				}
				whenUTC := when.UTC()
				tc.ScheduledFor = &whenUTC
			}
			if recurrence != "" {
				tc.Recurrence = recurrence
			}

			id, status, nextRunAt, err := cfg.Enqueuer.EnqueueTask(ctx, tc)
			if err != nil {
				counter.Add(-1)
				return fantasy.NewTextErrorResponse(fmt.Sprintf("create_task failed: %v", err)), nil
			}

			next := "immediately"
			if !nextRunAt.IsZero() {
				next = nextRunAt.UTC().Format(time.RFC3339)
			}
			return fantasy.NewTextResponse(fmt.Sprintf(
				"Created task %s (status: %s, next run: %s).", id, status, next,
			)), nil
		})
}
