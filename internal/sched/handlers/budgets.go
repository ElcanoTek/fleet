// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

// Per-principal rolling budgets (#601 part 2): the admin CRUD surface
// (GET/POST /admin/budgets, DELETE /admin/budgets/{budget_id}) and the shared
// task-create gate. Enforcement itself lives in internal/sched/budget — ONE
// Enforcer shared by every create path (POST /tasks, POST /tasks/batch, and
// the chat schedule_task seam wired in cmd/fleet), mirroring the
// priorityCapError shared-helper discipline so the paths cannot drift.
//
// Like GET /admin/usage, the CRUD endpoints register behind
// AdminOrUserAuthMiddleware with the admin gate enforced in-handler on
// PermissionAdmin (#458): the Next proxy can never send the admin X-API-Key,
// and budgets are global spend policy, so a non-admin member gets 403.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/budget"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// BudgetGate is the enforcement seam (satisfied by *budget.Enforcer), injected
// by cmd/fleet via SetBudgetGate. nil = no budget enforcement anywhere —
// today's behavior, byte-for-byte.
type BudgetGate interface {
	// CheckCreate admits (nil) or refuses (*budget.ExceededError) a task-create
	// for the given principals; any other error is an infrastructure failure.
	CheckCreate(ctx context.Context, p models.BudgetPrincipals) error
	// Statuses evaluates every configured budget with live window spend.
	Statuses(ctx context.Context) ([]models.BudgetStatus, error)
}

// SetBudgetGate wires the budget enforcement seam. Call before serving traffic.
func (h *Handlers) SetBudgetGate(g BudgetGate) {
	h.budgetGate = g
}

// budgetCapError is the budget mirror of priorityCapError: the ONE helper the
// in-process create paths call so they cannot drift on how budgets gate task
// creation. It returns nil with no gate wired or no matching budget (today's
// behavior), *budget.ExceededError at a hard bound, and any other error when
// the budget could not be verified (the caller fails closed with a 500).
func (h *Handlers) budgetCapError(ctx context.Context, creator taskCreator) error {
	if h.budgetGate == nil {
		return nil
	}
	p := models.BudgetPrincipals{User: creator.creatorUsername}
	if creator.creatorKey != nil {
		p.Key = *creator.creatorKey
	}
	return h.budgetGate.CheckCreate(ctx, p)
}

// writeBudgetRefusal maps a budgetCapError failure onto the wire: a hard-bound
// refusal is 402 Payment Required (the principal's spend allowance for the
// window is exhausted — distinct from the 429 the per-key rate limiter uses)
// with Retry-After pointing at the window rollover; anything else is an
// unverifiable budget, surfaced as 500 rather than admitting work unchecked.
func writeBudgetRefusal(w http.ResponseWriter, err error) {
	var exceeded *budget.ExceededError
	if errors.As(err, &exceeded) {
		// Use the enforcer-computed wait (its clock), clamped to ≥1s — RFC 9110
		// allows any non-negative delta and dropping the header entirely reads
		// as "retry immediately", which is never right for an exhausted window.
		wait := exceeded.RetryAfter
		if wait < time.Second {
			wait = time.Second
		}
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(wait.Seconds())))
		writeError(w, http.StatusPaymentRequired, exceeded.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, "Budget check failed")
}

// ListBudgets handles GET /admin/budgets: every configured budget with its
// live current-window evaluation (spend from the persisted metering, effective
// hard bounds after the global-ceiling clamp, soft-alert state).
func (h *Handlers) ListBudgets(w http.ResponseWriter, r *http.Request) {
	if !h.principalFromRequest(r).hasPermission(models.PermissionAdmin) {
		writeError(w, http.StatusForbidden, "Admin access required")
		return
	}
	var (
		statuses []models.BudgetStatus
		err      error
	)
	if h.budgetGate != nil {
		statuses, err = h.budgetGate.Statuses(r.Context())
	} else {
		// No gate wired (enforcement off): still list the raw configuration so
		// the admin surface never hides persisted rows; spend reads as zero.
		var budgets []models.Budget
		budgets, err = h.storage.ListBudgets(r.Context())
		for _, b := range budgets {
			statuses = append(statuses, models.BudgetStatus{Budget: b})
		}
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to list budgets")
		return
	}
	if statuses == nil {
		statuses = []models.BudgetStatus{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"budgets": statuses})
}

// UpsertBudget handles POST /admin/budgets: create or replace the budget for
// (scope, principal_id, window). An upsert resets the soft-alert marker so an
// edited budget re-arms its once-per-window alert.
func (h *Handlers) UpsertBudget(w http.ResponseWriter, r *http.Request) {
	if !h.principalFromRequest(r).hasPermission(models.PermissionAdmin) {
		writeError(w, http.StatusForbidden, "Admin access required")
		return
	}
	var bc models.BudgetCreate
	if err := readJSON(r, &bc); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if err := bc.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	b, err := h.storage.UpsertBudget(r.Context(), bc)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to save budget")
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// DeleteBudget handles DELETE /admin/budgets/{budget_id}.
func (h *Handlers) DeleteBudget(w http.ResponseWriter, r *http.Request) {
	if !h.principalFromRequest(r).hasPermission(models.PermissionAdmin) {
		writeError(w, http.StatusForbidden, "Admin access required")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "budget_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid budget id")
		return
	}
	deleted, err := h.storage.DeleteBudget(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to delete budget")
		return
	}
	if !deleted {
		writeError(w, http.StatusNotFound, "Budget not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
