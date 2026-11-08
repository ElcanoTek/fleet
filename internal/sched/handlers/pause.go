package handlers

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// taskFromPath loads the {task_id} path task, writing the 400/404 itself. It is
// lookup only: a handler that needs an authorization decision makes it on the
// returned task (permission, ownership, or the creator-scoped gates).
func (h *Handlers) taskFromPath(w http.ResponseWriter, r *http.Request) (*models.Task, bool) {
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
	return task, true
}

// taskResumableByPrincipal is the per-task gate for wake: the same
// permission-OR-owns shape CancelTask uses (an operator with cancel_task, or
// the creating user), narrowed by row visibility for the permission holder.
func taskResumableByPrincipal(p principal, task *models.Task) bool {
	if p.hasPermission(models.PermissionCancelTask) && taskVisibleToPrincipal(p, task) {
		return true
	}
	return p.ownsTask(task)
}

type wakeRequest struct {
	Event string `json:"event"`
	Note  string `json:"note,omitempty"`
}

// WakeTask handles POST /tasks/{task_id}/wake — fire a named event at a task
// parked by wake_on_event (self-wake, docs/SELF-WAKE.md), re-queueing it
// early. The event key must match the one the task is waiting for, so a
// caller can never wake an arbitrary sleeping task (and a timer-only sleep
// has no key to match). Mutating operator action → cancel permission or
// ownership (taskResumableByPrincipal).
func (h *Handlers) WakeTask(w http.ResponseWriter, r *http.Request) {
	p := h.principalFromRequest(r)
	task, ok := h.taskFromPath(w, r)
	if !ok {
		return
	}
	if !taskResumableByPrincipal(p, task) {
		writeError(w, http.StatusForbidden, "Waking a task requires operator permission or ownership of the task")
		return
	}
	if task.Status != models.TaskStatusPausedAwaitingWake {
		writeError(w, http.StatusConflict, "task is not awaiting a wake")
		return
	}
	var req wakeRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}
	event := strings.TrimSpace(req.Event)
	if event == "" {
		writeError(w, http.StatusBadRequest, "event is required")
		return
	}
	ok2, err := h.storage.WakeTaskByEvent(r.Context(), task.ID, event, strings.TrimSpace(req.Note))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to wake task")
		return
	}
	if !ok2 {
		writeError(w, http.StatusConflict, "task is not waiting for that event")
		return
	}
	logging.From(r.Context()).Info("Task woken by event", "task_id", safe.LogSafe(task.ID.String()), "event", safe.LogSafe(event), "by", safe.LogSafe(p.stopLabel()))
	h.kickTaskQueue()
	writeJSON(w, r, http.StatusOK, map[string]any{"status": string(models.TaskStatusPending)})
}
