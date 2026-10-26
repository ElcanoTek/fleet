// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// rerunCreator is the create pipeline's creator for a re-run or clone of
// source by p (docs/TASK-OWNERSHIP.md).
//
// A user's copy is theirs, as it has been since #270: the caller is the
// creator, exactly as for POST /tasks.
//
// The bootstrap admin key and a scoped API key have no user behind them, and
// attributing the copy to the caller made it ownerless. That broke the copy in
// production (2026-10-26): a hosted connection in its mcp_selection resolves
// only through the task OWNER's connections, so every run of the ownerless
// copy dead-lettered, and the copy had no one to send a digest to. So a copy
// made by such a principal keeps the source's owner — its user (CreatedBy,
// plus that user's username for the budget gate's user scope, as the create
// path records it for a user) — and, for the admin key, which has no key id
// of its own, the source's authorizing key. A scoped key stays the copy's
// authorizing key: spend is attributed to it, and ADR-0043 lets the key that
// made a task read it.
func (h *Handlers) rerunCreator(ctx context.Context, p principal, source *models.Task) (taskCreator, error) {
	creator := creatorFromPrincipal(p)
	if p.user != nil || source == nil {
		return creator, nil
	}
	if source.CreatedBy != nil {
		owner := *source.CreatedBy
		creator.creatorID = &owner
		names, err := h.storage.GetUsersByIDsWithContext(ctx, []uuid.UUID{owner})
		if err != nil {
			return creator, err
		}
		creator.creatorUsername = names[owner]
	}
	if creator.creatorKey == nil && source.CreatedByKeyID != nil {
		key := *source.CreatedByKeyID
		creator.creatorKey = &key
	}
	return creator, nil
}

// taskOwnerRequest is the PUT /tasks/{task_id}/owner body.
type taskOwnerRequest struct {
	// Username names an existing, enabled user — on a standard deployment the
	// user's email.
	Username string `json:"username"`
}

// taskOwnerResponse is the PUT /tasks/{task_id}/owner result: the task as it
// now reads, and every row the reassignment rewrote (the task first, then the
// unfinished later occurrences of its recurrence chain).
type taskOwnerResponse struct {
	Task           *models.Task `json:"task"`
	UpdatedTaskIDs []uuid.UUID  `json:"updated_task_ids"`
}

// SetTaskOwner handles PUT /tasks/{task_id}/owner: an admin sets the user who
// owns a task — its created_by, which decides whose hosted connections its runs
// resolve, who receives its owner digest, and who may read and edit it as its
// creator. The new owner also lands on the chain's unfinished later
// occurrences and on every occurrence spawned afterwards (Storage.SetTaskOwner),
// so a recurring job stays owned. Before this there was no way to give an
// ownerless task (one an admin-key clone used to mint) an owner at all.
//
// The task's mcp_selection must resolve for the new owner — the same check a
// create runs — so a reassignment cannot leave a chain that dead-letters on
// every run. 404 for an unknown task or an unknown (or disabled) user.
func (h *Handlers) SetTaskOwner(w http.ResponseWriter, r *http.Request) {
	p := h.principalFromRequest(r)
	if !p.hasPermission(models.PermissionAdmin) {
		writeError(w, http.StatusForbidden, "Admin access required")
		return
	}
	taskID, err := uuid.Parse(chi.URLParam(r, "task_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid task ID")
		return
	}
	var req taskOwnerRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	username := strings.TrimSpace(req.Username)
	if username == "" {
		writeError(w, http.StatusBadRequest, "username is required")
		return
	}

	ctx := r.Context()
	task, err := h.storage.GetTask(taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, "Task not found")
		return
	}
	owner, err := h.lookupTaskOwner(ctx, username)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to look up user")
		return
	}
	if owner == nil {
		writeError(w, http.StatusNotFound, "User not found")
		return
	}
	if err := h.checkMCPSelectionForOwner(ctx, &owner.ID, task.MCPSelection, task.CredentialAllowlist); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	updated, err := h.storage.SetTaskOwner(ctx, taskID, owner.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "Task not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "Failed to set task owner")
		return
	}
	//nolint:gosec // G706: taskID is a parsed uuid.UUID; the names pass logSafe.
	log.Printf("Task %s owner set to %s by %s (%d row(s) of its chain)", taskID, logSafe(owner.Username), logSafe(p.stopLabel()), len(updated))

	task, err = h.storage.GetTask(taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, "Task not found")
		return
	}
	if err := h.populateCreatedByUsernames(ctx, []*models.Task{task}); err != nil {
		log.Printf("Warning: failed to populate created_by username for task %s: %v", taskID, err) //nolint:gosec // G706: taskID is a parsed uuid.UUID; err is a storage error, not request input.
	}
	localizeTask(task)
	writeJSON(w, http.StatusOK, taskOwnerResponse{Task: task, UpdatedTaskIDs: updated})
}

// lookupTaskOwner resolves an enabled user by username: as given, then
// lowercased (provisioned usernames are lowercased emails). nil, nil = no such
// user.
func (h *Handlers) lookupTaskOwner(ctx context.Context, username string) (*models.User, error) {
	candidates := []string{username}
	if lower := strings.ToLower(username); lower != username {
		candidates = append(candidates, lower)
	}
	for _, name := range candidates {
		user, err := h.storage.GetUserByUsernameWithContext(ctx, name)
		if err == nil && user != nil {
			return user, nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	return nil, nil
}
