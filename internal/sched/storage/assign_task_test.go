package storage

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// leaseTaskToOwner atomically leases a pending task to a synthetic lease owner.
// It is the crash-recovery TEST SUBSTRATE: the lease/recovery tests need a task
// in the leased state so they can exercise RecoverExpiredLeases without the
// production claim path. It mirrors what ClaimNextPendingTask does — set
// status=leased, stamp lease_owner, set lease_expires_at — but takes the owner
// id explicitly so a test can drive expiry/ownership directly.
//
// It lives in a _test.go file (package storage, white-box) so it compiles into
// the test binary only and never ships. The worker-node registry was removed
// (#459), so the lease is keyed purely on lease_owner; there is no node row.
func (s *Storage) leaseTaskToOwner(taskID uuid.UUID, owner uuid.UUID) (*models.Task, error) {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx)
	if err != nil {
		return nil, err
	}
	// Rollback is a no-op after a successful Commit (returns sql.ErrTxDone); on
	// the error paths the function already returns the underlying error, and a
	// rollback failure in a defer can't be surfaced — so the result is
	// intentionally ignored.
	defer func() { _ = tx.Rollback() }()

	task, err := s.db.GetTaskForUpdate(ctx, tx, taskID)
	if err != nil {
		return nil, err
	}

	if task.Status != models.TaskStatusPending {
		return nil, nil
	}

	now := time.Now().UTC()
	task.Status = models.TaskStatusLeased
	leaseOwner := owner.String()
	task.LeaseOwner = &leaseOwner
	expiresAt := now.Add(LeaseDuration)
	task.LeaseExpiresAt = &expiresAt

	if err := s.db.UpdateTaskTx(ctx, tx, task); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return task, nil
}
