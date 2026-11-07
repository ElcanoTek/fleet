package main

// DB-backed regression for the chat manage_tasks adapter's write path: an
// approved "update" used to go through storage.UpdateTask — an unlocked
// full-row upsert fed the task read moments earlier — which wrote stale
// status/lease/result columns back over a run the scheduler and runner had
// since advanced (the #1104 double-execution shape). It now builds a TaskEdit
// and goes through UpdateEditableTask, which locks the row and refuses a task
// that is no longer pending/scheduled, and it validates a new cron before
// storing it. Gated on DATABASE_URL like the other DB-backed suites.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/httpapi"
	schedmodels "github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/testdb/fleetdb"
)

func TestApplyTaskMutation_LockedEditPath(t *testing.T) {
	t.Parallel()

	store := storage.New()
	store.SetDatabase(fleetdb.SchedDB(t))
	ctx := context.Background()
	now := time.Now().UTC()

	t.Run("a running task is refused, not rewound", func(t *testing.T) {
		lease := uuid.New().String()
		running := &schedmodels.Task{ID: uuid.New(), Prompt: "the long-running job", Status: schedmodels.TaskStatusRunning,
			Priority: schedmodels.PriorityNormal, CreatedAt: now, LeaseOwner: &lease}
		if _, err := store.AddTask(running); err != nil {
			t.Fatalf("AddTask: %v", err)
		}
		_, err := applyTaskMutation(ctx, store, running, httpapi.TaskMutationRequest{Prompt: "a rewritten prompt"})
		if err == nil || !strings.Contains(err.Error(), "no longer be edited") {
			t.Fatalf("mutating a running task: err=%v, want a 'no longer be edited' refusal", err)
		}
		got, err := store.GetTask(running.ID)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if got.Status != schedmodels.TaskStatusRunning || got.Prompt != "the long-running job" {
			t.Fatalf("refused mutation reached the live row: status=%s prompt=%q", got.Status, got.Prompt)
		}
	})

	t.Run("a scheduled task takes a validated cron and refreshes its next fire", func(t *testing.T) {
		future := now.Add(time.Hour)
		scheduled := &schedmodels.Task{ID: uuid.New(), Prompt: "the nightly job", Status: schedmodels.TaskStatusScheduled,
			ScheduledFor: &future, Recurrence: "0 9 * * *", Timezone: "UTC", Priority: schedmodels.PriorityNormal, CreatedAt: now}
		if _, err := store.AddTask(scheduled); err != nil {
			t.Fatalf("AddTask: %v", err)
		}

		if _, err := applyTaskMutation(ctx, store, scheduled, httpapi.TaskMutationRequest{Cron: "every day at nine"}); err == nil {
			t.Fatal("an unparseable cron must be refused before it is stored")
		}
		unchanged, err := store.GetTask(scheduled.ID)
		if err != nil || unchanged.Recurrence != "0 9 * * *" {
			t.Fatalf("refused cron reached the row: recurrence=%q err=%v", unchanged.Recurrence, err)
		}

		detail, err := applyTaskMutation(ctx, store, scheduled, httpapi.TaskMutationRequest{Cron: "0 10 * * *", Model: "test/model"})
		if err != nil {
			t.Fatalf("applyTaskMutation: %v", err)
		}
		if !strings.Contains(detail, "schedule 0 10 * * *") || !strings.Contains(detail, "model test/model") {
			t.Fatalf("detail = %q, want the schedule and model changes named", detail)
		}
		got, err := store.GetTask(scheduled.ID)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		if got.Recurrence != "0 10 * * *" || got.Model == nil || *got.Model != "test/model" {
			t.Fatalf("edit not applied: recurrence=%q model=%v", got.Recurrence, got.Model)
		}
		if got.Status != schedmodels.TaskStatusScheduled || got.ScheduledFor == nil || !got.ScheduledFor.After(now) || got.ScheduledFor.UTC().Hour() != 10 {
			t.Fatalf("next fire not refreshed from the new cron: status=%s scheduled_for=%v", got.Status, got.ScheduledFor)
		}
	})
}
