package handlers

// ask/notify pause HTTP surface (#510): resume a paused task with a human
// answer, and list the tasks awaiting input. The `ask`/`notify` tools + the
// pause transition live in the runner; here are the human-facing controls.

import (
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

type resumeRequest struct {
	Answer string `json:"answer"`
}

// ResumeTask handles POST /tasks/{task_id}/resume — answer a paused task's
// question and re-queue it. Mutating operator action → cancel permission.
func (h *Handlers) ResumeTask(w http.ResponseWriter, r *http.Request) {
	p := h.principalFromRequest(r)
	if !p.hasPermission(models.PermissionCancelTask) {
		writeError(w, http.StatusForbidden, "Resuming a paused task requires operator permission")
		return
	}
	task, ok := h.pauseTaskForRequest(w, r, p)
	if !ok {
		return
	}
	if task.Status != models.TaskStatusPausedAwaitingInput {
		writeError(w, http.StatusConflict, "task is not paused awaiting input")
		return
	}
	var req resumeRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Answer) == "" {
		writeError(w, http.StatusBadRequest, "answer is required")
		return
	}
	ok2, err := h.storage.ResumeTask(r.Context(), task.ID, req.Answer)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to resume task")
		return
	}
	if !ok2 {
		writeError(w, http.StatusConflict, "task is no longer paused")
		return
	}
	log.Printf("Task resumed: %s (by %s)", logSafe(task.ID.String()), logSafe(p.stopLabel())) //nolint:gosec // G706: task.ID is a parsed uuid.UUID and logSafe strips CR/LF.
	writeJSON(w, http.StatusOK, map[string]any{"status": string(models.TaskStatusPending)})
}

// ListPausedTasks handles GET /tasks/paused — the "needs a human answer" queue.
func (h *Handlers) ListPausedTasks(w http.ResponseWriter, r *http.Request) {
	p := h.principalFromRequest(r)
	if !p.hasPermission(models.PermissionViewTasks) {
		writeError(w, http.StatusForbidden, "Insufficient permissions")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	tasks, err := h.storage.ListPausedTasks(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list paused tasks")
		return
	}
	if tasks == nil {
		tasks = []*models.Task{}
	}
	for _, t := range tasks {
		localizeTask(t)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
}

// pauseTaskForRequest loads the path task and enforces the scoped-principal
// visibility gate (mirrors CancelTask). Named distinctly so it never collides
// with a sibling helper.
func (h *Handlers) pauseTaskForRequest(w http.ResponseWriter, r *http.Request, p principal) (*models.Task, bool) {
	taskID, err := uuid.Parse(chi.URLParam(r, "task_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid task ID")
		return nil, false
	}
	task, err := h.storage.GetTask(taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, "Task not found")
		return nil, false
	}
	if scopes := p.scopes(); len(scopes) > 0 && !taskVisibleToScopes(task, scopes, p.ownerID()) {
		writeError(w, http.StatusForbidden, "Task not within allowed scopes")
		return nil, false
	}
	return task, true
}
