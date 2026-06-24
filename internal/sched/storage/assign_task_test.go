package storage

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// AssignTaskToNode atomically leases a pending task to a node. It is the
// crash-recovery TEST SUBSTRATE: the lease/recovery tests use a node row as a
// synthetic lease owner so they can exercise RecoverExpiredLeases without the
// production claim path. It lives in a _test.go file (package storage,
// white-box) so it is compiled into the test binary only and never ships in the
// production binary — the production claim path is ClaimNextPendingTask.
func (s *Storage) AssignTaskToNode(taskID uuid.UUID, nodeID uuid.UUID) (*models.Task, error) {
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

	now := time.Now().UTC()
	if task.Status != models.TaskStatusPending {
		return nil, nil
	}

	node, err := s.db.GetNodeForUpdate(ctx, tx, nodeID)
	if err != nil {
		return nil, err
	}

	task.Status = models.TaskStatusLeased
	task.AssignedNodeID = &nodeID
	leaseOwner := nodeID.String()
	task.LeaseOwner = &leaseOwner
	expiresAt := now.Add(LeaseDuration)
	task.LeaseExpiresAt = &expiresAt

	if err := s.db.UpdateTaskTx(ctx, tx, task); err != nil {
		return nil, err
	}

	node.Status = models.NodeStatusBusy
	node.CurrentTaskID = &task.ID
	if err := s.db.UpdateNodeTx(ctx, tx, node); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return task, nil
}
