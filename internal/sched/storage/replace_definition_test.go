package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestReplaceTaskDefinition_DerivesDispatchState guards the #1104 wedge: the
// replace overlay recomputes status/scheduled_for with the same rule as
// creation. Before the fix the pre-replace status was preserved verbatim, so a
// record with no schedule replacing a scheduled one-shot left the row
// `scheduled` with a NULL scheduled_for — a state GetScheduledTasks (which
// requires scheduled_for IS NOT NULL) can never promote.
func TestReplaceTaskDefinition_DerivesDispatchState(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	future := time.Now().Add(48 * time.Hour).UTC()
	seed := models.NewTask(models.TaskCreate{Name: "oneshot", Prompt: "seed one-shot", ScheduledFor: &future})
	if seed.Status != models.TaskStatusScheduled {
		t.Fatalf("seed status = %s, want scheduled", seed.Status)
	}
	if _, err := store.AddTask(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Replace with a SCHEDULE-LESS record → immediately dispatchable (pending,
	// no scheduled_for), exactly what creating the record fresh would produce —
	// never scheduled+NULL.
	got, err := store.ReplaceTaskDefinition(ctx, seed.ID, models.TaskCreate{Name: "oneshot", Prompt: "replaced, no schedule"})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if got.Status != models.TaskStatusPending || got.ScheduledFor != nil {
		t.Fatalf("schedule-less replace: status=%s scheduled_for=%v, want pending/nil", got.Status, got.ScheduledFor)
	}
	// The persisted row must agree (the wedge was a stored state, not an
	// in-memory one).
	reloaded, err := store.GetTask(seed.ID)
	if err != nil || reloaded == nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Status != models.TaskStatusPending || reloaded.ScheduledFor != nil {
		t.Fatalf("persisted: status=%s scheduled_for=%v, want pending/nil", reloaded.Status, reloaded.ScheduledFor)
	}
	if reloaded.Prompt != "replaced, no schedule" {
		t.Errorf("persisted prompt = %q", reloaded.Prompt)
	}

	// Replace a (still-scheduled) task with a GATED record → parked
	// scheduled-for-now so the gate is evaluated before dispatch (the RunIf
	// contract) — the immediate-dispatch inversion of the same derivation bug.
	// A fresh seed: the first one is pending now, where a new gate is refused.
	gatedSeed := models.NewTask(models.TaskCreate{Name: "gated", Prompt: "seed gated", ScheduledFor: &future})
	if _, err := store.AddTask(gatedSeed); err != nil {
		t.Fatalf("seed gated: %v", err)
	}
	got, err = store.ReplaceTaskDefinition(ctx, gatedSeed.ID, models.TaskCreate{
		Name: "gated", Prompt: "replaced with gate",
		RunIf: &models.RunIf{Command: "true", TimeoutSeconds: 30},
	})
	if err != nil {
		t.Fatalf("gated replace: %v", err)
	}
	if got.Status != models.TaskStatusScheduled || got.ScheduledFor == nil {
		t.Fatalf("gated replace: status=%s scheduled_for=%v, want scheduled with a time", got.Status, got.ScheduledFor)
	}
}

// TestReplaceTaskDefinition_PersistsFullDefinition pins the definition columns
// UpdateTaskTx previously omitted (name, trigger_type, allow_event_triggers,
// serialization_key): the CLI overlay used to drop several of these entirely
// (#1104), and even a complete overlay is only as good as the write underneath.
func TestReplaceTaskDefinition_PersistsFullDefinition(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	seed := models.NewTask(models.TaskCreate{Name: "full", Prompt: "seed"})
	if _, err := store.AddTask(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	key := "one-at-a-time"
	title := "Replaced Title"
	if _, err := store.ReplaceTaskDefinition(ctx, seed.ID, models.TaskCreate{
		Name: "full", Title: title, Prompt: "replaced",
		AllowEventTriggers: true,
		CarryContext:       true,
		SerializationKey:   &key,
	}); err != nil {
		t.Fatalf("replace: %v", err)
	}

	got, err := store.GetTask(seed.ID)
	if err != nil || got == nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Title != title || !got.AllowEventTriggers || !got.CarryContext {
		t.Errorf("definition fields dropped: title=%q allow_event_triggers=%v carry_context=%v", got.Title, got.AllowEventTriggers, got.CarryContext)
	}
	if got.SerializationKey == nil || *got.SerializationKey != key {
		t.Errorf("serialization_key not persisted: %v", got.SerializationKey)
	}
}

// TestReplaceTaskDefinition_RefusesNonEditable is the #1104 double-execution
// guard: the replace runs under the same row lock + editability re-check as
// UpdateEditableTask, so a task a runner has claimed (leased/running) or that
// already finished is refused with ErrTaskNotEditable — never silently rewound
// to `scheduled` with its lease nulled (which re-dispatched a live run) or
// flipped from terminal back to runnable (which erased its result).
func TestReplaceTaskDefinition_RefusesNonEditable(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	// A claimed (leased) task: replace must refuse and leave the lease intact.
	seed := models.NewTask(models.TaskCreate{Name: "claimed", Prompt: "seed claimed"})
	if _, err := store.AddTask(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	claimed, err := store.ClaimNextPendingTask(ctx, "worker-1")
	if err != nil || claimed == nil || claimed.ID != seed.ID {
		t.Fatalf("claim: %v %v", claimed, err)
	}

	_, err = store.ReplaceTaskDefinition(ctx, seed.ID, models.TaskCreate{Name: "claimed", Prompt: "must not land"})
	if !errors.Is(err, ErrTaskNotEditable) {
		t.Fatalf("replace of leased task: err = %v, want ErrTaskNotEditable", err)
	}
	got, err := store.GetTask(seed.ID)
	if err != nil || got == nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Status != models.TaskStatusLeased || got.LeaseOwner == nil || *got.LeaseOwner != "worker-1" {
		t.Errorf("refused replace disturbed the lease: status=%s owner=%v", got.Status, got.LeaseOwner)
	}
	if got.Prompt != "seed claimed" {
		t.Errorf("refused replace rewrote the definition: %q", got.Prompt)
	}

	// A terminal task: replace must refuse and keep the result.
	result := "the answer"
	done := time.Now().UTC()
	term := models.NewTask(models.TaskCreate{Name: "done", Prompt: "seed done"})
	term.Status = models.TaskStatusSuccess
	term.Result = &result
	term.CompletedAt = &done
	if _, err := store.AddTask(term); err != nil {
		t.Fatalf("seed terminal: %v", err)
	}
	_, err = store.ReplaceTaskDefinition(ctx, term.ID, models.TaskCreate{Name: "done", Prompt: "must not land"})
	if !errors.Is(err, ErrTaskNotEditable) {
		t.Fatalf("replace of terminal task: err = %v, want ErrTaskNotEditable", err)
	}
	got, err = store.GetTask(term.ID)
	if err != nil || got == nil {
		t.Fatalf("reload terminal: %v", err)
	}
	if got.Status != models.TaskStatusSuccess || got.Result == nil || *got.Result != result || got.CompletedAt == nil {
		t.Errorf("refused replace disturbed terminal state: status=%s result=%v completed_at=%v", got.Status, got.Result, got.CompletedAt)
	}
}
