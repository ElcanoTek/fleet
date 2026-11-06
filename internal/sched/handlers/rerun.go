package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// taskRerunRequest is the (optional) body of POST /tasks/{id}/rerun|clone.
type taskRerunRequest struct {
	Overrides taskRerunOverrides `json:"overrides"`
}

// RerunTask handles POST /tasks/{task_id}/rerun (#270): create a NEW one-time
// task copied from the source (scheduled_for=now, recurrence cleared), with
// optional field overrides. The original is untouched.
func (h *Handlers) RerunTask(w http.ResponseWriter, r *http.Request) {
	h.rerunOrClone(w, r, false)
}

// CloneTask handles POST /tasks/{task_id}/clone (#270): like rerun, but preserves
// the source's recurrence (computing the next fire time for a cron task).
func (h *Handlers) CloneTask(w http.ResponseWriter, r *http.Request) {
	h.rerunOrClone(w, r, true)
}

// rerunOrClone is the shared body for RerunTask/CloneTask. keepRecurrence=false
// (rerun) clears recurrence and fires immediately; true (clone) keeps it.
func (h *Handlers) rerunOrClone(w http.ResponseWriter, r *http.Request, keepRecurrence bool) {
	p := h.principalFromRequest(r)
	if !p.hasPermission(models.PermissionCreateTask) {
		writeError(w, http.StatusForbidden, "Insufficient permissions")
		return
	}

	taskID, err := uuid.Parse(chi.URLParam(r, "task_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid task ID")
		return
	}
	source, err := h.storage.GetTask(taskID)
	if err != nil || source == nil {
		writeError(w, http.StatusNotFound, "Task not found")
		return
	}
	// Own-rows visibility (#1082): the copy inherits the source's prompt and
	// config, so re-running/cloning a task you cannot see would read it.
	if !taskVisibleToPrincipal(p, source) {
		writeError(w, http.StatusForbidden, "Tasks are private to their creator")
		return
	}
	// The body is optional; only decode when present (rerun-with-no-changes sends none).
	var req taskRerunRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := readJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid request body")
			return
		}
	}

	loc := h.storage.Location()
	tc, berr := buildRerunTaskCreate(source, keepRecurrence, req.Overrides, loc)
	if berr != nil {
		writeError(w, http.StatusBadRequest, berr.Error())
		return
	}

	// Who owns the copy: the calling user, or — for the admin key and scoped
	// keys, which have no user — the source's owner (rerunCreator).
	creator, cerr := h.rerunCreator(r.Context(), p, source)
	if cerr != nil {
		writeError(w, http.StatusInternalServerError, "Failed to resolve the source task's owner")
		return
	}
	lineage := &rerunLineage{sourceID: source.ID, sourceRunIf: source.RunIf, sourceLineage: source.WorkspaceLineage()}
	if req.Overrides.NewLineage {
		lineage.sourceLineage = uuid.Nil // NewTask's fresh lineage stands
	}

	// The copy is a create like any other, so it goes through the ONE governed
	// pipeline (run_if privilege boundary, budget gate, priority ceiling,
	// attribution, mcp_selection check) rather than a validate-and-insert of
	// its own — the overrides expose run_if and priority, so a private path
	// here was a weaker route around every gate POST /tasks enforces.
	newTask, err := h.createTaskGovernedFrom(r.Context(), creator, tc, lineage)
	if err != nil {
		var refusal *createRefusalError
		if errors.As(err, &refusal) {
			writeError(w, refusal.status, refusal.detail)
			return
		}
		writeBudgetRefusal(w, err)
		return
	}
	verb := "re-run"
	if keepRecurrence {
		verb = "clone"
	}
	//nolint:gosec // G706: IDs are uuid.UUID values; their String() is canonical hex+dashes, no CR/LF.
	log.Printf("Task %s created (%s of %s)", newTask.ID, verb, source.ID)
	localizeTask(newTask)
	writeJSON(w, http.StatusCreated, newTask)
}

// buildRerunTaskCreate constructs the TaskCreate recipe for a re-run (#270) from
// a source task. keepRecurrence=false (rerun) clears recurrence and runs
// immediately; true (clone) preserves a cron recurrence (recomputing the next
// fire in the task's timezone, falling back to fallbackLoc). Overrides are then
// applied. It is pure (no h/HTTP) so the scheduling logic is unit-testable.
//
// IMPORTANT: an immediate run uses ScheduledFor=nil — the codebase's "run now"
// convention (a fresh pending task the worker claims at once). Setting &now would
// be rejected by validateTaskCreate's "scheduled time cannot be in the past"
// check, which re-samples a strictly-later now. A gated source is the one
// exception to "claims at once": TaskToCreate carries run_if, and NewTask parks
// any gated cron task scheduled-for-now so the scheduler evaluates the gate
// before dispatch (models.RunIf's enforcement contract) — a rerun must not be a
// path around the condition the gate exists to enforce.
func buildRerunTaskCreate(source *models.Task, keepRecurrence bool, o taskRerunOverrides, fallbackLoc *time.Location) (models.TaskCreate, error) {
	tc := models.TaskToCreate(source)
	// A copy is never the named DEFINITION. Name is the import/export identity
	// key and carries a partial unique index on non-empty names, so inheriting
	// the source's name collides with the source row still in the table and the
	// insert fails (a 500 on every re-run of a named task). Storage's recurrence
	// chain clears Name for exactly this reason; the copy paths must match.
	tc.Name = ""
	if keepRecurrence && strings.TrimSpace(tc.Recurrence) != "" {
		loc, _ := storage.TaskLocation(tc.Timezone, fallbackLoc)
		next, perr := storage.NextRun(tc.Recurrence, loc, time.Now())
		if perr != nil {
			return tc, fmt.Errorf("source task has an invalid recurrence")
		}
		tc.ScheduledFor = &next
	} else {
		tc.ScheduledFor = nil // immediate run-now
		if !keepRecurrence {
			// Rerun is one-time, so the recurrence's end conditions go with it:
			// TaskToCreate carries recurrence_until/recurrence_remaining, and
			// validateTaskCreate rejects either without a recurrence — which made
			// every rerun (and every "just this run" edit) of a recurring task
			// with an end date or run count fail with "recurrence_until requires
			// a recurrence". Matches the trigger-run projection in storage.
			tc.Recurrence = ""
			tc.RecurrenceUntil = nil
			tc.RecurrenceRemaining = nil
		}
	}
	if o.NewLineage {
		// TaskToCreate carried the source's lineage; a new job starts its own.
		tc.LineageID = nil
	}
	if err := applyRerunOverrides(&tc, o); err != nil {
		return tc, err
	}
	return tc, nil
}

// applyRerunOverrides mutates tc with any non-nil override fields (#270). Tags,
// when provided (non-nil, possibly empty), replace the inherited set. The only
// error is a malformed sandbox_limits / run_if object.
func applyRerunOverrides(tc *models.TaskCreate, o taskRerunOverrides) error {
	if o.Prompt != nil {
		tc.Prompt = *o.Prompt
	}
	if o.Model != nil {
		tc.Model = o.Model
	}
	if o.FallbackModel != nil {
		tc.FallbackModel = o.FallbackModel
	}
	if o.MaxIterations != nil {
		tc.MaxIterations = o.MaxIterations
	}
	if o.MaxCostUSD != nil {
		tc.MaxCostUSD = o.MaxCostUSD
	}
	if o.MaxTotalTokens != nil {
		tc.MaxTotalTokens = o.MaxTotalTokens
	}
	if o.Priority != nil {
		tc.Priority = *o.Priority
	}
	if o.AllowNetwork != nil {
		tc.AllowNetwork = *o.AllowNetwork
	}
	if o.AllowDelegation != nil {
		tc.AllowDelegation = o.AllowDelegation
	}
	if o.ThinkingBudgetTokens != nil {
		if *o.ThinkingBudgetTokens < 0 {
			tc.ThinkingBudgetTokens = nil // clear → inherit the global default
		} else {
			v := *o.ThinkingBudgetTokens
			tc.ThinkingBudgetTokens = &v
		}
	}
	if o.Description != nil {
		tc.Description = *o.Description
	}
	if o.Title != nil {
		tc.Title = *o.Title
	}
	if o.Tags != nil {
		tc.Tags = o.Tags
	}
	if o.Persona != nil {
		tc.Persona = *o.Persona
	}
	if o.MCPSelection != nil {
		tc.MCPSelection = o.MCPSelection
	}
	if o.Files != nil {
		tc.Files = o.Files
		tc.FileNames = nil
	}
	if o.FileNames != nil {
		tc.FileNames = o.FileNames
	}
	if o.ExpectedDurationMinutes != nil {
		if *o.ExpectedDurationMinutes <= 0 {
			tc.ExpectedDurationMinutes = nil // clear the SLA
		} else {
			v := *o.ExpectedDurationMinutes
			tc.ExpectedDurationMinutes = &v
		}
	}
	if o.SLAWarnMultiplier != nil {
		tc.SLAWarnMultiplier = *o.SLAWarnMultiplier // 0 → default (ResolveSLAMultipliers)
	}
	if o.SLAFailMultiplier != nil {
		tc.SLAFailMultiplier = *o.SLAFailMultiplier
	}
	if o.CarryContext != nil {
		tc.CarryContext = *o.CarryContext
	}
	if o.InstructionSelfImprove != nil {
		tc.InstructionSelfImprove = *o.InstructionSelfImprove
	}
	return applyRerunRawOverrides(tc, o)
}

// applyRerunRawOverrides applies the two RawMessage overrides (sandbox_limits,
// run_if). A malformed object is the caller's error, reported as a 400 by
// rerunOrClone; the validated shape (RunIf command non-empty, limits bounds)
// is the same validateTaskCreate pass every create goes through.
func applyRerunRawOverrides(tc *models.TaskCreate, o taskRerunOverrides) error {
	if present, isNull := rawJSONPresent(o.SandboxLimits); present {
		if isNull {
			tc.SandboxLimits = nil
		} else {
			var limits models.TaskSandboxLimits
			if err := json.Unmarshal(o.SandboxLimits, &limits); err != nil {
				return fmt.Errorf("sandbox_limits: %w", err)
			}
			if limits.IsZero() {
				tc.SandboxLimits = nil // an all-zero override is "global defaults"
			} else {
				tc.SandboxLimits = &limits
			}
		}
	}
	if present, isNull := rawJSONPresent(o.RunIf); present {
		if isNull {
			tc.RunIf = nil
		} else {
			var gate models.RunIf
			if err := json.Unmarshal(o.RunIf, &gate); err != nil {
				return fmt.Errorf("run_if: %w", err)
			}
			tc.RunIf = &gate
		}
	}
	return nil
}
