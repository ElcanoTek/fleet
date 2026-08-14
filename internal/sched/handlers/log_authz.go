// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

// Per-task run-log (transcript) authorization (#980).
//
// A run log is the most sensitive artifact a task produces: it carries the
// verbatim tool traffic — connector query results, whatever PII the agent
// handled — plus per-run cost data. Before this file existed, every run-log read
// authorized on PermissionViewLogs ALONE, with no per-task scoping: any
// principal holding view_logs (including a scoped fleet_task_* key minted for
// one automation, and every readonly key) could GET the transcript of EVERY task
// on the box, with task ids enumerable through GET /tasks. On a multi-client
// deployment that is a cross-tenant read from a single leaked key.
//
// The platform already drew the per-task boundary for the LESS sensitive
// artifact class: workspace files (#287) are creator-private via
// taskWorkspaceOwned. This makes transcripts consistent with that: a principal
// reads its own tasks' transcripts, and reading anyone else's requires the
// explicit fleet-wide PermissionViewAllLogs (implied by PermissionAdmin).
//
// The gate lives here, in ONE resolver every transcript route calls, rather than
// being re-derived per handler — the previous shape (three near-identical inline
// checks) is how the latest-log endpoint and its history/stream siblings drifted
// into agreeing on a check that authorized nothing.

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// taskCreatedByPrincipal reports whether the given task was created by this
// principal — the creating user, or the creating API key. It is deliberately
// NOT satisfied by the admin credential: callers layer their own admin/fleet-wide
// override on top, so that "who made this" stays separable from "who may read
// it". An unattributed task (no CreatedBy, no CreatedByKeyID — e.g. one seeded
// out-of-band) is owned by nobody and therefore readable only under a fleet-wide
// grant.
func taskCreatedByPrincipal(p principal, task *models.Task) bool {
	if task == nil {
		return false
	}
	if p.user != nil && task.CreatedBy != nil && *task.CreatedBy == p.user.ID {
		return true
	}
	if p.apiKey != nil && task.CreatedByKeyID != nil && p.apiKey.KeyID == *task.CreatedByKeyID {
		return true
	}
	return false
}

// taskLogsReadable reports whether the principal may read the given task's run
// log. PermissionViewLogs (checked by the caller) admits it to the surface; this
// decides WHICH tasks. Fleet-wide reads require PermissionViewAllLogs — which
// hasPermission also reports true for the admin API key and any principal
// carrying PermissionAdmin.
func taskLogsReadable(p principal, task *models.Task) bool {
	if p.hasPermission(models.PermissionViewAllLogs) {
		return true
	}
	return taskCreatedByPrincipal(p, task)
}

// logReadableTask parses the path task id, loads the task, and enforces the full
// run-log gate: PermissionViewLogs plus taskLogsReadable. It writes the HTTP
// error and returns ok=false on any failure, so every transcript route
// (latest log, per-attempt history, live stream) shares one decision.
//
// notFoundMsg lets each route keep its established 404 wording; the ORDER of the
// checks is fixed here on purpose — an unreadable task 403s rather than 404s, so
// the response never turns into an oracle for which task ids exist beyond what
// GET /tasks already exposes to the same principal.
func (h *Handlers) logReadableTask(w http.ResponseWriter, r *http.Request, notFoundMsg string) (*models.Task, bool) {
	p := h.principalFromRequest(r)
	if !p.hasPermission(models.PermissionViewLogs) {
		writeError(w, http.StatusForbidden, "Insufficient permissions")
		return nil, false
	}

	taskID, err := uuid.Parse(chi.URLParam(r, "task_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid task ID")
		return nil, false
	}

	task, err := h.storage.GetTask(taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, notFoundMsg)
		return nil, false
	}

	if !taskLogsReadable(p, task) {
		writeError(w, http.StatusForbidden, "Run logs are private to the task creator")
		return nil, false
	}

	return task, true
}
