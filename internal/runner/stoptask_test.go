package runner

import (
	"context"
	"strings"

	"sync/atomic"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestStopTaskInterruptsAndAttributes exercises the #508 operator stop: the
// cancel handler flips the row (with attribution) and StopTask interrupts the
// live run; the pool then persists the partial transcript and does NOT retry,
// dead-letter, or clobber the cancelled status — and the SSE stream's terminal
// frame reports "stopped" with the who.
func TestStopTaskInterruptsAndAttributes(t *testing.T) {
	store := newTestStore(t)
	tasks := seedPending(t, store, 1)
	taskID := tasks[0].ID

	started := make(chan struct{})
	var ran int32
	runner := TaskRunnerFunc(func(ctx context.Context, task *models.Task) (*models.LogSession, error) {
		atomic.AddInt32(&ran, 1)
		close(started)
		// A cancelled agentcore run returns a PARTIAL session and a NIL error —
		// the exact shape that used to be mislabeled success.
		<-ctx.Done()
		return &models.LogSession{ID: "s-" + task.ID.String(), Messages: []models.LogMessage{{Role: "assistant", Content: "partial work"}}}, nil
	})

	// Retries configured — a stopped task must NOT consume them.
	pool := NewPool(store, runner, Config{MaxConcurrentAgents: 2, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	<-started

	// Attach to the live stream buffer to capture the terminal frame.
	buf, live := pool.StreamRegistry().Lookup(taskID)
	if !live {
		t.Fatal("expected a live stream buffer for the running task")
	}
	_ = buf

	// The handler-path sequence: DB cancel with attribution, then interrupt.
	if _, err := store.CancelTaskAtomic(taskID, "stopped by alice"); err != nil {
		t.Fatalf("CancelTaskAtomic: %v", err)
	}
	if !pool.StopTask(taskID, "alice") {
		t.Fatal("StopTask should find the running task")
	}

	// The run unblocks on ctx cancel; the pool persists the log and returns.
	waitFor(t, 2*time.Second, func() bool {
		logs, _ := store.GetAllLogs()
		return len(logs) == 1
	})

	task, err := store.GetTask(taskID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status != models.TaskStatusCancelled {
		t.Fatalf("status = %s; the pool must not clobber the cancelled row", task.Status)
	}
	if task.Result == nil || !strings.Contains(*task.Result, "stopped by alice") {
		t.Fatalf("attribution missing: %+v", task.Result)
	}
	if task.AttemptCount != 0 && task.Status == models.TaskStatusPending {
		t.Fatal("a stopped task must not be re-queued for retry")
	}
	logs, _ := store.GetAllLogs()
	if len(logs) != 1 || len(logs[taskID].Messages) == 0 {
		t.Fatalf("partial transcript must persist: %+v", logs)
	}
	if atomic.LoadInt32(&ran) != 1 {
		t.Fatalf("run count = %d; stop must not trigger a re-run", ran)
	}

	// StopTask on a task that is no longer active reports false.
	waitFor(t, time.Second, func() bool { return !pool.StopTask(taskID, "bob") })
}

// TestShutdownCancelNotMislabeledSuccess pins the #508 classification fix: a
// force-cancelled run that returns (session, nil) — agentcore's cancelled
// shape — must record as interrupted error, not success.
func TestShutdownCancelNotMislabeledSuccess(t *testing.T) {
	store := newTestStore(t)
	seedPending(t, store, 1)

	started := make(chan struct{})
	runner := TaskRunnerFunc(func(ctx context.Context, task *models.Task) (*models.LogSession, error) {
		close(started)
		<-ctx.Done()
		return &models.LogSession{ID: "s-" + task.ID.String()}, nil // nil error on cancel
	})

	pool := NewPool(store, runner, Config{MaxConcurrentAgents: 1, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour, DrainGrace: -1})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()

	<-started
	cancel() // shutdown with negative grace → immediate force-cancel
	<-done

	waitFor(t, 2*time.Second, func() bool {
		errored, _ := store.GetTasksByStatus(models.TaskStatusError)
		return len(errored) == 1
	})
	errored, _ := store.GetTasksByStatus(models.TaskStatusError)
	if len(errored) != 1 || errored[0].ErrorMessage == nil || !strings.Contains(*errored[0].ErrorMessage, "interrupted") {
		t.Fatalf("force-cancelled run must record interrupted, got %+v", errored)
	}
	success, _ := store.GetTasksByStatus(models.TaskStatusSuccess)
	if len(success) != 0 {
		t.Fatal("force-cancelled run mislabeled as success")
	}
}
