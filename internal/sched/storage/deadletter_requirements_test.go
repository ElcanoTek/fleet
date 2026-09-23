package storage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// A malformed EXECUTION REQUIREMENTS declaration parks the chain on its FIRST
// dead-letter (#1601): the prompt is copied verbatim into every successor, so
// the next occurrence would dead-letter the same way — the two-strike breaker
// (ADR-0070) is for causes that might not recur. A well-formed declaration
// (and no declaration) keeps the ADR-0070 behaviour: the first dead-letter
// spawns.
func TestDeadLetterBreakerParksMalformedRequirementsOnFirstDeadLetter(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetTimezone("UTC")
	ctx := context.Background()

	deadLetterFirst := func(t *testing.T, prompt string) *models.Task {
		t.Helper()
		task := &models.Task{
			ID: uuid.New(), Prompt: prompt, Status: models.TaskStatusPending, Priority: 10,
			Recurrence: "0 18 * * 1-5", Timezone: "UTC", CreatedAt: time.Now().UTC(),
		}
		if _, err := store.AddTask(task); err != nil {
			t.Fatalf("AddTask: %v", err)
		}
		owner := uuid.New()
		if _, err := store.leaseTaskToOwner(task.ID, owner); err != nil {
			t.Fatalf("leaseTaskToOwner: %v", err)
		}
		if _, err := store.DeadLetterTaskWithContext(ctx, task.ID, owner,
			"non-retryable failure (terminal): execution requirements: invalid server or tool identifier", 1); err != nil {
			t.Fatalf("DeadLetterTaskWithContext: %v", err)
		}
		return task
	}

	// Successors are counted by predecessor pointer: the subtests share one
	// store, so "every other row" would count the earlier cases' rows too.
	successors := func(t *testing.T, id uuid.UUID) int {
		t.Helper()
		all, err := store.GetAllTasks()
		if err != nil {
			t.Fatalf("GetAllTasks: %v", err)
		}
		n := 0
		for _, tk := range all {
			if tk.PreviousOccurrenceID != nil && *tk.PreviousOccurrenceID == id {
				n++
			}
		}
		return n
	}

	malformed := deadLetterFirst(t, "Refresh the page.\n"+models.ExecutionRequirementsMarker+"\n"+
		`{"mcp_servers":["fast_io + fastio_helpers","pages"]}`)
	if n := successors(t, malformed.ID); n != 0 {
		t.Fatalf("successors of a malformed-declaration dead-letter = %d, want 0 (park on the first)", n)
	}
	if !recurrenceSpawned(t, store, malformed.ID) {
		t.Fatal("the parked occurrence's spawn credit must be settled, or the sweep re-evaluates it forever")
	}
	got, err := store.GetTask(malformed.ID)
	if err != nil || got.RecurrenceParkedAt == nil {
		t.Fatalf("the chain must be parked (recurrence_parked_at set), got %+v %v", got, err)
	}
	if repaired, err := store.ReconcileRecurrences(ctx); err != nil || repaired != 0 {
		t.Fatalf("the sweep re-drove a parked chain: repaired=%d err=%v", repaired, err)
	}

	for _, prompt := range []string{
		"Refresh the page.\n" + models.ExecutionRequirementsMarker + "\n" + `{"mcp_servers":["fast_io","fastio_helpers","pages"]}`,
		"Refresh the page with no declaration.",
	} {
		task := deadLetterFirst(t, prompt)
		if n := successors(t, task.ID); n != 1 {
			t.Fatalf("a well-formed task's first dead-letter spawned %d successors, want 1 (ADR-0070 unchanged)", n)
		}
	}
}
