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
	"github.com/ElcanoTek/fleet/internal/identity"
	schedmodels "github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/testdb/fleetdb"
	"github.com/ElcanoTek/fleet/internal/tools"
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

// A user asked for a small change to a scheduled email; the model, unable to
// read the stored prompt, retyped it and dropped its rules. Edits apply to the
// stored text, and an edit that does not match leaves the task alone.
func TestApplyTaskMutation_PromptEdits(t *testing.T) {
	t.Parallel()

	store := storage.New()
	store.SetDatabase(fleetdb.SchedDB(t))
	ctx := context.Background()
	now := time.Now().UTC()

	t.Run("prompt edits apply to the stored prompt and keep the rest", func(t *testing.T) {
		future := now.Add(time.Hour)
		stored := "Pull the last 7 days.\nNever pass a country filter.\nColumns: date, spend."
		task := &schedmodels.Task{ID: uuid.New(), Prompt: stored, Status: schedmodels.TaskStatusScheduled,
			ScheduledFor: &future, Recurrence: "0 6 * * *", Timezone: "UTC", Priority: schedmodels.PriorityNormal, CreatedAt: now}
		if _, err := store.AddTask(task); err != nil {
			t.Fatalf("AddTask: %v", err)
		}

		_, err := applyTaskMutation(ctx, store, task, httpapi.TaskMutationRequest{
			PromptEdits: []tools.PromptEdit{{Old: "Columns: date, spend.", New: "Columns: date, spend, trend."}, {Old: "a rule nobody wrote", New: "x"}},
		})
		if err == nil || !strings.Contains(err.Error(), "prompt edit 2") {
			t.Fatalf("err = %v, want edit 2 refused", err)
		}
		if got, _ := store.GetTask(task.ID); got == nil || got.Prompt != stored {
			t.Fatalf("a refused edit reached the row: %+v", got)
		}

		detail, err := applyTaskMutation(ctx, store, task, httpapi.TaskMutationRequest{
			PromptEdits:  []tools.PromptEdit{{Old: "last 7 days", New: "last 14 days"}},
			PromptAppend: "Add a 14-day trend section.",
		})
		if err != nil {
			t.Fatalf("applyTaskMutation: %v", err)
		}
		if detail != "prompt (1 edit, text appended)" {
			t.Errorf("detail = %q", detail)
		}
		got, err := store.GetTask(task.ID)
		if err != nil {
			t.Fatalf("reload: %v", err)
		}
		want := "Pull the last 14 days.\nNever pass a country filter.\nColumns: date, spend.\n\nAdd a 14-day trend section."
		if got.Prompt != want {
			t.Fatalf("prompt = %q, want %q", got.Prompt, want)
		}
		if got.Recurrence != "0 6 * * *" || got.Status != schedmodels.TaskStatusScheduled {
			t.Fatalf("an edit touched more than the prompt: recurrence=%q status=%s", got.Recurrence, got.Status)
		}

		// An edit that empties the prompt is refused by the API's own bound.
		if _, err := applyTaskMutation(ctx, store, got, httpapi.TaskMutationRequest{
			PromptEdits: []tools.PromptEdit{{Old: got.Prompt, New: ""}},
		}); err == nil || !strings.Contains(err.Error(), "at least") {
			t.Fatalf("emptying the prompt: err = %v, want the length bound", err)
		}
		if again, _ := store.GetTask(task.ID); again == nil || again.Prompt != want {
			t.Fatalf("a refused edit reached the row: %+v", again)
		}
	})
}

// match.model is applied in memory, so it must see every page the storage
// filter returns: matches past the first page were silently dropped.
func TestResolveManagedTasksModelPagesThroughAllRows(t *testing.T) {
	t.Parallel()

	store := storage.New()
	store.SetDatabase(fleetdb.SchedDB(t))
	ctx := context.Background()
	now := time.Now().UTC()
	owner, err := store.Identity().CreateAccount(ctx, identity.NewAccount{Email: "pager@example.com", Role: identity.RoleMember})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	add := func(model string, n int) {
		for i := 0; i < n; i++ {
			m := model
			task := &schedmodels.Task{ID: uuid.New(), Prompt: "a job", Status: schedmodels.TaskStatusPending,
				Priority: schedmodels.PriorityNormal, CreatedAt: now, CreatedBy: &owner.ID, Model: &m}
			if _, err := store.AddTask(task); err != nil {
				t.Fatalf("AddTask: %v", err)
			}
		}
	}
	add("old/model", 2)
	add("other/model", managedTasksScanPage+5)
	add("old/model", 1)

	got, err := resolveManagedTasks(store, httpapi.TaskMutationRequest{Match: &httpapi.TaskMutationMatch{Model: "old/model"}}, &owner.ID)
	if err != nil {
		t.Fatalf("resolveManagedTasks: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("matched %d tasks on old/model, want all 3 across pages", len(got))
	}

	add("old/model", tools.MaxManagedTasks)
	if _, err := resolveManagedTasks(store, httpapi.TaskMutationRequest{Match: &httpapi.TaskMutationMatch{Model: "old/model"}}, &owner.ID); err == nil ||
		!strings.Contains(err.Error(), "more than") {
		t.Fatalf("err = %v, want the cap refusal", err)
	}
}

func TestTaskSnapshot(t *testing.T) {
	t.Parallel()
	m := "x-ai/grok-4.6"
	task := &schedmodels.Task{ID: uuid.New(), Prompt: "p", Status: schedmodels.TaskStatusScheduled, Recurrence: "0 6 * * *",
		Timezone: "UTC", Model: &m, Tags: []string{"email"}}
	got := taskSnapshot(task, "Daily email")
	if got.ID != task.ID.String() || got.Label != "Daily email" || got.Prompt != "p" || got.Cron != "0 6 * * *" ||
		got.Model != m || got.Status != "scheduled" || len(got.Tags) != 1 {
		t.Fatalf("snapshot = %#v", got)
	}
}

// show runs through the update's own resolution and ownership filter: the
// requester reads the prompts of their own tasks, and someone else's task named
// by id is reported without its prompt. Nothing is written either way.
func TestTaskManagerProviderShow(t *testing.T) {
	t.Parallel()

	store := storage.New()
	store.SetDatabase(fleetdb.SchedDB(t))
	ctx := context.Background()
	now := time.Now().UTC()

	owner, err := store.Identity().CreateAccount(ctx, identity.NewAccount{Email: "owner@example.com", Role: identity.RoleMember})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	other, err := store.Identity().CreateAccount(ctx, identity.NewAccount{Email: "other@example.com", Role: identity.RoleMember})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	future := now.Add(time.Hour)
	mine := &schedmodels.Task{ID: uuid.New(), Title: "Morning email", Prompt: "Morning email. Never pass a country filter.",
		Status: schedmodels.TaskStatusScheduled, ScheduledFor: &future, Recurrence: "0 6 * * *", Timezone: "UTC",
		Priority: schedmodels.PriorityNormal, CreatedAt: now, CreatedBy: &owner.ID}
	theirs := &schedmodels.Task{ID: uuid.New(), Title: "Their email", Prompt: "Morning email for someone else.",
		Status: schedmodels.TaskStatusScheduled, ScheduledFor: &future, Recurrence: "0 6 * * *", Timezone: "UTC",
		Priority: schedmodels.PriorityNormal, CreatedAt: now, CreatedBy: &other.ID}
	for _, task := range []*schedmodels.Task{mine, theirs} {
		if _, err := store.AddTask(task); err != nil {
			t.Fatalf("AddTask: %v", err)
		}
	}
	manage := taskManagerProvider(store)

	res, err := manage(ctx, httpapi.TaskMutationRequest{Action: tools.ManageTasksActionShow,
		Match: &httpapi.TaskMutationMatch{Query: "Morning email"}, RequestedBy: "owner@example.com"})
	if err != nil {
		t.Fatalf("show by match: %v", err)
	}
	if len(res.Shown) != 1 || res.Shown[0].ID != mine.ID.String() || res.Shown[0].Prompt != mine.Prompt || res.Shown[0].Cron != "0 6 * * *" {
		t.Fatalf("show by match = %#v, want only the requester's own task", res.Shown)
	}
	if len(res.Changed) != 0 {
		t.Fatalf("show reported changes: %#v", res.Changed)
	}

	res, err = manage(ctx, httpapi.TaskMutationRequest{Action: tools.ManageTasksActionShow,
		TaskIDs: []string{theirs.ID.String()}, RequestedBy: "owner@example.com"})
	if err != nil {
		t.Fatalf("show by id: %v", err)
	}
	if len(res.Shown) != 0 || len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Detail, "did not create") {
		t.Fatalf("show of someone else's task = %#v, want it skipped without its prompt", res)
	}
	for _, task := range []*schedmodels.Task{mine, theirs} {
		if got, _ := store.GetTask(task.ID); got == nil || got.Prompt != task.Prompt {
			t.Fatalf("show wrote to a task: %+v", got)
		}
	}
}
