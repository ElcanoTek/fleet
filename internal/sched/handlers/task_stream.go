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
// Auth + ownership are IDENTICAL to GetLogs (GET /logs/{task_id}): the shared
// transcript gate in log_authz.go — PermissionViewLogs plus per-task ownership,
// or the explicit fleet-wide PermissionViewAllLogs. Streaming a transcript live
// must never be a way around the gate on reading it afterwards.
func (h *Handlers) StreamTaskLogs(w http.ResponseWriter, r *http.Request) {
	task, ok := h.logReadableTask(w, r, "No log found for this task")
	if !ok {
		return
	}
	taskID := task.ID

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
	// The replay's terminal frame reports the task's REAL outcome (#508 — it
	// was previously hardcoded "succeeded", misreporting failed/stopped runs).
	// The row the authorization gate already loaded is the same row this used to
	// re-fetch, so the status is read from it rather than hitting storage twice.
	replayStoredLog(w, taskID, session, replayTerminalStatus(task.Status))
}

// replayTerminalStatus maps a terminal task status onto the stream's status
// vocabulary (running|succeeded|failed|stopped).
func replayTerminalStatus(status models.TaskStatus) string {
	switch status {
	case models.TaskStatusSuccess:
		return "succeeded"
	case models.TaskStatusCancelled:
		return "stopped"
	case models.TaskStatusError, models.TaskStatusDeadLettered:
		return "failed"
	default:
		return "succeeded"
	}
}

// replayStoredLog re-emits a completed task's persisted log as SSE frames using the
// same event types the live buffer emits (agent_message / tool_call / tool_result),
// followed by a terminal status frame, then closes. This gives a client attaching
// AFTER completion the same shape it would have seen live. Best-effort: a write
// error means the client went away, so we stop.
func replayStoredLog(w http.ResponseWriter, taskID uuid.UUID, session *models.LogSession, terminalStatus string) {
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

	toolNames := make(map[string]string)
	for _, m := range session.Messages {
		// Tool calls the assistant issued in this message.
		for _, tc := range m.ToolCalls {
			toolNames[tc.ID] = tc.Name
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
			name := m.ToolName
			if name == "" {
				name = toolNames[callID]
			}
			if !emit("tool_result", map[string]any{
				"type": "tool_result", "call_id": callID, "name": name, "output": m.Content, "error": m.IsError,
			}) {
				return
			}
		}
	}

	emit("status", map[string]any{
		"type": "status", "status": terminalStatus, "task_id": taskID.String(), "cost_usd": session.Cost,
	})
}

// SetTaskStopper injects the runner pool's live-run interrupt (#508) so
// CancelTask can halt a task executing in this process, not just flip its DB
// row. Mirrors SetTaskStreamProvider.
func (h *Handlers) SetTaskStopper(stop func(taskID uuid.UUID, who string) bool) {
	h.taskStopper = stop
}

// SetTaskKicker injects the runner pool's claim-loop wake (runner.Pool.Kick)
// so a write that makes a task immediately claimable dispatches it now rather
// than at the pool's next poll tick. Mirrors SetTaskStreamProvider: set once
// before serving, nil-safe at every call site.
func (h *Handlers) SetTaskKicker(kick func()) {
	h.taskKicker = kick
}

// kickTaskQueue fires the wired task kicker, if any. Call it AFTER the
// pending-making write has committed — the kick races the pool's scan, and a
// scan that runs before the commit simply finds nothing (the next tick still
// picks the task up, so the race costs latency, never a lost task).
func (h *Handlers) kickTaskQueue() {
	if h.taskKicker != nil {
		h.taskKicker()
	}
}
