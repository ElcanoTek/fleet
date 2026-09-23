package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// The prod shape of #1601: one punctuation error in
// the copied EXECUTION REQUIREMENTS line.
const malformedRequirementsPrompt = "Refresh the page from today's CSV.\n" +
	models.ExecutionRequirementsMarker + "\n" +
	`{"mcp_servers":["fast_io + fastio_helpers","pages"],"required_tools":["mcp_pages_get_page_data"]}` + "\n" +
	"Finish with a one-line summary."

const wantRequirementsRejection = `execution requirements: invalid server or tool identifier "fast_io + fastio_helpers"; allowed ^[a-zA-Z0-9_.-]{1,200}$`

// Every create path funnels into validateTaskCreate (create, edit, clone,
// rerun, HTTP import, batch, estimate), so a malformed declaration is refused
// there, naming the identifier and the rule; a well-formed one and a prompt
// with no declaration pass.
func TestValidateTaskCreate_ExecutionRequirements(t *testing.T) {
	h := newValidateTestHandlers()
	err := h.validateTaskCreate(&models.TaskCreate{Prompt: malformedRequirementsPrompt})
	if err == nil || err.Error() != wantRequirementsRejection {
		t.Fatalf("validateTaskCreate = %v, want %q", err, wantRequirementsRejection)
	}
	fixed := strings.Replace(malformedRequirementsPrompt, `"fast_io + fastio_helpers"`, `"fast_io","fastio_helpers"`, 1)
	if err := h.validateTaskCreate(&models.TaskCreate{Prompt: fixed}); err != nil {
		t.Fatalf("a well-formed declaration was refused: %v", err)
	}
	if err := h.validateTaskCreate(&models.TaskCreate{Prompt: "An ordinary prompt with no declaration at all"}); err != nil {
		t.Fatalf("a prompt without a declaration was refused: %v", err)
	}
}

// End to end over HTTP (DB-backed): create, edit and definition import all
// refuse the malformed line with the same message, and nothing is written.
func TestTaskWritesRejectMalformedExecutionRequirements(t *testing.T) {
	store, h, cleanup := setupFullHandler(t)
	defer cleanup()
	r := chi.NewRouter()
	r.Post("/tasks", h.CreateTask)
	r.Group(func(rr chi.Router) {
		rr.Use(h.AdminOrUserAuthMiddleware)
		rr.Put("/tasks/{task_id}", h.UpdateTask)
		rr.Post("/tasks/import", h.HandleTaskImport)
	})
	send := func(method, path string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", "test-admin-key")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	t.Run("create", func(t *testing.T) {
		w := send(http.MethodPost, "/tasks", models.TaskCreate{Prompt: malformedRequirementsPrompt})
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `invalid server or tool identifier \"fast_io + fastio_helpers\"`) {
			t.Fatalf("create: %d %s", w.Code, w.Body.String())
		}
		if tasks, err := store.GetAllTasks(); err != nil || len(tasks) != 0 {
			t.Fatalf("a refused create wrote %d task(s) (err %v)", len(tasks), err)
		}
	})

	t.Run("edit", func(t *testing.T) {
		future := time.Now().UTC().Add(time.Hour)
		task := &models.Task{ID: uuid.New(), Prompt: "a scheduled task whose prompt is fine", Status: models.TaskStatusScheduled,
			CreatedAt: time.Now().UTC(), ScheduledFor: &future}
		if _, err := store.AddTask(task); err != nil {
			t.Fatalf("seed: %v", err)
		}
		w := send(http.MethodPut, "/tasks/"+task.ID.String(), models.TaskCreate{Prompt: malformedRequirementsPrompt, ScheduledFor: &future})
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "fast_io + fastio_helpers") {
			t.Fatalf("edit: %d %s", w.Code, w.Body.String())
		}
		got, err := store.GetTask(task.ID)
		if err != nil || got.Prompt != task.Prompt {
			t.Fatalf("a refused edit changed the prompt: %v %v", got, err)
		}
	})

	t.Run("import", func(t *testing.T) {
		env := models.TaskExportEnvelope{Version: models.TaskExportVersion, Tasks: []models.TaskExportRecord{{Name: "page-refresh", Prompt: malformedRequirementsPrompt}}}
		w := send(http.MethodPost, "/tasks/import", env)
		var resp models.TaskImportResponse
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp.Created != 0 || !strings.Contains(w.Body.String(), "fast_io + fastio_helpers") {
			t.Fatalf("import: %d %s", w.Code, w.Body.String())
		}
	})
}
