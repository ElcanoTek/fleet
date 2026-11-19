// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"context"
	"errors"
	"net/http"
	"reflect"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// This file is the one set of create-time gates every handler that writes a
// task definition from a request runs. New tasks go through admitCreate:
// POST /tasks, A2A SendMessage, re-run and clone (createTaskGovernedFrom),
// POST /tasks/batch and POST /tasks/import. Writes onto an existing row go
// through admitEdit: PUT /tasks/{id} and import conflict=replace.
//
// Every create and edit path runs this one list;
// TestTaskWritesGoThroughTheGates fails on a handler that writes a task
// without it.

// taskGates is the gate state of one request. The per-principal gates (the
// rolling budget, the authorizing key's hourly counter) run once for the
// request; the per-task gates (admitCreate, admitEdit) run once per record.
// It holds only what the request decided — the gate functions are Handlers
// methods — so the deployment's configuration never travels inside a value
// built from request data.
type taskGates struct {
	creator taskCreator
	// budgetChecked records that the budget gate already admitted this
	// request: batch and import run it once, before any record.
	budgetChecked bool
}

// taskGatesFor is the gate state of one request by creator.
func taskGatesFor(creator taskCreator) *taskGates {
	return &taskGates{creator: creator}
}

// Refusals the edit-shaped run_if rule returns. errors.New keeps the text in
// the browser's localization inventory (scripts/check_localizations_test.go).
var (
	errRunIfChangeNeedsAdmin = errors.New("run_if: a host-side pre-run gate can only be changed by an admin")
	errRunIfPendingDispatch  = errors.New("run_if: task is already pending dispatch, so its gate can no longer be evaluated for this run; change the gate while the task is scheduled, or cancel and recreate it")
)

// refuse is a createRefusalError with err's text.
func refuse(status int, err error) *createRefusalError {
	return &createRefusalError{status: status, detail: err.Error()}
}

// checkBudget runs the per-principal rolling budget (#601 part 2) for this
// request, once: the budget bounds the principal, not any one task. At a hard
// bound it returns the budget gate's own error, for the caller to render
// (writeBudgetRefusal's 402 + Retry-After, or A2A's retry metadata); a soft
// crossing fires its once-per-window alert inside the gate.
func (h *Handlers) checkBudget(ctx context.Context, g *taskGates) error {
	if g.budgetChecked {
		return nil
	}
	if err := h.budgetCapError(ctx, g.creator); err != nil {
		return err
	}
	g.budgetChecked = true
	return nil
}

// chargeRecords charges the authorizing key's hourly request counter for a
// request that creates records tasks. Validating the key charged one, so a
// bulk create of N costs N, the same as N calls to POST /tasks (#227). False
// means the key's remaining hourly allowance cannot cover them: the caller
// answers 429. The admin key and user sessions have no hourly counter.
func (h *Handlers) chargeRecords(g *taskGates, records int) bool {
	if g.creator.creatorKey == nil {
		return true
	}
	return h.apiKeys.ConsumeN(*g.creator.creatorKey, records-1)
}

// admitCreate runs every per-task create gate on tc, in this order, and
// returns the task ready to persist. tc is normalized in place.
//
//  1. created_by_task_id is cleared. Lineage is server-authoritative (#277):
//     only the in-run create_task tool sets it, and that tool writes through
//     storage.EnqueueTask, not through a handler.
//  2. validateTaskCreate (400).
//  3. A per-task max_cost_usd or max_total_tokens above the deployment ceiling
//     needs admin (403, #1533).
//  4. run_if needs admin (403). With a lineage (re-run, clone) the rule is
//     edit-shaped, exactly UpdateTask's: a non-admin may keep the source's
//     gate (normalized comparison, so a lossy client echo is not a change) but
//     may not add, change or remove one. An admin's value is authoritative.
//  5. The per-principal budget, once per request (402).
//  6. models.NewTask, attribution (taskCreator.attribute) and, for a copy, its
//     source and lineage.
//  7. The mcp_selection check against the owner the run will resolve hosted
//     connections for (task.CreatedBy, just stamped), instead of
//     dead-lettering every occurrence at dispatch (400).
//  8. The authorizing key's priority ceiling (#230), on the post-default
//     priority (task.Priority, 0→Normal), so it reflects what would run (403).
//
// A refusal is a *createRefusalError with the status and text POST /tasks
// answers; a budget refusal is the budget gate's own error. Batch and import
// report a refusal's text per record, in their own envelope.
func (h *Handlers) admitCreate(ctx context.Context, g *taskGates, tc *models.TaskCreate, lineage *rerunLineage) (*models.Task, error) {
	tc.CreatedByTaskID = nil
	if err := h.validateTaskCreate(tc); err != nil {
		return nil, refuse(http.StatusBadRequest, err)
	}
	if msg := h.requireAdminForCeilingsAboveDeployment(g.creator.hasAdminPermission, tc); msg != "" {
		return nil, &createRefusalError{status: http.StatusForbidden, detail: msg}
	}
	if lineage == nil {
		if msg := requireAdminForRunIf(g.creator.hasAdminPermission, tc.RunIf); msg != "" {
			return nil, &createRefusalError{status: http.StatusForbidden, detail: msg}
		}
	} else if !g.creator.hasAdminPermission && !reflect.DeepEqual(tc.RunIf.Normalized(), lineage.sourceRunIf.Normalized()) {
		return nil, refuse(http.StatusForbidden, errRunIfChangeNeedsAdmin)
	}

	if err := h.checkBudget(ctx, g); err != nil {
		return nil, err
	}

	task := models.NewTask(*tc)
	g.creator.attribute(task)
	if lineage != nil {
		sourceID := lineage.sourceID
		task.SourceTaskID = &sourceID
		if lineage.sourceLineage != uuid.Nil {
			task.LineageID = lineage.sourceLineage
		}
	}

	if err := h.checkTaskMCPSelection(ctx, task); err != nil {
		return nil, refuse(http.StatusBadRequest, err)
	}
	if err := priorityCapError(g.creator.creatorKeyMaxPriority, task.Priority); err != nil {
		return nil, refuse(http.StatusForbidden, err)
	}
	return task, nil
}

// taskEditScope says how an edit's payload maps onto the row, so admitEdit
// checks what the write changes.
type taskEditScope struct {
	// replace: the payload is the whole definition (import conflict=replace).
	// Otherwise it is PUT /tasks/{id}, where an omitted mcp_selection,
	// credential_allowlist or priority keeps the stored value.
	replace bool
	// priorityPresent: the PUT payload named priority (decodeTaskUpdate). A
	// replace always writes it.
	priorityPresent bool
}

// admitEdit is admitCreate's gate set for a write onto an existing row, in
// PUT /tasks/{id}'s order, which differs from admitCreate's: the priority
// ceiling comes right after the ceiling rule, before run_if and the
// mcp_selection check (admitCreate checks it last, on the minted task). The
// row keeps its owner, so there is no attribution, and an edit creates
// nothing, so there is no budget gate.
//
//  1. validateTaskCreate (400).
//  2. A ceiling above the deployment's needs admin (403).
//  3. The key's priority ceiling, when the write sets priority (403).
//  4. run_if, edit-shaped: a non-admin may keep the row's gate but may not
//     add, change or remove one (403). A new or changed gate on a pending row
//     is refused (409): a gate is evaluated only at the scheduled→pending
//     promotion (models.RunIf's enforcement contract), which a pending task
//     has passed, and the dispatch-state recompute would yank the imminent
//     dispatch back onto the scheduler path. Removing a gate stays allowed.
//  5. The mcp_selection check against the row's owner (400). A replace always
//     checks it. PUT checks it only when the edit changes the selection, so a
//     prompt fix on a task whose connection lapsed since is not refused.
//
// A refusal is a *createRefusalError with the status and text PUT /tasks/{id}
// answers.
func (h *Handlers) admitEdit(ctx context.Context, g *taskGates, existing *models.Task, tc *models.TaskCreate, scope taskEditScope) error {
	if err := h.validateTaskCreate(tc); err != nil {
		return refuse(http.StatusBadRequest, err)
	}
	if msg := h.requireAdminForCeilingsAboveDeployment(g.creator.hasAdminPermission, tc); msg != "" {
		return &createRefusalError{status: http.StatusForbidden, detail: msg}
	}
	if scope.replace || scope.priorityPresent {
		if err := priorityCapError(g.creator.creatorKeyMaxPriority, models.NormalizePriority(tc.Priority)); err != nil {
			return refuse(http.StatusForbidden, err)
		}
	}
	runIfChanged := !reflect.DeepEqual(tc.RunIf.Normalized(), existing.RunIf.Normalized())
	if runIfChanged && !g.creator.hasAdminPermission {
		return refuse(http.StatusForbidden, errRunIfChangeNeedsAdmin)
	}
	if runIfChanged && tc.RunIf != nil && existing.Status == models.TaskStatusPending {
		return refuse(http.StatusConflict, errRunIfPendingDispatch)
	}

	switch {
	case scope.replace:
		if err := h.checkMCPSelectionForOwner(ctx, existing.CreatedBy, tc.MCPSelection, tc.CredentialAllowlist); err != nil {
			return refuse(http.StatusBadRequest, err)
		}
	case tc.MCPSelection != nil && !reflect.DeepEqual(tc.MCPSelection, existing.MCPSelection):
		allowlist := existing.CredentialAllowlist
		if tc.CredentialAllowlist != nil {
			allowlist = tc.CredentialAllowlist
		}
		if err := h.checkMCPSelectionForOwner(ctx, existing.CreatedBy, tc.MCPSelection, allowlist); err != nil {
			return refuse(http.StatusBadRequest, err)
		}
	}
	return nil
}
