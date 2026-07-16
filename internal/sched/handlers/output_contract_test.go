package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func TestGetTaskOutputTerminalContract(t *testing.T) {
	router, store, cleanup := setupTestHandlerWithStore(t)
	defer cleanup()
	schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)

	valid := &models.Task{
		ID: uuid.New(), Prompt: "valid", Status: models.TaskStatusSuccess, CreatedAt: time.Now().UTC(),
		OutputSchema: schema, OutputJSON: json.RawMessage(`{"ok":true}`),
	}
	if _, err := store.AddTask(valid); err != nil {
		t.Fatal(err)
	}

	// Bypass the new storage invariant only to emulate a row written by an old
	// fleet release. The endpoint must expose this as conflict, never 404.
	legacy := &models.Task{
		ID: uuid.New(), Prompt: "legacy", Status: models.TaskStatusSuccess, CreatedAt: time.Now().UTC(), OutputSchema: schema,
	}
	if err := store.DB().AddTask(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	corrupt := &models.Task{
		ID: uuid.New(), Prompt: "corrupt", Status: models.TaskStatusSuccess, CreatedAt: time.Now().UTC(),
		OutputSchema: schema, OutputJSON: json.RawMessage(`{"ok":"not-a-boolean"}`),
	}
	if err := store.DB().AddTask(context.Background(), corrupt); err != nil {
		t.Fatal(err)
	}

	failed := &models.Task{
		ID: uuid.New(), Prompt: "failed", Status: models.TaskStatusDeadLettered, CreatedAt: time.Now().UTC(),
		OutputSchema: schema, OutputJSON: json.RawMessage(`{"ok":true}`),
	}
	if _, err := store.AddTask(failed); err != nil {
		t.Fatal(err)
	}
	freeform := &models.Task{ID: uuid.New(), Prompt: "free", Status: models.TaskStatusSuccess, CreatedAt: time.Now().UTC()}
	if _, err := store.AddTask(freeform); err != nil {
		t.Fatal(err)
	}

	get := func(id uuid.UUID) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/tasks/"+id.String()+"/output", nil)
		req.Header.Set("X-API-Key", "test-admin-key")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}

	if w := get(valid.ID); w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != `{"ok":true}` {
		t.Fatalf("valid output: status=%d body=%s", w.Code, w.Body.String())
	}
	if w := get(legacy.ID); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "contract violated") {
		t.Fatalf("legacy impossible success: status=%d body=%s", w.Code, w.Body.String())
	}
	if w := get(corrupt.ID); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "invalid") {
		t.Fatalf("legacy corrupt success: status=%d body=%s", w.Code, w.Body.String())
	}
	if w := get(failed.ID); w.Code != http.StatusConflict {
		t.Fatalf("failed structured task with stale output: status=%d body=%s", w.Code, w.Body.String())
	}
	if w := get(freeform.ID); w.Code != http.StatusNotFound {
		t.Fatalf("free-form task: status=%d body=%s", w.Code, w.Body.String())
	}
}
