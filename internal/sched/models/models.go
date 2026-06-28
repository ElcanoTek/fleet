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
	"fmt"
	"regexp"
	"strings"
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

// CredentialAllowlistEntry names one permitted (server, account) MCP pair for a
// task. Account=="" means the default/shared seat only. Like MCPChoice it
// mirrors agentcore.CredentialAllowlistEntry byte-for-byte at the JSON level so
// a task's allowlist hands straight to the run loop's Gate-3, and is mirrored
// here (rather than imported) to keep the sched data layer free of an agentcore
// dependency.
type CredentialAllowlistEntry struct {
	Server  string `json:"server"`
	Account string `json:"account,omitempty"`
}

// CredentialAllowlist is the per-task list of permitted (server, account) pairs.
//
//   - nil           → no restriction (inherit global; the prior behaviour).
//   - non-nil empty → deny ALL MCP calls.
//   - populated     → only the listed pairs are permitted.
//
// The nil-vs-empty distinction is load-bearing, so it is stored as a NULLABLE
// JSONB column (NULL ⇒ nil) rather than coerced to an empty array.
type CredentialAllowlist []CredentialAllowlistEntry

// DefaultLoopMaxIterations bounds a loop whose config omits MaxIterations.
const DefaultLoopMaxIterations = 5

// LoopConfig, when non-nil, turns a scheduled task into an iterative
// worker+verifier loop (#179). Each iteration runs the worker agent to
// completion, then evaluates the exit condition; if it passes the loop
// succeeds, if it fails and iterations remain the worker is re-run with the
// prior output appended as context. nil = an ordinary one-shot task.
type LoopConfig struct {
	// MaxIterations caps the number of worker+verify cycles (<=0 →
	// DefaultLoopMaxIterations).
	MaxIterations int `json:"max_iterations"`
	// ExitCondition selects the pass/fail evaluation for each iteration:
	//   "shell:<cmd>" — run <cmd> in the task sandbox; exit 0 = pass
	//   "llm"         — ask VerifierModel the VerifierPrompt; YES = pass
	//   "regex:<pat>" — match <pat> against the last assistant message; match = pass
	ExitCondition string `json:"exit_condition"`
	// VerifierModel is the OpenRouter slug for the "llm" exit; empty → the task's
	// fallback model.
	VerifierModel string `json:"verifier_model,omitempty"`
	// VerifierPrompt is the YES/NO prompt for the "llm" exit. The worker's last
	// assistant message is appended as context automatically.
	VerifierPrompt string `json:"verifier_prompt,omitempty"`
	// TimeBudgetSeconds is an absolute wall-clock deadline across all iterations
	// (0 = no deadline).
	TimeBudgetSeconds int `json:"time_budget_seconds,omitempty"`
	// MaxCostUSD caps the accumulated cost across all iterations, checked BEFORE
	// each iteration (0 = no ceiling). Mirrors the per-run cost ceiling, applied
	// across runs.
	MaxCostUSD float64 `json:"max_cost_usd,omitempty"`
}

// ValidateExitCondition checks the loop's exit condition is a recognized form
// (shell:<cmd> / regex:<pattern> / llm) and that a regex: pattern compiles, so a
// statically-unsatisfiable config is rejected at task creation rather than
// burning the full iteration + cost budget only to always exhaust at runtime.
func (lc *LoopConfig) ValidateExitCondition() error {
	cond := strings.TrimSpace(lc.ExitCondition)
	switch {
	case cond == "llm":
		return nil
	case strings.HasPrefix(cond, "shell:"):
		if strings.TrimSpace(strings.TrimPrefix(cond, "shell:")) == "" {
			return fmt.Errorf("shell: exit_condition requires a command")
		}
		return nil
	case strings.HasPrefix(cond, "regex:"):
		if _, err := regexp.Compile(strings.TrimPrefix(cond, "regex:")); err != nil {
			return fmt.Errorf("invalid regex exit_condition: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("exit_condition must be one of shell:<cmd>, regex:<pattern>, or llm (got %q)", lc.ExitCondition)
	}
}

// DefaultWorktreeBranchPrefix is the branch-name prefix used when a
// WorktreeConfig leaves BranchPrefix empty. Per-run branches are
// "<prefix><task_id>-<run_id>", deterministic and unique per run so concurrent
// tasks on the same repo never collide.
const DefaultWorktreeBranchPrefix = "fleet/task-"

// WorktreeConfig, when non-nil with Enabled set, gives each scheduled run its
// own git worktree + branch so concurrent tasks targeting the same repository
// cannot corrupt each other's working tree (#180). The task's workspace must be
// the root of a git repository.
//
// IMPORTANT (implementation note that the schema deliberately encodes): the
// worktree is created as a SUBDIRECTORY of the workspace root, not at an
// arbitrary /tmp path. A git worktree's .git is a file pointing back to
// "<mainrepo>/.git/worktrees/<name>"; git only works if BOTH the worktree and
// the main repo are reachable at their host absolute paths inside the sandbox.
// The sandbox bind-mounts the workspace root at the same absolute path, so a
// subdir of it satisfies that linkage; a standalone /tmp worktree would not.
type WorktreeConfig struct {
	// Enabled turns per-run worktree isolation on. A non-nil config with
	// Enabled=false is an explicit "off" (distinct from nil = never configured),
	// which lets an edit disable isolation without dropping other fields.
	Enabled bool `json:"enabled"`
	// BaseBranch is the ref the worktree branches from (e.g. "main"); empty =
	// the repository's current HEAD.
	BaseBranch string `json:"base_branch,omitempty"`
	// BranchPrefix prefixes the per-run branch name; empty →
	// DefaultWorktreeBranchPrefix.
	BranchPrefix string `json:"branch_prefix,omitempty"`
	// AutoCleanup removes the worktree (and its branch) after the run. When
	// false the worktree is left in place for inspection / manual push, to be
	// reclaimed later by `fleet-admin worktree prune`.
	AutoCleanup bool `json:"auto_cleanup"`
	// CleanupDelaySeconds delays the post-run `git worktree remove` by this many
	// seconds (0 = remove immediately). Only consulted when AutoCleanup is set.
	CleanupDelaySeconds int `json:"cleanup_delay_seconds,omitempty"`
}

// Validate checks the worktree config is internally consistent so a
// statically-broken config is rejected at task creation rather than failing
// every run. A nil/disabled config is always valid.
func (wc *WorktreeConfig) Validate() error {
	if wc == nil || !wc.Enabled {
		return nil
	}
	if wc.CleanupDelaySeconds < 0 {
		return fmt.Errorf("cleanup_delay_seconds must be >= 0")
	}
	// A branch prefix is interpolated into a git ref ("<prefix><uuid>-<run>"), so
	// reject the common invalid forms up front: characters git forbids in ref
	// components (space, ~, ^, :, ?, *, [, \), the "@{" sequence, the ".." and
	// "//" sequences, and a ".lock" substring (a ref component may not end in
	// .lock). This catches the misconfigurations that would otherwise fail the
	// worktree-add at run time; git still makes the authoritative check.
	if strings.ContainsAny(wc.BranchPrefix, " ~^:?*[\\") ||
		strings.Contains(wc.BranchPrefix, "@{") ||
		strings.Contains(wc.BranchPrefix, "..") ||
		strings.Contains(wc.BranchPrefix, "//") ||
		strings.Contains(wc.BranchPrefix, ".lock") {
		return fmt.Errorf("branch_prefix is not a valid git ref-name fragment")
	}
	return nil
}

// Tag constraints (#212): keep tags human-typeable, URL-safe, and bounded so the
// catalogue + filter stay cheap and the column can't be abused to bloat a row.
const (
	MaxTagLength    = 64
	MaxTagsPerTask  = 20
	tagAllowedChars = "abcdefghijklmnopqrstuvwxyz0123456789-."
)

// NormalizeAndValidateTags lowercases + trims each tag, drops blanks, deduplicates
// (preserving first-seen order), and enforces the per-tag format (lowercase
// alphanumeric plus '-' and '.', ≤MaxTagLength) and the ≤MaxTagsPerTask count. It
// returns the cleaned slice (nil when empty) so callers persist a canonical form.
func NormalizeAndValidateTags(tags []string) ([]string, error) {
	if len(tags) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))
	for _, raw := range tags {
		t := strings.ToLower(strings.TrimSpace(raw))
		if t == "" {
			continue
		}
		if len(t) > MaxTagLength {
			return nil, fmt.Errorf("tag %q exceeds %d characters", t, MaxTagLength)
		}
		for _, r := range t {
			if !strings.ContainsRune(tagAllowedChars, r) {
				return nil, fmt.Errorf("tag %q contains invalid character %q (allowed: a-z, 0-9, '-', '.')", t, r)
			}
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	if len(out) > MaxTagsPerTask {
		return nil, fmt.Errorf("too many tags: %d (max %d)", len(out), MaxTagsPerTask)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// Iteration status values recorded in task_iterations.status.
const (
	IterationStatusRunning = "running"
	IterationStatusPassed  = "passed"
	IterationStatusFailed  = "failed"
	IterationStatusStopped = "stopped"
)

// TaskIteration is one worker+verify cycle of a looped task (#179), recorded for
// per-iteration telemetry. It is the Go analogue of the task_iterations row.
type TaskIteration struct {
	ID                  uuid.UUID  `json:"id"`
	TaskID              uuid.UUID  `json:"task_id"`
	IterationNumber     int        `json:"iteration_number"`
	StartedAt           time.Time  `json:"started_at"`
	CompletedAt         *time.Time `json:"completed_at,omitempty"`
	WorkerSessionID     string     `json:"worker_session_id,omitempty"`
	ExitConditionResult string     `json:"exit_condition_result,omitempty"`
	CostUSD             float64    `json:"cost_usd"`
	PromptTokens        int64      `json:"prompt_tokens"`
	CompletionTokens    int64      `json:"completion_tokens"`
	Status              string     `json:"status"`
}

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

// TriggerType distinguishes how a task is fired (#177).
type TriggerType string

const (
	// TriggerTypeCron is the default: the scheduler promotes the task when its
	// scheduled_for / recurrence is due.
	TriggerTypeCron TriggerType = "cron"
	// TriggerTypeWebhook marks a TEMPLATE task that the cron engine never runs.
	// It sits inert (status=scheduled, scheduled_for=NULL) until an authenticated
	// POST /triggers/{slug} spawns a fresh one-shot run cloned from it.
	TriggerTypeWebhook TriggerType = "webhook"
)

// IsValid reports whether t is a recognized trigger type.
func (t TriggerType) IsValid() bool {
	switch t {
	case TriggerTypeCron, TriggerTypeWebhook:
		return true
	default:
		return false
	}
}

// TaskTrigger binds a URL-safe slug + HMAC-SHA256 secret to a template task so
// external systems can spawn runs via POST /triggers/{slug} (#177). The secret
// is the per-trigger webhook credential — never the admin API key.
type TaskTrigger struct {
	ID             uuid.UUID `json:"id"`
	TaskID         uuid.UUID `json:"task_id"`
	Slug           string    `json:"slug"`
	Secret         string    `json:"secret"`
	PromptTemplate string    `json:"prompt_template"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
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
	Prompt        string       `json:"prompt"`
	Model         *string      `json:"model,omitempty"`
	FallbackModel *string      `json:"fallback_model,omitempty"`
	MaxIterations *int         `json:"max_iterations,omitempty"`
	MCPSelection  MCPSelection `json:"mcp_selection,omitempty"`
	// CredentialAllowlist restricts which (server, account) pairs this task may
	// call. nil inherits global (current behaviour); set an explicit list to
	// enforce least-privilege credential scoping. See CredentialAllowlist.
	CredentialAllowlist CredentialAllowlist `json:"credential_allowlist"`
	// LoopConfig, when non-nil, turns this task into an iterative worker+verifier
	// loop (#179). nil = ordinary one-shot task. See LoopConfig.
	LoopConfig *LoopConfig `json:"loop_config,omitempty"`
	// WorktreeConfig, when non-nil with Enabled, gives each run its own git
	// worktree + branch for filesystem isolation (#180). nil = shared workspace
	// (current behaviour). See WorktreeConfig.
	WorktreeConfig         *WorktreeConfig `json:"worktree_config,omitempty"`
	Priority               int             `json:"priority"`
	InstructionSelfImprove bool            `json:"instruction_self_improve,omitempty"`
	ScheduledFor           *time.Time      `json:"scheduled_for,omitempty"`
	Recurrence             string          `json:"recurrence,omitempty"`
	Files                  []string        `json:"files,omitempty"`
	// Tags are user-defined labels for organizing and filtering tasks (#212):
	// lowercase alphanumeric + '-'/'.', ≤64 chars each, ≤20 per task. Normalized
	// and validated at create/edit. nil/empty = untagged.
	Tags []string `json:"tags,omitempty"`
	// MaxRetries is the number of ADDITIONAL whole-task attempts after the first
	// when a run fails cleanly with a transient error. 0 (default) = no retries.
	MaxRetries *int `json:"max_retries,omitempty"`
	// AllowNetwork lets THIS scheduled task's bash/run_python execution sandbox
	// keep outbound egress. The default (false) seals the sandbox with
	// --network=none, matching the interactive lockdown path; egress is an
	// explicit opt-in for the tasks that genuinely need it.
	AllowNetwork bool `json:"allow_network,omitempty"`
	// Description is optional operator documentation for this task (#281):
	// free-form Markdown (why the task exists, cost, side effects, runbook,
	// owner). Empty = none. Distinct from the shared agent-notes wiki; never
	// injected into agent prompts. Capped at maxTaskDescriptionChars at creation.
	Description string `json:"description,omitempty"`
	// Timezone is the IANA timezone name (e.g. "America/New_York") used to
	// evaluate Recurrence in the task owner's local time. Empty falls back to
	// the server's FLEET_DEFAULT_TIMEZONE (then "UTC") at create time. The cron
	// expression fires at the wall-clock time in THIS zone; the resulting
	// scheduled_for instant is always stored in UTC.
	Timezone string `json:"timezone,omitempty"`
	// TriggerType selects how the task is fired (#177). Empty / "cron" is the
	// default cron-cadence behavior. "webhook" makes this a template task the
	// cron engine never runs: it sits inert until POST /triggers/{slug} spawns a
	// run cloned from it.
	TriggerType TriggerType `json:"trigger_type,omitempty"`
}

// Task represents a task to be executed by a worker.
type Task struct {
	ID            uuid.UUID    `json:"id"`
	Prompt        string       `json:"prompt"`
	Model         *string      `json:"model,omitempty"`
	FallbackModel *string      `json:"fallback_model,omitempty"`
	MaxIterations *int         `json:"max_iterations,omitempty"`
	MCPSelection  MCPSelection `json:"mcp_selection"`
	// CredentialAllowlist restricts which (server, account) pairs this task may
	// call. nil = inherit global. See TaskCreate.CredentialAllowlist.
	CredentialAllowlist CredentialAllowlist `json:"credential_allowlist"`
	// LoopConfig, when non-nil, runs this task as an iterative worker+verifier
	// loop (#179). nil = ordinary one-shot. See LoopConfig.
	LoopConfig *LoopConfig `json:"loop_config,omitempty"`
	// WorktreeConfig, when non-nil with Enabled, runs each occurrence in its own
	// git worktree + branch (#180). nil = shared workspace. See WorktreeConfig.
	WorktreeConfig         *WorktreeConfig `json:"worktree_config,omitempty"`
	Priority               int             `json:"priority"`
	InstructionSelfImprove bool            `json:"instruction_self_improve,omitempty"`
	// AllowNetwork controls whether this task's execution sandbox keeps outbound
	// egress. Default false seals it (--network=none); see TaskCreate.AllowNetwork.
	AllowNetwork bool `json:"allow_network,omitempty"`
	// Description is optional operator documentation (#281). See TaskCreate.Description.
	Description    string     `json:"description,omitempty"`
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
	// Timezone is the IANA timezone the cron Recurrence is evaluated in. Always
	// present in responses ("UTC" for legacy/unset tasks). See TaskCreate.Timezone.
	Timezone string `json:"timezone"`
	// CreatedByKeyID is the scoped API key (if any) that submitted this task, set
	// server-side at creation so the completion path can attribute cost back to
	// the key for spending caps. Persisted; not settable by clients.
	CreatedByKeyID *string `json:"created_by_key_id,omitempty"`
	// NextRunAtLocal is ScheduledFor rendered in Timezone (RFC3339 with offset),
	// populated at query time for display so callers need no client-side tz math.
	// Not persisted; nil when the task has no scheduled_for.
	NextRunAtLocal *string    `json:"next_run_at_local,omitempty"`
	CreatedBy      *uuid.UUID `json:"created_by,omitempty"`
	Files          []string   `json:"files,omitempty"`
	// Tags are user-defined organizing labels (#212). See TaskCreate.Tags.
	Tags           []string   `json:"tags,omitempty"`
	LeaseOwner     *string    `json:"lease_owner,omitempty"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
	// AttemptCount is how many times this task has been re-queued after a clean,
	// transient failure (0 on the first run). MaxRetries caps it: the task may run
	// up to MaxRetries+1 times before a failure is terminal.
	AttemptCount int `json:"attempt_count"`
	MaxRetries   int `json:"max_retries"`
	// CreatedByUsername is populated at query time for display purposes (not persisted)
	CreatedByUsername *string `json:"created_by_username,omitempty"`
	// TriggerType is how this task is fired: "cron" (default) or "webhook". A
	// webhook task is an inert template; see TaskCreate.TriggerType.
	TriggerType TriggerType `json:"trigger_type"`
}

// NewTask creates a new Task with defaults.
func NewTask(tc TaskCreate) *Task {
	triggerType := tc.TriggerType
	if triggerType == "" {
		triggerType = TriggerTypeCron
	}

	status := TaskStatusPending
	if tc.ScheduledFor != nil && tc.ScheduledFor.After(time.Now()) {
		status = TaskStatusScheduled
	}
	// A webhook template is never run by the cron engine. Park it inert as a
	// scheduled task with no scheduled_for: GetScheduledTasks requires
	// scheduled_for IS NOT NULL, so it is never promoted; firing the webhook
	// spawns a fresh cron-type run cloned from it.
	if triggerType == TriggerTypeWebhook {
		status = TaskStatusScheduled
	}

	tz := tc.Timezone
	if tz == "" {
		tz = "UTC"
	}

	return &Task{
		ID:                     uuid.New(),
		Prompt:                 tc.Prompt,
		Model:                  tc.Model,
		FallbackModel:          tc.FallbackModel,
		MaxIterations:          tc.MaxIterations,
		MCPSelection:           tc.MCPSelection,
		CredentialAllowlist:    tc.CredentialAllowlist,
		LoopConfig:             tc.LoopConfig,
		WorktreeConfig:         tc.WorktreeConfig,
		Priority:               tc.Priority,
		InstructionSelfImprove: tc.InstructionSelfImprove,
		AllowNetwork:           tc.AllowNetwork,
		Description:            tc.Description,
		Status:                 status,
		CreatedAt:              time.Now().UTC(),
		ScheduledFor:           tc.ScheduledFor,
		Recurrence:             tc.Recurrence,
		Timezone:               tz,
		Files:                  tc.Files,
		Tags:                   tc.Tags,
		MaxRetries:             derefOr(tc.MaxRetries, 0),
		TriggerType:            triggerType,
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
	TaskID                 uuid.UUID           `json:"task_id"`
	Prompt                 string              `json:"prompt"`
	Model                  *string             `json:"model,omitempty"`
	FallbackModel          *string             `json:"fallback_model,omitempty"`
	MaxIterations          *int                `json:"max_iterations,omitempty"`
	MCPSelection           MCPSelection        `json:"mcp_selection,omitempty"`
	CredentialAllowlist    CredentialAllowlist `json:"credential_allowlist"`
	InstructionSelfImprove bool                `json:"instruction_self_improve,omitempty"`
	OrchestratorURL        string              `json:"orchestrator_url"`
	Files                  []string            `json:"files,omitempty"`
	FileChecksums          []string            `json:"file_checksums,omitempty"`
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
	// Spending caps (nil = unlimited).
	MaxCostPerDayUSD   *float64 `json:"max_cost_per_day_usd,omitempty"`
	MaxCostPerMonthUSD *float64 `json:"max_cost_per_month_usd,omitempty"`
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
	// Spending caps + live accumulators (nil cap = unlimited).
	MaxCostPerDayUSD   *float64 `json:"max_cost_per_day_usd,omitempty"`
	MaxCostPerMonthUSD *float64 `json:"max_cost_per_month_usd,omitempty"`
	CostTodayUSD       float64  `json:"cost_today_usd"`
	CostThisMonthUSD   float64  `json:"cost_this_month_usd"`
}

// APIKeySpending is the GET /keys/{id}/spending response: current spend vs caps
// with the next reset instants.
type APIKeySpending struct {
	KeyID              string    `json:"key_id"`
	CostTodayUSD       float64   `json:"cost_today_usd"`
	MaxCostPerDayUSD   *float64  `json:"max_cost_per_day_usd,omitempty"`
	CostThisMonthUSD   float64   `json:"cost_this_month_usd"`
	MaxCostPerMonthUSD *float64  `json:"max_cost_per_month_usd,omitempty"`
	DailyResetAt       time.Time `json:"daily_reset_at"`
	MonthlyResetAt     time.Time `json:"monthly_reset_at"`
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
