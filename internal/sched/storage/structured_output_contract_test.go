package storage

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/structuredoutput"
)

const storageOutputSchema = `{
  "type":"object",
  "properties":{"ok":{"type":"boolean"}},
  "required":["ok"],
  "additionalProperties":false
}`

func TestStructuredOutputSuccessIsAtomicAndFailClosed(t *testing.T) {
	store, database := newTestStore(t)
	owner := uuid.New()
	task := &models.Task{
		ID:           uuid.New(),
		Prompt:       "structured task",
		Status:       models.TaskStatusPending,
		CreatedAt:    time.Now().UTC(),
		OutputSchema: json.RawMessage(storageOutputSchema),
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatal(err)
	}
	leased, err := store.leaseTaskToOwner(task.ID, owner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Conn().ExecContext(t.Context(), `
		UPDATE tasks SET pending_question = 'which value?', pending_answer = 'true' WHERE id = $1`, task.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := store.UpdateTaskStatusAtomic(leased.ID, owner, &models.StatusUpdate{
		Status: models.TaskStatusSuccess,
	}); !errors.Is(err, ErrStructuredOutputContract) {
		t.Fatalf("success without output error = %v", err)
	}
	stillRunning, _ := store.GetTask(task.ID)
	if stillRunning.Status == models.TaskStatusSuccess || stillRunning.LeaseOwner == nil ||
		stillRunning.PendingQuestion == "" || stillRunning.PendingAnswer == "" {
		t.Fatalf("refused write mutated lifecycle/Q&A: status=%s lease=%v question=%q answer=%q",
			stillRunning.Status, stillRunning.LeaseOwner, stillRunning.PendingQuestion, stillRunning.PendingAnswer)
	}

	if _, err := store.UpdateTaskStatusAtomic(leased.ID, owner, &models.StatusUpdate{
		Status: models.TaskStatusSuccess, OutputJSON: json.RawMessage(`{"ok":"no"}`),
	}); !errors.Is(err, ErrStructuredOutputContract) {
		t.Fatalf("success with invalid output error = %v", err)
	}

	completed, err := store.UpdateTaskStatusAtomic(leased.ID, owner, &models.StatusUpdate{
		Status: models.TaskStatusSuccess, OutputJSON: json.RawMessage(`{ "ok": true }`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != models.TaskStatusSuccess || string(completed.OutputJSON) != `{"ok":true}` || completed.LeaseOwner != nil {
		t.Fatalf("completed = %+v", completed)
	}
	if completed.PendingQuestion != "" || completed.PendingAnswer != "" {
		t.Fatalf("successful transaction retained consumed Q&A: question=%q answer=%q", completed.PendingQuestion, completed.PendingAnswer)
	}
	reread, _ := store.GetTask(task.ID)
	var rereadOutput map[string]bool
	decodeErr := json.Unmarshal(reread.OutputJSON, &rereadOutput)
	if reread.Status != models.TaskStatusSuccess || decodeErr != nil || !rereadOutput["ok"] {
		t.Fatalf("atomic result not persisted: status=%s output=%s", reread.Status, reread.OutputJSON)
	}
}

func TestStructuredSuccessRejectsStaleOutputAndRecoveryClearsIt(t *testing.T) {
	store, database := newTestStore(t)
	owner := uuid.New()
	task := &models.Task{
		ID:           uuid.New(),
		Prompt:       "structured task",
		Status:       models.TaskStatusPending,
		CreatedAt:    time.Now().UTC(),
		OutputSchema: json.RawMessage(storageOutputSchema),
		// MaxRetries 1 so recovery re-queues (it dead-letters at
		// attempt_count >= max_retries, #1116).
		MaxRetries: 1,
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatal(err)
	}
	leased, err := store.leaseTaskToOwner(task.ID, owner)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the pre-contract two-step writer: it persisted a candidate while
	// still running, then crashed before the terminal status transition.
	if _, err := database.Conn().ExecContext(t.Context(), `
		UPDATE tasks
		SET output_json = '{"ok":true}'::jsonb,
		    lease_expires_at = now() - interval '1 second'
		WHERE id = $1`, task.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := store.UpdateTaskStatusAtomic(leased.ID, owner, &models.StatusUpdate{
		Status: models.TaskStatusSuccess,
	}); !errors.Is(err, ErrStructuredOutputContract) {
		t.Fatalf("stale output authorized success: %v", err)
	}
	beforeRecovery, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if beforeRecovery.Status == models.TaskStatusSuccess || len(beforeRecovery.OutputJSON) == 0 {
		t.Fatalf("refused transition changed/omitted fixture: status=%s output=%s", beforeRecovery.Status, beforeRecovery.OutputJSON)
	}

	if recovered, err := store.RecoverExpiredLeases(); err != nil || recovered != 1 {
		t.Fatalf("RecoverExpiredLeases = %d, %v", recovered, err)
	}
	afterRecovery, err := store.GetTask(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterRecovery.Status != models.TaskStatusPending || len(afterRecovery.OutputJSON) != 0 || afterRecovery.LeaseOwner != nil {
		t.Fatalf("recovered task retained stale state: status=%s output=%s lease=%v",
			afterRecovery.Status, afterRecovery.OutputJSON, afterRecovery.LeaseOwner)
	}
}

func TestStorageRejectsSchemaLimitsAtEveryEnqueueSeam(t *testing.T) {
	store, _ := newTestStore(t)
	tooLarge := json.RawMessage(`{"type":"object","description":"` + strings.Repeat("x", structuredoutput.MaxSchemaBytes) + `"}`)
	task := &models.Task{
		ID: uuid.New(), Prompt: "oversized", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC(), OutputSchema: tooLarge,
	}
	if _, err := store.AddTask(task); err == nil || !strings.Contains(err.Error(), "output_schema") {
		t.Fatalf("AddTask oversized schema error = %v", err)
	}
	if _, _, _, err := store.EnqueueTask(t.Context(), models.TaskCreate{
		Prompt: "oversized child", OutputSchema: tooLarge,
	}); err == nil || !strings.Contains(err.Error(), "output_schema") {
		t.Fatalf("EnqueueTask oversized schema error = %v", err)
	}
	if n, err := store.AddTaskBatch(t.Context(), []*models.Task{task}, true); err == nil || n != 0 {
		t.Fatalf("AddTaskBatch = n=%d err=%v", n, err)
	}
}

// A stale schema-INVALID output_json candidate on the row (legacy two-step
// writer / crashed prior lease) must not block non-success lease-checked
// writes — regression: every update re-validated the row's existing value, so
// the task could never record an error/interrupt and stranded `running` until
// lease expiry requeued it (a duplicate side-effect window). The success gate
// still refuses to promote such a candidate.
func TestStaleInvalidOutputDoesNotBlockErrorTransition(t *testing.T) {
	store, database := newTestStore(t)
	owner := uuid.New()
	task := &models.Task{
		ID:           uuid.New(),
		Prompt:       "structured task",
		Status:       models.TaskStatusPending,
		CreatedAt:    time.Now().UTC(),
		OutputSchema: json.RawMessage(storageOutputSchema),
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatal(err)
	}
	leased, err := store.leaseTaskToOwner(task.ID, owner)
	if err != nil {
		t.Fatal(err)
	}
	// Seed a schema-invalid candidate directly (what a crashed pre-#797 writer
	// leaves behind).
	if _, err := database.Conn().ExecContext(t.Context(), `
		UPDATE tasks SET output_json = '{"ok":"not-a-bool"}' WHERE id = $1`, task.ID); err != nil {
		t.Fatal(err)
	}

	msg := "boom"
	if _, err := store.UpdateTaskStatusAtomic(leased.ID, owner, &models.StatusUpdate{
		Status:  models.TaskStatusError,
		Message: &msg,
	}); err != nil {
		t.Fatalf("error transition blocked by stale candidate: %v", err)
	}
	got, _ := store.GetTask(task.ID)
	if got.Status != models.TaskStatusError {
		t.Errorf("status = %s, want error", got.Status)
	}
}
