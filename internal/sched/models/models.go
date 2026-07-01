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

// RunIfOnErrorRun and RunIfOnErrorSkip are the two recognized on_error values
// for a RunIf gate (#269): "run" runs the task anyway when the check itself
// errors (the safe default), "skip" skips the task when the check errors.
const (
	RunIfOnErrorRun  = "run"
	RunIfOnErrorSkip = "skip"
)

// RunIf is the optional pre-run shell gate for a scheduled task (#269): when
// set, the scheduler evaluates Command on the host (NOT in the sandbox — it is
// a lightweight gate, not an agent tool call) before promoting a due task. The
// task is promoted only when the command exits with ExitCodeIs; otherwise it is
// skipped (next_run_at advances, skip_count increments). nil = the legacy
// unconditional promotion path; existing tasks are unaffected.
//
// IMPORTANT (security note encoded in the schema): the check runs on the host
// as the fleet process user with a restricted PATH. It is NOT a sandboxed agent
// tool call — by design, so a misconfigured check cannot burn a model budget or
// touch MCP credentials — but it DOES mean a run_if command has the host-user
// privileges of the fleet process. Operators must treat run_if commands as
// trusted, exactly like the fleet binary itself; the validation path rejects
// empty commands but does not sandbox them.
type RunIf struct {
	// Command is the shell command evaluated by `sh -c`. Must be non-empty when
	// RunIf is present. Bash-specific constructs are NOT guaranteed — use POSIX sh.
	Command string `json:"command"`
	// ExitCodeIs is the exit code that means "run the task". Default 0.
	ExitCodeIs int `json:"exit_code_is,omitempty"`
	// TimeoutSeconds is the hard wall-clock timeout for the check, enforced via
	// exec.CommandContext. Clamped to [1, 300] at validation; default 30.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// OnError governs the check-itself-errored case (timeout, crash, signal):
	//   "run"  (default) — run the task anyway (safe default)
	//   "skip"           — skip the task
	OnError string `json:"on_error,omitempty"`
}

// Validate rejects a statically-broken RunIf at task creation so a misconfigured
// gate fails fast rather than always-skipping or always-running at runtime. A
// nil RunIf is always valid (the legacy unconditional path).
func (r *RunIf) Validate() error {
	if r == nil {
		return nil
	}
	if strings.TrimSpace(r.Command) == "" {
		return fmt.Errorf("run_if.command must be non-empty")
	}
	if r.TimeoutSeconds < 1 || r.TimeoutSeconds > 300 {
		return fmt.Errorf("run_if.timeout_seconds must be between 1 and 300")
	}
	switch r.OnError {
	case "", RunIfOnErrorRun, RunIfOnErrorSkip:
	default:
		return fmt.Errorf("run_if.on_error must be %q or %q (got %q)", RunIfOnErrorRun, RunIfOnErrorSkip, r.OnError)
	}
	return nil
}

// EffectiveOnError returns the resolved on_error policy, defaulting to "run"
// (the safe default — run the task anyway when the check itself errors).
func (r *RunIf) EffectiveOnError() string {
	if r == nil || r.OnError == "" {
		return RunIfOnErrorRun
	}
	return r.OnError
}

// EffectiveTimeoutSeconds returns the resolved timeout, defaulting to 30s.
func (r *RunIf) EffectiveTimeoutSeconds() int {
	if r == nil || r.TimeoutSeconds <= 0 {
		return 30
	}
	return r.TimeoutSeconds
}

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

// TaskSandboxLimits overrides the global FLEET_SANDBOX_* cgroup ceilings for a
// single task's execution container (#205). Each field is optional; a zero value
// means "use the global default". Stored as the nullable sandbox_limits JSONB
// column (NULL = all-global), and applied by the scheduled runner when it
// cold-starts the task's container — a tightening of the mandatory sandbox, never
// a way to escape it.
type TaskSandboxLimits struct {
	MemoryMB int     `json:"memory_mb,omitempty"` // container memory ceiling in MiB (e.g. 2048)
	CPUs     float64 `json:"cpus,omitempty"`      // fractional CPU ceiling (e.g. 2.0)
	Pids     int     `json:"pids,omitempty"`      // max process IDs (e.g. 512)
}

// IsZero reports whether no override is set (every field is the zero value), so a
// caller can treat an all-zero struct the same as nil (#205).
func (l *TaskSandboxLimits) IsZero() bool {
	return l == nil || (l.MemoryMB == 0 && l.CPUs == 0 && l.Pids == 0)
}

// Failure classes (#201): the vocabulary the runner classifies a clean run
// failure into, and that a RetryPolicy's retry_on/no_retry_on lists reference.
// Only classes backed by a distinct agentcore sentinel today are
// distinguishable; everything else is FailureTerminal. (Finer classes —
// timeout / governance / validation — need new agentcore sentinels: a follow-up.)
const (
	FailureTransient     = "transient"      // retry budget / stream blip — a fresh run may succeed
	FailureCostCeiling   = "cost_ceiling"   // cost/token ceiling hit — will recur; default no-retry
	FailureContextBudget = "context_budget" // context exhausted after compaction — default no-retry
	FailureTerminal      = "terminal"       // unknown / deterministic — never retried
)

// retryFailureClasses is the set a retry_on/no_retry_on list may name.
var retryFailureClasses = map[string]struct{}{
	FailureTransient: {}, FailureCostCeiling: {}, FailureContextBudget: {}, FailureTerminal: {},
}

// Backoff strategies + defaults for RetryPolicy (#201).
const (
	BackoffExponential              = "exponential"
	BackoffFixed                    = "fixed"
	DefaultRetryInitialDelaySeconds = 30
	DefaultRetryMaxDelaySeconds     = 600
)

// RetryPolicy holds per-task retry backoff knobs and failure-class gating (#201).
// nil → the legacy policy: transient failures only, 30s→10m exponential backoff.
// The retry COUNT is Task.MaxRetries (NOT duplicated here, to keep one source of
// truth); this policy governs the DELAY and WHICH failure classes retry.
type RetryPolicy struct {
	// Backoff is "exponential" (default) or "fixed".
	Backoff string `json:"backoff,omitempty"`
	// InitialDelaySeconds is the first retry delay (default 30). For exponential it
	// doubles per attempt; for fixed it is used every attempt.
	InitialDelaySeconds int `json:"initial_delay_seconds,omitempty"`
	// MaxDelaySeconds caps the delay (default 600).
	MaxDelaySeconds int `json:"max_delay_seconds,omitempty"`
	// RetryOn lists failure classes that DO trigger a retry. nil → [transient].
	RetryOn []string `json:"retry_on,omitempty"`
	// NoRetryOn lists failure classes that block a retry regardless of RetryOn.
	NoRetryOn []string `json:"no_retry_on,omitempty"`
}

// Validate rejects a statically-broken retry policy at task creation.
func (rp *RetryPolicy) Validate() error {
	if rp == nil {
		return nil
	}
	switch rp.Backoff {
	case "", BackoffExponential, BackoffFixed:
	default:
		return fmt.Errorf("backoff must be %q or %q (got %q)", BackoffExponential, BackoffFixed, rp.Backoff)
	}
	if rp.InitialDelaySeconds < 0 || rp.MaxDelaySeconds < 0 {
		return fmt.Errorf("retry delays must be >= 0")
	}
	if rp.MaxDelaySeconds > 0 && rp.InitialDelaySeconds > rp.MaxDelaySeconds {
		return fmt.Errorf("initial_delay_seconds (%d) cannot exceed max_delay_seconds (%d)", rp.InitialDelaySeconds, rp.MaxDelaySeconds)
	}
	for _, list := range [][]string{rp.RetryOn, rp.NoRetryOn} {
		for _, c := range list {
			if _, ok := retryFailureClasses[c]; !ok {
				return fmt.Errorf("unknown failure class %q (allowed: transient, cost_ceiling, context_budget, terminal)", c)
			}
		}
	}
	return nil
}

// ShouldRetryClass decides whether a failure of the given class should retry
// under this policy. NoRetryOn wins; then RetryOn (nil → only transient).
// A nil policy defaults to "transient retries, nothing else" — today's behavior.
func (rp *RetryPolicy) ShouldRetryClass(class string) bool {
	if rp == nil {
		return class == FailureTransient
	}
	for _, c := range rp.NoRetryOn {
		if c == class {
			return false
		}
	}
	if rp.RetryOn == nil {
		return class == FailureTransient
	}
	for _, c := range rp.RetryOn {
		if c == class {
			return true
		}
	}
	return false
}

// EffectiveBackoff returns the resolved (initialSeconds, maxSeconds, exponential)
// for this policy, applying defaults. A nil policy yields the legacy 30s/600s
// exponential values.
func (rp *RetryPolicy) EffectiveBackoff() (initialSeconds, maxSeconds int, exponential bool) {
	initialSeconds, maxSeconds, exponential = DefaultRetryInitialDelaySeconds, DefaultRetryMaxDelaySeconds, true
	if rp == nil {
		return
	}
	if rp.InitialDelaySeconds > 0 {
		initialSeconds = rp.InitialDelaySeconds
	}
	if rp.MaxDelaySeconds > 0 {
		maxSeconds = rp.MaxDelaySeconds
	}
	if rp.Backoff == BackoffFixed {
		exponential = false
	}
	if maxSeconds < initialSeconds {
		maxSeconds = initialSeconds
	}
	return
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
	// TaskStatusDeadLettered is the dead-letter-queue terminal state (#253): a task
	// that exhausted its retry budget on a transient failure, OR failed with a
	// non-retryable error, is routed here instead of bare `error` so operators can
	// review and replay it. It is distinct from `error`, which still covers
	// per-attempt / interrupted / panicked failures that are NOT final. The runner
	// is the ONLY writer of this status (workers cannot self-report it).
	TaskStatusDeadLettered TaskStatus = "dead_lettered"
)

// IsValidReportedStatus reports whether s is a status a worker is allowed to
// report for its own task. The orchestrator owns the rest of the lifecycle.
// TaskStatusDeadLettered is intentionally excluded: only the runner's terminal
// switch quarantines a task (#253), never a self-reporting worker.
func (s TaskStatus) IsValidReportedStatus() bool {
	switch s {
	case TaskStatusLeased, TaskStatusRunning, TaskStatusAnalyzing, TaskStatusSuccess, TaskStatusError:
		return true
	default:
		return false
	}
}

// IsTerminal reports whether s is a final state a task will not leave on its own
// — success, error, cancelled, or dead-lettered. Used to tell "result not ready
// yet" apart from "no result will ever come" (#244).
func (s TaskStatus) IsTerminal() bool {
	switch s {
	case TaskStatusSuccess, TaskStatusError, TaskStatusCancelled, TaskStatusDeadLettered:
		return true
	default:
		return false
	}
}

// Task priority (#230). Convention: LOWER integer = HIGHER urgency, matching
// POSIX nice / ionice and most job schedulers. The pending queue is claimed in
// ascending effective_priority, then FIFO (created_at ASC) within a tier.
//
// Two columns back this: Priority is the immutable value the submitter asked
// for; EffectivePriority is what the scheduler actually orders by. They are
// equal at creation; only the anti-starvation sweep lowers EffectivePriority
// (never Priority), so a long-waiting low-urgency task eventually gets claimed
// without rewriting what the operator requested.
const (
	// PriorityMin / PriorityMax bound both columns (enforced by a DB CHECK and by
	// validateTaskLimits). 0 is reserved as the "unset" sentinel: NewTask maps a
	// zero-value priority to PriorityNormal, so the smallest value a caller can
	// explicitly request and have honored as-is is 1 (more urgent than Critical).
	PriorityMin = 0
	PriorityMax = 100

	// Named tiers — documented reference points on the 0–100 scale. The API
	// accepts any in-range integer; these name the conventional values.
	PriorityCritical = 10 // immediate interruption of batch work
	PriorityHigh     = 25
	PriorityNormal   = 50 // the default applied to an unset (zero-value) priority
	PriorityLow      = 75
	PriorityBulk     = 90

	// StarvationFloorPriority is the most-urgent value the anti-starvation sweep
	// will promote a waiting task to. It deliberately stops at High (never
	// Critical) so relief for a starving batch task can never preempt genuinely
	// critical work. Only tasks whose Priority AND current EffectivePriority are
	// both LESS urgent than this floor are eligible for promotion.
	StarvationFloorPriority = PriorityHigh
)

// NormalizePriority maps a submitted priority to the value actually stored:
// the zero value (unset) becomes PriorityNormal; everything else is returned
// unchanged. Bounds are enforced separately at validation (#230).
func NormalizePriority(p int) int {
	if p == 0 {
		return PriorityNormal
	}
	return p
}

// QueuePriorityBucket is the pending-task count and longest wait at a single
// effective_priority value (#230) — the raw per-priority rollup the queue-stats
// endpoint aggregates into named tiers.
type QueuePriorityBucket struct {
	Priority         int
	Count            int
	OldestAgeSeconds int
}

// QueueTierStat is the pending depth and longest wait for one named priority
// tier, returned by GET /admin/queue (#230).
type QueueTierStat struct {
	Tier             string `json:"tier"`
	MinPriority      int    `json:"min_priority"`
	MaxPriority      int    `json:"max_priority"`
	Count            int    `json:"count"`
	OldestAgeSeconds int    `json:"oldest_age_seconds"`
}

// QueueStats is the operator's view of the pending queue (#230): total depth,
// the oldest pending wait overall, and the depth + oldest wait per tier, so
// backlog and starvation are visible at a glance.
type QueueStats struct {
	PendingTotal     int             `json:"pending_total"`
	OldestAgeSeconds int             `json:"oldest_age_seconds"`
	Tiers            []QueueTierStat `json:"tiers"`
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
	PermissionViewLogs   Permission = "view_logs"
	PermissionManageKeys Permission = "manage_keys"
	PermissionAdmin      Permission = "admin"
)

// RolePermissions maps role names to their permission sets.
var RolePermissions = map[string][]Permission{
	"admin":    {PermissionAdmin},
	"client":   {PermissionCreateTask, PermissionViewTasks, PermissionViewLogs},
	"readonly": {PermissionViewTasks, PermissionViewLogs},
}

// TaskCreate is the request model for creating a new task.
type TaskCreate struct {
	// Name is an optional, human-readable label for the task (#238). It is the
	// identity key used by the task-definition import/export endpoints
	// (GET /tasks/export, POST /tasks/import) for conflict detection on import.
	// Empty = unnamed (the historical default); a non-empty name must be unique
	// across tasks (enforced by a partial DB unique index). Not injected into the
	// agent prompt; purely an operator convenience + import/export key.
	Name          string       `json:"name,omitempty"`
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
	WorktreeConfig *WorktreeConfig `json:"worktree_config,omitempty"`
	// SandboxLimits, when non-nil, overrides the global FLEET_SANDBOX_* cgroup
	// ceilings for this task's container (#205). nil = use the global defaults.
	SandboxLimits *TaskSandboxLimits `json:"sandbox_limits,omitempty"`
	// OutputSchema, when non-nil, enables structured-output mode (#244): it is a
	// draft-07 JSON Schema object the agent's final answer must conform to. The
	// scheduled driver appends the schema to the system prompt and, after the run,
	// validates the final text as JSON against it (storing the result in the
	// task's OutputJSON). Validated at create time (must compile). nil = free-form
	// text mode (the default).
	OutputSchema           json.RawMessage `json:"output_schema,omitempty"`
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
	// RetryPolicy customizes retry backoff + which failure classes retry (#201).
	// nil = legacy policy (transient-only, 30s→10m exponential). See RetryPolicy.
	RetryPolicy *RetryPolicy `json:"retry_policy,omitempty"`
	// AllowNetwork lets THIS scheduled task's bash/run_python execution sandbox
	// keep outbound egress. The default (false) seals the sandbox with
	// --network=none, matching the interactive lockdown path; egress is an
	// explicit opt-in for the tasks that genuinely need it.
	AllowNetwork bool `json:"allow_network,omitempty"`
	// AllowDelegation opts THIS task into agent delegation (#264): the spawn_subagent
	// native tool is registered so the run can fan out scoped subtasks to governed
	// child runs (sliced budget, depth/fan-out caps, parent_task_id linkage). The
	// default (false) registers nothing — byte-for-byte unchanged. It composes with
	// the fleet-wide FLEET_SUBAGENTS_ENABLED operator flag as OR (either enables it),
	// and is honoured ONLY in scheduled mode (delegation never enters interactive chat).
	AllowDelegation bool `json:"allow_delegation,omitempty"`
	// Persona is the optional per-task persona override (#221): a personas/<name>.yaml
	// (named without extension, e.g. "security-auditor") whose domain-expertise
	// block is injected into the system prompt. Empty = the runner's global
	// persona. An unknown name falls back to the global default at dispatch.
	Persona string `json:"persona,omitempty"`
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
	// AllowTaskCreation is the per-task capability bit that lets a SCHEDULED run
	// of THIS task spawn follow-up tasks via the built-in create_task tool (#277).
	// Default false: an interactive/unprivileged run never sees the tool, and a
	// scheduled run whose task did not opt in cannot self-schedule — there is no
	// privilege escalation and no unbounded self-scheduling loop. This is the
	// authority gate; the tool is registered ONLY when this is true.
	AllowTaskCreation bool `json:"allow_task_creation,omitempty"`
	// AllowRecurringTaskCreation is the stricter, separately-toggled governance
	// bit that additionally lets create_task set a cron recurrence on a spawned
	// task (#277). It is meaningless without AllowTaskCreation; with the parent
	// only granted AllowTaskCreation, create_task refuses any non-empty recurrence
	// so a single opt-in cannot mint an unbounded recurring fleet of tasks.
	AllowRecurringTaskCreation bool `json:"allow_recurring_task_creation,omitempty"`
	// CreatedByTaskID records the task whose scheduled run spawned this one via
	// create_task (#277), for audit/lineage. Set server-side by the create_task
	// tool, NOT by external clients (it is ignored on the public POST /tasks path).
	CreatedByTaskID *uuid.UUID `json:"created_by_task_id,omitempty"`
	// RunIf, when non-nil, is the pre-run shell gate (#269): the scheduler
	// evaluates Command on the host before promoting a due task and skips it when
	// the check fails. nil = the legacy unconditional promotion path. See RunIf.
	RunIf *RunIf `json:"run_if,omitempty"`
	// ExpectedDurationMinutes is the operator's expectation for typical runtime
	// (#274 SLA monitoring). Nil means no SLA is configured for this task; the
	// SLA monitor goroutine ignores it. When set, the warn/fail thresholds are
	// expected * SLAWarnMultiplier / expected * SLAFailMultiplier.
	ExpectedDurationMinutes *int `json:"expected_duration_minutes,omitempty"`
	// SLAWarnMultiplier scales ExpectedDurationMinutes to the WARN threshold.
	// Defaults to DefaultSLAWarnMultiplier (1.5) when a task carries an expected
	// duration but omits an explicit multiplier.
	SLAWarnMultiplier float64 `json:"sla_warn_multiplier,omitempty"`
	// SLAFailMultiplier scales ExpectedDurationMinutes to the FAIL threshold.
	// Defaults to DefaultSLAFailMultiplier (2.0); crossing it latches
	// SLABreached.
	SLAFailMultiplier float64 `json:"sla_fail_multiplier,omitempty"`
	// SLABreached is latched true once the fail threshold is crossed (#274). It
	// is set server-side by the SLA monitor, never by clients; cleared on
	// replay/re-run.
	SLABreached bool `json:"sla_breached,omitempty"`
	// ActualDurationSeconds is completed_at - started_at in whole seconds
	// (#274), populated server-side on the terminal transition. nil until the
	// task reaches a terminal status (or if StartedAt was never recorded).
	ActualDurationSeconds *int `json:"actual_duration_seconds,omitempty"`
}

// SLA monitoring defaults (#274). Applied in NewTask when a task carries an
// ExpectedDurationMinutes but omits the corresponding multiplier, and consulted
// by the SLA monitor when a row's multiplier is the column default.
const (
	DefaultSLAWarnMultiplier = 1.5
	DefaultSLAFailMultiplier = 2.0
)

// Task represents a task to be executed by a worker.
type Task struct {
	ID uuid.UUID `json:"id"`
	// Name is the optional operator label / import-export identity key (#238).
	// Empty = unnamed. See TaskCreate.Name.
	Name          string       `json:"name,omitempty"`
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
	WorktreeConfig *WorktreeConfig `json:"worktree_config,omitempty"`
	// SandboxLimits overrides the global sandbox cgroup ceilings for this task's
	// container (#205). nil = global defaults. See TaskCreate.SandboxLimits.
	SandboxLimits *TaskSandboxLimits `json:"sandbox_limits,omitempty"`
	// OutputSchema is the draft-07 JSON Schema enabling structured-output mode
	// (#244). nil = free-form text mode. See TaskCreate.OutputSchema.
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	// OutputJSON is the validated structured result when OutputSchema was set and
	// the agent produced conforming JSON (#244). nil when no schema was declared
	// or validation failed (the free-form Result still holds the text either way).
	OutputJSON json.RawMessage `json:"output_json,omitempty"`
	// Artifacts is the manifest of named output files the run's agent explicitly
	// PUBLISHED via the publish_artifact tool (#204) — a curated list of
	// deliverables (a marshaled []TaskArtifact), distinct from the raw workspace
	// the file-browser endpoints expose. nil when the run published none. Each
	// entry's Path is a workspace-relative path downloadable via the workspace
	// file endpoint. Persisted on the run's success path; not client-settable.
	Artifacts json.RawMessage `json:"artifacts,omitempty"`
	Priority  int             `json:"priority"`
	// EffectivePriority is the value the scheduler actually orders the pending
	// queue by (#230). Equal to Priority at creation; only the anti-starvation
	// sweep lowers it (never Priority) so a long-waiting task is eventually
	// claimed without rewriting what the submitter requested. Lower = more urgent.
	EffectivePriority      int  `json:"effective_priority"`
	InstructionSelfImprove bool `json:"instruction_self_improve,omitempty"`
	// AllowNetwork controls whether this task's execution sandbox keeps outbound
	// egress. Default false seals it (--network=none); see TaskCreate.AllowNetwork.
	AllowNetwork bool `json:"allow_network,omitempty"`
	// AllowDelegation opts this task into agent delegation (#264). Default false
	// registers no spawn_subagent tool; see TaskCreate.AllowDelegation.
	AllowDelegation bool `json:"allow_delegation,omitempty"`
	// Persona is the per-task persona override (#221). See TaskCreate.Persona.
	Persona string `json:"persona,omitempty"`
	// Description is optional operator documentation (#281). See TaskCreate.Description.
	Description    string     `json:"description,omitempty"`
	Status         TaskStatus `json:"status"`
	AgentSessionID *string    `json:"agent_session_id,omitempty"`
	// WorkspacePath is the host filesystem path of the per-run workspace directory
	// the agent wrote into (#287). Set by the runner when the run begins; surfaced
	// by the workspace file-browser endpoints. nil/empty = no workspace recorded
	// (legacy rows or a run that never reached execution). Persisted; not settable
	// by clients.
	WorkspacePath *string    `json:"workspace_path,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
	Result        *string    `json:"result,omitempty"`
	ErrorMessage  *string    `json:"error_message,omitempty"`
	// ErrorAnalysis is the JSON-validated post-failure LLM diagnosis (#317): a
	// structured {category, summary, remediation} object explaining a TERMINAL
	// failure and how to fix it, distinct from the raw ErrorMessage string. nil
	// when the task did not fail terminally, analysis was disabled, or the
	// diagnosis call/validation failed (best-effort). Written async after the
	// terminal transition via storage.SetTaskErrorAnalysis (lease-free, since the
	// lease is already released); read back into GET /tasks/{id}/error-analysis.
	ErrorAnalysis json.RawMessage `json:"error_analysis,omitempty"`
	ScheduledFor  *time.Time      `json:"scheduled_for,omitempty"`
	Recurrence    string          `json:"recurrence,omitempty"`
	// Timezone is the IANA timezone the cron Recurrence is evaluated in. Always
	// present in responses ("UTC" for legacy/unset tasks). See TaskCreate.Timezone.
	Timezone string `json:"timezone"`
	// CreatedByKeyID is the scoped API key (if any) that submitted this task, set
	// server-side at creation so the completion path can attribute cost back to
	// the key for spending caps. Persisted; not settable by clients.
	CreatedByKeyID *string `json:"created_by_key_id,omitempty"`
	// SourceTaskID is the task this one was re-run / cloned from (#270), set
	// server-side by POST /tasks/{id}/rerun|clone. nil for original tasks.
	// Persisted; not settable by clients.
	SourceTaskID *uuid.UUID `json:"source_task_id,omitempty"`
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
	// RetryPolicy customizes retry backoff + failure-class gating (#201). nil =
	// legacy policy. See TaskCreate.RetryPolicy.
	RetryPolicy *RetryPolicy `json:"retry_policy,omitempty"`
	// DeadLetteredAt is when the task entered the dead-letter queue (#253), set by
	// the runner alongside Status=dead_lettered. nil for tasks that never quarantined
	// (cleared on replay). Persisted; set server-side, not settable by clients.
	DeadLetteredAt *time.Time `json:"dead_lettered_at,omitempty"`
	// DeadLetterReason is the final terminal-attempt failure message captured when
	// the task was dead-lettered (#253). nil unless dead-lettered (cleared on replay).
	DeadLetterReason *string `json:"dead_letter_reason,omitempty"`
	// DeadLetterAttempts is the total number of attempts made before the task was
	// dead-lettered (#253): AttemptCount+1 at quarantine time. 0 unless dead-lettered
	// (cleared on replay).
	DeadLetterAttempts int `json:"dead_letter_attempts,omitempty"`
	// CreatedByUsername is populated at query time for display purposes (not persisted)
	CreatedByUsername *string `json:"created_by_username,omitempty"`
	// TriggerType is how this task is fired: "cron" (default) or "webhook". A
	// webhook task is an inert template; see TaskCreate.TriggerType.
	TriggerType TriggerType `json:"trigger_type"`
	// AllowTaskCreation gates the built-in create_task tool for THIS task's
	// scheduled runs (#277). Default false. See TaskCreate.AllowTaskCreation.
	AllowTaskCreation bool `json:"allow_task_creation,omitempty"`
	// AllowRecurringTaskCreation additionally gates create_task's recurrence
	// field (#277). Default false. See TaskCreate.AllowRecurringTaskCreation.
	AllowRecurringTaskCreation bool `json:"allow_recurring_task_creation,omitempty"`
	// CreatedByTaskID is the parent task whose scheduled run spawned this one via
	// create_task (#277). nil for tasks not spawned by a task. Persisted; set
	// server-side, not settable by external clients.
	CreatedByTaskID *uuid.UUID `json:"created_by_task_id,omitempty"`
	// RunIf, when non-nil, is the pre-run shell gate (#269). See TaskCreate.RunIf.
	RunIf *RunIf `json:"run_if,omitempty"`
	// SkipCount is how many times this task's RunIf gate has failed and the
	// occurrence was skipped (#269). The task stays `scheduled` and its
	// scheduled_for advances to the next cron tick; this counter accumulates.
	SkipCount int `json:"skip_count,omitempty"`
	// LastSkipAt is when the most recent RunIf failure skipped this task (#269).
	// nil when the task has never been skipped. Persisted; set server-side.
	LastSkipAt *time.Time `json:"last_skip_at,omitempty"`
	// LastSkipReason is the human-readable reason captured on the most recent skip
	// (#269): the failing command's exit code + stderr, or "check timed out".
	// nil when the task has never been skipped. Persisted; set server-side.
	LastSkipReason *string `json:"last_skip_reason,omitempty"`
	// ExpectedDurationMinutes is the operator's expectation for typical runtime
	// (#274 SLA monitoring). Nil means no SLA is configured; the monitor ignores
	// the task. See TaskCreate.ExpectedDurationMinutes.
	ExpectedDurationMinutes *int `json:"expected_duration_minutes,omitempty"`
	// SLAWarnMultiplier / SLAFailMultiplier scale the expectation to the warn/fail
	// thresholds (#274). Resolved to the defaults in NewTask when an expected
	// duration is set but the multiplier is 0; otherwise persisted as written.
	SLAWarnMultiplier float64 `json:"sla_warn_multiplier,omitempty"`
	SLAFailMultiplier float64 `json:"sla_fail_multiplier,omitempty"`
	// SLABreached is latched true once the fail threshold is crossed (#274). Set
	// server-side by the SLA monitor; cleared on replay/re-run.
	SLABreached bool `json:"sla_breached,omitempty"`
	// ActualDurationSeconds is completed_at - started_at in whole seconds
	// (#274), populated server-side on the terminal transition. nil until then.
	ActualDurationSeconds *int `json:"actual_duration_seconds,omitempty"`
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

	// Resolve the SLA multipliers once, at task creation, so the monitor and
	// report read stable values from the row (#274). A task with no
	// ExpectedDurationMinutes has no SLA; the multipliers are still persisted
	// (their column is NOT NULL DEFAULT) so a later edit that adds an expected
	// duration doesn't have to backfill them. 0 / negative values map to the
	// defaults; explicit non-default values are validated by ValidateSLA.
	warnMul := tc.SLAWarnMultiplier
	if warnMul <= 0 {
		warnMul = DefaultSLAWarnMultiplier
	}
	failMul := tc.SLAFailMultiplier
	if failMul <= 0 {
		failMul = DefaultSLAFailMultiplier
	}

	// Resolve the scheduling priority once at creation (#230): the zero value
	// (unset) becomes Normal, and EffectivePriority starts equal to Priority —
	// the anti-starvation sweep is the only thing that later lowers it.
	priority := NormalizePriority(tc.Priority)

	return &Task{
		ID:                         uuid.New(),
		Name:                       tc.Name,
		Prompt:                     tc.Prompt,
		Model:                      tc.Model,
		FallbackModel:              tc.FallbackModel,
		MaxIterations:              tc.MaxIterations,
		MCPSelection:               tc.MCPSelection,
		CredentialAllowlist:        tc.CredentialAllowlist,
		LoopConfig:                 tc.LoopConfig,
		WorktreeConfig:             tc.WorktreeConfig,
		SandboxLimits:              tc.SandboxLimits,
		OutputSchema:               tc.OutputSchema,
		Priority:                   priority,
		EffectivePriority:          priority,
		InstructionSelfImprove:     tc.InstructionSelfImprove,
		AllowNetwork:               tc.AllowNetwork,
		AllowDelegation:            tc.AllowDelegation,
		Persona:                    tc.Persona,
		Description:                tc.Description,
		Status:                     status,
		CreatedAt:                  time.Now().UTC(),
		ScheduledFor:               tc.ScheduledFor,
		Recurrence:                 tc.Recurrence,
		Timezone:                   tz,
		Files:                      tc.Files,
		Tags:                       tc.Tags,
		MaxRetries:                 derefOr(tc.MaxRetries, 0),
		RetryPolicy:                tc.RetryPolicy,
		TriggerType:                triggerType,
		AllowTaskCreation:          tc.AllowTaskCreation,
		AllowRecurringTaskCreation: tc.AllowRecurringTaskCreation,
		CreatedByTaskID:            tc.CreatedByTaskID,
		RunIf:                      tc.RunIf,
		ExpectedDurationMinutes:    tc.ExpectedDurationMinutes,
		SLAWarnMultiplier:          warnMul,
		SLAFailMultiplier:          failMul,
	}
}

// ValidateSLA checks an expected-duration / multiplier triple for internal
// consistency so a statically-broken SLA config is rejected at task creation
// rather than firing spurious alerts at runtime (#274). nil expected duration
// is always valid (no SLA); a non-positive multiplier is normalized to the
// default by NewTask, but an explicit fail multiplier at or below the warn
// multiplier is rejected (the fail threshold would never be reachable
// independently of the warn).
func ValidateSLA(expected *int, warnMul, failMul float64) error {
	if expected == nil {
		return nil
	}
	if *expected <= 0 {
		return fmt.Errorf("expected_duration_minutes must be > 0")
	}
	if warnMul < 0 || failMul < 0 {
		return fmt.Errorf("sla multipliers must be >= 0")
	}
	// Resolve the same defaults NewTask applies before comparing thresholds.
	if warnMul <= 0 {
		warnMul = DefaultSLAWarnMultiplier
	}
	if failMul <= 0 {
		failMul = DefaultSLAFailMultiplier
	}
	if failMul <= warnMul {
		return fmt.Errorf("sla_fail_multiplier (%.2f) must exceed sla_warn_multiplier (%.2f)", failMul, warnMul)
	}
	return nil
}

// derefOr returns *p, or def when p is nil.
func derefOr(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

// BatchTaskCreate is the request body for POST /tasks/batch (#227): a slice of
// TaskCreate recipes plus an atomicity flag. atomic=false (default) is
// best-effort (207 Multi-Status on partial failure); atomic=true wraps every
// insert in a single DB transaction so a single validation failure aborts all.
type BatchTaskCreate struct {
	Tasks  []TaskCreate `json:"tasks"`
	Atomic bool         `json:"atomic,omitempty"`
}

// BatchCreated pairs an assigned task UUID with the index it held in the
// request slice, so a caller can correlate results without relying on order.
type BatchCreated struct {
	ID    uuid.UUID `json:"id"`
	Index int       `json:"index"`
}

// BatchFailed records the request index and human-readable error for a task the
// batch could not create (validation or DB failure).
type BatchFailed struct {
	Index int    `json:"index"`
	Error string `json:"error"`
}

// BatchTaskResult is the response body for POST /tasks/batch in both modes. The
// HTTP status carries the outcome class: 200 all-succeeded, 207 partial
// (non-atomic only), 422 total failure (atomic rollback or every task invalid).
type BatchTaskResult struct {
	Created []BatchCreated `json:"created"`
	Failed  []BatchFailed  `json:"failed"`
	Count   int            `json:"count"`
}

// TaskToCreate rebuilds the TaskCreate "recipe" from an existing task — the
// inverse of NewTask over the create-relevant fields. It is the basis for re-run
// / clone (#270): the caller adjusts ScheduledFor / Recurrence and applies any
// overrides, then NewTask mints a fresh task. Runtime-only fields (Status,
// AttemptCount, lease, results, SourceTaskID) are intentionally NOT carried.
func TaskToCreate(t *Task) TaskCreate {
	maxRetries := t.MaxRetries
	return TaskCreate{
		Name:                   t.Name,
		Prompt:                 t.Prompt,
		Model:                  t.Model,
		FallbackModel:          t.FallbackModel,
		MaxIterations:          t.MaxIterations,
		MCPSelection:           t.MCPSelection,
		CredentialAllowlist:    t.CredentialAllowlist,
		LoopConfig:             t.LoopConfig,
		WorktreeConfig:         t.WorktreeConfig,
		SandboxLimits:          t.SandboxLimits,
		OutputSchema:           t.OutputSchema,
		RetryPolicy:            t.RetryPolicy,
		Description:            t.Description,
		Tags:                   t.Tags,
		Priority:               t.Priority,
		InstructionSelfImprove: t.InstructionSelfImprove,
		AllowNetwork:           t.AllowNetwork,
		AllowDelegation:        t.AllowDelegation,
		ScheduledFor:           t.ScheduledFor,
		Recurrence:             t.Recurrence,
		Timezone:               t.Timezone,
		Files:                  t.Files,
		MaxRetries:             &maxRetries,
		TriggerType:            t.TriggerType,
		// Capability flags are part of the create-recipe so a re-run/clone keeps
		// the same governance posture (#277). CreatedByTaskID is per-spawn lineage,
		// like SourceTaskID, and is intentionally NOT carried.
		AllowTaskCreation:          t.AllowTaskCreation,
		AllowRecurringTaskCreation: t.AllowRecurringTaskCreation,
		// RunIf is part of the create-recipe so a re-run/clone keeps the same
		// pre-run gate (#269). Skip tracking fields (SkipCount, LastSkipAt,
		// LastSkipReason) are per-occurrence telemetry, NOT carried.
		RunIf: t.RunIf,
		// SLA config is part of the recipe so a re-run/clone keeps the same
		// expected-duration + multiplier posture (#274). SLABreached /
		// ActualDurationSeconds are runtime-only (like Status / AttemptCount)
		// and are intentionally NOT carried.
		ExpectedDurationMinutes: t.ExpectedDurationMinutes,
		SLAWarnMultiplier:       t.SLAWarnMultiplier,
		SLAFailMultiplier:       t.SLAFailMultiplier,
	}
}

// TaskExportRecord is the portable definition of a single scheduled task (#238).
// It is a subset of TaskCreate — every field here maps 1:1 to a TaskCreate
// field. Runtime state (id, created_at, status, attempt_count, lease, result,
// created_by, …) is intentionally excluded so an export envelope carries only
// the configuration needed to recreate a task on a target box.
type TaskExportRecord struct {
	// Name is the human-readable label used for conflict detection on import.
	// Empty = unnamed (the task is always created fresh on import). Non-empty
	// must be unique within the target deployment (enforced by a partial DB
	// unique index). It maps to TaskCreate.Name.
	Name                       string              `json:"name,omitempty"                       yaml:"name,omitempty"`
	Prompt                     string              `json:"prompt"                               yaml:"prompt"`
	Model                      *string             `json:"model,omitempty"                      yaml:"model,omitempty"`
	FallbackModel              *string             `json:"fallback_model,omitempty"             yaml:"fallback_model,omitempty"`
	MaxIterations              *int                `json:"max_iterations,omitempty"             yaml:"max_iterations,omitempty"`
	MCPSelection               MCPSelection        `json:"mcp_selection,omitempty"              yaml:"mcp_selection,omitempty"`
	CredentialAllowlist        CredentialAllowlist `json:"credential_allowlist,omitempty" yaml:"credential_allowlist,omitempty"`
	LoopConfig                 *LoopConfig         `json:"loop_config,omitempty"                yaml:"loop_config,omitempty"`
	WorktreeConfig             *WorktreeConfig     `json:"worktree_config,omitempty"         yaml:"worktree_config,omitempty"`
	SandboxLimits              *TaskSandboxLimits  `json:"sandbox_limits,omitempty"          yaml:"sandbox_limits,omitempty"`
	OutputSchema               json.RawMessage     `json:"output_schema,omitempty"           yaml:"output_schema,omitempty"`
	Priority                   int                 `json:"priority,omitempty"                   yaml:"priority,omitempty"`
	InstructionSelfImprove     bool                `json:"instruction_self_improve,omitempty"  yaml:"instruction_self_improve,omitempty"`
	AllowNetwork               bool                `json:"allow_network,omitempty"              yaml:"allow_network,omitempty"`
	AllowDelegation            bool                `json:"allow_delegation,omitempty"           yaml:"allow_delegation,omitempty"`
	Persona                    string              `json:"persona,omitempty"                    yaml:"persona,omitempty"`
	Description                string              `json:"description,omitempty"                yaml:"description,omitempty"`
	ScheduledFor               *time.Time          `json:"scheduled_for,omitempty"              yaml:"scheduled_for,omitempty"`
	Recurrence                 string              `json:"recurrence,omitempty"                 yaml:"recurrence,omitempty"`
	Timezone                   string              `json:"timezone,omitempty"                   yaml:"timezone,omitempty"`
	Files                      []string            `json:"files,omitempty"                      yaml:"files,omitempty"`
	Tags                       []string            `json:"tags,omitempty"                       yaml:"tags,omitempty"`
	MaxRetries                 *int                `json:"max_retries,omitempty"                yaml:"max_retries,omitempty"`
	RetryPolicy                *RetryPolicy        `json:"retry_policy,omitempty"               yaml:"retry_policy,omitempty"`
	TriggerType                TriggerType         `json:"trigger_type,omitempty"               yaml:"trigger_type,omitempty"`
	AllowTaskCreation          bool                `json:"allow_task_creation,omitempty"        yaml:"allow_task_creation,omitempty"`
	AllowRecurringTaskCreation bool                `json:"allow_recurring_task_creation,omitempty" yaml:"allow_recurring_task_creation,omitempty"`
	// SLA monitoring config (#274) is part of the portable definition so an
	// exported task keeps its expected-duration + multiplier posture on reimport,
	// mirroring the clone-recipe (TaskToTaskCreate). Runtime SLA state
	// (sla_breached, actual_duration_seconds) is runtime-only and excluded, like
	// id/status/lease. The multipliers are carried only alongside an expected
	// duration (see TaskToExportRecord) since they are meaningless without it.
	ExpectedDurationMinutes *int    `json:"expected_duration_minutes,omitempty" yaml:"expected_duration_minutes,omitempty"`
	SLAWarnMultiplier       float64 `json:"sla_warn_multiplier,omitempty"       yaml:"sla_warn_multiplier,omitempty"`
	SLAFailMultiplier       float64 `json:"sla_fail_multiplier,omitempty"       yaml:"sla_fail_multiplier,omitempty"`
}

// TaskExportVersion is the current envelope schema version. Bump only on an
// incompatible shape change; import rejects an unknown version rather than
// guessing. v1 is the initial format (#238).
const TaskExportVersion = "1"

// TaskExportEnvelope is the top-level container for exported task definitions
// (#238). The version field allows format evolution without breaking importers;
// exported_at is a server-side timestamp.
type TaskExportEnvelope struct {
	Version    string             `json:"version"      yaml:"version"`
	ExportedAt time.Time          `json:"exported_at"  yaml:"exported_at"`
	Tasks      []TaskExportRecord `json:"tasks"        yaml:"tasks"`
}

// TaskImportConflict controls what happens when an imported task name collides
// with an existing task on the target (#238).
type TaskImportConflict string

const (
	// TaskImportConflictError (default) aborts the entire import (HTTP 409)
	// before any writes when any name collides.
	TaskImportConflictError TaskImportConflict = "error"
	// TaskImportConflictSkip leaves the existing task untouched and creates the
	// non-colliding records.
	TaskImportConflictSkip TaskImportConflict = "skip"
	// TaskImportConflictReplace updates the colliding task in place (matched by
	// name); non-colliding tasks are created. Requires admin permission.
	TaskImportConflictReplace TaskImportConflict = "replace"
)

// TaskImportResultStatus is the per-task outcome of an import.
type TaskImportResultStatus string

const (
	TaskImportCreated  TaskImportResultStatus = "created"
	TaskImportSkipped  TaskImportResultStatus = "skipped"
	TaskImportReplaced TaskImportResultStatus = "replaced"
	TaskImportErrored  TaskImportResultStatus = "error"
)

// TaskImportResult is the per-task outcome returned by POST /tasks/import (#238).
type TaskImportResult struct {
	Name   string                 `json:"name"`
	Status TaskImportResultStatus `json:"status"`
	ID     *uuid.UUID             `json:"id,omitempty"`
	Reason string                 `json:"reason,omitempty"`
	Error  string                 `json:"error,omitempty"`
}

// TaskImportResponse is the full response from POST /tasks/import (#238).
type TaskImportResponse struct {
	DryRun   bool               `json:"dry_run"`
	Total    int                `json:"total"`
	Created  int                `json:"created"`
	Skipped  int                `json:"skipped"`
	Replaced int                `json:"replaced"`
	Errors   int                `json:"errors"`
	Results  []TaskImportResult `json:"results"`
}

// ExportRecordToTaskCreate converts a portable TaskExportRecord back into the
// TaskCreate "recipe" used to mint a fresh task (#238). It is the inverse of
// TaskToExportRecord over the definition fields; runtime state is never carried
// (NewTask assigns id/status/timestamps fresh on the target).
func ExportRecordToTaskCreate(rec TaskExportRecord) TaskCreate {
	var maxRetries *int
	if rec.MaxRetries != nil {
		v := *rec.MaxRetries
		maxRetries = &v
	}
	return TaskCreate{
		Name:                       rec.Name,
		Prompt:                     rec.Prompt,
		Model:                      rec.Model,
		FallbackModel:              rec.FallbackModel,
		MaxIterations:              rec.MaxIterations,
		MCPSelection:               rec.MCPSelection,
		CredentialAllowlist:        rec.CredentialAllowlist,
		LoopConfig:                 rec.LoopConfig,
		WorktreeConfig:             rec.WorktreeConfig,
		SandboxLimits:              rec.SandboxLimits,
		OutputSchema:               rec.OutputSchema,
		Priority:                   rec.Priority,
		InstructionSelfImprove:     rec.InstructionSelfImprove,
		AllowNetwork:               rec.AllowNetwork,
		AllowDelegation:            rec.AllowDelegation,
		Persona:                    rec.Persona,
		Description:                rec.Description,
		ScheduledFor:               rec.ScheduledFor,
		Recurrence:                 rec.Recurrence,
		Timezone:                   rec.Timezone,
		Files:                      rec.Files,
		Tags:                       rec.Tags,
		MaxRetries:                 maxRetries,
		RetryPolicy:                rec.RetryPolicy,
		TriggerType:                rec.TriggerType,
		AllowTaskCreation:          rec.AllowTaskCreation,
		AllowRecurringTaskCreation: rec.AllowRecurringTaskCreation,
		// SLA config (#274). Passed straight through; NewTask normalizes a
		// zero/absent multiplier to the default exactly as the public create path
		// does, so an imported definition resolves identically to one created via
		// POST /tasks.
		ExpectedDurationMinutes: rec.ExpectedDurationMinutes,
		SLAWarnMultiplier:       rec.SLAWarnMultiplier,
		SLAFailMultiplier:       rec.SLAFailMultiplier,
	}
}

// TaskToExportRecord extracts the portable definition of a Task — the inverse of
// ExportRecordToTaskCreate. Runtime fields (id, status, attempt_count, lease,
// results, created_by, timestamps, dead-letter, lineage) are dropped so an
// export envelope carries only the configuration needed to recreate the task.
func TaskToExportRecord(t *Task) TaskExportRecord {
	// MaxRetries is an int on Task (0 = no retries) but a *int on the export
	// record. Preserve a non-zero value; drop 0 (omitempty) so the default
	// round-trips as "unset" rather than a redundant explicit zero.
	var maxRetries *int
	if t.MaxRetries != 0 {
		v := t.MaxRetries
		maxRetries = &v
	}
	rec := TaskExportRecord{
		Name:                       t.Name,
		Prompt:                     t.Prompt,
		Model:                      t.Model,
		FallbackModel:              t.FallbackModel,
		MaxIterations:              t.MaxIterations,
		MCPSelection:               t.MCPSelection,
		CredentialAllowlist:        t.CredentialAllowlist,
		LoopConfig:                 t.LoopConfig,
		WorktreeConfig:             t.WorktreeConfig,
		SandboxLimits:              t.SandboxLimits,
		OutputSchema:               t.OutputSchema,
		Priority:                   t.Priority,
		InstructionSelfImprove:     t.InstructionSelfImprove,
		AllowNetwork:               t.AllowNetwork,
		AllowDelegation:            t.AllowDelegation,
		Persona:                    t.Persona,
		Description:                t.Description,
		ScheduledFor:               t.ScheduledFor,
		Recurrence:                 t.Recurrence,
		Timezone:                   t.Timezone,
		Files:                      t.Files,
		Tags:                       t.Tags,
		MaxRetries:                 maxRetries,
		RetryPolicy:                t.RetryPolicy,
		TriggerType:                t.TriggerType,
		AllowTaskCreation:          t.AllowTaskCreation,
		AllowRecurringTaskCreation: t.AllowRecurringTaskCreation,
	}
	// SLA config (#274) only travels with an expected duration: the multipliers
	// are meaningless without one, and a NOT NULL column default (1.5/2.0) would
	// otherwise serialize onto every non-SLA task as noise. A task WITH an
	// expectation carries its multipliers verbatim (default or custom) so the
	// thresholds round-trip exactly.
	if t.ExpectedDurationMinutes != nil {
		rec.ExpectedDurationMinutes = t.ExpectedDurationMinutes
		rec.SLAWarnMultiplier = t.SLAWarnMultiplier
		rec.SLAFailMultiplier = t.SLAFailMultiplier
	}
	return rec
}

// StatusUpdate is a status update for a task (from the in-process worker).
type StatusUpdate struct {
	TaskID         uuid.UUID  `json:"task_id"`
	Status         TaskStatus `json:"status"`
	Message        *string    `json:"message,omitempty"`
	Progress       *float64   `json:"progress,omitempty"`
	AgentSessionID *string    `json:"agent_session_id,omitempty"`
	// WorkspacePath records the per-run workspace directory (#287). When non-nil
	// the storage layer persists it on the task; nil leaves the existing value
	// untouched (so a later status update doesn't clobber a recorded path).
	WorkspacePath *string `json:"workspace_path,omitempty"`
	// OutputJSON carries the validated structured result (#244). When non-empty the
	// storage layer persists it on the task's output_json; empty leaves the
	// existing value untouched. Set on a running-status update by the runner before
	// the terminal success, mirroring WorkspacePath.
	OutputJSON json.RawMessage `json:"output_json,omitempty"`
	// Artifacts carries the published-artifact manifest (#204), a marshaled
	// []TaskArtifact. When non-empty the storage layer persists it on the task's
	// artifacts column; empty leaves the existing value untouched. Set on a
	// running-status update by the runner before terminal success, like OutputJSON.
	Artifacts json.RawMessage `json:"artifacts,omitempty"`
	Timestamp *time.Time      `json:"timestamp,omitempty"`
}

// TaskArtifact is one named output file a scheduled run's agent published via the
// publish_artifact tool (#204). Path is workspace-relative (the same namespace
// the workspace file-browser serves), so a client lists artifacts via
// GET /tasks/{id}/artifacts and downloads each via the workspace file endpoint.
type TaskArtifact struct {
	Name        string `json:"name"`                  // base filename, sanitized
	Path        string `json:"path"`                  // workspace-relative path
	Description string `json:"description,omitempty"` // optional agent-supplied note
	Size        int64  `json:"size"`                  // bytes at publish time
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

// DashboardStats contains statistics for the dashboard.
type DashboardStats struct {
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
	Name string `json:"name"`
	// Type, when set, mints a typed key (#190): one of admin|task|webhook|readonly.
	// When empty, the legacy role-based path is used. A webhook key requires
	// AllowedTriggerSlugs.
	Type                string   `json:"type,omitempty"`
	AllowedTriggerSlugs []string `json:"allowed_trigger_slugs,omitempty"`
	AllowedNodePatterns []string `json:"allowed_node_patterns"`
	Role                *string  `json:"role,omitempty"`
	RateLimit           int      `json:"rate_limit"`
	ExpiresInDays       *int     `json:"expires_in_days,omitempty"`
	Description         string   `json:"description"`
	// Spending caps (nil = unlimited).
	MaxCostPerDayUSD   *float64 `json:"max_cost_per_day_usd,omitempty"`
	MaxCostPerMonthUSD *float64 `json:"max_cost_per_month_usd,omitempty"`
	// MaxPriority caps how urgent a task this key may submit (#230): the key
	// cannot create a task at a priority MORE urgent (lower integer) than this.
	// nil = uncapped. Range [PriorityMin, PriorityMax].
	MaxPriority *int `json:"max_priority,omitempty"`
}

// APIKeyResponse is the response model for API key operations.
type APIKeyResponse struct {
	KeyID               string     `json:"key_id"`
	Name                string     `json:"name"`
	KeyPrefix           string     `json:"key_prefix"`
	Type                string     `json:"type,omitempty"`
	AllowedTriggerSlugs []string   `json:"allowed_trigger_slugs,omitempty"`
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
	// MaxPriority is the key's task-urgency ceiling (#230); nil = uncapped.
	MaxPriority *int `json:"max_priority,omitempty"`
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

// SLAReportTask is one row of the SLA report (#274): the actual-duration
// distribution (p50/p95) and breach rate for a (prompt, expected_duration)
// bucket over the report window. Tasks without an expected_duration_minutes
// are excluded; the bucket key is the task's prompt (fleet has no separate
// `name` column, so prompt is the closest stable grouping key — recurring
// tasks repeat the same prompt verbatim).
type SLAReportTask struct {
	TaskName          string  `json:"task_name"`
	ExpectedMinutes   int     `json:"expected_minutes"`
	P50ActualMinutes  float64 `json:"p50_actual_minutes"`
	P95ActualMinutes  float64 `json:"p95_actual_minutes"`
	BreachRatePercent float64 `json:"breach_rate_pct"`
	SampleCount       int     `json:"sample_count"`
}

// SLAReport is the GET /admin/sla-report response (#274): the per-bucket SLA
// actuals for the report window. Period is the human label ("last_7_days");
// WindowDays is the numeric bound (default 7, capped at 90).
type SLAReport struct {
	Period     string          `json:"period"`
	WindowDays int             `json:"window_days"`
	Tasks      []SLAReportTask `json:"tasks"`
}

// EvalRun is one eval & regression harness invocation (#502): the set-level
// aggregate persisted to eval_runs after `fleet eval run <set>` replays a
// set's goldens through the governed loop. Results holds the marshaled
// per-case detail ([]evals.CaseResult — scorer verdicts, judge reasoning,
// cost/duration per case); the typed columns are what history listings and
// regression comparisons query.
type EvalRun struct {
	ID          uuid.UUID `json:"id"`
	EvalSet     string    `json:"eval_set"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	// BundleSHA fingerprints the replayed bundle content (evals.BundleFingerprint):
	// runs are model-comparable only at an equal sha.
	BundleSHA string  `json:"bundle_sha,omitempty"`
	Total     int     `json:"total"`
	Passed    int     `json:"passed"`
	MeanScore float64 `json:"mean_score"`
	// Threshold is the set's pass-fraction gate at run time; Pass records
	// whether passed/total met it.
	Threshold float64         `json:"threshold"`
	Pass      bool            `json:"pass"`
	CostUSD   float64         `json:"cost_usd"`
	Results   json.RawMessage `json:"results,omitempty"`
}
