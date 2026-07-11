package storage

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestSerializationKeyMutualExclusion verifies the claim-time gate (#709,
// moc#442 parity): at most one task per serialization_key may be active
// (leased/running/analyzing) at a time, the key is held through the whole
// active lifecycle, and a terminal transition releases it. The blocked task is
// skipped — it stays pending for a later claim pass, never failed.
func TestSerializationKeyMutualExclusion(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	owner := uuid.New()

	// Two pending tasks with the same key; task1 is more urgent so the claim
	// order is deterministic.
	task1 := models.NewTask(models.TaskCreate{Prompt: "serialized one", Priority: 10, SerializationKey: strPtr("client:acme")})
	task2 := models.NewTask(models.TaskCreate{Prompt: "serialized two", Priority: 50, SerializationKey: strPtr("client:acme")})
	if _, err := store.AddTask(task1); err != nil {
		t.Fatalf("add task1: %v", err)
	}
	if _, err := store.AddTask(task2); err != nil {
		t.Fatalf("add task2: %v", err)
	}

	claimed, err := store.ClaimNextPendingTask(ctx, owner.String())
	if err != nil {
		t.Fatalf("claim task1: %v", err)
	}
	if claimed == nil || claimed.ID != task1.ID {
		t.Fatalf("expected task1 to be claimed first, got %v", claimed)
	}
	if claimed.SerializationKey == nil || *claimed.SerializationKey != "client:acme" {
		t.Errorf("serialization key did not round-trip: %v", claimed.SerializationKey)
	}

	// While task1 is leased, the same-key task2 must not be claimable.
	blocked, err := store.ClaimNextPendingTask(ctx, owner.String())
	if err != nil {
		t.Fatalf("claim while key active errored: %v", err)
	}
	if blocked != nil {
		t.Fatalf("expected no claim while task1 holds the key, got %s", blocked.ID)
	}
	still, err := store.GetTask(task2.ID)
	if err != nil {
		t.Fatalf("reload task2: %v", err)
	}
	if still.Status != models.TaskStatusPending {
		t.Errorf("blocked task must stay pending (skipped, not failed), got %s", still.Status)
	}

	// The key stays held through running.
	if _, err := store.UpdateTaskStatusAtomic(task1.ID, owner, &models.StatusUpdate{Status: models.TaskStatusRunning}); err != nil {
		t.Fatalf("report running: %v", err)
	}
	blocked, err = store.ClaimNextPendingTask(ctx, owner.String())
	if err != nil {
		t.Fatalf("claim while running errored: %v", err)
	}
	if blocked != nil {
		t.Fatalf("expected no claim while task1 is running, got %s", blocked.ID)
	}

	// Terminal transition releases the key: task2 becomes claimable.
	if _, err := store.UpdateTaskStatusAtomic(task1.ID, owner, &models.StatusUpdate{Status: models.TaskStatusSuccess}); err != nil {
		t.Fatalf("report success: %v", err)
	}
	next, err := store.ClaimNextPendingTask(ctx, owner.String())
	if err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	if next == nil || next.ID != task2.ID {
		t.Fatalf("expected task2 claimable after task1 completed, got %v", next)
	}
	if next.Status != models.TaskStatusLeased {
		t.Errorf("expected leased, got %s", next.Status)
	}
}

// TestSerializationKeyBlockedTaskDoesNotStarveQueue verifies the visibility
// filter: a pending task blocked by an active same-key task must not consume
// the claim's single LIMIT-1 candidate slot — a different eligible task behind
// it is claimed instead (#709).
func TestSerializationKeyBlockedTaskDoesNotStarveQueue(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	owner := uuid.New()

	keyed1 := models.NewTask(models.TaskCreate{Prompt: "keyed head", Priority: 10, SerializationKey: strPtr("client:acme")})
	keyed2 := models.NewTask(models.TaskCreate{Prompt: "keyed blocked", Priority: 20, SerializationKey: strPtr("client:acme")})
	unkeyed := models.NewTask(models.TaskCreate{Prompt: "unkeyed behind", Priority: 90})
	for _, task := range []*models.Task{keyed1, keyed2, unkeyed} {
		if _, err := store.AddTask(task); err != nil {
			t.Fatalf("add task: %v", err)
		}
	}

	first, err := store.ClaimNextPendingTask(ctx, owner.String())
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if first == nil || first.ID != keyed1.ID {
		t.Fatalf("expected keyed1 first, got %v", first)
	}

	// keyed2 is blocked; the LESS urgent unkeyed task must be claimed instead
	// of the queue stalling on the blocked head.
	second, err := store.ClaimNextPendingTask(ctx, owner.String())
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if second == nil || second.ID != unkeyed.ID {
		t.Fatalf("expected the unkeyed task to be claimed past the blocked one, got %v", second)
	}

	third, err := store.ClaimNextPendingTask(ctx, owner.String())
	if err != nil {
		t.Fatalf("third claim: %v", err)
	}
	if third != nil {
		t.Fatalf("expected nothing claimable (keyed2 still blocked), got %s", third.ID)
	}
}

// TestSerializationKeyNilUnaffected verifies tasks without a key (including
// keys normalized away as empty/whitespace) never gate each other.
func TestSerializationKeyNilUnaffected(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()
	owner := uuid.New()

	task1 := models.NewTask(models.TaskCreate{Prompt: "nil-key one", Priority: 10})
	task2 := models.NewTask(models.TaskCreate{Prompt: "empty-key two", Priority: 20, SerializationKey: strPtr("  ")})
	if task2.SerializationKey != nil {
		t.Errorf("whitespace key must normalize to nil, got %q", *task2.SerializationKey)
	}
	if _, err := store.AddTask(task1); err != nil {
		t.Fatalf("add task1: %v", err)
	}
	if _, err := store.AddTask(task2); err != nil {
		t.Fatalf("add task2: %v", err)
	}

	first, err := store.ClaimNextPendingTask(ctx, owner.String())
	if err != nil || first == nil {
		t.Fatalf("first nil-key claim failed: %v %v", first, err)
	}
	second, err := store.ClaimNextPendingTask(ctx, owner.String())
	if err != nil || second == nil {
		t.Fatalf("second nil-key claim failed while first active: %v %v", second, err)
	}
	if second.SerializationKey != nil {
		t.Errorf("stored key should be nil, got %q", *second.SerializationKey)
	}
}

// TestSerializationKeyCarriedByRecurrence verifies a recurring task's next
// occurrence keeps the serialization key (#709): TaskToCreate is the canonical
// clone recipe scheduleNextRecurrence uses, so successive occurrences must stay
// mutually exclusive with same-key tasks.
func TestSerializationKeyCarriedByRecurrence(t *testing.T) {
	task := models.NewTask(models.TaskCreate{
		Prompt:           "recurring serialized",
		Recurrence:       "0 9 * * *",
		SerializationKey: strPtr("client:acme"),
	})
	tc := models.TaskToCreate(task)
	if tc.SerializationKey == nil || *tc.SerializationKey != "client:acme" {
		t.Fatalf("TaskToCreate dropped the serialization key: %v", tc.SerializationKey)
	}
	next := models.NewTask(tc)
	if next.SerializationKey == nil || *next.SerializationKey != "client:acme" {
		t.Fatalf("next occurrence lost the serialization key: %v", next.SerializationKey)
	}
}
