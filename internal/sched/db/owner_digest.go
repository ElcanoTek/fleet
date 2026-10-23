package db

import (
	"context"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// OwnerDigestCandidates returns the rows the daily owner digest
// (internal/ownerdigest, docs/OWNER-DIGEST.md) classifies. Only owned tasks
// (created_by set) are returned. Two sets, each capped at limit rows:
//
//  1. Per owner and job, the LATEST terminal run that finished at or after
//     since — but only when that latest run is a problem: a blocked success
//     (run_outcome 'blocked'), an error or a dead-letter. "Job" is
//     lineage_id (ADR-0069: shared by every occurrence, re-run and clone of
//     one job), falling back to the row's own id; the owner is part of the
//     key so a clone made by someone else cannot hide this owner's problem.
//     A later successful or cancelled run of the same job therefore drops
//     the earlier problem: the digest reports what is still wrong, not every
//     failure of the day. Only scheduled work counts — a recurring
//     occurrence, or a one-off with a scheduled_for — since a task run on
//     the spot is watched by the person who started it.
//  2. Cron occurrences still waiting (scheduled or pending) whose
//     scheduled_for is before overdueBefore: due, but never started.
//
// The digest re-checks every condition in Go (ownerdigest.Build), so this
// query only has to be a superset of what it reports.
func (db *Database) OwnerDigestCandidates(ctx context.Context, since, overdueBefore time.Time, limit int) ([]*models.Task, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT `+taskColumns+` FROM (
			SELECT DISTINCT ON (created_by, COALESCE(lineage_id, id::text)) *
			FROM tasks
			WHERE created_by IS NOT NULL
			  AND completed_at >= $1
			  AND status IN ($2, $3, $4, $5)
			  AND ((recurrence IS NOT NULL AND recurrence <> '') OR scheduled_for IS NOT NULL)
			ORDER BY created_by, COALESCE(lineage_id, id::text), completed_at DESC, id DESC
		) AS tasks
		WHERE (status = $2 AND run_outcome = $6) OR status IN ($3, $4)
		ORDER BY completed_at ASC, id ASC
		LIMIT $7`,
		since,
		string(models.TaskStatusSuccess),
		string(models.TaskStatusError),
		string(models.TaskStatusDeadLettered),
		string(models.TaskStatusCancelled),
		models.RunOutcomeBlocked,
		limit)
	if err != nil {
		return nil, err
	}
	latest, err := db.rowsToTasks(rows)
	_ = rows.Close()
	if err != nil {
		return nil, err
	}

	rows, err = db.conn.QueryContext(ctx, `
		SELECT `+taskColumns+` FROM tasks
		WHERE created_by IS NOT NULL
		  AND status IN ($1, $2)
		  AND trigger_type = 'cron'
		  AND scheduled_for IS NOT NULL
		  AND scheduled_for < $3
		ORDER BY scheduled_for ASC, id ASC
		LIMIT $4`,
		string(models.TaskStatusScheduled), string(models.TaskStatusPending), overdueBefore, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	overdue, err := db.rowsToTasks(rows)
	if err != nil {
		return nil, err
	}
	return append(latest, overdue...), nil
}
