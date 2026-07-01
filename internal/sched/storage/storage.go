// Package storage provides the storage layer for tasks (sched). Ported from
// moc's internal/storage. Concurrency is handled by PostgreSQL through
// transactions and row-level locking. There is no worker-node registry: on one
// box a single synthetic in-process worker claims work through
// db.ClaimNextPendingTask (FOR UPDATE SKIP LOCKED) under a lease keyed by a
// synthetic lease_owner, never a node table. The MatchGlob helper survives for
// scoped-visibility checks in the handlers.
package storage

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
	"golang.org/x/crypto/bcrypt"

	"github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// ErrTaskNotEditable is returned by UpdateEditableTask when a task left the
// editable state (pending or scheduled) between the caller's read and the
// locked write.
var ErrTaskNotEditable = errors.New("task is no longer editable")

// ErrTaskNotDeadLettered is returned by ReplayDeadLetteredTask when the target
// task is not in the dead_lettered state (#253) — only quarantined tasks replay.
var ErrTaskNotDeadLettered = errors.New("task is not dead-lettered")

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

// Storage provides persistent storage for scheduled tasks.
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

// MigrationStatus reports the orchestrator DB's applied vs pending migrations
// (#256). Read-only; delegates to the db layer's status reader. The context
// bounds the queries.
func (s *Storage) MigrationStatus(ctx context.Context) (db.MigrationReport, error) {
	return db.MigrationStatus(ctx, s.db.Conn())
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

// Initialize initializes the persistent storage with the given pool tuning (#276).
func (s *Storage) Initialize(dbPath string, pool db.PoolConfig) error {
	return s.db.Init(dbPath, pool)
}

// DefaultPoolConfig re-exports db.DefaultPoolConfig so callers that only import
// the storage package (e.g. the admin CLI and handler tests) can pass the
// behavior-preserving baseline without importing internal/sched/db (#276).
func DefaultPoolConfig() db.PoolConfig { return db.DefaultPoolConfig() }

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

// AddTaskBatch inserts a slice of validated tasks for the batch submission
// endpoint (#227). When atomic is true the whole insert runs inside a single
// transaction (BeginTx/Commit): a DB failure rolls every row back and the
// caller surfaces a 422. When atomic is false the multi-row INSERT is issued
// without an explicit tx — a single-statement INSERT is itself atomic per row,
// so the batch is best-effort (the caller already split valid from invalid at
// the validation layer). Returns the count actually inserted (== len(tasks) on
// success).
func (s *Storage) AddTaskBatch(ctx context.Context, tasks []*models.Task, atomic bool) (int, error) {
	if len(tasks) == 0 {
		return 0, nil
	}
	if atomic {
		tx, err := s.db.BeginTx(ctx)
		if err != nil {
			return 0, err
		}
		defer func() { _ = tx.Rollback() }()
		if err := s.db.AddTaskBatchTx(ctx, tx, tasks); err != nil {
			return 0, err
		}
		if err := tx.Commit(); err != nil {
			return 0, err
		}
		return len(tasks), nil
	}
	if err := s.db.AddTaskBatch(ctx, tasks); err != nil {
		return 0, err
	}
	return len(tasks), nil
}

// EnqueueTask is the storage-layer task-create plumbing the in-process create_task
// tool calls (#277): it validates the optional cron recurrence, mints a task from
// tc via models.NewTask (the SAME constructor the public POST /tasks path uses),
// persists it, and returns the new task's id, status, and next-run instant.
//
// It deliberately reuses NewTask + AddTask rather than forking a parallel create
// path. The #277 capability gate (allow_task_creation, the per-run spawn cap,
// budget + recurrence checks) is enforced UPSTREAM in the create_task tool before
// this is ever called; this method is the persistence seam, not the authority
// gate. It does NOT perform the handler's user/key authentication — the caller is
// the already-authorized scheduled run itself, and lineage (CreatedByTaskID) is
// set by the tool, never by an external client.
func (s *Storage) EnqueueTask(ctx context.Context, tc models.TaskCreate) (uuid.UUID, string, time.Time, error) {
	tc.Prompt = strings.TrimSpace(tc.Prompt)
	if tc.Prompt == "" {
		return uuid.Nil, "", time.Time{}, fmt.Errorf("prompt is required")
	}

	if tc.Recurrence = strings.TrimSpace(tc.Recurrence); tc.Recurrence != "" {
		schedule, err := cron.ParseStandard(tc.Recurrence)
		if err != nil {
			return uuid.Nil, "", time.Time{}, fmt.Errorf("recurrence must be a standard 5-field cron expression")
		}
		// With no explicit one-time run, wait for the next cron trigger (evaluated
		// in the storage timezone) rather than running immediately. Always stored
		// as an absolute UTC instant, matching the handler's create path.
		if tc.ScheduledFor == nil {
			next := schedule.Next(time.Now().In(s.location)).UTC()
			tc.ScheduledFor = &next
		}
	}

	task := models.NewTask(tc)
	if _, err := s.AddTaskWithContext(ctx, task); err != nil {
		return uuid.Nil, "", time.Time{}, err
	}
	var nextRunAt time.Time
	if task.ScheduledFor != nil {
		nextRunAt = *task.ScheduledFor
	}
	return task.ID, string(task.Status), nextRunAt, nil
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

// ListTasksForExport returns task definitions for GET /tasks/export (#238). See
// db.ListTasksForExport for filter semantics.
func (s *Storage) ListTasksForExport(ctx context.Context, ids []uuid.UUID, recurrenceOnly bool) ([]*models.Task, error) {
	return s.db.ListTasksForExport(ctx, ids, recurrenceOnly)
}

// FindTaskIDsByName resolves task IDs by non-empty name (#238). Import
// conflict-detection pre-flight.
func (s *Storage) FindTaskIDsByName(ctx context.Context, names []string) (map[string]uuid.UUID, error) {
	return s.db.FindTaskIDsByName(ctx, names)
}

// GetTaskByName returns the task whose non-empty name matches, or (nil, nil)
// when no such task exists (#238).
func (s *Storage) GetTaskByName(ctx context.Context, name string) (*models.Task, error) {
	return s.db.GetTaskByName(ctx, name)
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

// PromoteStarvedTasks raises the effective priority of pending tasks that have
// waited past the starvation window (#230). Returns the count promoted.
func (s *Storage) PromoteStarvedTasks(ctx context.Context, windowMinutes int) (int64, error) {
	return s.db.PromoteStarvedTasks(ctx, windowMinutes)
}

// PendingQueueStats returns the per-effective-priority rollup of the pending
// queue for GET /admin/queue (#230).
func (s *Storage) PendingQueueStats(ctx context.Context) ([]models.QueuePriorityBucket, error) {
	return s.db.PendingQueueStats(ctx)
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

// CleanupOldRuns prunes terminal task runs older than retentionDays while always
// keeping the most recent keepPerTask runs per task (#252). See db.CleanupOldRuns.
func (s *Storage) CleanupOldRuns(ctx context.Context, retentionDays, keepPerTask int) (int, error) {
	return s.db.CleanupOldRuns(ctx, retentionDays, keepPerTask)
}

// SetLogArchiveKey configures the optional host-side AES-256-GCM key used to
// encrypt archived log payloads (#272). See db.SetLogArchiveKey — the key is
// held in memory only and never logged.
func (s *Storage) SetLogArchiveKey(key []byte) { s.db.SetLogArchiveKey(key) }

// ArchiveOldLogs compresses (optionally encrypts) log payloads for terminal
// tasks older than days, in place. Returns (rows archived, bytes saved, error).
// See db.ArchiveOldLogs.
func (s *Storage) ArchiveOldLogs(ctx context.Context, days int) (int, int64, error) {
	return s.db.ArchiveOldLogs(ctx, days)
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

// EnsureAdminUser provisions (or promotes) username as an admin so a bootstrap
// operator reaches the Operations Center through the shared chat session cookie
// without a manual `fleet-admin sched user add` step (#458). username is the
// lowercased email the header-trust/cookie path resolves against (lookupMember).
// Idempotent and config-authoritative: an existing admin is left untouched, an
// existing non-admin is promoted to admin, and a missing user is created with a
// random, UNUSABLE bcrypt password hash — a bootstrap admin authenticates ONLY
// via the cookie/header-trust path, never the moc username/password login.
func (s *Storage) EnsureAdminUser(ctx context.Context, username string) error {
	username = strings.ToLower(strings.TrimSpace(username))
	if username == "" {
		return nil
	}
	existing, err := s.db.GetUserByUsername(ctx, username)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if existing != nil {
		if existing.Role == "admin" {
			return nil
		}
		return s.db.UpdateUserRole(ctx, existing.ID, "admin")
	}
	// New bootstrap admin: a 32-byte random secret bcrypt-hashed so the moc
	// password login can never succeed for this account (cookie-auth only).
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	hash, err := bcrypt.GenerateFromPassword(secret, bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = s.AddUser(&models.User{
		ID:           uuid.New(),
		Username:     username,
		PasswordHash: string(hash),
		Role:         "admin",
		CreatedAt:    time.Now().UTC(),
	})
	return err
}

// GetUserByUsernameWithContext gets a user by username with context.
func (s *Storage) GetUserByUsernameWithContext(ctx context.Context, username string) (*models.User, error) {
	return s.db.GetUserByUsername(ctx, username)
}

// GetUserByToken gets a user by session token.
func (s *Storage) GetUserByToken(token string) (*models.User, error) {
	return s.db.GetUserByToken(context.Background(), token)
}

// GetScheduledTasks gets scheduled tasks ready to run up to a limit.
func (s *Storage) GetScheduledTasks(cutoff time.Time, limit int) ([]*models.Task, error) {
	return s.db.GetScheduledTasks(context.Background(), cutoff, limit)
}

// CancelTaskAtomic cancels a task atomically. reason records WHO/why (#508 —
// e.g. "stopped by admin"); stored on the task's Result so the attribution
// survives as the terminal record. Empty keeps the legacy unattributed cancel.
func (s *Storage) CancelTaskAtomic(taskID uuid.UUID, reason string) (*models.Task, error) {
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
	if reason != "" {
		task.Result = &reason
	}

	if err := s.db.UpdateTaskTx(ctx, tx, task); err != nil {
		return nil, err
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
	AllowDelegation        bool
	// Persona replaces the task's per-task persona override (#221), assigned
	// unconditionally from the full edit payload (empty = use the global persona).
	Persona string
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
	task.AllowDelegation = edit.AllowDelegation
	task.Persona = edit.Persona
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
	if !hasValidLease {
		return nil, fmt.Errorf("worker does not hold the lease on this task")
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

	// Record the per-run workspace path (#287) when the runner supplies it. A nil
	// WorkspacePath leaves any previously-recorded value untouched, so a later
	// terminal status update doesn't wipe the path the running update set.
	if update.WorkspacePath != nil {
		task.WorkspacePath = update.WorkspacePath
	}

	// Record the validated structured output (#244) when the runner supplies it,
	// on the running-status update it sends just before terminal success. Empty
	// leaves the existing value untouched (so the later success update, which
	// carries no OutputJSON, doesn't wipe it).
	if len(update.OutputJSON) > 0 {
		task.OutputJSON = update.OutputJSON
	}

	// Record the published-artifact manifest (#204) when the runner supplies it,
	// on the running-status update it sends just before terminal success. Empty
	// leaves the existing value untouched, mirroring OutputJSON.
	if len(update.Artifacts) > 0 {
		task.Artifacts = update.Artifacts
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
	if !hasValidLease {
		return nil, fmt.Errorf("worker does not hold the lease on this task")
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
	// Reset the per-run SLA artifacts (#274): the prior attempt's actual
	// duration is now stale (no completion) and a fresh attempt should not
	// inherit the prior attempt's breach latch.
	task.ActualDurationSeconds = nil
	task.SLABreached = false
	task.LeaseOwner = nil
	task.LeaseExpiresAt = nil
	if msg != "" {
		task.ErrorMessage = &msg
	}

	if err := s.db.UpdateTaskTx(ctx, tx, task); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return task, nil
}

// DeadLetterTaskWithContext routes a terminally-failed task to the dead-letter
// queue (#253): it sets Status=dead_lettered, records dead_lettered_at /
// dead_letter_reason / dead_letter_attempts, stamps CompletedAt + ErrorMessage
// (so the row reads as terminal everywhere a completed/errored task does), and
// clears the lease — all in one tx, gated on the caller still owning the lease
// (a stale runner's quarantine is rejected, like RequeueTaskForRetryWithContext).
//
// It is the terminal sibling of RequeueTaskForRetryWithContext: the runner calls
// requeue while retries remain and a transient class allows it, and calls this
// once retries are exhausted or the failure is non-retryable. Like the requeue
// path it deliberately does NOT call scheduleNextRecurrence — a dead-lettered
// occurrence does not auto-spawn the next cron tick; the recurrence resumes on
// the next normal completion, and the quarantined occurrence awaits replay.
// attempts is the total number of attempts made (AttemptCount+1 at the call site).
func (s *Storage) DeadLetterTaskWithContext(ctx context.Context, taskID, nodeID uuid.UUID, reason string, attempts int) (*models.Task, error) {
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
	if !hasValidLease {
		return nil, fmt.Errorf("worker does not hold the lease on this task")
	}
	// Already-terminal tasks are never re-quarantined.
	if task.Status == models.TaskStatusSuccess || task.Status == models.TaskStatusError ||
		task.Status == models.TaskStatusCancelled || task.Status == models.TaskStatusDeadLettered {
		return task, nil
	}

	now := time.Now().UTC()
	task.Status = models.TaskStatusDeadLettered
	task.CompletedAt = &now
	task.DeadLetteredAt = &now
	task.DeadLetterAttempts = attempts
	task.LeaseOwner = nil
	task.LeaseExpiresAt = nil
	if reason != "" {
		task.ErrorMessage = &reason
		task.DeadLetterReason = &reason
	}

	if err := s.db.UpdateTaskTx(ctx, tx, task); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return task, nil
}

// GetDeadLetteredTasks returns dead-lettered tasks (#253), newest-quarantined
// first, for the DLQ review listing (`fleet-admin sched dlq list`). limit/offset
// paginate; a non-positive limit returns all matching rows.
func (s *Storage) GetDeadLetteredTasks(ctx context.Context, limit, offset int) ([]*models.Task, error) {
	return s.db.GetDeadLetteredTasks(ctx, limit, offset)
}

// GetRunningTasksWithSLA returns the in-flight tasks that carry an SLA (#274),
// for the SLA monitor goroutine. See db.GetRunningTasksWithSLA.
func (s *Storage) GetRunningTasksWithSLA(ctx context.Context) ([]*models.Task, error) {
	return s.db.GetRunningTasksWithSLA(ctx)
}

// MarkSLABreached latches sla_breached=true on a task the SLA monitor flagged
// as having crossed its fail threshold (#274). See db.MarkSLABreached.
func (s *Storage) MarkSLABreached(ctx context.Context, taskID uuid.UUID) error {
	return s.db.MarkSLABreached(ctx, taskID)
}

// SetTaskErrorAnalysis persists the async post-failure error diagnosis (#317) on
// a task. Lease-free (the diagnosis runs after the terminal transition). See
// db.SetErrorAnalysis.
func (s *Storage) SetTaskErrorAnalysis(ctx context.Context, taskID uuid.UUID, raw json.RawMessage) error {
	return s.db.SetErrorAnalysis(ctx, taskID, raw)
}

// GetSLAReport aggregates the per-prompt SLA actuals over windowDays (#274).
// See db.GetSLAReport.
func (s *Storage) GetSLAReport(ctx context.Context, windowDays int) (*models.SLAReport, error) {
	return s.db.GetSLAReport(ctx, windowDays)
}

// ReplayDeadLetteredTask re-enqueues a dead-lettered task (#253): it resets the
// SAME row to a fresh pending slate — AttemptCount=0, the DLQ columns cleared,
// status=pending, scheduled_for/started_at/completed_at/error cleared — so the
// scheduler's normal claim path picks it up again. It is gated on the task being
// in the dead_lettered state (ErrTaskNotDeadLettered otherwise), mirroring the
// editability guards on the other operator mutations. Returns the updated task.
func (s *Storage) ReplayDeadLetteredTask(ctx context.Context, taskID uuid.UUID) (*models.Task, error) {
	tx, err := s.db.BeginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	task, err := s.db.GetTaskForUpdate(ctx, tx, taskID)
	if err != nil {
		return nil, err
	}
	if task.Status != models.TaskStatusDeadLettered {
		return nil, ErrTaskNotDeadLettered
	}

	task.Status = models.TaskStatusPending
	task.AttemptCount = 0
	task.ScheduledFor = nil
	task.StartedAt = nil
	task.CompletedAt = nil
	task.ErrorMessage = nil
	// Reset the SLA artifacts (#274): a replayed task is a fresh slate, so
	// neither the prior attempt's actual duration nor its breach latch carries.
	task.ActualDurationSeconds = nil
	task.SLABreached = false
	task.DeadLetteredAt = nil
	task.DeadLetterReason = nil
	task.DeadLetterAttempts = 0
	task.LeaseOwner = nil
	task.LeaseExpiresAt = nil

	if err := s.db.UpdateTaskTx(ctx, tx, task); err != nil {
		return nil, err
	}
	// A replayed task is a fresh slate: clear the prior attempt's error_analysis
	// (#317) so the re-run doesn't carry a stale diagnosis. UpdateTaskTx omits
	// error_analysis (it's write-once against status updates), so clear it
	// explicitly in the same tx rather than through the task struct.
	if _, err := tx.ExecContext(ctx, `UPDATE tasks SET error_analysis = NULL WHERE id = $1`, taskID); err != nil {
		return nil, err
	}
	task.ErrorAnalysis = nil
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
		Persona:             task.Persona,
		Description:         task.Description,
		Priority:            task.Priority,
		ScheduledFor:        &nextTime,
		Recurrence:          task.Recurrence,
		Timezone:            task.Timezone,
		Files:               task.Files,
		Tags:                task.Tags,
		RunIf:               task.RunIf,
		// Carry the Captain's Log opt-in forward (#285/#322) so a recurring
		// self-improving task keeps its persistent-memory capability on every
		// occurrence rather than silently losing it (the new occurrence must have
		// the flag set for its run to register remember/recall + inject memory).
		InstructionSelfImprove: task.InstructionSelfImprove,
	})
	newTask.CreatedBy = task.CreatedBy
	// Carry the originating API key forward so recurring task cost keeps counting
	// against the key's spending caps.
	newTask.CreatedByKeyID = task.CreatedByKeyID

	if _, err := s.AddTaskWithContext(ctx, newTask); err != nil {
		log.Printf("Error creating next recurring task for %s: %v", task.ID, err)
		return
	}
	log.Printf("Scheduled next recurrence for task %s at %s", task.ID, nextTime)

	// Carry the completing occurrence's persistent memory (#198/#285) forward to
	// the new occurrence. Memory is keyed by task_id and each recurrence is a NEW
	// task row, so WITHOUT this copy a recurring Captain's Log task would start
	// cold every time — defeating the feature (e.g. "alert only if the price
	// changed since last week"). Best-effort + parameterized; the canonical
	// task_memories data layer is internal/sched/taskmemory.go. The FK is
	// satisfied because newTask was just inserted above.
	if _, cerr := s.db.Conn().ExecContext(ctx, `
		INSERT INTO task_memories (id, task_id, key, value, created_at, updated_at)
		SELECT gen_random_uuid(), $1, key, value, created_at, updated_at
		FROM task_memories WHERE task_id = $2`,
		newTask.ID, task.ID); cerr != nil {
		log.Printf("recurring task %s: failed to carry persistent memory forward to %s: %v", task.ID, newTask.ID, cerr)
	}
}

// ComputeNextRun evaluates a task's cron recurrence in its own timezone and
// returns the next occurrence as an absolute UTC instant. Used by the
// scheduler's skip path (#269) to advance a skipped task's scheduled_for to
// the next cron tick. It mirrors scheduleNextRecurrence's tz math but does NOT
// spawn a new task row (the skip path reuses the SAME occurrence row).
func (s *Storage) ComputeNextRun(task *models.Task) (time.Time, error) {
	schedule, err := cron.ParseStandard(task.Recurrence)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse recurrence: %w", err)
	}
	loc := s.location
	if task.Timezone != "" {
		if l, lerr := time.LoadLocation(task.Timezone); lerr == nil {
			loc = l
		}
	}
	return schedule.Next(time.Now().In(loc)).UTC(), nil
}

// RecordSkip records a pre-run-gate skip on a still-scheduled task (#269): it
// re-locks the row inside a transaction, re-checks the task is still scheduled
// (a concurrent cancel/claim wins and the skip becomes a no-op), advances
// scheduled_for to nextRun, increments skip_count, and stamps last_skip_at +
// last_skip_reason. Returns the updated task. nextRun is computed by the
// caller via ComputeNextRun (so the scheduler owns the cron math).
func (s *Storage) RecordSkip(ctx context.Context, taskID uuid.UUID, reason string, nextRun time.Time) (*models.Task, error) {
	tx, err := s.db.BeginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	task, err := s.db.RecordSkip(ctx, tx, taskID, reason, nextRun)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return task, nil
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
	return globalStorage.Initialize(filepath.Join(dataDir, "orchestrator.db"), db.DefaultPoolConfig())
}

// Eval & regression harness (#502)

// AddEvalRun persists one immutable eval-run record.
func (s *Storage) AddEvalRun(ctx context.Context, r *models.EvalRun) error {
	return s.db.AddEvalRun(ctx, r)
}

// ListEvalRuns returns the newest-first eval-run history for a set (every set
// when evalSet is empty), capped at limit.
func (s *Storage) ListEvalRuns(ctx context.Context, evalSet string, limit int) ([]*models.EvalRun, error) {
	return s.db.ListEvalRuns(ctx, evalSet, limit)
}

// LatestEvalRun returns the most recent eval run for a set, or nil when the
// set has never run.
func (s *Storage) LatestEvalRun(ctx context.Context, evalSet string) (*models.EvalRun, error) {
	return s.db.LatestEvalRun(ctx, evalSet)
}

// Dataset / table agent (#514)

// CreateDataset persists a dataset definition.
func (s *Storage) CreateDataset(ctx context.Context, d *models.Dataset) error {
	return s.db.CreateDataset(ctx, d)
}

// GetDataset returns one dataset with per-status row counts.
func (s *Storage) GetDataset(ctx context.Context, id uuid.UUID) (*models.Dataset, error) {
	return s.db.GetDataset(ctx, id)
}

// ListDatasets returns every dataset, newest first.
func (s *Storage) ListDatasets(ctx context.Context) ([]*models.Dataset, error) {
	return s.db.ListDatasets(ctx)
}

// UpdateDatasetStatus applies a guarded status transition.
func (s *Storage) UpdateDatasetStatus(ctx context.Context, id uuid.UUID, from []string, to string) (bool, error) {
	return s.db.UpdateDatasetStatus(ctx, id, from, to)
}

// DeleteDataset removes a dataset and its rows.
func (s *Storage) DeleteDataset(ctx context.Context, id uuid.UUID) error {
	return s.db.DeleteDataset(ctx, id)
}

// AddDatasetRows bulk-appends rows.
func (s *Storage) AddDatasetRows(ctx context.Context, datasetID uuid.UUID, cells []json.RawMessage) (int, error) {
	return s.db.AddDatasetRows(ctx, datasetID, cells)
}

// ListDatasetRows pages a dataset's rows, optionally by status.
func (s *Storage) ListDatasetRows(ctx context.Context, datasetID uuid.UUID, status string, limit, offset int) ([]*models.DatasetRow, error) {
	return s.db.ListDatasetRows(ctx, datasetID, status, limit, offset)
}

// ClaimNextDatasetRow atomically claims one pending row for a worker.
func (s *Storage) ClaimNextDatasetRow(ctx context.Context, datasetID uuid.UUID) (*models.DatasetRow, error) {
	return s.db.ClaimNextDatasetRow(ctx, datasetID)
}

// FinishDatasetRow records one row run's outcome.
func (s *Storage) FinishDatasetRow(ctx context.Context, rowID uuid.UUID, proposed json.RawMessage, note, errMsg string, costUSD float64) error {
	return s.db.FinishDatasetRow(ctx, rowID, proposed, note, errMsg, costUSD)
}

// ApproveDatasetRows merges proposed values into cells for review-approved rows.
func (s *Storage) ApproveDatasetRows(ctx context.Context, datasetID uuid.UUID, ids []uuid.UUID) (int, error) {
	return s.db.ApproveDatasetRows(ctx, datasetID, ids)
}

// ResetDatasetRows returns rows to pending for a re-run.
func (s *Storage) ResetDatasetRows(ctx context.Context, datasetID uuid.UUID, ids []uuid.UUID) (int, error) {
	return s.db.ResetDatasetRows(ctx, datasetID, ids)
}

// ResetStaleRunningDatasets is the boot sweep for crash-orphaned runs.
func (s *Storage) ResetStaleRunningDatasets(ctx context.Context) error {
	return s.db.ResetStaleRunningDatasets(ctx)
}

// Self-improving memory (#516): feedback + learned instructions.

// AddTaskFeedback records one feedback signal.
func (s *Storage) AddTaskFeedback(ctx context.Context, f *models.TaskFeedback) error {
	return s.db.AddTaskFeedback(ctx, f)
}

// UnconsumedFeedback returns a task's fresh feedback signals.
func (s *Storage) UnconsumedFeedback(ctx context.Context, taskID uuid.UUID) ([]*models.TaskFeedback, error) {
	return s.db.UnconsumedFeedback(ctx, taskID)
}

// ProposeLearnedInstruction stages a distilled instruction (marks its evidence consumed).
func (s *Storage) ProposeLearnedInstruction(ctx context.Context, taskID uuid.UUID, content string, evidenceIDs []uuid.UUID, now int64) (*models.TaskLearnedInstruction, error) {
	return s.db.ProposeLearnedInstruction(ctx, taskID, content, evidenceIDs, now)
}

// ListLearnedInstructions returns a task's instructions, newest version first.
func (s *Storage) ListLearnedInstructions(ctx context.Context, taskID uuid.UUID) ([]*models.TaskLearnedInstruction, error) {
	return s.db.ListLearnedInstructions(ctx, taskID)
}

// ActiveLearnedInstruction returns the task's active instruction (run-time injection target), or nil.
func (s *Storage) ActiveLearnedInstruction(ctx context.Context, taskID uuid.UUID) (*models.TaskLearnedInstruction, error) {
	return s.db.ActiveLearnedInstruction(ctx, taskID)
}

// ActivateLearnedInstruction activates (or reverts to) a version.
func (s *Storage) ActivateLearnedInstruction(ctx context.Context, taskID uuid.UUID, version int, who string, now int64) (*models.TaskLearnedInstruction, error) {
	return s.db.ActivateLearnedInstruction(ctx, taskID, version, who, now)
}

// DeactivateLearnedInstructions archives the active instruction (full revert).
func (s *Storage) DeactivateLearnedInstructions(ctx context.Context, taskID uuid.UUID) (bool, error) {
	return s.db.DeactivateLearnedInstructions(ctx, taskID)
}

// ask/notify pause (#510)

// PauseTaskForQuestion parks a running task awaiting a human answer.
func (s *Storage) PauseTaskForQuestion(ctx context.Context, taskID, leaseOwner uuid.UUID, question string) (bool, error) {
	return s.db.PauseTaskForQuestion(ctx, taskID, leaseOwner, question)
}

// ResumeTask answers a paused task and re-queues it.
func (s *Storage) ResumeTask(ctx context.Context, taskID uuid.UUID, answer string) (bool, error) {
	return s.db.ResumeTask(ctx, taskID, answer)
}

// ClearPendingQA clears a resumed task's Q&A once the run consumed them.
func (s *Storage) ClearPendingQA(ctx context.Context, taskID, leaseOwner uuid.UUID) error {
	return s.db.ClearPendingQA(ctx, taskID, leaseOwner)
}

// ListPausedTasks returns tasks awaiting a human answer.
func (s *Storage) ListPausedTasks(ctx context.Context, limit int) ([]*models.Task, error) {
	return s.db.ListPausedTasks(ctx, limit)
}
