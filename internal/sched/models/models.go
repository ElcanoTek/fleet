// Package models contains data models for the fleet orchestrator (sched).
//
// Ported from moc's internal/models. The node/lease machinery is preserved
// verbatim so the crash-recovery substrate (leases, RecoverExpiredLeases) and
// its tests carry over unchanged. The one schema change vs moc: per-task node
// routing (target_node_*) is replaced by a per-task MCP + credential-account
// selection (MCPSelection), modeled on chat's per-conversation opt-in.
package models

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// SessionTokenDuration is how long session tokens are valid.
const SessionTokenDuration = 7 * 24 * time.Hour

// HashToken creates a SHA-256 hash for storing tokens securely.
func HashToken(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}

// HashTokenIfNeeded returns the token if it's already a SHA-256 hash, otherwise hashes it.
func HashTokenIfNeeded(token string) string {
	if len(token) == 64 {
		if _, err := hex.DecodeString(token); err == nil {
			return token
		}
	}
	return HashToken(token)
}

// MCPChoice names one chosen MCP server and its credential account for a task.
// Account=="" means the default/shared seat. This mirrors
// agentcore.MCPChoice byte-for-byte at the JSON level so a task's selection
// can be handed straight to agentcore.BindMCPSelection. It is mirrored here
// (rather than imported) to keep the sched data layer free of an agentcore
// dependency.
type MCPChoice struct {
	Server  string `json:"server"`
	Account string `json:"account,omitempty"`
}

// MCPSelection is the per-task list of chosen servers.
type MCPSelection []MCPChoice

// User represents a system user.
type User struct {
	ID             uuid.UUID  `json:"id"`
	Username       string     `json:"username"`
	PasswordHash   string     `json:"-"`
	Role           string     `json:"role"`
	Scopes         []string   `json:"scopes"`
	CreatedAt      time.Time  `json:"created_at"`
	LastLogin      *time.Time `json:"last_login,omitempty"`
	SessionToken   *string    `json:"-"`
	TokenExpiresAt *time.Time `json:"-"`
}

// UserCreate represents the request to create a user.
type UserCreate struct {
	Username string   `json:"username"`
	Password string   `json:"password"`
	Role     string   `json:"role"`
	Scopes   []string `json:"scopes"`
}

// UserLogin represents a login request.
type UserLogin struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// UserResponse represents the public user data.
type UserResponse struct {
	ID        uuid.UUID `json:"id"`
	Username  string    `json:"username"`
	Role      string    `json:"role"`
	Scopes    []string  `json:"scopes"`
	CreatedAt time.Time `json:"created_at"`
}

// LoginResponse represents the response after successful login.
type LoginResponse struct {
	Token string       `json:"token"`
	User  UserResponse `json:"user"`
}

// NodeStatus represents the current status of a runner node (the synthetic
// in-box worker; see internal/runner).
type NodeStatus string

const (
	NodeStatusIdle    NodeStatus = "idle"
	NodeStatusBusy    NodeStatus = "busy"
	NodeStatusOffline NodeStatus = "offline"
	NodeStatusError   NodeStatus = "error"
)

// IsValid reports whether s is a recognized node status.
func (s NodeStatus) IsValid() bool {
	switch s {
	case NodeStatusIdle, NodeStatusBusy, NodeStatusOffline, NodeStatusError:
		return true
	default:
		return false
	}
}

// TaskStatus represents the status of a task in the system.
type TaskStatus string

const (
	TaskStatusPending   TaskStatus = "pending"
	TaskStatusScheduled TaskStatus = "scheduled"
	TaskStatusAssigned  TaskStatus = "assigned"
	TaskStatusLeased    TaskStatus = "leased"
	TaskStatusRunning   TaskStatus = "running"
	TaskStatusAnalyzing TaskStatus = "analyzing"
	TaskStatusSuccess   TaskStatus = "success"
	TaskStatusError     TaskStatus = "error"
	TaskStatusCancelled TaskStatus = "cancelled"
)

// IsValidReportedStatus reports whether s is a status a worker is allowed to
// report for its own task. The orchestrator owns the rest of the lifecycle.
func (s TaskStatus) IsValidReportedStatus() bool {
	switch s {
	case TaskStatusLeased, TaskStatusRunning, TaskStatusAnalyzing, TaskStatusSuccess, TaskStatusError:
		return true
	default:
		return false
	}
}

// Permission represents available permissions for API keys.
type Permission string

const (
	PermissionCreateTask Permission = "create_task"
	PermissionViewTasks  Permission = "view_tasks"
	PermissionCancelTask Permission = "cancel_task"
	PermissionViewNodes  Permission = "view_nodes"
	PermissionViewLogs   Permission = "view_logs"
	PermissionManageKeys Permission = "manage_keys"
	PermissionAdmin      Permission = "admin"
)

// RolePermissions maps role names to their permission sets.
var RolePermissions = map[string][]Permission{
	"admin":    {PermissionAdmin},
	"client":   {PermissionCreateTask, PermissionViewTasks, PermissionViewLogs, PermissionViewNodes},
	"readonly": {PermissionViewTasks, PermissionViewNodes, PermissionViewLogs},
}

// NodeRegistration is the request model for node registration.
type NodeRegistration struct {
	Hostname string  `json:"hostname"`
	Name     *string `json:"name,omitempty"`
	OSType   string  `json:"os_type"`
}

// Node represents a registered worker node in the fleet.
type Node struct {
	ID             uuid.UUID  `json:"id"`
	Hostname       string     `json:"hostname"`
	Name           string     `json:"name"`
	APIKey         string     `json:"api_key"`
	PreviousAPIKey *string    `json:"-"`
	KeyRotatedAt   *time.Time `json:"-"`
	OSType         string     `json:"os_type"`
	Status         NodeStatus `json:"status"`
	LastHeartbeat  time.Time  `json:"last_heartbeat"`
	CurrentTaskID  *uuid.UUID `json:"current_task_id,omitempty"`
	RegisteredAt   time.Time  `json:"registered_at"`
}

// KeyRotationGracePeriod is how long the previous API key remains valid after rotation.
const KeyRotationGracePeriod = 5 * time.Minute

// NewNode creates a new Node with defaults.
func NewNode(reg NodeRegistration) *Node {
	now := time.Now().UTC()
	name := reg.Hostname
	if reg.Name != nil {
		name = *reg.Name
	}
	return &Node{
		ID:            uuid.New(),
		Hostname:      reg.Hostname,
		Name:          name,
		APIKey:        uuid.New().String(),
		OSType:        reg.OSType,
		Status:        NodeStatusIdle,
		LastHeartbeat: now,
		RegisteredAt:  now,
	}
}

// TaskCreate is the request model for creating a new task.
type TaskCreate struct {
	Prompt                 string       `json:"prompt"`
	Model                  *string      `json:"model,omitempty"`
	FallbackModel          *string      `json:"fallback_model,omitempty"`
	MaxIterations          *int         `json:"max_iterations,omitempty"`
	MCPSelection           MCPSelection `json:"mcp_selection,omitempty"`
	Priority               int          `json:"priority"`
	InstructionSelfImprove bool         `json:"instruction_self_improve,omitempty"`
	ScheduledFor           *time.Time   `json:"scheduled_for,omitempty"`
	Recurrence             string       `json:"recurrence,omitempty"`
	Files                  []string     `json:"files,omitempty"`
	// MaxRetries is the number of ADDITIONAL whole-task attempts after the first
	// when a run fails cleanly with a transient error. 0 (default) = no retries.
	MaxRetries *int `json:"max_retries,omitempty"`
	// AllowNetwork lets THIS scheduled task's bash/run_python execution sandbox
	// keep outbound egress. The default (false) seals the sandbox with
	// --network=none, matching the interactive lockdown path; egress is an
	// explicit opt-in for the tasks that genuinely need it.
	AllowNetwork bool `json:"allow_network,omitempty"`
}

// Task represents a task to be executed by a worker.
type Task struct {
	ID                     uuid.UUID    `json:"id"`
	Prompt                 string       `json:"prompt"`
	Model                  *string      `json:"model,omitempty"`
	FallbackModel          *string      `json:"fallback_model,omitempty"`
	MaxIterations          *int         `json:"max_iterations,omitempty"`
	MCPSelection           MCPSelection `json:"mcp_selection"`
	Priority               int          `json:"priority"`
	InstructionSelfImprove bool         `json:"instruction_self_improve,omitempty"`
	// AllowNetwork controls whether this task's execution sandbox keeps outbound
	// egress. Default false seals it (--network=none); see TaskCreate.AllowNetwork.
	AllowNetwork   bool       `json:"allow_network,omitempty"`
	Status         TaskStatus `json:"status"`
	AssignedNodeID *uuid.UUID `json:"assigned_node_id,omitempty"`
	AgentSessionID *string    `json:"agent_session_id,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
	Result         *string    `json:"result,omitempty"`
	ErrorMessage   *string    `json:"error_message,omitempty"`
	ScheduledFor   *time.Time `json:"scheduled_for,omitempty"`
	Recurrence     string     `json:"recurrence,omitempty"`
	CreatedBy      *uuid.UUID `json:"created_by,omitempty"`
	Files          []string   `json:"files,omitempty"`
	LeaseOwner     *string    `json:"lease_owner,omitempty"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
	// AttemptCount is how many times this task has been re-queued after a clean,
	// transient failure (0 on the first run). MaxRetries caps it: the task may run
	// up to MaxRetries+1 times before a failure is terminal.
	AttemptCount int `json:"attempt_count"`
	MaxRetries   int `json:"max_retries"`
	// CreatedByUsername is populated at query time for display purposes (not persisted)
	CreatedByUsername *string `json:"created_by_username,omitempty"`
}

// NewTask creates a new Task with defaults.
func NewTask(tc TaskCreate) *Task {
	status := TaskStatusPending
	if tc.ScheduledFor != nil && tc.ScheduledFor.After(time.Now()) {
		status = TaskStatusScheduled
	}

	return &Task{
		ID:                     uuid.New(),
		Prompt:                 tc.Prompt,
		Model:                  tc.Model,
		FallbackModel:          tc.FallbackModel,
		MaxIterations:          tc.MaxIterations,
		MCPSelection:           tc.MCPSelection,
		Priority:               tc.Priority,
		InstructionSelfImprove: tc.InstructionSelfImprove,
		AllowNetwork:           tc.AllowNetwork,
		Status:                 status,
		CreatedAt:              time.Now().UTC(),
		ScheduledFor:           tc.ScheduledFor,
		Recurrence:             tc.Recurrence,
		Files:                  tc.Files,
		MaxRetries:             derefOr(tc.MaxRetries, 0),
	}
}

// derefOr returns *p, or def when p is nil.
func derefOr(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

// StatusUpdate is a status update for a task (from the in-process worker).
type StatusUpdate struct {
	TaskID         uuid.UUID  `json:"task_id"`
	Status         TaskStatus `json:"status"`
	Message        *string    `json:"message,omitempty"`
	Progress       *float64   `json:"progress,omitempty"`
	AgentSessionID *string    `json:"agent_session_id,omitempty"`
	Timestamp      *time.Time `json:"timestamp,omitempty"`
}

// TaskAssignment is the task assignment carried to the worker.
type TaskAssignment struct {
	TaskID                 uuid.UUID    `json:"task_id"`
	Prompt                 string       `json:"prompt"`
	Model                  *string      `json:"model,omitempty"`
	FallbackModel          *string      `json:"fallback_model,omitempty"`
	MaxIterations          *int         `json:"max_iterations,omitempty"`
	MCPSelection           MCPSelection `json:"mcp_selection,omitempty"`
	InstructionSelfImprove bool         `json:"instruction_self_improve,omitempty"`
	OrchestratorURL        string       `json:"orchestrator_url"`
	Files                  []string     `json:"files,omitempty"`
	FileChecksums          []string     `json:"file_checksums,omitempty"`
}

// NodeHeartbeat is the heartbeat from a node to indicate it's still alive.
type NodeHeartbeat struct {
	NodeID        uuid.UUID  `json:"node_id"`
	Status        NodeStatus `json:"status"`
	CurrentTaskID *uuid.UUID `json:"current_task_id,omitempty"`
}

// DashboardStats contains statistics for the dashboard.
type DashboardStats struct {
	TotalNodes          int `json:"total_nodes"`
	ActiveNodes         int `json:"active_nodes"`
	IdleNodes           int `json:"idle_nodes"`
	OfflineNodes        int `json:"offline_nodes"`
	PendingTasks        int `json:"pending_tasks"`
	RunningTasks        int `json:"running_tasks"`
	CompletedTasksToday int `json:"completed_tasks_today"`
	FailedTasksToday    int `json:"failed_tasks_today"`
}

// LogToolCall represents a structured tool call in a log message
type LogToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// LogMessage is a single message from an agent session log.
type LogMessage struct {
	ID          string        `json:"id"`
	Role        string        `json:"role"`
	Content     string        `json:"content"`
	Reasoning   string        `json:"reasoning,omitempty"`
	Model       *string       `json:"model,omitempty"`
	Provider    *string       `json:"provider,omitempty"`
	CreatedAt   int64         `json:"created_at"`
	FinishedAt  *int64        `json:"finished_at,omitempty"`
	MessageType *string       `json:"message_type,omitempty"`
	ToolCalls   []LogToolCall `json:"tool_calls,omitempty"`
	ToolCallID  *string       `json:"tool_call_id,omitempty"`
}

// LogSession is an agent session with its messages.
type LogSession struct {
	ID                  string       `json:"id"`
	Title               string       `json:"title"`
	PromptTokens        int          `json:"prompt_tokens"`
	CompletionTokens    int          `json:"completion_tokens"`
	CachedTokens        int          `json:"cached_tokens,omitempty"`
	CacheCreationTokens int          `json:"cache_creation_tokens,omitempty"`
	Cost                float64      `json:"cost"`
	CreatedAt           int64        `json:"created_at"`
	UpdatedAt           int64        `json:"updated_at"`
	Messages            []LogMessage `json:"messages"`
}

// MarshalJSON implements custom JSON marshaling for LogSession.
func (ls LogSession) MarshalJSON() ([]byte, error) {
	type Alias LogSession
	messages := ls.Messages
	if messages == nil {
		messages = []LogMessage{}
	}
	return json.Marshal(&struct {
		Alias
		Messages []LogMessage `json:"messages"`
	}{
		Alias:    Alias(ls),
		Messages: messages,
	})
}

// LogSubmission is a log submission for a task.
type LogSubmission struct {
	TaskID  uuid.UUID  `json:"task_id"`
	Session LogSession `json:"session"`
}

// MaxLogSubmissionSize is the maximum size of a log submission in bytes (24MB).
const MaxLogSubmissionSize = 24 * 1024 * 1024

// APIKeyCreate is the request model for creating an API key.
type APIKeyCreate struct {
	Name                string   `json:"name"`
	AllowedNodePatterns []string `json:"allowed_node_patterns"`
	Role                *string  `json:"role,omitempty"`
	RateLimit           int      `json:"rate_limit"`
	ExpiresInDays       *int     `json:"expires_in_days,omitempty"`
	Description         string   `json:"description"`
}

// APIKeyResponse is the response model for API key operations.
type APIKeyResponse struct {
	KeyID               string     `json:"key_id"`
	Name                string     `json:"name"`
	KeyPrefix           string     `json:"key_prefix"`
	AllowedNodePatterns []string   `json:"allowed_node_patterns"`
	Permissions         []string   `json:"permissions"`
	RateLimit           int        `json:"rate_limit"`
	CreatedAt           time.Time  `json:"created_at"`
	RotatedAt           *time.Time `json:"rotated_at,omitempty"`
	ExpiresAt           *time.Time `json:"expires_at,omitempty"`
	Enabled             bool       `json:"enabled"`
	Description         string     `json:"description"`
}

// APIKeyCreated is the response model when a new key is created.
type APIKeyCreated struct {
	APIKeyResponse
	APIKey string `json:"api_key"`
}

// APIKeyRotated is the response model when a key is rotated.
type APIKeyRotated struct {
	APIKeyResponse
	APIKey           string `json:"api_key"`
	GracePeriodHours int    `json:"grace_period_hours"`
}

// AuditLogResponse is the response model for audit log entries.
type AuditLogResponse struct {
	Entries []map[string]interface{} `json:"entries"`
	Total   int                      `json:"total"`
}

// HealthResponse is the health check response.
type HealthResponse struct {
	Status    string `json:"status"`
	Version   string `json:"version"`
	Timestamp string `json:"timestamp"`
}

// CleanupResponse is the response for task cleanup.
type CleanupResponse struct {
	DeletedCount int `json:"deleted_count"`
}

// BulkModelUpdate is the POST /tasks/model request: re-assign the pinned model
// (and optional fallback) across scheduled tasks. FromModel, when set, limits the
// change to tasks currently pinned to that slug (e.g. a deprecated model). DryRun
// returns the tasks that WOULD change without writing.
type BulkModelUpdate struct {
	Model         string `json:"model"`
	FallbackModel string `json:"fallback_model"`
	FromModel     string `json:"from_model"`
	DryRun        bool   `json:"dry_run"`
}

// BulkModelUpdateResult is the POST /tasks/model response. On a dry run it lists
// the matched tasks + count without writing; on a real run it reports how many
// were updated.
type BulkModelUpdateResult struct {
	DryRun       bool    `json:"dry_run"`
	UpdatedCount int     `json:"updated_count,omitempty"`
	MatchedCount int     `json:"matched_count,omitempty"`
	Tasks        []*Task `json:"tasks,omitempty"`
}

// DeleteNodeResponse is the response for node deletion.
type DeleteNodeResponse struct {
	Status string `json:"status"`
	NodeID string `json:"node_id"`
}

// DeleteKeyResponse is the response for API key deletion.
type DeleteKeyResponse struct {
	Deleted bool   `json:"deleted"`
	KeyID   string `json:"key_id"`
}

// ErrorResponse is the standard error response.
type ErrorResponse struct {
	Detail string `json:"detail"`
}

// FileInfo contains metadata about an uploaded file including its checksum.
type FileInfo struct {
	Filename string `json:"filename"`
	Checksum string `json:"checksum"`
	Size     int64  `json:"size"`
}

// PaginatedResponse wraps paginated results with metadata.
type PaginatedResponse struct {
	Data   interface{} `json:"data"`
	Total  int         `json:"total"`
	Limit  int         `json:"limit"`
	Offset int         `json:"offset"`
}
