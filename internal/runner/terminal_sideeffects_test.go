package runner

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/notify"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestNoSuccessSideEffectsWhenTerminalWriteFails pins the #580 fix on the
// success branch: when the terminal success write is rejected (an operator
// cancel cleared the lease in the window before StopTask records its marker),
// the run must fire NO success notification and NO email reply — the DB says
// cancelled, and the external world must agree.
func TestNoSuccessSideEffectsWhenTerminalWriteFails(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// An email-triggered run so BOTH suppressed side effects are armed: the
	// terminal notification and the #511 reply-back to the external sender.
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

	started := make(chan struct{})
	release := make(chan struct{})
	runner := TaskRunnerFunc(func(_ context.Context, task *models.Task) (*models.LogSession, error) {
		close(started)
		<-release
		return &models.LogSession{
			ID: "s-" + task.ID.String(),
			Messages: []models.LogMessage{
				{ID: "a1", Role: "assistant", Content: "Here is your result."},
			},
		}, nil
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

	<-started
	// Operator cancel wins the race: the row flips to cancelled and the lease
	// is cleared, but StopTask's in-process marker is deliberately NOT set —
	// the small window #580 describes. The run then takes the success branch
	// and its terminal write must fail.
	if _, err := store.CancelTaskAtomic(runID, "stopped by test"); err != nil {
		t.Fatalf("CancelTaskAtomic: %v", err)
	}
	close(release)

	// The run finishes and any (buggy) side effects would fire off-thread; give
	// them a generous beat before asserting silence.
	time.Sleep(400 * time.Millisecond)
	cancel()
	<-done

	if got := notif.drain(); len(got) != 0 {
		t.Errorf("terminal write failed but %d notification(s) fired: %+v", len(got), got)
	}
	if got := replier.snapshot(); len(got) != 0 {
		t.Errorf("terminal write failed but %d email reply(ies) fired: %+v", len(got), got)
	}
	final, err := store.GetTask(runID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if final.Status != models.TaskStatusCancelled {
		t.Errorf("final status = %s, want cancelled preserved", final.Status)
	}
}

// TestNoFailureSideEffectsWhenNoTerminalStateLands is the dead-letter mirror of
// the #580 fix: when both the dead-letter transition AND the fallback error
// write are rejected (lease lost — the row is cancelled), no failure
// notification fires.
func TestNoFailureSideEffectsWhenNoTerminalStateLands(t *testing.T) {
	store := newTestStore(t)
	seedTask(t, store, 0, 0) // MaxRetries 0 → a retryable class exhausts immediately

	started := make(chan struct{})
	release := make(chan struct{})
	runner := TaskRunnerFunc(func(_ context.Context, _ *models.Task) (*models.LogSession, error) {
		close(started)
		<-release
		return nil, retryableErr()
	})

	notif := &fakeNotifier{}
	pool := NewPool(store, runner, Config{MaxConcurrentAgents: 1, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour, Notifier: notif})
	poolCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(poolCtx); close(done) }()

	<-started
	var taskID uuid.UUID
	waitFor(t, time.Second, func() bool {
		running, _ := store.GetTasksByStatus(models.TaskStatusRunning)
		if len(running) == 1 {
			taskID = running[0].ID
			return true
		}
		return false
	})
	if _, err := store.CancelTaskAtomic(taskID, "stopped by test"); err != nil {
		t.Fatalf("CancelTaskAtomic: %v", err)
	}
	close(release)

	time.Sleep(400 * time.Millisecond)
	cancel()
	<-done

	if got := notif.drain(); len(got) != 0 {
		t.Errorf("no terminal state landed but %d notification(s) fired: %+v", len(got), got)
	}
}

// TestSuccessSideEffectsFireWhenTerminalWriteLands is the positive control for
// the #580 gating: a normally-completing run still fires its success
// notification — the gate suppresses only runs whose terminal write failed.
func TestSuccessSideEffectsFireWhenTerminalWriteLands(t *testing.T) {
	store := newTestStore(t)
	seedPending(t, store, 1)

	notif := &fakeNotifier{}
	runner := TaskRunnerFunc(func(_ context.Context, task *models.Task) (*models.LogSession, error) {
		return &models.LogSession{ID: "s-" + task.ID.String()}, nil
	})
	pool := NewPool(store, runner, Config{MaxConcurrentAgents: 1, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour, Notifier: notif})
	poolCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(poolCtx); close(done) }()

	waitFor(t, 3*time.Second, func() bool { return len(notif.drain()) == 1 })
	cancel()
	<-done

	if got := notif.drain(); len(got) != 1 || got[0].Status != notify.StatusSuccess {
		t.Fatalf("expected exactly one success notification, got %+v", got)
	}
}

// TestStaleGoroutineCleanupPreservesFreshClaim pins the #581 fix: after a lease
// recovery re-queues a task and the SAME pool re-claims it (a fresh token), the
// stale goroutine's cleanup defer must NOT delete the fresh claim's active-map
// entry — otherwise the live run loses stillOwns fencing (its real terminal
// write is skipped), lease renewal, and StopTask reachability.
func TestStaleGoroutineCleanupPreservesFreshClaim(t *testing.T) {
	store := newTestStore(t)
	seedPending(t, store, 1)

	var (
		mu    sync.Mutex
		calls int
	)
	startedA := make(chan struct{})
	startedB := make(chan struct{})
	releaseA := make(chan struct{})
	releaseB := make(chan struct{})
	runner := TaskRunnerFunc(func(_ context.Context, task *models.Task) (*models.LogSession, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			close(startedA)
			<-releaseA
			return &models.LogSession{ID: "stale-" + task.ID.String()}, nil
		}
		close(startedB)
		<-releaseB
		return &models.LogSession{ID: "fresh-" + task.ID.String()}, nil
	})

	pool := NewPool(store, runner, Config{MaxConcurrentAgents: 2, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour})
	poolCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(poolCtx); close(done) }()

	<-startedA // goroutine A holds the task with token T1

	// Simulate a stalled renewal: force-expire A's lease and run the recovery
	// backstop, re-queueing the task while A is still in flight.
	var taskID uuid.UUID
	waitFor(t, time.Second, func() bool {
		running, _ := store.GetTasksByStatus(models.TaskStatusRunning)
		if len(running) == 1 {
			taskID = running[0].ID
			return true
		}
		return false
	})
	task, _ := store.GetTask(taskID)
	task.MaxRetries = 1 // recovery re-queues only below the retry budget (#1116)
	task.LeaseExpiresAt = ptrTime(time.Now().UTC().Add(-time.Minute))
	if _, err := store.UpdateTask(task); err != nil {
		t.Fatalf("force-expire: %v", err)
	}
	if n, err := pool.RecoverExpiredLeases(); err != nil || n != 1 {
		t.Fatalf("recover: n=%d err=%v, want 1, nil", n, err)
	}

	<-startedB // the same pool re-claimed the task as goroutine B (token T2)
	if got := pool.ActiveTasks(); got != 1 {
		t.Fatalf("ActiveTasks = %d after re-claim, want 1 (same task ID, fresh token)", got)
	}

	// A finishes: its terminal write is skipped (stillOwns is false) and — the
	// #581 fix — its cleanup must leave B's active entry in place. Give A's
	// goroutine a beat to fully unwind before asserting.
	close(releaseA)
	time.Sleep(200 * time.Millisecond)
	if got := pool.ActiveTasks(); got != 1 {
		t.Fatalf("ActiveTasks = %d after the stale goroutine exited, want 1 (fresh claim preserved)", got)
	}

	// B still owns the task, so its real terminal write lands.
	close(releaseB)
	waitFor(t, 3*time.Second, func() bool {
		ok, _ := store.GetTasksByStatus(models.TaskStatusSuccess)
		return len(ok) == 1
	})
	cancel()
	<-done

	ok, _ := store.GetTasksByStatus(models.TaskStatusSuccess)
	if len(ok) != 1 || ok[0].ID != taskID {
		t.Fatalf("fresh claim's success write did not land: %+v", ok)
	}
}

// TestResumedAnswerSurvivesRetry pins the #582 fix: a resumed
// (paused-awaiting-human) task that fails retryably re-queues WITH the human's
// answer still in pending_answer, so the retried claim injects it again. The
// Q&A columns are cleared only once the run reaches a terminal outcome.
func TestResumedAnswerSurvivesRetry(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Drive a task through the real ask/resume lifecycle: claim → running →
	// paused_awaiting_input → resumed with the human's answer.
	task := &models.Task{ID: uuid.New(), Prompt: "resumed work", Status: models.TaskStatusPending, Priority: 1, CreatedAt: time.Now().UTC(), MaxRetries: 1}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	owner := uuid.New()
	claimed, err := store.ClaimNextPendingTask(ctx, owner.String())
	if err != nil || claimed == nil {
		t.Fatalf("claim: %v (task=%v)", err, claimed)
	}
	if _, err := store.UpdateTaskStatusAtomic(task.ID, owner, &models.StatusUpdate{Status: models.TaskStatusRunning}); err != nil {
		t.Fatalf("running: %v", err)
	}
	if ok, err := store.PauseTaskForQuestion(ctx, task.ID, owner, "which currency?"); err != nil || !ok {
		t.Fatalf("pause: ok=%v err=%v", ok, err)
	}
	if ok, err := store.ResumeTask(ctx, task.ID, "USD"); err != nil || !ok {
		t.Fatalf("resume: ok=%v err=%v", ok, err)
	}

	// First resumed attempt fails retryably; the retry must still see the answer.
	var (
		mu      sync.Mutex
		answers []string
	)
	runner := TaskRunnerFunc(func(_ context.Context, task *models.Task) (*models.LogSession, error) {
		mu.Lock()
		answers = append(answers, task.PendingAnswer)
		n := len(answers)
		mu.Unlock()
		if n == 1 {
			return nil, retryableErr()
		}
		return &models.LogSession{ID: "s-" + task.ID.String()}, nil
	})
	pool := NewPool(store, runner, Config{MaxConcurrentAgents: 1, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour})
	poolCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(poolCtx); close(done) }()

	// Wait for the retry re-queue, then fast-forward the backoff so the pool
	// claims the retry immediately instead of waiting out the 30s curve.
	waitFor(t, 3*time.Second, func() bool {
		s, _ := store.GetTasksByStatus(models.TaskStatusScheduled)
		return len(s) == 1
	})
	requeued, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask after requeue: %v", err)
	}
	if requeued.PendingAnswer != "USD" || requeued.PendingQuestion != "which currency?" {
		t.Fatalf("re-queued task lost the pending Q&A: question=%q answer=%q", requeued.PendingQuestion, requeued.PendingAnswer)
	}
	requeued.Status = models.TaskStatusPending
	requeued.ScheduledFor = nil
	if _, err := store.UpdateTask(requeued); err != nil {
		t.Fatalf("fast-forward retry: %v", err)
	}

	waitFor(t, 3*time.Second, func() bool {
		ok, _ := store.GetTasksByStatus(models.TaskStatusSuccess)
		return len(ok) == 1
	})
	cancel()
	<-done

	mu.Lock()
	got := append([]string(nil), answers...)
	mu.Unlock()
	if len(got) != 2 || got[0] != "USD" || got[1] != "USD" {
		t.Fatalf("attempt answers = %q, want the human answer on BOTH attempts", got)
	}
	// Terminal success clears the Q&A columns so a later run never re-injects.
	final, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask final: %v", err)
	}
	if final.PendingQuestion != "" || final.PendingAnswer != "" {
		t.Errorf("terminal outcome must clear pending Q&A, got question=%q answer=%q", final.PendingQuestion, final.PendingAnswer)
	}
}
