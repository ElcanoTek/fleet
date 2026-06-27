package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestCredentialAllowlistRoundTrip pins the load-bearing nil-vs-empty distinction
// (#184) through the nullable JSONB column: nil ("inherit global") and empty
// ("deny all") must NOT collapse into each other across a write/read cycle.
func TestCredentialAllowlistRoundTrip(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	mk := func(al models.CredentialAllowlist) *models.Task {
		return &models.Task{
			ID:                  uuid.New(),
			Prompt:              "p",
			Status:              models.TaskStatusPending,
			CreatedAt:           time.Now().UTC(),
			CredentialAllowlist: al,
		}
	}

	// nil → SQL NULL → reads back nil (inherit global).
	nilTask := mk(nil)
	if _, err := store.AddTaskWithContext(ctx, nilTask); err != nil {
		t.Fatalf("add nil task: %v", err)
	}
	if got, err := store.GetTask(nilTask.ID); err != nil {
		t.Fatalf("get: %v", err)
	} else if got.CredentialAllowlist != nil {
		t.Errorf("nil allowlist must round-trip as nil, got %#v", got.CredentialAllowlist)
	}

	// empty non-nil → "[]" → reads back non-nil empty (deny all).
	emptyTask := mk(models.CredentialAllowlist{})
	if _, err := store.AddTaskWithContext(ctx, emptyTask); err != nil {
		t.Fatalf("add empty task: %v", err)
	}
	if got, err := store.GetTask(emptyTask.ID); err != nil {
		t.Fatalf("get: %v", err)
	} else if got.CredentialAllowlist == nil || len(got.CredentialAllowlist) != 0 {
		t.Errorf("empty (deny-all) allowlist must round-trip as non-nil empty, got %#v", got.CredentialAllowlist)
	}

	// populated → reads back equal.
	popTask := mk(models.CredentialAllowlist{{Server: "github", Account: "client_a"}, {Server: "sendgrid"}})
	if _, err := store.AddTaskWithContext(ctx, popTask); err != nil {
		t.Fatalf("add populated task: %v", err)
	}
	got, err := store.GetTask(popTask.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.CredentialAllowlist) != 2 ||
		got.CredentialAllowlist[0] != (models.CredentialAllowlistEntry{Server: "github", Account: "client_a"}) ||
		got.CredentialAllowlist[1] != (models.CredentialAllowlistEntry{Server: "sendgrid"}) {
		t.Errorf("populated allowlist did not round-trip: %#v", got.CredentialAllowlist)
	}
}

// TestUpdateTaskCredentialAllowlist exercises the set → read-back → clear cycle
// and the editable-state guard.
func TestUpdateTaskCredentialAllowlist(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	task := &models.Task{ID: uuid.New(), Prompt: "p", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTaskWithContext(ctx, task); err != nil {
		t.Fatalf("add: %v", err)
	}

	// Set a scoped allowlist.
	al := models.CredentialAllowlist{{Server: "github", Account: "client_a"}}
	if _, err := store.UpdateTaskCredentialAllowlist(ctx, task.ID, al); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got, err := store.GetTask(task.ID); err != nil {
		t.Fatalf("get: %v", err)
	} else if len(got.CredentialAllowlist) != 1 || got.CredentialAllowlist[0].Server != "github" {
		t.Errorf("set did not persist: %#v", got.CredentialAllowlist)
	}

	// Clear (nil) → reverts to global inherit.
	if _, err := store.UpdateTaskCredentialAllowlist(ctx, task.ID, nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got, err := store.GetTask(task.ID); err != nil {
		t.Fatalf("get: %v", err)
	} else if got.CredentialAllowlist != nil {
		t.Errorf("clear must revert to nil, got %#v", got.CredentialAllowlist)
	}

	// A task that has left the editable state is rejected.
	leased := &models.Task{ID: uuid.New(), Prompt: "p", Status: models.TaskStatusLeased, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTaskWithContext(ctx, leased); err != nil {
		t.Fatalf("add leased: %v", err)
	}
	if _, err := store.UpdateTaskCredentialAllowlist(ctx, leased.ID, al); !errors.Is(err, ErrTaskNotEditable) {
		t.Errorf("expected ErrTaskNotEditable on a leased task, got %v", err)
	}
}
