package handlers

// Scheduler UX 2.0 (#504): an upcoming-runs view over the existing scheduler
// data. Recurring tasks' next N occurrences come from a cron walk; one-shot
// scheduled tasks contribute their single scheduled_for. No new run-records
// table — this is a computed view (the MVP the issue calls for).

import (
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// UpcomingRun is one projected future execution.
type UpcomingRun struct {
	TaskID     string    `json:"task_id"`
	Name       string    `json:"name,omitempty"`
	Prompt     string    `json:"prompt"`
	Recurrence string    `json:"recurrence,omitempty"`
	NextRun    time.Time `json:"next_run"`
	Recurring  bool      `json:"recurring"`
}

const (
	upcomingDefaultLimit    = 50
	upcomingPerTaskMax      = 5   // occurrences projected per recurring task
	upcomingHorizonMaxTasks = 500 // safety bound on tasks scanned
)

// GetUpcomingRuns handles GET /tasks/upcoming — the calendar/timeline feed.
// For each scheduled (recurring or one-shot) task it projects future runs and
// returns them sorted by time, capped at ?limit (default 50).
func (h *Handlers) GetUpcomingRuns(w http.ResponseWriter, r *http.Request) {
	p := h.principalFromRequest(r)
	if !p.hasPermission(models.PermissionViewTasks) {
		writeError(w, http.StatusForbidden, "Insufficient permissions")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = upcomingDefaultLimit
	}

	// The claimable-but-not-yet-run tasks: scheduled + pending. (A running task
	// has no "next" until it recurs; a terminal one is history.)
	var tasks []*models.Task
	for _, st := range []models.TaskStatus{models.TaskStatusScheduled, models.TaskStatusPending} {
		batch, err := h.storage.GetTasksByStatus(st)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to load scheduled tasks")
			return
		}
		tasks = append(tasks, batch...)
		if len(tasks) > upcomingHorizonMaxTasks {
			tasks = tasks[:upcomingHorizonMaxTasks]
			break
		}
	}

	now := time.Now()
	runs := make([]UpcomingRun, 0, len(tasks))
	for _, t := range tasks {
		// Scoped principals only see tasks within their scope.
		if scopes := p.scopes(); len(scopes) > 0 && !taskVisibleToScopes(t, scopes, p.ownerID()) {
			continue
		}
		runs = append(runs, projectRuns(t, now)...)
	}
	sort.Slice(runs, func(a, b int) bool { return runs[a].NextRun.Before(runs[b].NextRun) })
	if len(runs) > limit {
		runs = runs[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"upcoming": runs})
}

// projectRuns computes a task's upcoming executions: up to upcomingPerTaskMax
// cron occurrences for a recurring task (in its timezone), or its single
// scheduled_for for a one-shot.
func projectRuns(t *models.Task, now time.Time) []UpcomingRun {
	base := UpcomingRun{TaskID: t.ID.String(), Name: t.Name, Prompt: t.Prompt, Recurrence: t.Recurrence}
	if t.Recurrence != "" {
		schedule, err := cron.ParseStandard(t.Recurrence)
		if err != nil {
			return nil
		}
		loc := taskLocation(t.Timezone)
		next := now.In(loc)
		out := make([]UpcomingRun, 0, upcomingPerTaskMax)
		for i := 0; i < upcomingPerTaskMax; i++ {
			next = schedule.Next(next)
			if next.IsZero() {
				break
			}
			r := base
			r.Recurring = true
			r.NextRun = next
			out = append(out, r)
		}
		return out
	}
	if t.ScheduledFor != nil && t.ScheduledFor.After(now) {
		r := base
		r.NextRun = *t.ScheduledFor
		return []UpcomingRun{r}
	}
	return nil
}

// taskLocation resolves a task's timezone to a *time.Location (UTC on empty or
// unparseable), so cron occurrences honor the task's zone (matching how the
// scheduler evaluates recurrence).
func taskLocation(tz string) *time.Location {
	if tz == "" {
		return time.UTC
	}
	if loc, err := time.LoadLocation(tz); err == nil {
		return loc
	}
	return time.UTC
}
