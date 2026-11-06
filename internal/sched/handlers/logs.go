package handlers

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// GetLogs handles GET /logs/{task_id}. Authorization is the shared transcript
// gate (see log_authz.go): view_logs plus per-task ownership, or the explicit
// fleet-wide view_all_logs.
func (h *Handlers) GetLogs(w http.ResponseWriter, r *http.Request) {
	task, ok := h.logReadableTask(w, r, "Logs not found for this task")
	if !ok {
		return
	}

	session, err := h.storage.GetLog(task.ID)
	if err != nil || session == nil {
		writeError(w, http.StatusNotFound, "Logs not found for this task")
		return
	}

	writeJSON(w, http.StatusOK, session)
}

// logHistoryTaskForRequest parses the path task id and enforces exactly the
// GetLogs gate (the shared transcript gate in log_authz.go), so the per-attempt
// history endpoints can never leak more than the latest-log one.
func (h *Handlers) logHistoryTaskForRequest(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	task, ok := h.logReadableTask(w, r, "Logs not found for this task")
	if !ok {
		return uuid.Nil, false
	}
	return task.ID, true
}

// GetLogHistory handles GET /logs/{task_id}/history — the task's superseded
// transcripts (per-attempt run log history), newest first, metadata only.
func (h *Handlers) GetLogHistory(w http.ResponseWriter, r *http.Request) {
	taskID, ok := h.logHistoryTaskForRequest(w, r)
	if !ok {
		return
	}
	entries, err := h.storage.ListRunLogHistory(r.Context(), taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list log history")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// GetLogHistoryEntry handles GET /logs/{task_id}/history/{entry_id} — one
// superseded transcript, in the same LogSession shape GetLogs returns.
func (h *Handlers) GetLogHistoryEntry(w http.ResponseWriter, r *http.Request) {
	taskID, ok := h.logHistoryTaskForRequest(w, r)
	if !ok {
		return
	}
	entryID, err := strconv.ParseInt(chi.URLParam(r, "entry_id"), 10, 64)
	if err != nil || entryID <= 0 {
		writeError(w, http.StatusBadRequest, "Invalid history entry ID")
		return
	}
	session, err := h.storage.GetRunLogEntry(r.Context(), taskID, entryID)
	if err != nil || session == nil {
		writeError(w, http.StatusNotFound, "Log history entry not found")
		return
	}
	writeJSON(w, http.StatusOK, session)
}
