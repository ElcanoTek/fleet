// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

// Live SSE log streaming for in-progress scheduled tasks (#200).
//
// GET /tasks/{task_id}/stream lets the orchestrator UI tail a running task's run
// log the way chat tails a turn. It reuses the worker pool's per-task event buffer
// (internal/runner) — the SAME Observer event stream the captain's-log writer
// consumes — rather than inventing a new bus. When the task is no longer in flight
// it falls back to a one-shot SSE replay of the persisted log, so the same client
// works whether the task is mid-run or already finished.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TaskStream is the narrow live-stream surface the handler attaches a client to.
// internal/runner's per-task buffer satisfies it; the handler depends only on this
// so it never imports the worker pool.
type TaskStream interface {
	Attach(ctx context.Context, lastEventID uint64, w http.ResponseWriter) error
}

// TaskStreamLookup resolves a task's live stream buffer, returning false when the
// task is not currently in flight (so the handler replays the persisted log).
type TaskStreamLookup func(taskID uuid.UUID) (TaskStream, bool)

// SetTaskStreamProvider wires the live per-task SSE stream lookup (#200). cmd/fleet
// adapts the worker pool's runner.TaskStreamRegistry to this so GET
// /tasks/{id}/stream can attach a client to a running task. nil leaves every task
// served by the persisted-log replay path only.
func (h *Handlers) SetTaskStreamProvider(lookup TaskStreamLookup) {
	h.taskStreamLookup = lookup
}

// StreamTaskLogs handles GET /tasks/{task_id}/stream — an SSE endpoint that tails a
// running task's run log live, or replays the persisted log one-shot when the task
// is no longer in flight.
//
// Auth + ownership are IDENTICAL to GetLogs (GET /logs/{task_id}): PermissionViewLogs
// plus, for a scoped principal, the same per-task scope check. The new endpoint is
// additive and changes neither GetLogs nor the chat SSE.
func (h *Handlers) StreamTaskLogs(w http.ResponseWriter, r *http.Request) {
	p := h.principalFromRequest(r)
	if !p.hasPermission(models.PermissionViewLogs) {
		writeError(w, http.StatusForbidden, "Insufficient permissions")
		return
	}

	taskIDStr := chi.URLParam(r, "task_id")
	taskID, err := uuid.Parse(taskIDStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid task ID")
		return
	}

	// Ownership: a scoped principal may only stream a task within its scopes —
	// exactly the GetLogs gate, so the live view leaks no more than the stored one.
	if scopes := p.scopes(); len(scopes) > 0 {
		task, terr := h.storage.GetTask(taskID)
		if terr != nil || task == nil {
			writeError(w, http.StatusNotFound, "No log found for this task")
			return
		}
		if !taskVisibleToScopes(task, scopes, p.ownerID()) {
			writeError(w, http.StatusForbidden, "Task not within allowed scopes")
			return
		}
	}

	// Live path: attach to the in-flight buffer with Last-Event-ID replay so a
	// reconnecting EventSource resumes without losing events.
	if h.taskStreamLookup != nil {
		if buf, live := h.taskStreamLookup(taskID); live {
			var lastID uint64
			if s := r.Header.Get("Last-Event-ID"); s != "" {
				lastID, _ = strconv.ParseUint(s, 10, 64)
			}
			if err := buf.Attach(r.Context(), lastID, w); err != nil &&
				!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				//nolint:gosec // G706 false positive: taskID is a uuid.UUID parsed via uuid.Parse; String() is canonical hex+dashes and cannot carry CR/LF.
				log.Printf("StreamTaskLogs: attach error for task %s: %v", taskID, err)
			}
			return
		}
	}

	// Not in flight: fall back to the persisted log as a one-shot SSE replay.
	session, err := h.storage.GetLog(taskID)
	if err != nil || session == nil {
		writeError(w, http.StatusNotFound, "No log found for this task")
		return
	}
	replayStoredLog(w, taskID, session)
}

// replayStoredLog re-emits a completed task's persisted log as SSE frames using the
// same event types the live buffer emits (agent_message / tool_call / tool_result),
// followed by a terminal status frame, then closes. This gives a client attaching
// AFTER completion the same shape it would have seen live. Best-effort: a write
// error means the client went away, so we stop.
func replayStoredLog(w http.ResponseWriter, taskID uuid.UUID, session *models.LogSession) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "Streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	var id uint64
	emit := func(name string, payload any) bool {
		id++
		data, mErr := json.Marshal(payload)
		if mErr != nil {
			return true // skip an unmarshalable frame rather than abort the replay
		}
		if _, wErr := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", id, name, string(data)); wErr != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	for _, m := range session.Messages {
		// Tool calls the assistant issued in this message.
		for _, tc := range m.ToolCalls {
			if !emit("tool_call", map[string]any{
				"type": "tool_call", "call_id": tc.ID, "name": tc.Name, "input": tc.Arguments,
			}) {
				return
			}
		}
		switch m.Role {
		case "assistant":
			if m.Content != "" {
				if !emit("agent_message", map[string]any{
					"type": "agent_message", "role": "assistant", "content": m.Content, "msg_id": m.ID,
				}) {
					return
				}
			}
		case "tool":
			callID := ""
			if m.ToolCallID != nil {
				callID = *m.ToolCallID
			}
			if !emit("tool_result", map[string]any{
				"type": "tool_result", "call_id": callID, "output": m.Content, "error": false,
			}) {
				return
			}
		}
	}

	emit("status", map[string]any{
		"type": "status", "status": "succeeded", "task_id": taskID.String(), "cost_usd": session.Cost,
	})
}
