package handlers

// ask/notify pause HTTP surface (#510): resume a paused task with a human
// answer, and list the tasks awaiting input. The `ask`/`notify` tools + the
// pause transition live in the runner; here are the human-facing controls.

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

type resumeRequest struct {
	Answer string `json:"answer"`
}

// maxResumeAnswerChars bounds the human answer injected into a resumed run.
// The answer lands in the model context verbatim, so it is capped like the
// other free-text task fields (the description cap) rather than left unbounded.
const maxResumeAnswerChars = maxTaskDescriptionChars

// taskResumableByPrincipal is the per-task gate for resume/wake: the same
// permission-OR-owns shape CancelTask uses (an operator with cancel_task, or
// the creating user), narrowed by row visibility for the permission holder.
// Both answers are model input for the run — a resume answer verbatim, a wake
// note as the event payload — so a cancel_task holder must not be able to
// steer a task it cannot even see; the creating user may always answer their
// own task's question, as they may stop it. taskFromPath is lookup only (see
// its doc), so this decision is the handler's, like every sibling mutation.
func taskResumableByPrincipal(p principal, task *models.Task) bool {
	if p.hasPermission(models.PermissionCancelTask) && taskVisibleToPrincipal(p, task) {
		return true
	}
	return p.ownsTask(task)
}

// ResumeTask handles POST /tasks/{task_id}/resume — answer a paused task's
// question and re-queue it. Mutating operator action → cancel permission or
// ownership (taskResumableByPrincipal).
func (h *Handlers) ResumeTask(w http.ResponseWriter, r *http.Request) {
	p := h.principalFromRequest(r)
	task, ok := h.taskFromPath(w, r)
	if !ok {
		return
	}
	if !taskResumableByPrincipal(p, task) {
		writeError(w, http.StatusForbidden, "Resuming a paused task requires operator permission or ownership of the task")
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
	if utf8.RuneCountInString(req.Answer) > maxResumeAnswerChars {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("answer cannot exceed %d characters", maxResumeAnswerChars))
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
	h.kickTaskQueue()
	writeJSON(w, http.StatusOK, map[string]any{"status": string(models.TaskStatusPending)})
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
// ownership, mirroring ResumeTask (taskResumableByPrincipal).
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
	log.Printf("Task woken by event: %s (event %s, by %s)", logSafe(task.ID.String()), logSafe(event), logSafe(p.stopLabel())) //nolint:gosec // G706: task.ID is a parsed uuid.UUID and logSafe strips CR/LF.
	h.kickTaskQueue()
	writeJSON(w, http.StatusOK, map[string]any{"status": string(models.TaskStatusPending)})
}

// ListPausedTasks handles GET /tasks/paused — the "needs a human answer" queue.
func (h *Handlers) ListPausedTasks(w http.ResponseWriter, r *http.Request) {
	p := h.principalFromRequest(r)
	if !p.hasPermission(models.PermissionViewTasks) {
		writeError(w, http.StatusForbidden, "Insufficient permissions")
		return
	}
	limit, ok := parseLimit(w, r, pausedDefaultLimit, pausedLimitMax)
	if !ok {
		return
	}
	tasks, err := h.storage.ListPausedTasks(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list paused tasks")
		return
	}
	// Own-rows visibility (#1082): ListPausedTasks selects by status alone, with
	// no principal predicate in SQL, and the projection carries each task's
	// prompt — so it has to be scoped here like GET /tasks, /tasks/export and
	// /tasks/upcoming. Without this a `client`-role principal read every
	// principal's paused prompts, and learned their task UUIDs besides.
	tasks = visibleTasks(p, tasks)
	if tasks == nil {
		tasks = []*models.Task{}
	}
	for _, t := range tasks {
		localizeTask(t)
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
}
