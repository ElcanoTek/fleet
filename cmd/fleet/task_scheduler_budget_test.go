package main

// DB-backed test for the THIRD task-create path's budget gate (#601 part 2):
// the chat schedule_task seam (taskSchedulerProvider) must run the SAME shared
// enforcement POST /tasks and /tasks/batch run, keyed on the approving chat
// user's email — otherwise scheduling from chat would be a budget bypass.
// Gated on DATABASE_URL like the other DB-backed suites.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/httpapi"
	"github.com/ElcanoTek/fleet/internal/sched/budget"
	schedmodels "github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

func TestTaskSchedulerProvider_BudgetGate(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping DB-backed test")
	}
	tmpDir := t.TempDir()
	store := storage.New()
	if err := store.Initialize(filepath.Join(tmpDir, "test.db"), storage.DefaultPoolConfig()); err != nil {
		if strings.Contains(err.Error(), "connection refused") {
			t.Skipf("database unavailable: %v", err)
		}
		t.Fatalf("storage init: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	for _, q := range []string{"DELETE FROM budgets", "DELETE FROM logs", "DELETE FROM tasks", "DELETE FROM users"} {
		if _, err := store.DB().Conn().ExecContext(ctx, q); err != nil {
			t.Fatalf("cleanup %q: %v", q, err)
		}
	}

	// alice has a $10/day budget and has already spent $12 today — seeded as a
	// REAL task + iteration attributed to her user row, the same metering the
	// part-1 usage model aggregates.
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	alice := &schedmodels.User{ID: uuid.New(), Username: "alice@example.com", Role: "client", CreatedAt: now}
	if _, err := store.AddUser(alice); err != nil {
		t.Fatalf("AddUser: %v", err)
	}
	task := &schedmodels.Task{ID: uuid.New(), Prompt: "seed", Status: schedmodels.TaskStatusSuccess, CreatedAt: now, CreatedBy: &alice.ID}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	if err := store.AddTaskIteration(ctx, &schedmodels.TaskIteration{
		ID: uuid.New(), TaskID: task.ID, IterationNumber: 1, Status: "completed",
		StartedAt: now.Add(-time.Hour), CostUSD: 12, PromptTokens: 100,
	}); err != nil {
		t.Fatalf("AddTaskIteration: %v", err)
	}
	if _, err := store.UpsertBudget(ctx, schedmodels.BudgetCreate{
		Scope: schedmodels.BudgetScopeUser, PrincipalID: "alice@example.com",
		Window: schedmodels.BudgetWindowDay, HardUSD: func() *float64 { v := 10.0; return &v }(),
	}); err != nil {
		t.Fatalf("UpsertBudget: %v", err)
	}

	enforcer := budget.New(budget.Config{Store: store, Now: func() time.Time { return now }})
	// The deployment default keeps these requests runnable, so this test stays
	// about the budget gate rather than tripping the create-time model gate (#1014).
	provider := taskSchedulerProvider(store, enforcer, "test/model")

	// The budgeted user is refused BEFORE any task is created.
	_, err := provider(ctx, httpapi.TaskScheduleRequest{Prompt: "do the thing tomorrow", RequestedBy: "alice@example.com"})
	var exceeded *budget.ExceededError
	if !errors.As(err, &exceeded) {
		t.Fatalf("want ExceededError from the chat path, got %v", err)
	}
	tasks, err := store.GetAllTasks()
	if err != nil {
		t.Fatalf("GetAllTasks: %v", err)
	}
	if len(tasks) != 1 { // only the seeded task
		t.Errorf("refused schedule_task must not create a task (have %d rows)", len(tasks))
	}

	// A different user (no budget) schedules fine — behavior unchanged.
	res, err := provider(ctx, httpapi.TaskScheduleRequest{Prompt: "unbudgeted user schedules", RequestedBy: "bob@example.com"})
	if err != nil {
		t.Fatalf("unbudgeted schedule_task: %v", err)
	}
	if res.ID == "" {
		t.Error("expected a created task id")
	}

	// EnqueueTask skips the HTTP handler's validation, so the chat path enforces
	// the create-time model gate itself (#1014) — otherwise this is the one seam
	// that can enqueue a task the dispatcher will only ever dead-letter.
	t.Run("no model and no deployment default is refused", func(t *testing.T) {
		before, err := store.GetAllTasks()
		if err != nil {
			t.Fatalf("GetAllTasks: %v", err)
		}
		modelless := taskSchedulerProvider(store, enforcer, "")
		if _, err := modelless(ctx, httpapi.TaskScheduleRequest{
			Prompt: "would never run", RequestedBy: "carol@example.com",
		}); err == nil || !strings.Contains(err.Error(), "no model configured") {
			t.Fatalf("want a no-model refusal, got %v", err)
		}
		after, err := store.GetAllTasks()
		if err != nil {
			t.Fatalf("GetAllTasks: %v", err)
		}
		if len(after) != len(before) {
			t.Errorf("a refused schedule_task must not create a task (%d → %d rows)", len(before), len(after))
		}
	})

	// Attribution (#980): run-log reads are creator-scoped, so a chat-scheduled
	// task must record the approving user when that user exists in sched —
	// otherwise its own author cannot read its transcript.
	t.Run("attributes the task to the approving chat user", func(t *testing.T) {
		erin := &schedmodels.User{ID: uuid.New(), Username: "erin@example.com", Role: "client", CreatedAt: now}
		if _, err := store.AddUser(erin); err != nil {
			t.Fatalf("AddUser: %v", err)
		}
		res, err := provider(ctx, httpapi.TaskScheduleRequest{
			Prompt: "attributed to erin", RequestedBy: "Erin@Example.com", // case-insensitive lookup
		})
		if err != nil {
			t.Fatalf("schedule_task: %v", err)
		}
		created, err := store.GetTask(uuid.MustParse(res.ID))
		if err != nil || created == nil {
			t.Fatalf("GetTask: %v", err)
		}
		if created.CreatedBy == nil || *created.CreatedBy != erin.ID {
			t.Fatalf("want created_by=%s, got %v", erin.ID, created.CreatedBy)
		}
	})

	// A chat user with no sched account falls through to an unattributed task
	// rather than failing the schedule — the pre-#980 behavior for that case.
	t.Run("unknown chat user leaves the task unattributed", func(t *testing.T) {
		res, err := provider(ctx, httpapi.TaskScheduleRequest{
			Prompt: "no sched account", RequestedBy: "nobody@example.com",
		})
		if err != nil {
			t.Fatalf("schedule_task: %v", err)
		}
		created, err := store.GetTask(uuid.MustParse(res.ID))
		if err != nil || created == nil {
			t.Fatalf("GetTask: %v", err)
		}
		if created.CreatedBy != nil {
			t.Fatalf("want an unattributed task, got created_by=%v", created.CreatedBy)
		}
	})

	// An explicit model is enough even with no deployment default.
	t.Run("explicit model needs no deployment default", func(t *testing.T) {
		modelless := taskSchedulerProvider(store, enforcer, "")
		res, err := modelless(ctx, httpapi.TaskScheduleRequest{
			Prompt: "pins its own model", RequestedBy: "dave@example.com", Model: "vendor/pinned",
		})
		if err != nil {
			t.Fatalf("schedule_task with an explicit model: %v", err)
		}
		if res.ID == "" {
			t.Error("expected a created task id")
		}
	})
}
