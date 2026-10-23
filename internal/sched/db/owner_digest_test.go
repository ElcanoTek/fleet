package db

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestOwnerDigestCandidates pins what the daily owner digest is handed: per
// owner and job, the latest terminal run in the window when it is a problem
// (blocked / error / dead-lettered), and cron occurrences still waiting past
// the overdue cutoff. Everything else — a job whose latest run succeeded or
// was cancelled, unscheduled one-offs, runs outside the window, ownerless
// rows, webhook templates, occurrences not yet overdue — stays out.
func TestOwnerDigestCandidates(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	since := now.Add(-24 * time.Hour)
	overdueBefore := now.Add(-2 * time.Hour)
	ownerA, ownerB := uuid.New(), uuid.New()

	type row struct {
		name        string
		owner       *uuid.UUID
		lineage     string
		status      models.TaskStatus
		outcome     string
		recurring   bool
		scheduled   *time.Time
		completed   *time.Time
		triggerType string
	}
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	rows := []row{
		// J1: dead-lettered, then a later success of the same job → healthy.
		{name: "j1-dead", owner: &ownerA, lineage: "j1", status: models.TaskStatusDeadLettered, recurring: true, completed: at(-20 * time.Hour)},
		{name: "j1-ok", owner: &ownerA, lineage: "j1", status: models.TaskStatusSuccess, recurring: true, completed: at(-2 * time.Hour)},
		// J2: ok, then blocked → reported (the blocked run).
		{name: "j2-ok", owner: &ownerA, lineage: "j2", status: models.TaskStatusSuccess, recurring: true, completed: at(-20 * time.Hour)},
		{name: "j2-blocked", owner: &ownerA, lineage: "j2", status: models.TaskStatusSuccess, outcome: models.RunOutcomeBlocked, recurring: true, completed: at(-1 * time.Hour)},
		// Owner B's clone of J2 succeeded later; it must not hide A's problem.
		{name: "j2-clone-b", owner: &ownerB, lineage: "j2", status: models.TaskStatusSuccess, recurring: true, completed: at(-10 * time.Minute)},
		// J3: dead-lettered recurring → reported.
		{name: "j3-dead", owner: &ownerA, lineage: "j3", status: models.TaskStatusDeadLettered, recurring: true, completed: at(-3 * time.Hour)},
		// J4: a scheduled one-off that errored → reported.
		{name: "j4-error", owner: &ownerA, lineage: "j4", status: models.TaskStatusError, scheduled: at(-6 * time.Hour), completed: at(-5 * time.Hour)},
		// J5: run on the spot (no schedule) → not the digest's business.
		{name: "j5-adhoc", owner: &ownerA, lineage: "j5", status: models.TaskStatusDeadLettered, completed: at(-2 * time.Hour)},
		// J6: outside the window.
		{name: "j6-old", owner: &ownerA, lineage: "j6", status: models.TaskStatusDeadLettered, recurring: true, completed: at(-30 * time.Hour)},
		// J7: failed, then the owner cancelled the next occurrence → not reported.
		{name: "j7-dead", owner: &ownerA, lineage: "j7", status: models.TaskStatusDeadLettered, recurring: true, completed: at(-8 * time.Hour)},
		{name: "j7-cancel", owner: &ownerA, lineage: "j7", status: models.TaskStatusCancelled, recurring: true, completed: at(-7 * time.Hour)},
		// Ownerless failure.
		{name: "j8-nobody", lineage: "j8", status: models.TaskStatusDeadLettered, recurring: true, completed: at(-2 * time.Hour)},
		// Overdue occurrences.
		{name: "o1-late", owner: &ownerA, lineage: "o1", status: models.TaskStatusScheduled, recurring: true, scheduled: at(-3 * time.Hour)},
		{name: "o2-pending", owner: &ownerB, lineage: "o2", status: models.TaskStatusPending, recurring: true, scheduled: at(-4 * time.Hour)},
		{name: "o3-not-yet", owner: &ownerA, lineage: "o3", status: models.TaskStatusScheduled, recurring: true, scheduled: at(-1 * time.Hour)},
		{name: "o4-webhook", owner: &ownerA, lineage: "o4", status: models.TaskStatusScheduled, scheduled: at(-5 * time.Hour), triggerType: "webhook"},
		{name: "o5-nobody", lineage: "o5", status: models.TaskStatusScheduled, recurring: true, scheduled: at(-5 * time.Hour)},
		{name: "o6-running", owner: &ownerA, lineage: "o6", status: models.TaskStatusRunning, recurring: true, scheduled: at(-5 * time.Hour)},
	}
	for _, r := range rows {
		task := &models.Task{ID: uuid.New(), Title: r.name, Prompt: r.name, Status: r.status, Priority: 1, Timezone: "UTC", CreatedAt: now.Add(-48 * time.Hour), CreatedBy: r.owner, ScheduledFor: r.scheduled}
		if r.recurring {
			task.Recurrence = "@daily"
		}
		if err := db.AddTask(ctx, task); err != nil {
			t.Fatalf("seed %s: %v", r.name, err)
		}
		var outcome any
		if r.outcome != "" {
			outcome = r.outcome
		}
		trigger := r.triggerType
		if trigger == "" {
			trigger = "cron"
		}
		if _, err := db.conn.ExecContext(ctx,
			`UPDATE tasks SET lineage_id = $2, completed_at = $3, run_outcome = $4, trigger_type = $5, status = $6 WHERE id = $1`,
			task.ID, r.lineage, r.completed, outcome, trigger, string(r.status)); err != nil {
			t.Fatalf("stamp %s: %v", r.name, err)
		}
	}

	got, err := db.OwnerDigestCandidates(ctx, since, overdueBefore, 100)
	if err != nil {
		t.Fatalf("OwnerDigestCandidates: %v", err)
	}
	names := make([]string, 0, len(got))
	for _, task := range got {
		names = append(names, task.Title)
	}
	sort.Strings(names)
	want := []string{"j2-blocked", "j3-dead", "j4-error", "o1-late", "o2-pending"}
	if len(names) != len(want) {
		t.Fatalf("candidates = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("candidates = %v, want %v", names, want)
		}
	}

	// The limit caps each set.
	capped, err := db.OwnerDigestCandidates(ctx, since, overdueBefore, 1)
	if err != nil {
		t.Fatalf("capped: %v", err)
	}
	if len(capped) != 2 {
		t.Fatalf("limit 1 returned %d rows, want one per set", len(capped))
	}
}
