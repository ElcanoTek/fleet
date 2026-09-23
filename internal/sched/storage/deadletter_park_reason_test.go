package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// malformedParkPrompt is the prod shape of #1601: one punctuation error in the
// copied EXECUTION REQUIREMENTS line.
const malformedParkPrompt = "Refresh the page.\n" + models.ExecutionRequirementsMarker + "\n" +
	`{"mcp_servers":["fast_io + fastio_helpers","pages"]}`

// A chain parked by a malformed declaration records WHY (migration 073): the
// dead-letter write hands the reason back to the runner for the notification,
// and the row keeps it for the Operations Center. A plain replay is refused —
// it would dead-letter again — and changes nothing; a replay with a corrected
// prompt resumes the same row, so the schedule continues with its task memory
// and the successor carries the corrected prompt.
func TestMalformedParkRecordsWhyAndResumesWithACorrectedPrompt(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetTimezone("UTC")
	ctx := context.Background()

	task := &models.Task{
		ID: uuid.New(), Prompt: malformedParkPrompt, Status: models.TaskStatusPending, Priority: 10,
		Recurrence: "0 18 * * 1-5", Timezone: "UTC", CreatedAt: time.Now().UTC(),
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	if _, err := store.DB().Conn().ExecContext(ctx,
		`INSERT INTO task_memories (task_id, key, value, created_at, updated_at) VALUES ($1, 'last_refresh', '2026-09-22', 1, 1)`, task.ID); err != nil {
		t.Fatalf("seed task memory: %v", err)
	}
	owner := uuid.New()
	if _, err := store.leaseTaskToOwner(task.ID, owner); err != nil {
		t.Fatalf("leaseTaskToOwner: %v", err)
	}
	dl, err := store.DeadLetterTaskWithContext(ctx, task.ID, owner, "non-retryable failure (terminal): execution requirements: …", 1)
	if err != nil {
		t.Fatalf("DeadLetterTaskWithContext: %v", err)
	}
	const wantDetail = `invalid server or tool identifier "fast_io + fastio_helpers" in mcp_servers[0]`
	if dl.RecurrenceParkedReason == nil || !strings.Contains(*dl.RecurrenceParkedReason, wantDetail) ||
		!strings.Contains(*dl.RecurrenceParkedReason, "Replay this occurrence with a corrected prompt") {
		t.Fatalf("the dead-letter write must return the park reason, got %v", dl.RecurrenceParkedReason)
	}
	stored, err := store.GetTask(task.ID)
	if err != nil || stored.RecurrenceParkedAt == nil || stored.RecurrenceParkedReason == nil || *stored.RecurrenceParkedReason != *dl.RecurrenceParkedReason {
		t.Fatalf("the park reason must be persisted with the stamp, got %+v %v", stored, err)
	}

	assertMalformedReplaysRefused(t, store, task.ID, wantDetail)
	assertCorrectedReplayResumes(t, store, task.ID)
}

// assertMalformedReplaysRefused: a plain replay reruns the malformed prompt,
// and a malformed replacement is no better — both are refused, and nothing
// changes.
func assertMalformedReplaysRefused(t *testing.T, store *Storage, id uuid.UUID, wantDetail string) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.ReplayDeadLetteredTask(ctx, id); !errors.Is(err, ErrReplayMalformedRequirements) || !strings.Contains(err.Error(), wantDetail) {
		t.Fatalf("replay of a malformed prompt = %v, want ErrReplayMalformedRequirements naming the identifier", err)
	}
	if _, err := store.ReplayDeadLetteredTaskWithPrompt(ctx, id, "Still wrong.\n"+models.ExecutionRequirementsMarker+"\n{bad}"); !errors.Is(err, ErrReplayMalformedRequirements) {
		t.Fatalf("replay with a malformed replacement = %v, want ErrReplayMalformedRequirements", err)
	}
	after, err := store.GetTask(id)
	if err != nil || after.Status != models.TaskStatusDeadLettered || after.RecurrenceParkedAt == nil || after.Prompt != malformedParkPrompt {
		t.Fatalf("a refused replay must change nothing, got %+v %v", after, err)
	}
}

// assertCorrectedReplayResumes: the corrected replay resumes the same row, and
// its successful run spawns exactly one successor carrying the corrected
// prompt and the chain's task memory.
func assertCorrectedReplayResumes(t *testing.T, store *Storage, id uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	task := &models.Task{ID: id}
	corrected := "Refresh the page.\n" + models.ExecutionRequirementsMarker + "\n" + `{"mcp_servers":["fast_io","fastio_helpers","pages"]}`
	replayed, err := store.ReplayDeadLetteredTaskWithPrompt(ctx, task.ID, "  "+corrected+"\n")
	if err != nil {
		t.Fatalf("replay with a corrected prompt: %v", err)
	}
	if replayed.ID != task.ID || replayed.Status != models.TaskStatusPending || replayed.Prompt != corrected ||
		replayed.RecurrenceParkedAt != nil || replayed.RecurrenceParkedReason != nil {
		t.Fatalf("replayed = %+v, want the same row pending with the corrected prompt and no park", replayed)
	}
	reread, err := store.GetTask(task.ID)
	if err != nil || reread.Prompt != corrected || reread.RecurrenceParkedAt != nil || reread.RecurrenceParkedReason != nil {
		t.Fatalf("persisted replay = %+v %v, want the corrected prompt and the park cleared", reread, err)
	}
	if recurrenceSpawned(t, store, task.ID) {
		t.Fatal("the parked chain's spawn credit must be re-armed")
	}

	owner2 := uuid.New()
	if _, err := store.leaseTaskToOwner(task.ID, owner2); err != nil {
		t.Fatalf("re-lease: %v", err)
	}
	if _, err := store.UpdateTaskStatusAtomic(task.ID, owner2, &models.StatusUpdate{Status: models.TaskStatusSuccess, Message: strPtr("done")}); err != nil {
		t.Fatalf("UpdateTaskStatusAtomic: %v", err)
	}
	succ := successorsOf(t, store, map[uuid.UUID]bool{task.ID: true})
	if len(succ) != 1 || succ[0].Prompt != corrected {
		t.Fatalf("successors = %+v, want one carrying the corrected prompt", succ)
	}
	var value string
	if err := store.DB().Conn().QueryRowContext(ctx,
		`SELECT value FROM task_memories WHERE task_id = $1 AND key = 'last_refresh'`, succ[0].ID).Scan(&value); err != nil || value != "2026-09-22" {
		t.Fatalf("the successor must carry the chain's task memory, got %q %v", value, err)
	}
}

// The two-strike park records its reason too.
func TestTwoStrikeParkRecordsWhy(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetTimezone("UTC")
	first := &models.Task{
		ID: uuid.New(), Prompt: "daily digest", Status: models.TaskStatusPending, Priority: 10,
		Recurrence: "@daily", Timezone: "UTC", CreatedAt: time.Now().UTC(),
	}
	if _, err := store.AddTask(first); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	deadLetterRecurringOccurrence(t, store, first.ID)
	if got, err := store.GetTask(first.ID); err != nil || got.RecurrenceParkedReason != nil {
		t.Fatalf("a first dead-letter that spawned must record no park reason, got %+v %v", got, err)
	}
	succ := successorsOf(t, store, map[uuid.UUID]bool{first.ID: true})
	if len(succ) != 1 {
		t.Fatalf("successors of first = %d, want 1", len(succ))
	}
	deadLetterRecurringOccurrence(t, store, succ[0].ID)
	got, err := store.GetTask(succ[0].ID)
	if err != nil || got.RecurrenceParkedReason == nil || !strings.Contains(*got.RecurrenceParkedReason, "2 consecutive occurrences were dead-lettered") {
		t.Fatalf("two-strike park reason = %+v %v", got, err)
	}
	if _, err := store.ReplayDeadLetteredTask(context.Background(), succ[0].ID); err != nil {
		t.Fatalf("a well-formed parked chain replays as before: %v", err)
	}
	if got, err := store.GetTask(succ[0].ID); err != nil || got.RecurrenceParkedReason != nil {
		t.Fatalf("replay must clear the park reason, got %+v %v", got, err)
	}
}
