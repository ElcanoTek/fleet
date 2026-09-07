package runner

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/scheduledrun"
)

// seedCarryTask adds a pending task plus a persisted PRIOR run log whose final
// assistant message is priorAnswer, so priorRunHandoff has something to carry.
func seedCarryTask(t *testing.T, store *storage.Storage, recurrence string, carry bool, priorAnswer string) {
	t.Helper()
	task := &models.Task{
		ID:           uuid.New(),
		Prompt:       "recurring analysis",
		Status:       models.TaskStatusPending,
		Priority:     1,
		Recurrence:   recurrence,
		CarryContext: carry,
		CreatedAt:    time.Now().UTC(),
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	if priorAnswer != "" {
		prior := &models.LogSession{
			ID: "prior-" + task.ID.String(),
			Messages: []models.LogMessage{
				{ID: "u1", Role: "user", Content: "do the analysis"},
				{ID: "a1", Role: "assistant", Content: priorAnswer},
			},
		}
		if _, err := store.AddLog(task.ID, prior); err != nil {
			t.Fatalf("AddLog(prior): %v", err)
		}
	}
}

// captureRunner records the prior-run context observed inside the run for the
// first task it executes, then returns success.
func captureRunner() (TaskRunner, func() (string, bool)) {
	var (
		mu       sync.Mutex
		captured string
		seen     bool
	)
	runner := TaskRunnerFunc(func(ctx context.Context, task *models.Task) (*models.LogSession, error) {
		mu.Lock()
		captured = scheduledrun.PriorRunContextFromContext(ctx)
		seen = true
		mu.Unlock()
		return &models.LogSession{ID: "s-" + task.ID.String()}, nil
	})
	get := func() (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		return captured, seen
	}
	return runner, get
}

func runPoolOnce(t *testing.T, store *storage.Storage, runner TaskRunner, seen func() bool) {
	t.Helper()
	pool := NewPool(store, runner, Config{MaxConcurrentAgents: 2, PollInterval: 20 * time.Millisecond, LeaseRenewInterval: time.Hour})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()
	waitFor(t, 2*time.Second, seen)
	cancel()
	<-done
}

// TestCarryContext_InjectsPriorRunHandoff: a recurring carry_context task's run
// sees the prior run's final assistant message injected as prior-run context.
func TestCarryContext_InjectsPriorRunHandoff(t *testing.T) {
	store := newTestStore(t)
	seedCarryTask(t, store, "0 9 * * *", true, "Sales were up 12% week over week.")

	runner, get := captureRunner()
	runPoolOnce(t, store, runner, func() bool { _, s := get(); return s })

	got, _ := get()
	if got != "Sales were up 12% week over week." {
		t.Fatalf("expected prior-run handoff injected, got %q", got)
	}
}

// TestCarryContext_DisabledNoInjection: carry_context off → no prior context,
// even with a prior log present.
func TestCarryContext_DisabledNoInjection(t *testing.T) {
	store := newTestStore(t)
	seedCarryTask(t, store, "0 9 * * *", false, "Prior output that must NOT be carried.")

	runner, get := captureRunner()
	runPoolOnce(t, store, runner, func() bool { _, s := get(); return s })

	if got, _ := get(); got != "" {
		t.Fatalf("expected no prior-run context when carry_context is off, got %q", got)
	}
}

// TestCarryContext_OneShotNoInjection: carry_context on but the task is one-shot
// (no recurrence) → no prior context (carry is a recurring-only feature).
func TestCarryContext_OneShotNoInjection(t *testing.T) {
	store := newTestStore(t)
	seedCarryTask(t, store, "", true, "Prior output for a one-shot task.")

	runner, get := captureRunner()
	runPoolOnce(t, store, runner, func() bool { _, s := get(); return s })

	if got, _ := get(); got != "" {
		t.Fatalf("expected no prior-run context for a one-shot task, got %q", got)
	}
}

// TestCarryContext_ClampsLongHandoff: an over-long prior answer is truncated to
// the bounded window so context-carry stays cheap.
func TestCarryContext_ClampsLongHandoff(t *testing.T) {
	store := newTestStore(t)
	long := strings.Repeat("x", carryContextMaxChars+500)
	seedCarryTask(t, store, "0 9 * * *", true, long)

	runner, get := captureRunner()
	runPoolOnce(t, store, runner, func() bool { _, s := get(); return s })

	got, _ := get()
	if !strings.HasSuffix(got, "…[truncated]") {
		t.Fatalf("expected truncation marker on clamped handoff, got suffix %q", got[max(0, len(got)-20):])
	}
	if len(got) > carryContextMaxChars+len("…[truncated]") {
		t.Fatalf("handoff not clamped: len=%d", len(got))
	}
}

// TestCarryContext_FirstRunNoPriorLog: carry_context on, recurring, but no prior
// run yet → no context (nothing to carry), and the run still proceeds.
func TestCarryContext_FirstRunNoPriorLog(t *testing.T) {
	store := newTestStore(t)
	seedCarryTask(t, store, "0 9 * * *", true, "")

	runner, get := captureRunner()
	runPoolOnce(t, store, runner, func() bool { _, s := get(); return s })

	if got, _ := get(); got != "" {
		t.Fatalf("expected no prior-run context on first run, got %q", got)
	}
}

// TestCarryContext_FollowsRecurrenceLineage: a genuine recurrence is a NEW
// row (storage.scheduleNextRecurrence clones under a fresh id), so the run's
// own id has no log yet. The handoff must follow previous_occurrence_id to
// the occurrence that just completed — before that pointer existed every real
// recurrence started cold and the feature only fired on a retry of one row.
func TestCarryContext_FollowsRecurrenceLineage(t *testing.T) {
	store := newTestStore(t)

	// Occurrence #1: terminal, with its transcript persisted under ITS id.
	prevID := uuid.New()
	done := time.Now().UTC()
	prev := &models.Task{
		ID: prevID, Prompt: "recurring analysis", Status: models.TaskStatusSuccess, Priority: 1,
		Recurrence: "0 9 * * *", CarryContext: true, CreatedAt: time.Now().UTC().Add(-time.Hour), CompletedAt: &done,
	}
	if _, err := store.AddTask(prev); err != nil {
		t.Fatalf("AddTask(prev): %v", err)
	}
	if _, err := store.AddLog(prevID, &models.LogSession{
		ID: "prior-" + prevID.String(),
		Messages: []models.LogMessage{
			{ID: "u1", Role: "user", Content: "do the analysis"},
			{ID: "a1", Role: "assistant", Content: "Churn fell to 2.1% last week."},
		},
	}); err != nil {
		t.Fatalf("AddLog(prev): %v", err)
	}

	// Occurrence #2: the spawned successor — fresh id, no log, lineage set.
	next := &models.Task{
		ID: uuid.New(), Prompt: "recurring analysis", Status: models.TaskStatusPending, Priority: 1,
		Recurrence: "0 9 * * *", CarryContext: true, CreatedAt: time.Now().UTC(), PreviousOccurrenceID: &prevID,
	}
	if _, err := store.AddTask(next); err != nil {
		t.Fatalf("AddTask(next): %v", err)
	}

	runner, get := captureRunner()
	runPoolOnce(t, store, runner, func() bool { _, s := get(); return s })

	if got, _ := get(); got != "Churn fell to 2.1% last week." {
		t.Fatalf("expected the previous occurrence's answer carried into the successor, got %q", got)
	}
}
