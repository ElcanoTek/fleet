package handlers

// Scheduler UX 2.0 (#504): an upcoming-runs view over the existing scheduler
// data. Recurring tasks' next N occurrences come from a cron walk; one-shot
// scheduled tasks contribute their single scheduled_for. No new run-records
// table — this is a computed view (the MVP the issue calls for).

import (
	"net/http"
	"sort"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// UpcomingRun is one projected future execution.
type UpcomingRun struct {
	TaskID string `json:"task_id"`
	Name   string `json:"name,omitempty"`
	// Title is the task's display label. Unlike Name it survives every
	// occurrence of a recurring task, so it is the field the timeline should
	// label a run with; Name stays for the import/export identity.
	Title      string    `json:"title,omitempty"`
	Prompt     string    `json:"prompt"`
	Recurrence string    `json:"recurrence,omitempty"`
	NextRun    time.Time `json:"next_run"`
	Recurring  bool      `json:"recurring"`
}

const (
	upcomingDefaultLimit    = 50
	upcomingLimitMax        = 1000
	upcomingPerTaskMax      = 5   // occurrences projected per recurring task (no ?until)
	upcomingWindowPerTask   = 366 // per-task safety cap when projecting to a ?until horizon
	upcomingHorizonMaxTasks = 500 // safety bound on tasks scanned
)

// GetUpcomingRuns handles GET /tasks/upcoming — the calendar/timeline feed.
// For each scheduled (recurring or one-shot) task it projects future runs and
// returns them sorted by time, capped at ?limit (default 50, max 1000).
//
// Without ?until, each recurring task contributes its next upcomingPerTaskMax
// occurrences (the original count-based view). With ?until=RFC3339 the
// projection is horizon-based: every occurrence up to that instant is emitted
// (per-task safety cap upcomingWindowPerTask), so a calendar view that passes
// its visible range shows the truth for any week it navigates to instead of
// going dark after the fifth occurrence. Both modes honor a task's recurrence
// end conditions (recurrence_until / recurrence_remaining).
func (h *Handlers) GetUpcomingRuns(w http.ResponseWriter, r *http.Request) {
	p := h.principalFromRequest(r)
	if !p.hasPermission(models.PermissionViewTasks) {
		writeError(w, http.StatusForbidden, "Insufficient permissions")
		return
	}
	limit, ok := parseLimit(w, r, upcomingDefaultLimit, upcomingLimitMax)
	if !ok {
		return
	}
	var horizon time.Time // zero = count-based projection (legacy)
	if raw := r.URL.Query().Get("until"); raw != "" {
		parsed, perr := time.Parse(time.RFC3339, raw)
		if perr != nil {
			writeError(w, http.StatusBadRequest, "until must be an RFC3339 timestamp")
			return
		}
		horizon = parsed
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
	// Own-rows visibility (#1082): the projection carries each task's prompt,
	// so it is scoped like GET /tasks. Filtered after the cap for simplicity —
	// a scoped principal on a box with >upcomingHorizonMaxTasks queued tasks
	// may see fewer of its own runs than exist, never someone else's.
	tasks = visibleTasks(p, tasks)

	now := time.Now()
	runs := make([]UpcomingRun, 0, len(tasks))
	for _, t := range tasks {
		runs = append(runs, projectRuns(t, now, horizon)...)
	}
	sort.Slice(runs, func(a, b int) bool { return runs[a].NextRun.Before(runs[b].NextRun) })
	if len(runs) > limit {
		runs = runs[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{"upcoming": runs})
}

// projectRuns computes a task's upcoming executions: cron occurrences for a
// recurring task (in its timezone) — count-based without a horizon,
// horizon-based with one — or its single scheduled_for for a one-shot. The
// task's own end conditions always apply: nothing past recurrence_until is
// projected, and a recurrence_remaining budget caps the number of occurrences
// (the scheduled row itself is the first of the remaining runs).
func projectRuns(t *models.Task, now time.Time, horizon time.Time) []UpcomingRun {
	base := UpcomingRun{TaskID: t.ID.String(), Name: t.Name, Title: t.Title, Prompt: t.Prompt, Recurrence: t.Recurrence}
	if t.Recurrence != "" {
		schedule, err := cron.ParseStandard(t.Recurrence)
		if err != nil {
			return nil
		}
		maxOcc := upcomingPerTaskMax
		if !horizon.IsZero() {
			maxOcc = upcomingWindowPerTask
		}
		if t.RecurrenceRemaining != nil && *t.RecurrenceRemaining < maxOcc {
			maxOcc = *t.RecurrenceRemaining
		}
		loc := taskLocation(t.Timezone)
		next := now.In(loc)
		// The row's scheduled_for is the authoritative next fire — the scheduler
		// promotes on it, not on a fresh cron walk from now — so a recurring
		// task postponed by an edit, a run_if skip backoff, or a reconcile must
		// project THAT instant first, then walk the cron from it. Seeding from
		// now instead showed the timeline a run the scheduler would never make.
		// A past scheduled_for (a due row the next tick will promote) falls
		// back to walking from now, as the one-shot branch below already does.
		seedFromRow := t.ScheduledFor != nil && t.ScheduledFor.After(now)
		if seedFromRow {
			next = t.ScheduledFor.In(loc)
		}
		out := make([]UpcomingRun, 0, min(maxOcc, upcomingPerTaskMax))
		for i := 0; i < maxOcc; i++ {
			if i > 0 || !seedFromRow {
				next = schedule.Next(next)
			}
			if next.IsZero() {
				break
			}
			if !horizon.IsZero() && next.After(horizon) {
				break
			}
			if t.RecurrenceUntil != nil && next.After(*t.RecurrenceUntil) {
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
