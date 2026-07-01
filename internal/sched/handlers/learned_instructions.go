package handlers

// Self-improving memory HTTP surface (#516): capture feedback on a task's
// output, and manage the versioned learned instructions distilled from it.
// Distillation is STAGED (enterprise default): a proposal is created when
// feedback crosses a threshold, but only an explicit activation changes a
// run's behavior — and activation is fully revertible (re-activate an older
// version, or deactivate to remove it).

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// LearnedInstructionDistiller turns a task's down-vote critiques into one
// standing instruction. Satisfied by *agent.Manager.DistillLearnedInstruction;
// nil = distillation off (feedback is still recorded, just never auto-proposed).
type LearnedInstructionDistiller interface {
	DistillLearnedInstruction(ctx context.Context, taskPrompt string, downCritiques []string, priorInstruction string) string
}

// SetLearnedInstructionDistiller injects the distiller (main.go, gated on
// FLEET_SELF_IMPROVE_ENABLED).
func (h *Handlers) SetLearnedInstructionDistiller(d LearnedInstructionDistiller) {
	h.learnedDistiller = d
}

// distillThreshold is how many fresh down-signals trigger a distilled proposal.
const distillThreshold = 3

type feedbackRequest struct {
	Rating   string `json:"rating"`
	Critique string `json:"critique,omitempty"`
}

// SubmitFeedback handles POST /tasks/{task_id}/feedback — record one signal,
// and when fresh down-signals cross the threshold, distill a proposal
// (off-thread; never blocks the response).
func (h *Handlers) SubmitFeedback(w http.ResponseWriter, r *http.Request) {
	p := h.principalFromRequest(r)
	if !p.hasPermission(models.PermissionViewTasks) {
		writeError(w, http.StatusForbidden, "Insufficient permissions")
		return
	}
	task, ok := h.taskForRequest(w, r, &p)
	if !ok {
		return
	}
	var req feedbackRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}
	if req.Rating != models.FeedbackUp && req.Rating != models.FeedbackDown {
		writeError(w, http.StatusBadRequest, "rating must be 'up' or 'down'")
		return
	}
	fb := &models.TaskFeedback{
		TaskID:    task.ID,
		Rating:    req.Rating,
		Critique:  req.Critique,
		CreatedAt: time.Now().Unix(),
		CreatedBy: p.stopLabel(),
	}
	if err := h.storage.AddTaskFeedback(r.Context(), fb); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to record feedback")
		return
	}
	h.maybeDistill(task)
	writeJSON(w, http.StatusOK, fb)
}

// maybeDistill fires an off-thread distillation when fresh down-signals reached
// the threshold. Best-effort: errors are logged, never surfaced.
func (h *Handlers) maybeDistill(task *models.Task) {
	if h.learnedDistiller == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		signals, err := h.storage.UnconsumedFeedback(ctx, task.ID)
		if err != nil {
			log.Printf("self-improve: load feedback for task %s: %v", logSafe(task.ID.String()), err) //nolint:gosec // G706 false positive: task.ID is a uuid.UUID and logSafe strips CR/LF.
			return
		}
		var critiques []string
		var evidence []uuid.UUID
		for _, s := range signals {
			if s.Rating == models.FeedbackDown {
				evidence = append(evidence, s.ID)
				if s.Critique != "" {
					critiques = append(critiques, s.Critique)
				}
			}
		}
		if len(evidence) < distillThreshold {
			return
		}
		prior := ""
		if active, aerr := h.storage.ActiveLearnedInstruction(ctx, task.ID); aerr == nil && active != nil {
			prior = active.Content
		}
		// A down-vote with no words still counts as a signal; give the distiller
		// a placeholder so it can still act on "this output was bad N times".
		if len(critiques) == 0 {
			critiques = []string{"(no written critique — the output was rated unsatisfactory)"}
		}
		content := h.learnedDistiller.DistillLearnedInstruction(ctx, task.Prompt, critiques, prior)
		if content == "" {
			return // nothing worth proposing; leave signals unconsumed for next time
		}
		if _, err := h.storage.ProposeLearnedInstruction(ctx, task.ID, content, evidence, time.Now().Unix()); err != nil {
			log.Printf("self-improve: propose instruction for task %s: %v", logSafe(task.ID.String()), err) //nolint:gosec // G706 false positive: task.ID is a uuid.UUID and logSafe strips CR/LF.
		}
	}()
}

// LearnedInstructions handles GET/POST /tasks/{task_id}/learned-instructions
// and POST .../{version}/activate, DELETE .../active (deactivate).
func (h *Handlers) LearnedInstructions(w http.ResponseWriter, r *http.Request) {
	p := h.principalFromRequest(r)
	if !p.hasPermission(models.PermissionViewTasks) {
		writeError(w, http.StatusForbidden, "Insufficient permissions")
		return
	}
	task, ok := h.taskForRequest(w, r, &p)
	if !ok {
		return
	}

	versionStr := chi.URLParam(r, "version")
	switch {
	case r.Method == http.MethodGet && versionStr == "":
		list, err := h.storage.ListLearnedInstructions(r.Context(), task.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to list learned instructions")
			return
		}
		if list == nil {
			list = []*models.TaskLearnedInstruction{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"learned_instructions": list})

	case r.Method == http.MethodDelete && versionStr == "":
		// Deactivate: full revert to "no learned instruction". Mutating →
		// requires the cancel permission (an operator action), like activation.
		if !p.hasPermission(models.PermissionCancelTask) {
			writeError(w, http.StatusForbidden, "Activating/reverting a learned instruction requires operator permission")
			return
		}
		had, err := h.storage.DeactivateLearnedInstructions(r.Context(), task.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to deactivate")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deactivated": had})

	case r.Method == http.MethodPost && versionStr != "":
		if !p.hasPermission(models.PermissionCancelTask) {
			writeError(w, http.StatusForbidden, "Activating a learned instruction requires operator permission")
			return
		}
		version, err := strconv.Atoi(versionStr)
		if err != nil || version <= 0 {
			writeError(w, http.StatusBadRequest, "invalid version")
			return
		}
		li, err := h.storage.ActivateLearnedInstruction(r.Context(), task.ID, version, p.stopLabel(), time.Now().Unix())
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, li)

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// taskForRequest loads the path task and enforces the scoped-principal
// visibility gate (mirrors the CancelTask/StreamTaskLogs ownership check).
func (h *Handlers) taskForRequest(w http.ResponseWriter, r *http.Request, p *principal) (*models.Task, bool) {
	taskID, err := uuid.Parse(chi.URLParam(r, "task_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid task ID")
		return nil, false
	}
	task, err := h.storage.GetTask(taskID)
	if err != nil || task == nil {
		writeError(w, http.StatusNotFound, "Task not found")
		return nil, false
	}
	if scopes := p.scopes(); len(scopes) > 0 && !taskVisibleToScopes(task, scopes, p.ownerID()) {
		writeError(w, http.StatusForbidden, "Task not within allowed scopes")
		return nil, false
	}
	return task, true
}
