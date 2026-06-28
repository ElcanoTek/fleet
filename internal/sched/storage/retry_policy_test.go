package storage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestRetryPolicyRoundTrip pins the nullable retry_policy JSONB column (#201).
func TestRetryPolicyRoundTrip(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	// nil → legacy → reads back nil.
	legacy := &models.Task{ID: uuid.New(), Prompt: "p", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTaskWithContext(ctx, legacy); err != nil {
		t.Fatalf("add legacy: %v", err)
	}
	if got, err := store.GetTask(legacy.ID); err != nil {
		t.Fatalf("get: %v", err)
	} else if got.RetryPolicy != nil {
		t.Errorf("nil retry_policy must round-trip as nil, got %#v", got.RetryPolicy)
	}

	// Populated → reads back equal.
	custom := &models.Task{
		ID: uuid.New(), Prompt: "p", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC(),
		RetryPolicy: &models.RetryPolicy{
			Backoff:             models.BackoffExponential,
			InitialDelaySeconds: 60,
			MaxDelaySeconds:     3600,
			RetryOn:             []string{models.FailureTransient, models.FailureContextBudget},
			NoRetryOn:           []string{models.FailureCostCeiling},
		},
	}
	if _, err := store.AddTaskWithContext(ctx, custom); err != nil {
		t.Fatalf("add custom: %v", err)
	}
	got, err := store.GetTask(custom.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	rp := got.RetryPolicy
	if rp == nil || rp.Backoff != models.BackoffExponential || rp.InitialDelaySeconds != 60 ||
		rp.MaxDelaySeconds != 3600 || len(rp.RetryOn) != 2 || len(rp.NoRetryOn) != 1 {
		t.Errorf("retry_policy did not round-trip: %#v", rp)
	}

	// Edit set/clear via the flag.
	set := TaskEdit{Prompt: "p", RetryPolicy: &models.RetryPolicy{Backoff: models.BackoffFixed}, SetRetryPolicy: true}
	if upd, err := store.UpdateEditableTask(ctx, legacy.ID, set); err != nil {
		t.Fatalf("edit set: %v", err)
	} else if upd.RetryPolicy == nil || upd.RetryPolicy.Backoff != models.BackoffFixed {
		t.Errorf("edit should have set retry_policy, got %#v", upd.RetryPolicy)
	}
	clr := TaskEdit{Prompt: "p", RetryPolicy: nil, SetRetryPolicy: true}
	if upd, err := store.UpdateEditableTask(ctx, legacy.ID, clr); err != nil {
		t.Fatalf("edit clear: %v", err)
	} else if upd.RetryPolicy != nil {
		t.Errorf("edit should have cleared retry_policy, got %#v", upd.RetryPolicy)
	}
}
