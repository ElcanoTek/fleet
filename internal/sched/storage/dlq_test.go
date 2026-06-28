package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestDeadLetterColumnsRoundTrip pins the persistence of the DLQ columns (#253):
// a task carrying dead_lettered_at / dead_letter_reason / dead_letter_attempts
// round-trips through AddTask + GetTask, and a plain task reads them back zero/nil.
func TestDeadLetterColumnsRoundTrip(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	plain := &models.Task{ID: uuid.New(), Prompt: "p", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTaskWithContext(ctx, plain); err != nil {
		t.Fatalf("add plain: %v", err)
	}
	got, err := store.GetTask(plain.ID)
	if err != nil {
		t.Fatalf("get plain: %v", err)
	}
	if got.DeadLetteredAt != nil || got.DeadLetterReason != nil || got.DeadLetterAttempts != 0 {
		t.Errorf("plain task must have empty DLQ columns; got at=%v reason=%v attempts=%d",
			got.DeadLetteredAt, got.DeadLetterReason, got.DeadLetterAttempts)
	}

	at := time.Now().UTC().Truncate(time.Millisecond)
	reason := "retry budget exhausted after 3 attempt(s): boom"
	dl := &models.Task{
		ID: uuid.New(), Prompt: "p", Status: models.TaskStatusDeadLettered, CreatedAt: time.Now().UTC(),
		DeadLetteredAt: &at, DeadLetterReason: &reason, DeadLetterAttempts: 3,
	}
	if _, err := store.AddTaskWithContext(ctx, dl); err != nil {
		t.Fatalf("add dead-lettered: %v", err)
	}
	got, err = store.GetTask(dl.ID)
	if err != nil {
		t.Fatalf("get dead-lettered: %v", err)
	}
	if got.Status != models.TaskStatusDeadLettered {
		t.Errorf("status = %s, want dead_lettered", got.Status)
	}
	if got.DeadLetteredAt == nil || !got.DeadLetteredAt.Equal(at) {
		t.Errorf("dead_lettered_at = %v, want %v", got.DeadLetteredAt, at)
	}
	if got.DeadLetterReason == nil || *got.DeadLetterReason != reason {
		t.Errorf("dead_letter_reason = %v, want %q", got.DeadLetterReason, reason)
	}
	if got.DeadLetterAttempts != 3 {
		t.Errorf("dead_letter_attempts = %d, want 3", got.DeadLetterAttempts)
	}
}

// TestDeadLetterTaskRespectsLeaseOwnership pins the lease guard on the quarantine
// transition (#253), mirroring the requeue guard: a non-owning node cannot
// dead-letter a leased task, the owner can, and the result is terminal with the
// DLQ columns set.
func TestDeadLetterTaskRespectsLeaseOwnership(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	task := &models.Task{ID: uuid.New(), Prompt: "p", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC(), MaxRetries: 1}
	if _, err := store.AddTaskWithContext(ctx, task); err != nil {
		t.Fatalf("add: %v", err)
	}
	owner := uuid.New()
	claimed, err := store.ClaimNextPendingTask(ctx, owner.String())
	if err != nil || claimed == nil {
		t.Fatalf("claim: %v (task=%v)", err, claimed)
	}

	stale := uuid.New()
	if _, err := store.DeadLetterTaskWithContext(ctx, claimed.ID, stale, "x", 2); err == nil {
		t.Fatal("dead-letter by a non-owning node must be rejected")
	}

	updated, err := store.DeadLetterTaskWithContext(ctx, claimed.ID, owner, "retry budget exhausted", 2)
	if err != nil {
		t.Fatalf("owner dead-letter should succeed: %v", err)
	}
	if updated.Status != models.TaskStatusDeadLettered {
		t.Errorf("status = %s, want dead_lettered", updated.Status)
	}
	if updated.CompletedAt == nil || updated.DeadLetteredAt == nil {
		t.Errorf("dead-lettered task must stamp completed_at + dead_lettered_at")
	}
	if updated.LeaseOwner != nil || updated.LeaseExpiresAt != nil {
		t.Errorf("dead-lettered task lease must be cleared")
	}
	if updated.DeadLetterAttempts != 2 {
		t.Errorf("dead_letter_attempts = %d, want 2", updated.DeadLetterAttempts)
	}

	// It surfaces in the DLQ listing.
	dlq, err := store.GetDeadLetteredTasks(ctx, 0, 0)
	if err != nil {
		t.Fatalf("list DLQ: %v", err)
	}
	if len(dlq) != 1 || dlq[0].ID != claimed.ID {
		t.Errorf("DLQ listing should contain exactly the dead-lettered task, got %d", len(dlq))
	}
}

// TestReplayDeadLetteredTaskGuards pins the replay guard (#253): a pending task
// cannot be replayed (ErrTaskNotDeadLettered), and replaying a dead-lettered task
// resets it to a fresh pending slate that GetDeadLetteredTasks no longer returns.
func TestReplayDeadLetteredTaskGuards(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	pending := &models.Task{ID: uuid.New(), Prompt: "p", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTaskWithContext(ctx, pending); err != nil {
		t.Fatalf("add pending: %v", err)
	}
	if _, err := store.ReplayDeadLetteredTask(ctx, pending.ID); !errors.Is(err, ErrTaskNotDeadLettered) {
		t.Errorf("replaying a pending task should error ErrTaskNotDeadLettered, got %v", err)
	}

	at := time.Now().UTC()
	reason := "non-retryable failure: boom"
	dl := &models.Task{
		ID: uuid.New(), Prompt: "p", Status: models.TaskStatusDeadLettered, CreatedAt: time.Now().UTC(),
		DeadLetteredAt: &at, DeadLetterReason: &reason, DeadLetterAttempts: 1, AttemptCount: 0,
	}
	if _, err := store.AddTaskWithContext(ctx, dl); err != nil {
		t.Fatalf("add dead-lettered: %v", err)
	}
	replayed, err := store.ReplayDeadLetteredTask(ctx, dl.ID)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replayed.Status != models.TaskStatusPending || replayed.AttemptCount != 0 {
		t.Errorf("replayed = status %s attempt %d, want pending/0", replayed.Status, replayed.AttemptCount)
	}
	if replayed.DeadLetteredAt != nil || replayed.DeadLetterReason != nil || replayed.DeadLetterAttempts != 0 {
		t.Errorf("replay must clear the DLQ columns")
	}
	if dlq, _ := store.GetDeadLetteredTasks(ctx, 0, 0); len(dlq) != 0 {
		t.Errorf("DLQ must be empty after replay, got %d", len(dlq))
	}
}
