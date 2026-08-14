package handlers

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// GetSubagentLog handles GET /logs/{task_id}/subagents/{child_session_id}
// (#1043 visibility): it serves a spawned child's own transcript — the sibling
// session-log file the child's governed run wrote (redacted at write time by
// the same redaction pass every session log gets).
//
// Authorization is layered:
//  1. the shared transcript gate (logReadableTask — view_logs plus per-task
//     ownership, or view_all_logs), identical to /logs/{task_id};
//  2. LINKAGE — the child id must appear in a subagent_spawned entry of this
//     task's persisted log (latest or a superseded history attempt), so a
//     caller can never use one task they may read to fetch another task's
//     child transcript;
//  3. the id must match the strict subagent-UUID shape BEFORE any filesystem
//     path is derived from it, so the request cannot smuggle path components.
//
// The transcript is a best-effort artifact by design (the issue's "may already
// be filesystem" contract): the file lives next to the process's session-log
// path and is not retained by the DB, so after a host wipe the endpoint
// returns 404 while the parent log's linkage entry (spend, role, workdir,
// result) remains the durable record.
func (h *Handlers) GetSubagentLog(w http.ResponseWriter, r *http.Request) {
	task, ok := h.logReadableTask(w, r, "Logs not found for this task")
	if !ok {
		return
	}
	childID := chi.URLParam(r, "child_session_id")
	if !agent.IsSubagentSessionID(childID) {
		writeError(w, http.StatusBadRequest, "Invalid sub-agent session id")
		return
	}

	if !h.taskLogReferencesSubagent(r, task, childID) {
		writeError(w, http.StatusNotFound, "No sub-agent with this id is recorded on this task's log")
		return
	}

	path := agent.ChildLogFilePath(childID)
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is derived from the process's own log-file config plus a strictly-validated subagent UUID — no request-controlled path components.
	if err != nil {
		writeError(w, http.StatusNotFound,
			"Sub-agent transcript file not available (it may have been cleaned up on this host)")
		return
	}
	var child models.LogSession
	if err := json.Unmarshal(data, &child); err != nil {
		writeError(w, http.StatusInternalServerError, "Sub-agent transcript file is unreadable")
		return
	}
	writeJSON(w, http.StatusOK, child)
}

// taskLogReferencesSubagent reports whether the task's persisted transcript —
// the latest log, or any superseded per-attempt history entry — carries a
// subagent_spawned linkage entry naming childID. This is the ownership proof
// tying a child transcript to a task the caller may already read.
func (h *Handlers) taskLogReferencesSubagent(r *http.Request, task *models.Task, childID string) bool {
	if session, err := h.storage.GetLog(task.ID); err == nil && sessionReferencesSubagent(session, childID) {
		return true
	}
	metas, err := h.storage.ListRunLogHistory(r.Context(), task.ID)
	if err != nil {
		return false
	}
	for _, meta := range metas {
		entry, err := h.storage.GetRunLogEntry(r.Context(), task.ID, meta.ID)
		if err != nil || entry == nil {
			continue
		}
		if sessionReferencesSubagent(entry, childID) {
			return true
		}
	}
	return false
}

// sessionReferencesSubagent scans a persisted log session for a
// subagent_spawned linkage entry whose payload names childID.
func sessionReferencesSubagent(session *models.LogSession, childID string) bool {
	if session == nil {
		return false
	}
	for _, m := range session.Messages {
		if m.MessageType == nil || *m.MessageType != "subagent_spawned" {
			continue
		}
		if strings.Contains(m.Content, `"child_session_id":"`+childID+`"`) {
			return true
		}
	}
	return false
}
