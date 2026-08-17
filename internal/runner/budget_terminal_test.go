package runner

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/notify"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestBudgetStoppedRunFailsNotSucceeds is the pool-level regression guard for
// #1105: a scheduled run that hit its cost/token ceiling used to come back from
// the driver as a nil error, fall into executeTask's default branch, and be
// recorded as SUCCESS — success notification and, on an email-triggered run,
// a success reply to the external sender. With the driver now surfacing the
// stop as ErrCostCeilingExceeded, the run must land as a terminal failure with
// the cost_ceiling class (non-retryable by default), fire the FAILURE
// notification, and never reply to the triggering email.
func TestBudgetStoppedRunFailsNotSucceeds(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// An email-triggered run so BOTH success side effects are armed: the
	// terminal notification flavor and the #511 reply-back to the sender.
	template := &models.Task{ID: uuid.New(), Prompt: "handle mail", Status: models.TaskStatusScheduled, Priority: 1, TriggerType: models.TriggerTypeWebhook}
	if _, err := store.AddTask(template); err != nil {
		t.Fatalf("AddTask template: %v", err)
	}
	trigID := uuid.New()
	if err := store.CreateTrigger(ctx, &models.TaskTrigger{ID: trigID, TaskID: template.ID, Slug: "r1", Secret: "s", Kind: models.TriggerKindEmail, EmailPolicy: &models.EmailTriggerPolicy{}}); err != nil {
		t.Fatalf("CreateTrigger: %v", err)
	}
	runID, err := store.SpawnEmailRun(ctx, &models.TaskTrigger{TaskID: template.ID}, "do the work", false)
	if err != nil {
		t.Fatalf("SpawnEmailRun: %v", err)
	}
	ev := &models.TriggerEvent{TriggerID: trigID, IdempotencyKey: "<orig@mail>", Sender: "asker@corp.com", Subject: "Please help", MessageID: "<orig@mail>"}
	if _, err := store.RecordTriggerEvent(ctx, ev); err != nil {
		t.Fatalf("RecordTriggerEvent: %v", err)
	}
	if err := store.SetTriggerEventRunID(ctx, ev.ID, runID); err != nil {
		t.Fatalf("SetTriggerEventRunID: %v", err)
	}

	// The driver's #1105 shape: partial session log + the budget sentinel.
	runner := TaskRunnerFunc(func(_ context.Context, task *models.Task) (*models.LogSession, error) {
		session := &models.LogSession{
			ID: "s-" + task.ID.String(),
			Messages: []models.LogMessage{
				{ID: "a1", Role: "assistant", Content: "partial work before the ceiling"},
			},
		}
		return session, fmt.Errorf("%w: run stopped after $50.0000 spent without finishing the task", agentcore.ErrCostCeilingExceeded)
	})

	notif := &fakeNotifier{}
	replier := &fakeReplier{}
	pool := NewPool(store, runner, Config{
		MaxConcurrentAgents: 1,
		PollInterval:        20 * time.Millisecond,
		LeaseRenewInterval:  time.Hour,
		Notifier:            notif,
		EmailReplier:        replier,
	})
	poolCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(poolCtx); close(done) }()

	// cost_ceiling is non-retryable by default, so the run dead-letters on its
	// first attempt.
	waitFor(t, 5*time.Second, func() bool {
		task, gerr := store.GetTask(runID)
		return gerr == nil && task.Status == models.TaskStatusDeadLettered
	})
	// The failure notification fires off-thread; any (buggy) success reply
	// would too — give both a generous beat before asserting.
	waitFor(t, 3*time.Second, func() bool { return len(notif.drain()) == 1 })
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	final, err := store.GetTask(runID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if final.Status != models.TaskStatusDeadLettered {
		t.Fatalf("final status = %s, want dead_lettered (budget stop must not record success)", final.Status)
	}
	if final.DeadLetterReason == nil || !strings.Contains(*final.DeadLetterReason, models.FailureCostCeiling) {
		t.Errorf("dead-letter reason = %v, want the %q class", final.DeadLetterReason, models.FailureCostCeiling)
	}
	got := notif.drain()
	if len(got) != 1 || got[0].Status != notify.StatusFailure {
		t.Fatalf("notifications = %+v, want exactly one FAILURE", got)
	}
	if replies := replier.snapshot(); len(replies) != 0 {
		t.Errorf("budget-stopped run replied to the triggering email: %+v", replies)
	}
}

// TestCancelledRunWithoutMarkerFailsNotSucceeds pins the Cancelled half of
// #1105 at the pool: a run the driver reports as ErrRunCancelled with NO
// stop/pause marker and a live task ctx (e.g. a provider deadline the run
// classified as a cancel) must land as a terminal failure — previously the nil
// error fell through to the success branch with a truncated transcript.
func TestCancelledRunWithoutMarkerFailsNotSucceeds(t *testing.T) {
	store := newTestStore(t)
	tasks := seedPending(t, store, 1)

	runner := TaskRunnerFunc(func(_ context.Context, task *models.Task) (*models.LogSession, error) {
		return &models.LogSession{ID: "s-" + task.ID.String()},
			fmt.Errorf("%w: context deadline exceeded", agentcore.ErrRunCancelled)
	})

	notif := &fakeNotifier{}
	pool := NewPool(store, runner, Config{MaxConcurrentAgents: 1, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour, Notifier: notif})
	poolCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(poolCtx); close(done) }()

	waitFor(t, 5*time.Second, func() bool {
		task, gerr := store.GetTask(tasks[0].ID)
		return gerr == nil && task.Status == models.TaskStatusDeadLettered
	})
	waitFor(t, 3*time.Second, func() bool { return len(notif.drain()) == 1 })
	cancel()
	<-done

	final, err := store.GetTask(tasks[0].ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if final.Status != models.TaskStatusDeadLettered {
		t.Fatalf("final status = %s, want dead_lettered (unattributed cancel must not record success)", final.Status)
	}
	if got := notif.drain(); len(got) != 1 || got[0].Status != notify.StatusFailure {
		t.Fatalf("notifications = %+v, want exactly one FAILURE", got)
	}
}
