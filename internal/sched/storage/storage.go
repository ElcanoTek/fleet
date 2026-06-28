// Package storage provides the storage layer for nodes and tasks (sched).
// Ported from moc's internal/storage. Concurrency is handled by PostgreSQL
// through transactions and row-level locking. The node-glob routing
// (NodeMatchesTask / GetPendingTaskForNode) is dropped: on one box there is a
// single synthetic in-box worker, and task claiming goes through
// db.ClaimNextPendingTask (FOR UPDATE SKIP LOCKED), not node matching. The
// MatchGlob helper survives for scoped-visibility checks in the handlers.
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"

	"github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// ErrTaskNotEditable is returned by UpdateEditableTask when a task left the
// editable state (pending or scheduled) between the caller's read and the
// locked write.
var ErrTaskNotEditable = errors.New("task is no longer editable")

// MatchGlob implements simple glob matching similar to Python's fnmatch. It
// survives the node-routing removal because scoped API keys / users still match
// node-name scope patterns for visibility checks.
func MatchGlob(pattern, name string) bool {
	return match(pattern, name)
}

func match(pattern, name string) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			for len(pattern) > 0 && pattern[0] == '*' {
				pattern = pattern[1:]
			}
			if len(pattern) == 0 {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if match(pattern, name[i:]) {
					return true
				}
			}
			return false
		case '?':
			if len(name) == 0 {
				return false
			}
			pattern = pattern[1:]
			name = name[1:]
		default:
			if len(name) == 0 || pattern[0] != name[0] {
				return false
			}
			pattern = pattern[1:]
			name = name[1:]
		}
	}
	return len(name) == 0
}

// Storage provides persistent storage for nodes and tasks.
type Storage struct {
	db       *db.Database
	location *time.Location
}

// New creates a new Storage instance.
func New() *Storage {
	return &Storage{
		db:       db.New(),
		location: time.UTC,
	}
}

// SetDatabase injects a Database (used by tests to share an initialized conn).
func (s *Storage) SetDatabase(database *db.Database) {
	s.db = database
}

// SetTimezone sets the timezone for scheduling operations.
func (s *Storage) SetTimezone(timezone string) {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		log.Printf("Warning: Invalid timezone '%s', using UTC: %v", timezone, err)
		s.location = time.UTC
		return
	}
	s.location = loc
}

// Location returns the configured timezone location for scheduling.
func (s *Storage) Location() *time.Location { return s.location }

// Initialize initializes the persistent storage.
func (s *Storage) Initialize(dbPath string) error { return s.db.Init(dbPath) }

// Close closes the storage.
func (s *Storage) Close() error { return s.db.Close() }

// DB returns the underlying database for advanced operations.
func (s *Storage) DB() *db.Database { return s.db }

// LeaseDuration is the fixed lease window for a claimed task. Kept identical to
// moc's value so the lease/recovery tests carry over unchanged. Intentionally a
// hardcoded constant rather than an operator knob: on a single-box, vertically
// scaled deployment a configurable lease window earns nothing, and the runner's
// renew ticker already keeps the lease fresh well inside this window. Revisit
// only if long-running scheduled agents ever need a wider window.
const LeaseDuration = 5 * time.Minute

// Node operations

// AddNode adds a new node to the registry.
func (s *Storage) AddNode(node *models.Node) (*models.Node, error) {
	return s.AddNodeWithContext(context.Background(), node)
}

// AddNodeWithContext adds a new node with context.
func (s *Storage) AddNodeWithContext(ctx context.Context, node *models.Node) (*models.Node, error) {
	nodeToStore := *node
	nodeToStore.APIKey = models.HashTokenIfNeeded(node.APIKey)
	if err := s.db.AddNode(ctx, &nodeToStore); err != nil {
		return nil, err
	}
	return node, nil
}

// GetNode gets a node by ID.
func (s *Storage) GetNode(nodeID uuid.UUID) (*models.Node, error) {
	return s.db.GetNode(context.Background(), nodeID)
}

// GetNodeByAPIKey gets a node by its API key.
func (s *Storage) GetNodeByAPIKey(apiKey string) (*models.Node, error) {
	return s.db.GetNodeByAPIKey(context.Background(), apiKey)
}

// GetAllNodes gets all registered nodes.
func (s *Storage) GetAllNodes() ([]*models.Node, error) {
	return s.db.GetAllNodes(context.Background())
}

// GetAllNodesPaginated gets nodes with pagination.
func (s *Storage) GetAllNodesPaginated(limit, offset int) ([]*models.Node, int, error) {
	return s.db.GetAllNodesPaginated(context.Background(), limit, offset)
}

// GetNodesScopedPaginated gets nodes with pagination filtering by scopes.
func (s *Storage) GetNodesScopedPaginated(limit, offset int, scopes []string) ([]*models.Node, int, error) {
	return s.db.GetNodesScopedPaginated(context.Background(), limit, offset, scopes)
}

// GetNodeNamesByIDs gets node names for a list of node IDs.
func (s *Storage) GetNodeNamesByIDs(nodeIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	return s.db.GetNodeNamesByIDs(context.Background(), nodeIDs)
}

// UpdateNode updates an existing node.
func (s *Storage) UpdateNode(node *models.Node) (*models.Node, error) {
	nodeToStore := *node
	nodeToStore.APIKey = models.HashTokenIfNeeded(node.APIKey)
	if err := s.db.UpdateNode(context.Background(), &nodeToStore); err != nil {
		return nil, err
	}
	return node, nil
}

// RemoveNode removes a node from the registry.
func (s *Storage) RemoveNode(nodeID uuid.UUID) (bool, error) {
	return s.db.RemoveNode(context.Background(), nodeID)
}

// UpdateNodeHeartbeat updates a node's heartbeat timestamp and status, and
// (for a busy heartbeat) renews the lease on its active task — preserved
// verbatim so the in-process worker's lease-renew ticker can reuse it.
func (s *Storage) UpdateNodeHeartbeat(nodeID uuid.UUID, status models.NodeStatus, taskID *uuid.UUID) (*models.Node, error) {
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

	now := time.Now().UTC()

	// Lock order is ALWAYS task-then-node.
	var taskToRenew *models.Task
	if status == models.NodeStatusBusy && taskID != nil {
		task, err := s.db.GetTaskForUpdate(ctx, tx, *taskID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if task != nil {
			hasValidLease := task.LeaseOwner != nil && *task.LeaseOwner == nodeID.String()
			isAssigned := task.AssignedNodeID != nil && *task.AssignedNodeID == nodeID
			isRenewableStatus := task.Status == models.TaskStatusLeased ||
				task.Status == models.TaskStatusRunning ||
				task.Status == models.TaskStatusAnalyzing
			if (hasValidLease || isAssigned) && isRenewableStatus {
				expiresAt := now.Add(LeaseDuration)
				task.LeaseExpiresAt = &expiresAt
				taskToRenew = task
			}
		}
	}

	node, err := s.db.GetNodeForUpdate(ctx, tx, nodeID)
	if err != nil {
		return nil, err
	}

	node.LastHeartbeat = now
	node.Status = status
	node.CurrentTaskID = taskID
	if err := s.db.UpdateNodeTx(ctx, tx, node); err != nil {
		return nil, err
	}

	if taskToRenew != nil {
		if err := s.db.UpdateTaskTx(ctx, tx, taskToRenew); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return node, nil
}

// Task operations

// AddTask adds a new task.
func (s *Storage) AddTask(task *models.Task) (*models.Task, error) {
	return s.AddTaskWithContext(context.Background(), task)
}

// AddTaskWithContext adds a new task with context.
func (s *Storage) AddTaskWithContext(ctx context.Context, task *models.Task) (*models.Task, error) {
	if err := s.db.AddTask(ctx, task); err != nil {
		return nil, err
	}
	return task, nil
}

// GetTask gets a task by ID.
func (s *Storage) GetTask(taskID uuid.UUID) (*models.Task, error) {
	return s.db.GetTask(context.Background(), taskID)
}

// AddTaskIteration upserts a per-iteration telemetry row for a looped task (#179).
func (s *Storage) AddTaskIteration(ctx context.Context, it *models.TaskIteration) error {
	return s.db.AddTaskIteration(ctx, it)
}

// ListTaskIterations returns a task's iterations in order.
func (s *Storage) ListTaskIterations(ctx context.Context, taskID uuid.UUID) ([]*models.TaskIteration, error) {
	return s.db.ListTaskIterations(ctx, taskID)
}

// GetAllTasks gets all tasks.
func (s *Storage) GetAllTasks() ([]*models.Task, error) {
	return s.db.GetAllTasks(context.Background())
}

// GetAllTasksPaginated gets tasks with pagination.
func (s *Storage) GetAllTasksPaginated(limit, offset int) ([]*models.Task, int, error) {
	return s.db.GetAllTasksPaginated(context.Background(), limit, offset)
}

// GetTasksFiltered gets tasks with optional filters and pagination.
func (s *Storage) GetTasksFiltered(filter db.TaskFilter, limit, offset int) ([]*models.Task, int, error) {
	return s.db.GetTasksFiltered(context.Background(), filter, limit, offset)
}

// GetUsersByIDs gets users by a list of IDs.
func (s *Storage) GetUsersByIDs(userIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	return s.db.GetUsersByIDs(context.Background(), userIDs)
}

// GetUsersByIDsWithContext gets users by a list of IDs with context.
func (s *Storage) GetUsersByIDsWithContext(ctx context.Context, userIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	return s.db.GetUsersByIDs(ctx, userIDs)
}

// UpdateTask updates an existing task.
func (s *Storage) UpdateTask(task *models.Task) (*models.Task, error) {
	if err := s.db.UpdateTask(context.Background(), task); err != nil {
		return nil, err
	}
	return task, nil
}

// BulkUpdateScheduledTaskModel updates model + fallback_model on scheduled tasks.
func (s *Storage) BulkUpdateScheduledTaskModel(ctx context.Context, model, fallbackModel, fromModel string) (int, error) {
	return s.db.UpdateTasksModelBatch(ctx, model, fallbackModel, fromModel)
}

// ListScheduledTasks returns all scheduled tasks.
func (s *Storage) ListScheduledTasks(ctx context.Context) ([]*models.Task, error) {
	return s.db.GetAllScheduledTasks(ctx)
}

// UpdateTasksStatusBatch transitions multiple tasks from fromStatus to toStatus.
func (s *Storage) UpdateTasksStatusBatch(taskIDs []uuid.UUID, fromStatus, toStatus models.TaskStatus) (int, error) {
	return s.db.UpdateTasksStatusBatch(context.Background(), taskIDs, fromStatus, toStatus)
}

// GetPendingTasks gets all pending tasks, sorted by priority.
func (s *Storage) GetPendingTasks() ([]*models.Task, error) {
	return s.db.GetPendingTasks(context.Background())
}

// ClaimNextPendingTask atomically claims and leases the next pending task for
// the given synthetic lease owner (FOR UPDATE SKIP LOCKED).
func (s *Storage) ClaimNextPendingTask(ctx context.Context, leaseOwner string) (*models.Task, error) {
	return s.db.ClaimNextPendingTask(ctx, leaseOwner, LeaseDuration)
}

// GetNodeByName gets a node by its name.
func (s *Storage) GetNodeByName(name string) (*models.Node, error) {
	return s.db.GetNodeByName(context.Background(), name)
}

// RecoverExpiredLeases resets tasks with expired leases back to pending.
func (s *Storage) RecoverExpiredLeases() (int, error) {
	return s.RecoverExpiredLeasesWithContext(context.Background())
}

// RecoverExpiredLeasesWithContext resets tasks with expired leases with context.
func (s *Storage) RecoverExpiredLeasesWithContext(ctx context.Context) (int, error) {
	now := time.Now().UTC()
	return s.db.RecoverExpiredLeases(ctx, now)
}

// GetRunningTasks gets all currently running tasks.
func (s *Storage) GetRunningTasks() ([]*models.Task, error) {
	return s.db.GetRunningTasks(context.Background())
}

// GetTasksByStatus gets all tasks with a specific status.
func (s *Storage) GetTasksByStatus(status models.TaskStatus) ([]*models.Task, error) {
	return s.db.GetTasksByStatus(context.Background(), status)
}

// GetTasksCompletedToday gets tasks completed today.
func (s *Storage) GetTasksCompletedToday() ([]*models.Task, error) {
	return s.db.GetTasksCompletedToday(context.Background())
}

// GetDashboardStats gets statistics for the dashboard.
func (s *Storage) GetDashboardStats() (*models.DashboardStats, error) {
	return s.db.GetDashboardStats(context.Background())
}

// GetDashboardStatsForUser gets statistics scoped to a user's permissions.
func (s *Storage) GetDashboardStatsForUser(userID *uuid.UUID, scopes []string) (*models.DashboardStats, error) {
	return s.db.GetDashboardStatsForUser(context.Background(), userID, scopes)
}

// Log operations

// AddLog stores a log session for a task.
func (s *Storage) AddLog(taskID uuid.UUID, session *models.LogSession) (*models.LogSession, error) {
	return s.AddLogWithContext(context.Background(), taskID, session)
}

// AddLogWithContext stores a log session for a task with context.
func (s *Storage) AddLogWithContext(ctx context.Context, taskID uuid.UUID, session *models.LogSession) (*models.LogSession, error) {
	if err := s.db.AddLog(ctx, taskID, session); err != nil {
		return nil, err
	}
	return session, nil
}

// GetLog gets the log session for a task.
func (s *Storage) GetLog(taskID uuid.UUID) (*models.LogSession, error) {
	return s.db.GetLog(context.Background(), taskID)
}

// GetAllLogs gets all stored log sessions.
func (s *Storage) GetAllLogs() (map[uuid.UUID]*models.LogSession, error) {
	return s.db.GetAllLogs(context.Background())
}

// Cleanup operations

// CleanupHistory deletes tasks and logs older than days.
func (s *Storage) CleanupHistory(days int) (int, error) {
	return s.db.DeleteOldHistory(context.Background(), days)
}

// User operations

// AddUser adds a new user.
func (s *Storage) AddUser(user *models.User) (*models.User, error) {
	if err := s.db.AddUser(context.Background(), user); err != nil {
		return nil, err
	}
	return user, nil
}

// UpdateUserRole changes an existing user's role.
func (s *Storage) UpdateUserRole(userID uuid.UUID, role string) error {
	return s.db.UpdateUserRole(context.Background(), userID, role)
}

// RenameUser changes an existing user's username.
func (s *Storage) RenameUser(userID uuid.UUID, newUsername string) error {
	return s.db.RenameUser(context.Background(), userID, newUsername)
}

// DeleteUser removes a user by ID.
func (s *Storage) DeleteUser(userID uuid.UUID) error {
	return s.db.DeleteUser(context.Background(), userID)
}

// GetUser gets a user by ID.
func (s *Storage) GetUser(userID uuid.UUID) (*models.User, error) {
	return s.db.GetUser(context.Background(), userID)
}

// ListUsers returns all users ordered by username.
func (s *Storage) ListUsers(ctx context.Context) ([]models.User, error) {
	return s.db.ListUsers(ctx)
}

// CountUsers returns the number of provisioned users (0-users guard).
func (s *Storage) CountUsers(ctx context.Context) (int, error) {
	return s.db.CountUsers(ctx)
}

// GetUserByUsername gets a user by username.
func (s *Storage) GetUserByUsername(username string) (*models.User, error) {
	return s.db.GetUserByUsername(context.Background(), username)
}

// GetUserByUsernameWithContext gets a user by username with context.
func (s *Storage) GetUserByUsernameWithContext(ctx context.Context, username string) (*models.User, error) {
	return s.db.GetUserByUsername(ctx, username)
}

// GetUserByToken gets a user by session token.
func (s *Storage) GetUserByToken(token string) (*models.User, error) {
	return s.db.GetUserByToken(context.Background(), token)
}

// CanNodeAccessFile checks if a node is assigned to a task containing filename.
func (s *Storage) CanNodeAccessFile(nodeID uuid.UUID, filename string) (bool, error) {
	return s.CanNodeAccessFileWithContext(context.Background(), nodeID, filename)
}

// CanNodeAccessFileWithContext checks file access with context.
func (s *Storage) CanNodeAccessFileWithContext(ctx context.Context, nodeID uuid.UUID, filename string) (bool, error) {
	return s.db.CanNodeAccessFile(ctx, nodeID, filename)
}

// GetScheduledTasks gets scheduled tasks ready to run up to a limit.
func (s *Storage) GetScheduledTasks(cutoff time.Time, limit int) ([]*models.Task, error) {
	return s.db.GetScheduledTasks(context.Background(), cutoff, limit)
}

// CancelTaskAtomic cancels a task atomically.
func (s *Storage) CancelTaskAtomic(taskID uuid.UUID) (*models.Task, error) {
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

	if task.Status == models.TaskStatusSuccess ||
		task.Status == models.TaskStatusError ||
		task.Status == models.TaskStatusCancelled {
		return nil, fmt.Errorf("cannot cancel task with status: %s", task.Status)
	}

	now := time.Now().UTC()
	task.Status = models.TaskStatusCancelled
	task.CompletedAt = &now
	task.LeaseOwner = nil
	task.LeaseExpiresAt = nil

	if err := s.db.UpdateTaskTx(ctx, tx, task); err != nil {
		return nil, err
	}

	if task.AssignedNodeID != nil {
		node, err := s.db.GetNodeForUpdate(ctx, tx, *task.AssignedNodeID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if node != nil {
			node.Status = models.NodeStatusIdle
			node.CurrentTaskID = nil
			if err := s.db.UpdateNodeTx(ctx, tx, node); err != nil {
				return nil, err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return task, nil
}

// TaskEdit carries the user-editable fields of a task.
type TaskEdit struct {
	Prompt                 string
	Model                  *string
	FallbackModel          *string
	MaxIterations          *int
	MCPSelection           models.MCPSelection
	Priority               int
	InstructionSelfImprove bool
	AllowNetwork           bool
	// Description replaces the task's operator documentation (#281). Like Prompt,
	// it is assigned unconditionally from the full edit payload (empty = clear).
	Description  string
	ScheduledFor *time.Time
	Recurrence   string
	// Timezone is the IANA timezone the cron Recurrence is evaluated in. The edit
	// handler pre-fills it from the existing task when the caller omits it, so it
	// is always a valid name here.
	Timezone string
	Files    []string
	// SetFiles distinguishes "leave files unchanged" from "replace with Files".
	SetFiles bool
	// Tags + SetTags mirror Files/SetFiles: the flag distinguishes "leave tags
	// unchanged" from "replace with Tags" (#212). Tags here are already
	// normalized/validated by the handler.
	Tags    []string
	SetTags bool
	// SetMCPSelection distinguishes "leave mcp_selection unchanged" from "replace".
	SetMCPSelection bool
	// CredentialAllowlist + SetCredentialAllowlist mirror the MCPSelection pair:
	// the flag distinguishes "leave unchanged" from "replace" (including replacing
	// with nil to revert to global inherit).
	CredentialAllowlist    models.CredentialAllowlist
	SetCredentialAllowlist bool
	// LoopConfig + SetLoopConfig mirror the same pattern: the flag distinguishes
	// "leave unchanged" from "replace" (including replacing with nil to disable
	// the loop).
	LoopConfig    *models.LoopConfig
	SetLoopConfig bool
	// WorktreeConfig + SetWorktreeConfig mirror the same pattern: the flag
	// distinguishes "leave unchanged" from "replace" (including replacing with
	// nil to disable worktree isolation).
	WorktreeConfig    *models.WorktreeConfig
	SetWorktreeConfig bool
	// RetryPolicy + SetRetryPolicy mirror the same pattern (#201): the flag
	// distinguishes "leave unchanged" from "replace" (including nil to revert to
	// the legacy retry policy).
	RetryPolicy    *models.RetryPolicy
	SetRetryPolicy bool
}

// UpdateEditableTask applies an edit to a task inside a transaction, re-locking
// the row and re-checking it is still editable. Status is recomputed from
// ScheduledFor. Returns ErrTaskNotEditable if no longer editable.
func (s *Storage) UpdateEditableTask(ctx context.Context, taskID uuid.UUID, edit TaskEdit) (*models.Task, error) {
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

	if task.Status != models.TaskStatusPending && task.Status != models.TaskStatusScheduled {
		return nil, ErrTaskNotEditable
	}

	task.Prompt = edit.Prompt
	task.Description = edit.Description
	task.Model = edit.Model
	task.FallbackModel = edit.FallbackModel
	task.MaxIterations = edit.MaxIterations
	if edit.SetMCPSelection {
		task.MCPSelection = edit.MCPSelection
	}
	if edit.SetCredentialAllowlist {
		task.CredentialAllowlist = edit.CredentialAllowlist
	}
	if edit.SetLoopConfig {
		task.LoopConfig = edit.LoopConfig
	}
	if edit.SetWorktreeConfig {
		task.WorktreeConfig = edit.WorktreeConfig
	}
	if edit.SetRetryPolicy {
		task.RetryPolicy = edit.RetryPolicy
	}
	task.Priority = edit.Priority
	task.InstructionSelfImprove = edit.InstructionSelfImprove
	task.AllowNetwork = edit.AllowNetwork
	task.ScheduledFor = edit.ScheduledFor
	task.Recurrence = edit.Recurrence
	if edit.Timezone != "" {
		task.Timezone = edit.Timezone
	}
	if edit.SetFiles {
		task.Files = edit.Files
	}
	if edit.SetTags {
		task.Tags = edit.Tags
	}

	if task.ScheduledFor != nil && task.ScheduledFor.After(time.Now().UTC()) {
		task.Status = models.TaskStatusScheduled
	} else {
		task.Status = models.TaskStatusPending
	}

	if err := s.db.UpdateTaskTx(ctx, tx, task); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return task, nil
}

// UpdateTaskCredentialAllowlist replaces a task's credential allowlist (#184),
// re-locking the row and re-checking it is still editable (pending/scheduled).
// A nil allowlist reverts the task to global inherit; a non-nil (possibly empty)
// list enforces least-privilege scoping. Returns ErrTaskNotEditable if the task
// has left the editable state. Only this field is touched.
func (s *Storage) UpdateTaskCredentialAllowlist(ctx context.Context, taskID uuid.UUID, allowlist models.CredentialAllowlist) (*models.Task, error) {
	tx, err := s.db.BeginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	task, err := s.db.GetTaskForUpdate(ctx, tx, taskID)
	if err != nil {
		return nil, err
	}
	if task.Status != models.TaskStatusPending && task.Status != models.TaskStatusScheduled {
		return nil, ErrTaskNotEditable
	}

	task.CredentialAllowlist = allowlist
	if err := s.db.UpdateTaskTx(ctx, tx, task); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return task, nil
}

// UpdateTaskDescription sets a task's operator-documentation field (#281),
// leaving all other fields untouched. Editable tasks only (pending/scheduled),
// mirroring the other targeted task edits.
func (s *Storage) UpdateTaskDescription(ctx context.Context, taskID uuid.UUID, description string) (*models.Task, error) {
	tx, err := s.db.BeginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	task, err := s.db.GetTaskForUpdate(ctx, tx, taskID)
	if err != nil {
		return nil, err
	}
	if task.Status != models.TaskStatusPending && task.Status != models.TaskStatusScheduled {
		return nil, ErrTaskNotEditable
	}

	task.Description = description
	if err := s.db.UpdateTaskTx(ctx, tx, task); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return task, nil
}

// UpdateTaskTags atomically adds and removes tags on a task (#212), leaving all
// other fields untouched. It runs under a row lock (GetTaskForUpdate) so
// concurrent tag edits don't lose updates. `add` and `remove` are already
// normalized by the caller; removal wins over add for any tag in both. The
// resulting set is re-validated (count/format) before persisting. Unlike the
// other targeted edits this is allowed in ANY status — tags are organizational
// metadata, useful on running/completed tasks too.
func (s *Storage) UpdateTaskTags(ctx context.Context, taskID uuid.UUID, add, remove []string) (*models.Task, error) {
	tx, err := s.db.BeginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	task, err := s.db.GetTaskForUpdate(ctx, tx, taskID)
	if err != nil {
		return nil, err
	}

	removeSet := make(map[string]struct{}, len(remove))
	for _, r := range remove {
		removeSet[r] = struct{}{}
	}
	merged := make([]string, 0, len(task.Tags)+len(add))
	for _, t := range task.Tags {
		if _, drop := removeSet[t]; !drop {
			merged = append(merged, t)
		}
	}
	for _, a := range add {
		if _, drop := removeSet[a]; !drop {
			merged = append(merged, a)
		}
	}
	normalized, err := models.NormalizeAndValidateTags(merged)
	if err != nil {
		return nil, err
	}
	task.Tags = normalized

	if err := s.db.UpdateTaskTx(ctx, tx, task); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return task, nil
}

// ListTagCatalogue returns every distinct tag in use with its task count (#212).
func (s *Storage) ListTagCatalogue(ctx context.Context) ([]db.TagCount, error) {
	return s.db.GetTagCatalogue(ctx)
}

// UpdateTaskStatusAtomic updates a task's status atomically, verifying lease
// ownership/assignment and handling completion + recurrence. Preserved verbatim
// from moc; the in-process worker reports status through this same path.
func (s *Storage) UpdateTaskStatusAtomic(taskID uuid.UUID, nodeID uuid.UUID, update *models.StatusUpdate) (*models.Task, error) {
	return s.UpdateTaskStatusAtomicWithContext(context.Background(), taskID, nodeID, update)
}

// UpdateTaskStatusAtomicWithContext is UpdateTaskStatusAtomic with context. The
// "nodeID" is the lease owner: a node UUID for the test substrate or the
// synthetic in-box worker's identity in production.
func (s *Storage) UpdateTaskStatusAtomicWithContext(ctx context.Context, taskID uuid.UUID, nodeID uuid.UUID, update *models.StatusUpdate) (*models.Task, error) {
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

	hasValidLease := task.LeaseOwner != nil && *task.LeaseOwner == nodeID.String()
	isAssigned := task.AssignedNodeID != nil && *task.AssignedNodeID == nodeID
	if !hasValidLease && !isAssigned {
		return nil, fmt.Errorf("node is not assigned to this task")
	}

	if task.Status == models.TaskStatusSuccess || task.Status == models.TaskStatusError || task.Status == models.TaskStatusCancelled {
		return task, nil
	}

	now := time.Now().UTC()
	if update.Status == models.TaskStatusRunning || update.Status == models.TaskStatusAnalyzing || update.Status == models.TaskStatusLeased {
		task.Status = update.Status
		expiresAt := now.Add(LeaseDuration)
		task.LeaseExpiresAt = &expiresAt
		if (task.Status == models.TaskStatusRunning || task.Status == models.TaskStatusAnalyzing) && task.StartedAt == nil {
			task.StartedAt = &now
		}
	} else {
		task.Status = update.Status
	}

	if update.AgentSessionID != nil {
		task.AgentSessionID = update.AgentSessionID
	}

	if update.Status == models.TaskStatusSuccess || update.Status == models.TaskStatusError {
		completedAt := time.Now().UTC()
		task.CompletedAt = &completedAt
		task.LeaseOwner = nil
		task.LeaseExpiresAt = nil
		if update.Message != nil {
			if update.Status == models.TaskStatusError {
				task.ErrorMessage = update.Message
			} else {
				task.Result = update.Message
			}
		}
		if task.AssignedNodeID != nil {
			node, err := s.db.GetNodeForUpdate(ctx, tx, *task.AssignedNodeID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			if node != nil && node.CurrentTaskID != nil && *node.CurrentTaskID == task.ID {
				node.Status = models.NodeStatusIdle
				node.CurrentTaskID = nil
				if err := s.db.UpdateNodeTx(ctx, tx, node); err != nil {
					return nil, err
				}
			}
		}
	}

	if err := s.db.UpdateTaskTx(ctx, tx, task); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	if (update.Status == models.TaskStatusSuccess || update.Status == models.TaskStatusError) && task.Recurrence != "" {
		s.scheduleNextRecurrence(context.Background(), task)
	}

	return task, nil
}

// RequeueTaskForRetryWithContext re-queues a cleanly-failed task for another
// whole-task attempt: it increments AttemptCount, sets Status=Scheduled with a
// future ScheduledFor (the backoff), clears the lease + StartedAt, leaves
// CompletedAt nil (the task is NOT terminal), and records the failure reason —
// all in one tx, gated on the caller still owning the lease (a stale runner's
// requeue is rejected, like UpdateTaskStatusAtomicWithContext). It deliberately
// does NOT call scheduleNextRecurrence — a retry is the SAME occurrence, not the
// next cron tick. Returns the updated task.
func (s *Storage) RequeueTaskForRetryWithContext(ctx context.Context, taskID, nodeID uuid.UUID, scheduledFor time.Time, msg string) (*models.Task, error) {
	tx, err := s.db.BeginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	task, err := s.db.GetTaskForUpdate(ctx, tx, taskID)
	if err != nil {
		return nil, err
	}

	hasValidLease := task.LeaseOwner != nil && *task.LeaseOwner == nodeID.String()
	isAssigned := task.AssignedNodeID != nil && *task.AssignedNodeID == nodeID
	if !hasValidLease && !isAssigned {
		return nil, fmt.Errorf("node is not assigned to this task")
	}
	// Already-terminal tasks are never resurrected by a retry.
	if task.Status == models.TaskStatusSuccess || task.Status == models.TaskStatusError || task.Status == models.TaskStatusCancelled {
		return task, nil
	}

	task.AttemptCount++
	task.Status = models.TaskStatusScheduled
	task.ScheduledFor = &scheduledFor
	task.StartedAt = nil
	task.CompletedAt = nil
	task.LeaseOwner = nil
	task.LeaseExpiresAt = nil
	if msg != "" {
		task.ErrorMessage = &msg
	}

	// Free the assigned node (mirror the terminal path) so it can pick up other work.
	if task.AssignedNodeID != nil {
		node, err := s.db.GetNodeForUpdate(ctx, tx, *task.AssignedNodeID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if node != nil && node.CurrentTaskID != nil && *node.CurrentTaskID == task.ID {
			node.Status = models.NodeStatusIdle
			node.CurrentTaskID = nil
			if err := s.db.UpdateNodeTx(ctx, tx, node); err != nil {
				return nil, err
			}
		}
		task.AssignedNodeID = nil
	}

	if err := s.db.UpdateTaskTx(ctx, tx, task); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return task, nil
}

// scheduleNextRecurrence creates the next occurrence of a recurring task.
func (s *Storage) scheduleNextRecurrence(ctx context.Context, task *models.Task) {
	schedule, err := cron.ParseStandard(task.Recurrence)
	if err != nil {
		log.Printf("Error parsing recurrence for task %s: %v", task.ID, err)
		return
	}

	// Evaluate the cron expression in the task's own timezone so a "9am" task
	// fires at 9am local, not 9am UTC. Fall back to the server-global location
	// (then UTC) if the stored name is somehow unloadable. The resulting instant
	// is stored in UTC — scheduled_for is always an absolute UTC instant.
	loc := s.location
	if task.Timezone != "" {
		if l, lerr := time.LoadLocation(task.Timezone); lerr == nil {
			loc = l
		} else {
			log.Printf("Task %s has invalid timezone %q; using server timezone: %v", task.ID, task.Timezone, lerr)
		}
	}
	now := time.Now().In(loc)
	nextTime := schedule.Next(now).UTC()

	newTask := models.NewTask(models.TaskCreate{
		Prompt:              task.Prompt,
		Model:               task.Model,
		FallbackModel:       task.FallbackModel,
		MaxIterations:       task.MaxIterations,
		MCPSelection:        task.MCPSelection,
		CredentialAllowlist: task.CredentialAllowlist,
		LoopConfig:          task.LoopConfig,
		WorktreeConfig:      task.WorktreeConfig,
		RetryPolicy:         task.RetryPolicy,
		Description:         task.Description,
		Priority:            task.Priority,
		ScheduledFor:        &nextTime,
		Recurrence:          task.Recurrence,
		Timezone:            task.Timezone,
		Files:               task.Files,
		Tags:                task.Tags,
	})
	newTask.CreatedBy = task.CreatedBy
	// Carry the originating API key forward so recurring task cost keeps counting
	// against the key's spending caps.
	newTask.CreatedByKeyID = task.CreatedByKeyID

	if _, err := s.AddTaskWithContext(ctx, newTask); err != nil {
		log.Printf("Error creating next recurring task for %s: %v", task.ID, err)
	} else {
		log.Printf("Scheduled next recurrence for task %s at %s", task.ID, nextTime)
	}
}

// Global storage instance
var globalStorage *Storage

// GetStorage returns the global storage instance.
func GetStorage() *Storage {
	if globalStorage == nil {
		globalStorage = New()
	}
	return globalStorage
}

// InitGlobalStorage initializes the global storage.
func InitGlobalStorage(dataDir string) error {
	globalStorage = New()
	return globalStorage.Initialize(filepath.Join(dataDir, "orchestrator.db"))
}
