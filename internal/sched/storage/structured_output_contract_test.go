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
	store, _ := newTestStore(t)
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

	if _, err := store.UpdateTaskStatusAtomic(leased.ID, owner, &models.StatusUpdate{
		Status: models.TaskStatusSuccess,
	}); !errors.Is(err, ErrStructuredOutputContract) {
		t.Fatalf("success without output error = %v", err)
	}
	stillRunning, _ := store.GetTask(task.ID)
	if stillRunning.Status == models.TaskStatusSuccess || stillRunning.LeaseOwner == nil {
		t.Fatalf("refused write mutated lifecycle: status=%s lease=%v", stillRunning.Status, stillRunning.LeaseOwner)
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
	reread, _ := store.GetTask(task.ID)
	if reread.Status != models.TaskStatusSuccess || string(reread.OutputJSON) != `{"ok":true}` {
		t.Fatalf("atomic result not persisted: status=%s output=%s", reread.Status, reread.OutputJSON)
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
