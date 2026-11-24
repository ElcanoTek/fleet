package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// claimRunning adds a pending task and takes it through the production claim
// and running report, as a worker does: a running row leased by the returned
// claim token.
func claimRunning(t *testing.T, store *Storage) (*models.Task, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	task := &models.Task{ID: uuid.New(), Prompt: "finished-run fixture", Status: models.TaskStatusPending, Priority: 1, CreatedAt: time.Now().UTC(), MaxRetries: 3}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	token := uuid.New()
	claimed, err := store.ClaimNextPendingTask(ctx, token.String())
	if err != nil || claimed == nil || claimed.ID != task.ID {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
	if _, err := store.UpdateTaskStatusAtomic(task.ID, token, &models.StatusUpdate{Status: models.TaskStatusRunning}); err != nil {
		t.Fatalf("running: %v", err)
	}
	return task, token
}

func finishedRunID(t *testing.T, store *Storage, id uuid.UUID) string {
	t.Helper()
	got, err := store.GetTask(id)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.FinishedRunID == nil {
		return ""
	}
	return *got.FinishedRunID
}

// The finished stamp is fenced like every lease-guarded write: only the claim
// that holds the lease on a leased/running row can stamp it, so a run that
// lost its lease can never mark a later claim's run finished.
func TestMarkRunFinishedIsFencedOnTheLease(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	ctx := context.Background()
	task, token := claimRunning(t, store)

	if ok, err := store.MarkRunFinished(ctx, task.ID, uuid.New()); err != nil || ok {
		t.Fatalf("stamp by a claim without the lease = %v, %v; want refused", ok, err)
	}
	if got := finishedRunID(t, store, task.ID); got != "" {
		t.Fatalf("finished_run_id = %q after a refused stamp", got)
	}
	if ok, err := store.MarkRunFinished(ctx, task.ID, token); err != nil || !ok {
		t.Fatalf("stamp by the lease holder = %v, %v; want it to land", ok, err)
	}
	if got := finishedRunID(t, store, task.ID); got != token.String() {
		t.Fatalf("finished_run_id = %q, want %s", got, token)
	}
	// A lease renewal leaves the stamp alone (it is not an outcome).
	if _, err := store.UpdateTaskStatusAtomic(task.ID, token, &models.StatusUpdate{Status: models.TaskStatusRunning}); err != nil {
		t.Fatalf("renewal: %v", err)
	}
	if got := finishedRunID(t, store, task.ID); got != token.String() {
		t.Fatalf("finished_run_id = %q after a renewal, want it kept", got)
	}

	// A row that holds no lease (paused, terminal) is never stamped.
	paused, owner := claimRunning(t, store)
	if ok, err := store.PauseTaskForQuestion(ctx, paused.ID, owner, "which env?"); err != nil || !ok {
		t.Fatalf("pause: %v %v", ok, err)
	}
	if ok, err := store.MarkRunFinished(ctx, paused.ID, owner); err != nil || ok {
		t.Fatalf("stamp on a paused row = %v, %v; want refused", ok, err)
	}
}

// Every outcome write stamps the claim that wrote it, in its own statement,
// and releases the lease: the pair the runner's read-back recognises.
func TestOutcomeWritesStampTheirRun(t *testing.T) {
	t.Parallel()

	store, _ := newTestStore(t)
	ctx := context.Background()
	writes := map[string]func(id, token uuid.UUID) error{
		"success": func(id, token uuid.UUID) error {
			_, err := store.UpdateTaskStatusAtomic(id, token, &models.StatusUpdate{Status: models.TaskStatusSuccess})
			return err
		},
		"error": func(id, token uuid.UUID) error {
			_, err := store.UpdateTaskStatusAtomic(id, token, &models.StatusUpdate{Status: models.TaskStatusError})
			return err
		},
		"retry requeue": func(id, token uuid.UUID) error {
			_, err := store.RequeueTaskForRetryWithContext(ctx, id, token, time.Now().Add(time.Minute), "retry")
			return err
		},
		"infra requeue": func(id, token uuid.UUID) error {
			_, err := store.RequeueTaskForInfraRetryWithContext(ctx, id, token, time.Now().Add(time.Minute), "connector down")
			return err
		},
		"dead-letter": func(id, token uuid.UUID) error {
			_, err := store.DeadLetterTaskWithContext(ctx, id, token, "exhausted", 1)
			return err
		},
		"ask pause": func(id, token uuid.UUID) error {
			_, err := store.PauseTaskForQuestion(ctx, id, token, "which env?")
			return err
		},
		"wake park": func(id, token uuid.UUID) error {
			_, err := store.PauseTaskForWake(ctx, id, token, time.Now().Add(time.Hour), "", "later")
			return err
		},
	}
	for name, write := range writes {
		task, token := claimRunning(t, store)
		if err := write(task.ID, token); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := store.GetTask(task.ID)
		if err != nil {
			t.Fatalf("%s: GetTask: %v", name, err)
		}
		if got.FinishedRunID == nil || *got.FinishedRunID != token.String() || got.LeaseOwner != nil {
			t.Errorf("%s: finished_run_id=%v lease_owner=%v, want the writing claim's token with the lease released",
				name, got.FinishedRunID, got.LeaseOwner)
		}
	}
}

// Lease recovery never runs a finished run again: a row whose run ended
// (finished_run_id = lease_owner) is dead-lettered as an unrecorded outcome,
// whatever its retry budget. A stamp left by an EARLIER claim does not match
// the current lease, so that row is a crash and re-queues as before; a
// finished row whose lease is still live is left to its run.
func TestRecoverExpiredLeasesDeadLettersFinishedRuns(t *testing.T) {
	t.Parallel()

	store, database := newTestStore(t)
	ctx := context.Background()
	stamp := func(id uuid.UUID, token string) {
		t.Helper()
		if _, err := database.Conn().ExecContext(ctx, `UPDATE tasks SET finished_run_id = $2 WHERE id = $1`, id, token); err != nil {
			t.Fatalf("stamp: %v", err)
		}
	}

	finished := seedExpiredLease(t, store, models.TaskStatusRunning, 1, 3) // its second attempt ended
	stamp(finished.ID, *finished.LeaseOwner)
	staleStamp := seedExpiredLease(t, store, models.TaskStatusRunning, 0, 3)
	stamp(staleStamp.ID, uuid.New().String())
	live := seedExpiredLease(t, store, models.TaskStatusRunning, 0, 3)
	stamp(live.ID, *live.LeaseOwner)
	if _, err := database.Conn().ExecContext(ctx, `UPDATE tasks SET lease_expires_at = now() + interval '5 minutes' WHERE id = $1`, live.ID); err != nil {
		t.Fatalf("renew: %v", err)
	}

	requeued, err := store.RecoverExpiredLeases()
	if err != nil {
		t.Fatalf("RecoverExpiredLeases: %v", err)
	}
	if requeued != 1 {
		t.Fatalf("requeued = %d, want 1 (only the crash)", requeued)
	}

	got, err := store.GetTask(finished.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status != models.TaskStatusDeadLettered || got.AttemptCount != 1 {
		t.Fatalf("finished run: status=%s attempt_count=%d, want dead_lettered and not re-queued", got.Status, got.AttemptCount)
	}
	if got.DeadLetterReason == nil || !strings.Contains(*got.DeadLetterReason, "outcome not recorded") ||
		got.ErrorMessage == nil || *got.ErrorMessage != *got.DeadLetterReason {
		t.Errorf("finished run: reason=%v error=%v, want the unrecorded-outcome reason on both", got.DeadLetterReason, got.ErrorMessage)
	}
	if got.DeadLetterAttempts != 2 || got.DeadLetteredAt == nil || got.CompletedAt == nil || got.ActualDurationSeconds == nil {
		t.Errorf("finished run must read as a quarantined terminal row: attempts=%d dead_lettered_at=%v completed_at=%v duration=%v",
			got.DeadLetterAttempts, got.DeadLetteredAt, got.CompletedAt, got.ActualDurationSeconds)
	}
	if got.LeaseOwner != nil || got.FinishedRunID != nil {
		t.Errorf("finished run keeps lease_owner=%v finished_run_id=%v, want both cleared", got.LeaseOwner, got.FinishedRunID)
	}

	if got, _ := store.GetTask(staleStamp.ID); got.Status != models.TaskStatusPending || got.AttemptCount != 1 {
		t.Errorf("crash under a stale stamp: status=%s attempt_count=%d, want re-queued as attempt 1", got.Status, got.AttemptCount)
	}
	if got, _ := store.GetTask(live.ID); got.Status != models.TaskStatusRunning || got.LeaseOwner == nil {
		t.Errorf("finished run with a live lease: status=%s lease_owner=%v, want untouched", got.Status, got.LeaseOwner)
	}

	// Replay treats it like any other quarantined row.
	replayed, err := store.ReplayDeadLetteredTask(ctx, finished.ID)
	if err != nil || replayed.Status != models.TaskStatusPending {
		t.Fatalf("replay = %v, %v; want pending", replayed, err)
	}
}

// Lease recovery leaves alone every row leased by a token the registered
// holder (the local runner pool) reports — in all three branches, however
// expired the lease — and recovers everything else as before. Unregistered,
// the same rows are recovered.
func TestRecoverExpiredLeasesSkipsLocallyHeldRuns(t *testing.T) {
	t.Parallel()

	store, database := newTestStore(t)
	ctx := context.Background()
	requeueable := seedExpiredLease(t, store, models.TaskStatusRunning, 0, 3)
	crashLooping := seedExpiredLease(t, store, models.TaskStatusRunning, 0, 0)
	finished := seedExpiredLease(t, store, models.TaskStatusRunning, 0, 3)
	if _, err := database.Conn().ExecContext(ctx, `UPDATE tasks SET finished_run_id = lease_owner WHERE id = $1`, finished.ID); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	remote := seedExpiredLease(t, store, models.TaskStatusLeased, 0, 3)
	// A running row with no lease owner or expiry at all is recovered too: a
	// NULL lease_owner is neither "held" nor "finished by its owner".
	ownerless := seedExpiredLease(t, store, models.TaskStatusRunning, 0, 3)
	if _, err := database.Conn().ExecContext(ctx, `UPDATE tasks SET lease_owner = NULL, lease_expires_at = NULL WHERE id = $1`, ownerless.ID); err != nil {
		t.Fatalf("clear lease: %v", err)
	}

	held := []string{*requeueable.LeaseOwner, *crashLooping.LeaseOwner, *finished.LeaseOwner}
	store.SetLeaseHolder(func() []string { return held })
	requeued, err := store.RecoverExpiredLeases()
	if err != nil {
		t.Fatalf("RecoverExpiredLeases: %v", err)
	}
	if requeued != 2 {
		t.Fatalf("requeued = %d, want 2 (the rows no local run holds)", requeued)
	}
	if got, _ := store.GetTask(ownerless.ID); got.Status != models.TaskStatusPending || got.AttemptCount != 1 {
		t.Errorf("ownerless row: status=%s attempt_count=%d, want re-queued as attempt 1", got.Status, got.AttemptCount)
	}
	for _, task := range []*models.Task{requeueable, crashLooping, finished} {
		got, err := store.GetTask(task.ID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if got.Status != models.TaskStatusRunning || got.LeaseOwner == nil || *got.LeaseOwner != *task.LeaseOwner {
			t.Errorf("held row %s: status=%s lease_owner=%v, want untouched", task.ID, got.Status, got.LeaseOwner)
		}
	}
	if got, _ := store.GetTask(remote.ID); got.Status != models.TaskStatusPending {
		t.Errorf("unheld row: status=%s, want re-queued as before", got.Status)
	}

	store.SetLeaseHolder(nil)
	if requeued, err := store.RecoverExpiredLeases(); err != nil || requeued != 1 {
		t.Fatalf("unregistered RecoverExpiredLeases = %d, %v; want the requeueable row back in the queue", requeued, err)
	}
	if got, _ := store.GetTask(crashLooping.ID); got.Status != models.TaskStatusDeadLettered {
		t.Errorf("crash-looping row once released: status=%s, want dead_lettered", got.Status)
	}
	if got, _ := store.GetTask(finished.ID); got.Status != models.TaskStatusDeadLettered {
		t.Errorf("finished row once released: status=%s, want dead_lettered", got.Status)
	}
}

// The retry requeue and the dead-letter report a COMMIT that failed as
// ErrTaskCommitOutcomeUnknown, like UpdateTaskStatusAtomicWithContext, so
// the runner reads the row back before deciding what happened. A deferred
// constraint trigger makes the UPDATE succeed and only COMMIT fail.
func TestRequeueAndDeadLetterReportAFailedCommitAsOutcomeUnknown(t *testing.T) {
	t.Parallel()

	store, database := newTestStore(t)
	ctx := context.Background()
	for _, stmt := range []string{
		`CREATE FUNCTION storage_test_fail_commit() RETURNS trigger AS $$
		BEGIN
			IF NEW.status IN ('scheduled', 'dead_lettered') THEN
				RAISE EXCEPTION 'test commit failure';
			END IF;
			RETURN NEW;
		END
		$$ LANGUAGE plpgsql`,
		`CREATE CONSTRAINT TRIGGER storage_test_fail_commit AFTER UPDATE ON tasks
			DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION storage_test_fail_commit()`,
	} {
		if _, err := database.Conn().ExecContext(ctx, stmt); err != nil {
			t.Fatalf("install commit failure: %v", err)
		}
	}

	requeued, requeueToken := claimRunning(t, store)
	_, err := store.RequeueTaskForRetryWithContext(ctx, requeued.ID, requeueToken, time.Now().Add(time.Minute), "retry")
	var pgErr *pgconn.PgError
	if !errors.Is(err, ErrTaskCommitOutcomeUnknown) || !errors.As(err, &pgErr) || pgErr.Code != "P0001" {
		t.Errorf("requeue commit failure = %v, want ErrTaskCommitOutcomeUnknown wrapping the P0001 cause", err)
	}
	quarantined, deadLetterToken := claimRunning(t, store)
	_, err = store.DeadLetterTaskWithContext(ctx, quarantined.ID, deadLetterToken, "exhausted", 1)
	if !errors.Is(err, ErrTaskCommitOutcomeUnknown) || !errors.As(err, &pgErr) || pgErr.Code != "P0001" {
		t.Errorf("dead-letter commit failure = %v, want ErrTaskCommitOutcomeUnknown wrapping the P0001 cause", err)
	}
	// Both transactions rolled back: the rows are still the claims' own.
	for _, id := range []uuid.UUID{requeued.ID, quarantined.ID} {
		if got, _ := store.GetTask(id); got.Status != models.TaskStatusRunning || got.LeaseOwner == nil {
			t.Errorf("row %s after the failed commit: status=%s lease_owner=%v, want running under its lease", id, got.Status, got.LeaseOwner)
		}
	}
}

// ClearRunFinished is fenced like the stamp: it clears a stamp only for the
// claim that holds the lease AND whose own token the stamp is. A different
// token, a row without the lease, and another run's stamp are all left alone.
func TestClearRunFinishedIsFencedOnTheLeaseAndItsOwnStamp(t *testing.T) {
	t.Parallel()

	store, database := newTestStore(t)
	ctx := context.Background()
	stamp := func(id uuid.UUID, token string) {
		t.Helper()
		if _, err := database.Conn().ExecContext(ctx, `UPDATE tasks SET finished_run_id = $2 WHERE id = $1`, id, token); err != nil {
			t.Fatalf("stamp: %v", err)
		}
	}

	// Its own stamp, under its own lease: cleared.
	own, ownToken := claimRunning(t, store)
	if ok, err := store.MarkRunFinished(ctx, own.ID, ownToken); err != nil || !ok {
		t.Fatalf("stamp: %v %v", ok, err)
	}
	if ok, err := store.ClearRunFinished(ctx, own.ID, uuid.New()); err != nil || ok {
		t.Fatalf("clear by a claim without the lease = %v, %v; want refused", ok, err)
	}
	if got := finishedRunID(t, store, own.ID); got != ownToken.String() {
		t.Fatalf("finished_run_id = %q after a refused clear, want %s", got, ownToken)
	}
	if ok, err := store.ClearRunFinished(ctx, own.ID, ownToken); err != nil || !ok {
		t.Fatalf("clear by the lease holder of its own stamp = %v, %v; want it to land", ok, err)
	}
	if got := finishedRunID(t, store, own.ID); got != "" {
		t.Fatalf("finished_run_id = %q after the clear, want none", got)
	}

	// Another run's stamp under this claim's lease: kept.
	other, otherToken := claimRunning(t, store)
	earlier := uuid.New().String()
	stamp(other.ID, earlier)
	if ok, err := store.ClearRunFinished(ctx, other.ID, otherToken); err != nil || ok {
		t.Fatalf("clear of another run's stamp = %v, %v; want refused", ok, err)
	}
	if got := finishedRunID(t, store, other.ID); got != earlier {
		t.Fatalf("finished_run_id = %q, want the other run's %s kept", got, earlier)
	}

	// A row that no longer holds the lease (its outcome released it): kept.
	parked, parkedToken := claimRunning(t, store)
	if ok, err := store.PauseTaskForWake(ctx, parked.ID, parkedToken, time.Now().Add(time.Hour), "", "later"); err != nil || !ok {
		t.Fatalf("park: %v %v", ok, err)
	}
	if ok, err := store.ClearRunFinished(ctx, parked.ID, parkedToken); err != nil || ok {
		t.Fatalf("clear on a row without the lease = %v, %v; want refused", ok, err)
	}
	if got := finishedRunID(t, store, parked.ID); got != parkedToken.String() {
		t.Fatalf("finished_run_id = %q, want the park's stamp kept", got)
	}
}
