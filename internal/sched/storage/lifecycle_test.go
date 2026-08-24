package storage

// Behavioral coupling tests between the task-lifecycle table
// (internal/sched/models/task_lifecycle.go, #1127) and this package's
// transition writers — the storage-level half of the per-writer matrix (the
// db-level writers are covered in sched/db/lifecycle_test.go).
//
// Seeding here is PRODUCTION-SHAPED: a lease is seeded only on leased/running
// rows, because that is the invariant the lease-guarded writers actually rest
// on — UpdateTaskStatusAtomicWithContext, RequeueTaskForRetryWithContext and
// DeadLetterTaskWithContext refuse by LEASE POSSESSION plus a refusal list of
// terminal statuses, not by a positive status guard. Only claim ever grants a
// lease and every writer that leaves {leased, running} clears it, so in
// production those are the only lease-holding statuses; the table's edges
// encode that effective reality. (Seeding artificial leases onto paused or
// terminal rows would surface guard-level edges the system can never reach —
// e.g. the terminal refusal lists omit dead_lettered in two writers — see the
// notes on each subtest.) This makes these checks BEHAVIORAL but conditional
// on the lease invariant, which TestLifecycleLeaseInvariant pins separately.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// storageLifecycleSeed is one production-shaped seeded row: the lease owner
// is meaningful only for the lease-holding statuses; for every other status
// it is a fresh UUID that matches no lease (the writers must refuse it).
type storageLifecycleSeed struct {
	id    uuid.UUID
	owner uuid.UUID
}

// seedStorageLifecycleRow inserts one row shaped the way the system actually
// produces that status: lease on leased/running (plus StartedAt when
// running), scheduled_for on scheduled, completed stamps on the terminal
// statuses, paused_at (via direct SQL — the column is insert-excluded, #1126)
// on the two parked statuses.
func seedStorageLifecycleRow(t *testing.T, store *Storage, database *db.Database, status models.TaskStatus) storageLifecycleSeed {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	owner := uuid.New()
	task := &models.Task{
		ID:         uuid.New(),
		Prompt:     "storage lifecycle matrix " + string(status),
		Status:     status,
		Priority:   10,
		MaxRetries: 2,
		CreatedAt:  now,
	}
	switch status {
	case models.TaskStatusScheduled:
		future := now.Add(time.Hour)
		task.ScheduledFor = &future
	case models.TaskStatusLeased, models.TaskStatusRunning:
		o := owner.String()
		exp := now.Add(5 * time.Minute)
		task.LeaseOwner = &o
		task.LeaseExpiresAt = &exp
		if status == models.TaskStatusRunning {
			task.StartedAt = &now
		}
	case models.TaskStatusSuccess, models.TaskStatusError, models.TaskStatusCancelled:
		task.StartedAt = &now
		completed := now.Add(time.Minute)
		task.CompletedAt = &completed
	case models.TaskStatusDeadLettered:
		task.StartedAt = &now
		completed := now.Add(time.Minute)
		task.CompletedAt = &completed
		task.DeadLetteredAt = &completed
		task.DeadLetterAttempts = 3
	default:
		// pending needs no extra columns; the paused clocks are insert-excluded
		// and stamped by the direct UPDATE below.
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("seed %s: %v", status, err)
	}
	if status == models.TaskStatusPausedAwaitingInput || status == models.TaskStatusPausedAwaitingWake {
		if _, err := database.Conn().ExecContext(context.Background(),
			`UPDATE tasks SET paused_at = $2, wake_at = CASE WHEN $3 THEN $4::timestamptz END WHERE id = $1`,
			task.ID, now, status == models.TaskStatusPausedAwaitingWake, now.Add(time.Hour)); err != nil {
			t.Fatalf("seed pause clocks: %v", err)
		}
	}
	return storageLifecycleSeed{id: task.ID, owner: owner}
}

// TestLifecycleLeaseInvariant pins the premise the production-shaped seeding
// (and the lease-guarded writers' effective from-sets) rest on: the lifecycle
// table routes every lease-granting edge into models.ActiveTaskStatuses, and
// every edge LEAVING an active status is owned by a writer that clears or
// re-scopes the lease. Encoded as: only claim targets an active status from a
// non-active one, and the worker-report writer is the only one moving between
// the two active statuses.
func TestLifecycleLeaseInvariant(t *testing.T) {
	for _, tr := range models.TaskLifecycle {
		toActive := statusInSet(tr.To, models.ActiveTaskStatuses)
		fromActive := tr.From != models.TaskLifecycleStart && statusInSet(tr.From, models.ActiveTaskStatuses)
		if toActive && !fromActive && tr.Writer != models.TaskWriterClaim {
			t.Errorf("edge %q→%q (writer %s) grants an active status outside the claim path — the lease invariant the guards rest on would break", tr.From, tr.To, tr.Writer)
		}
		if toActive && fromActive && tr.Writer != models.TaskWriterWorkerReport {
			t.Errorf("edge %q→%q (writer %s) moves between active statuses outside the worker-report path", tr.From, tr.To, tr.Writer)
		}
	}
}

func statusInSet(s models.TaskStatus, set []models.TaskStatus) bool {
	for _, v := range set {
		if v == s {
			return true
		}
	}
	return false
}

// TestLifecycleStorageWriterMatrix drives every storage-level transition
// writer against a production-shaped row in every status and asserts the
// outcomes match the lifecycle table exactly (BEHAVIORAL: the guard that runs
// is the production row-locked transaction).
func TestLifecycleStorageWriterMatrix(t *testing.T) {
	store, database := newTestStore(t)
	ctx := context.Background()

	reset := func() {
		if _, err := database.Conn().ExecContext(ctx, "DELETE FROM tasks"); err != nil {
			t.Fatalf("reset tasks: %v", err)
		}
	}
	seedAll := func() map[models.TaskStatus]storageLifecycleSeed {
		rows := make(map[models.TaskStatus]storageLifecycleSeed, len(models.AllTaskStatuses))
		for _, s := range models.AllTaskStatuses {
			rows[s] = seedStorageLifecycleRow(t, store, database, s)
		}
		return rows
	}
	assertAll := func(writer string, target models.TaskStatus, rows map[models.TaskStatus]storageLifecycleSeed) {
		t.Helper()
		for _, seeded := range models.AllTaskStatuses {
			got, err := store.GetTask(rows[seeded].id)
			if err != nil {
				t.Fatalf("re-read %s row: %v", seeded, err)
			}
			want := seeded
			if models.TaskTransitionExists(seeded, target, writer) {
				want = target
			}
			if got.Status != want {
				t.Errorf("%s from %q: row ended %q, want %q (table edge %q→%q exists: %v)",
					writer, seeded, got.Status, want, seeded, target,
					models.TaskTransitionExists(seeded, target, writer))
			}
		}
	}

	// The runner's status reports. Non-active rows hold no lease, so the call
	// fails ErrTaskLeaseNotHeld before any status logic — which is exactly the
	// effective guard the table encodes. NOTE (current reality, preserved):
	// the in-transaction refusal list is success/error/cancelled only; it is
	// lease clearance that keeps dead_lettered and the paused states out.
	// The running→running row is a self-edge (renewal + artifact/output
	// rides), so its assertion is status-vacuous; the surrounding rows are
	// the meaningful ones.
	for _, target := range []models.TaskStatus{
		models.TaskStatusRunning, models.TaskStatusSuccess, models.TaskStatusError,
	} {
		t.Run("storage.UpdateTaskStatusAtomicWithContext →"+string(target), func(t *testing.T) {
			reset()
			rows := seedAll()
			for _, s := range models.AllTaskStatuses {
				_, err := store.UpdateTaskStatusAtomicWithContext(ctx, rows[s].id, rows[s].owner,
					&models.StatusUpdate{Status: target})
				// The lease check runs before the terminal no-op, so on
				// production-shaped rows the call succeeds exactly on the
				// table's edges (everything else holds no lease and refuses).
				edge := models.TaskTransitionExists(s, target, models.TaskWriterWorkerReport)
				if edge != (err == nil) {
					t.Errorf("report %s from %s: err=%v, but table edge exists=%v", target, s, err, edge)
				}
			}
			assertAll(models.TaskWriterWorkerReport, target, rows)
		})
	}

	t.Run("storage.RequeueTaskForRetryWithContext", func(t *testing.T) {
		reset()
		rows := seedAll()
		when := time.Now().UTC().Add(time.Minute)
		for _, s := range models.AllTaskStatuses {
			// The lease check runs first, so on production-shaped rows the
			// requeue succeeds exactly on the table's edges.
			_, err := store.RequeueTaskForRetryWithContext(ctx, rows[s].id, rows[s].owner, when, "retry backoff")
			edge := models.TaskTransitionExists(s, models.TaskStatusScheduled, models.TaskWriterRetryRequeue)
			if edge != (err == nil) {
				t.Errorf("requeue from %s: err=%v, but table edge exists=%v", s, err, edge)
			}
		}
		assertAll(models.TaskWriterRetryRequeue, models.TaskStatusScheduled, rows)
	})

	t.Run("storage.DeadLetterTaskWithContext", func(t *testing.T) {
		reset()
		rows := seedAll()
		for _, s := range models.AllTaskStatuses {
			_, err := store.DeadLetterTaskWithContext(ctx, rows[s].id, rows[s].owner, "exhausted", 3)
			edge := models.TaskTransitionExists(s, models.TaskStatusDeadLettered, models.TaskWriterRunnerDeadLetter)
			if edge != (err == nil) {
				t.Errorf("dead-letter from %s: err=%v, but table edge exists=%v", s, err, edge)
			}
		}
		assertAll(models.TaskWriterRunnerDeadLetter, models.TaskStatusDeadLettered, rows)
	})

	// Operator cancel. NOTE (current reality, preserved — see #1127 report):
	// the refusal list is success/error/cancelled, so dead_lettered→cancelled
	// IS a legal edge and the table lists it.
	t.Run("storage.CancelTaskAtomic", func(t *testing.T) {
		reset()
		rows := seedAll()
		for _, s := range models.AllTaskStatuses {
			_, err := store.CancelTaskAtomic(rows[s].id, "stopped by lifecycle test")
			edge := models.TaskTransitionExists(s, models.TaskStatusCancelled, models.TaskWriterCancel)
			if edge != (err == nil) {
				t.Errorf("cancel from %s: err=%v, but table edge exists=%v", s, err, edge)
			}
		}
		assertAll(models.TaskWriterCancel, models.TaskStatusCancelled, rows)
	})

	t.Run("storage.ReplayDeadLetteredTask", func(t *testing.T) {
		reset()
		rows := seedAll()
		for _, s := range models.AllTaskStatuses {
			_, err := store.ReplayDeadLetteredTask(ctx, rows[s].id)
			edge := models.TaskTransitionExists(s, models.TaskStatusPending, models.TaskWriterDLQReplay)
			if edge != (err == nil) {
				t.Errorf("replay from %s: err=%v, but table edge exists=%v", s, err, edge)
			}
		}
		assertAll(models.TaskWriterDLQReplay, models.TaskStatusPending, rows)
	})

	// Edits re-derive the dispatch state (models.DeriveDispatchState) under
	// the pending/scheduled editability guard: a future schedule lands
	// `scheduled`, no schedule lands `pending`.
	future := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Microsecond)
	editFor := func(target models.TaskStatus) TaskEdit {
		edit := TaskEdit{Prompt: "edited", Priority: 25, Timezone: "UTC"}
		if target == models.TaskStatusScheduled {
			edit.ScheduledFor = &future
		}
		return edit
	}
	for _, target := range []models.TaskStatus{models.TaskStatusPending, models.TaskStatusScheduled} {
		t.Run("storage.UpdateEditableTask →"+string(target), func(t *testing.T) {
			reset()
			rows := seedAll()
			for _, s := range models.AllTaskStatuses {
				_, err := store.UpdateEditableTask(ctx, rows[s].id, editFor(target))
				edge := models.TaskTransitionExists(s, target, models.TaskWriterEdit)
				if edge != (err == nil) {
					t.Errorf("edit from %s: err=%v, but table edge exists=%v", s, err, edge)
				}
			}
			assertAll(models.TaskWriterEdit, target, rows)
		})
		t.Run("storage.ReplaceTaskDefinition →"+string(target), func(t *testing.T) {
			reset()
			rows := seedAll()
			for _, s := range models.AllTaskStatuses {
				tc := models.TaskCreate{Prompt: "replaced", Priority: 25}
				if target == models.TaskStatusScheduled {
					tc.ScheduledFor = &future
				}
				_, err := store.ReplaceTaskDefinition(ctx, rows[s].id, tc)
				edge := models.TaskTransitionExists(s, target, models.TaskWriterImportReplace)
				if edge != (err == nil) {
					t.Errorf("replace from %s: err=%v, but table edge exists=%v", s, err, edge)
				}
			}
			assertAll(models.TaskWriterImportReplace, target, rows)
		})
	}
}
