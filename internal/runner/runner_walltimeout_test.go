// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestWallTimeoutFromEnv pins the FLEET_TASK_WALL_TIMEOUT parsing contract
// (#724): unset → 4h default, "0" → disabled, a duration → that duration,
// garbage/negative → the default (a typo can never mean "unbounded").
func TestWallTimeoutFromEnv(t *testing.T) {
	cases := map[string]time.Duration{
		"":     DefaultTaskWallClockTimeout,
		"0":    0,
		"0s":   0,
		"90m":  90 * time.Minute,
		"2h":   2 * time.Hour,
		"nope": DefaultTaskWallClockTimeout,
		"-1h":  DefaultTaskWallClockTimeout,
	}
	for in, want := range cases {
		t.Setenv("FLEET_TASK_WALL_TIMEOUT", in)
		if got := wallTimeoutFromEnv(); got != want {
			t.Errorf("FLEET_TASK_WALL_TIMEOUT=%q: got %s, want %s", in, got, want)
		}
	}
}

// TestPoolWallClockTimeout pins the #724 enforcement: a run that outlives the
// wall-clock ceiling is cancelled via its context and fails terminally with a
// clear timeout error — the transient-retry policy is NOT consulted even
// though the task has retries left, because the timeout is deterministic.
func TestPoolWallClockTimeout(t *testing.T) {
	store := newTestStore(t)
	task := &models.Task{
		ID:         uuid.New(),
		Prompt:     "task",
		Status:     models.TaskStatusPending,
		Priority:   1,
		CreatedAt:  time.Now().UTC(),
		MaxRetries: 3, // retries available — the timeout must NOT use them
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	// The fake runner hangs until its context is cancelled — the "hung tool
	// call" shape — then surfaces the context error like agentcore would.
	runner := TaskRunnerFunc(func(ctx context.Context, _ *models.Task) (*models.LogSession, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})

	pool := NewPool(store, runner, Config{
		MaxConcurrentAgents: 1,
		PollInterval:        20 * time.Millisecond,
		LeaseRenewInterval:  time.Hour,
		WallClockTimeout:    150 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()

	waitFor(t, 5*time.Second, func() bool {
		got, err := store.GetTask(task.ID)
		return err == nil && got != nil && got.Status == models.TaskStatusError
	})
	cancel()
	<-done

	got, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != models.TaskStatusError {
		t.Fatalf("status = %s, want %s", got.Status, models.TaskStatusError)
	}
	if got.ErrorMessage == nil || !strings.Contains(*got.ErrorMessage, "wall-clock timeout") {
		t.Errorf("error message should name the wall-clock timeout, got %v", got.ErrorMessage)
	}
	// Deterministic failure: no retry attempt was scheduled (a transient
	// failure with MaxRetries=3 would have re-queued the task instead).
	if got.AttemptCount != 0 {
		t.Errorf("AttemptCount = %d, want 0 (the timeout must not consume retry attempts)", got.AttemptCount)
	}
}

// TestPoolWallClockTimeoutDisabled pins the opt-out: a negative
// Config.WallClockTimeout (or FLEET_TASK_WALL_TIMEOUT=0) installs no deadline,
// so a run longer than any would-be ceiling still completes normally.
func TestPoolWallClockTimeoutDisabled(t *testing.T) {
	store := newTestStore(t)
	seedPending(t, store, 1)

	runner := TaskRunnerFunc(func(ctx context.Context, task *models.Task) (*models.LogSession, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(120 * time.Millisecond):
			return &models.LogSession{ID: "s-" + task.ID.String()}, nil
		}
	})

	pool := NewPool(store, runner, Config{
		MaxConcurrentAgents: 1,
		PollInterval:        20 * time.Millisecond,
		LeaseRenewInterval:  time.Hour,
		WallClockTimeout:    -1, // explicitly disabled
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()

	waitFor(t, 5*time.Second, func() bool {
		tasks, _ := store.GetTasksByStatus(models.TaskStatusSuccess)
		return len(tasks) == 1
	})
	cancel()
	<-done
}
