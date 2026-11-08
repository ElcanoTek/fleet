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
	"sync"
	"time"

	"github.com/google/uuid"
)

// HashToken creates a SHA-256 hash for storing tokens securely.
func HashToken(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
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

// RunIf is the optional pre-run shell gate for a task (#269): when set, the
// scheduler evaluates Command on the host (NOT in the sandbox — it is a
// lightweight gate, not an agent tool call) before promoting a due task. The
// task is promoted only when the command exits with ExitCodeIs; otherwise it is
// skipped (next_run_at advances, skip_count increments). nil = the legacy
// unconditional promotion path; existing tasks are unaffected.
//
// ENFORCEMENT CONTRACT: the gate is evaluated at the scheduled→pending
// promotion in ProcessScheduledTasks — the ONLY place that evaluates it. To
// make that promotion unavoidable, NewTask and the task-edit recompute
// (storage.UpdateEditableTask) both park EVERY gated cron task as `scheduled`
// via DeriveDispatchState (defaulting a nil scheduled_for to now), so however
// the task was minted or mutated — a scheduled or immediate create, a rerun
// or clone, a webhook/email trigger spawn (the spawned run inherits the
// template's gate), an import, an edit — the gate runs before dispatch; an
// "immediate" gated run waits up to one scheduler tick. A webhook TEMPLATE
// itself stays inert and is never evaluated; its gate applies to each spawned
// run. A clean-failure retry re-parks the task `scheduled` with a backoff
// (RequeueTaskForRetryWithContext), so the gate IS re-evaluated before each
// retry attempt; lease-expiry recovery and `fleet sched dlq replay`
// reset the task straight to pending, re-dispatching WITHOUT a gate
// evaluation. The edit and import-replace paths refuse to attach or change a
// gate on a pending task — honoring it would re-park a dispatch that already
// passed its evaluation point — so that state change stays explicit: change
// the gate while the task is scheduled, or cancel and recreate it.
//
// IMPORTANT (security note encoded in the schema): the check runs on the host
// as the fleet process user with a restricted PATH. It is NOT a sandboxed agent
// tool call — by design, so a misconfigured check cannot burn a model budget or
// touch MCP credentials — but it DOES mean a run_if command has the host-user
// privileges of the fleet process. Operators must treat run_if commands as
// trusted, exactly like the fleet binary itself; the validation path rejects
// empty commands but does not sandbox them, which is why only a
// PermissionAdmin principal may author one (create, edit, or import).
type RunIf struct {
	// Command is the shell command evaluated by `sh -c`. Must be non-empty when
	// RunIf is present. Bash-specific constructs are NOT guaranteed — use POSIX sh.
	Command string `json:"command"`
	// ExitCodeIs is the exit code that means "run the task". Default 0.
	ExitCodeIs int `json:"exit_code_is,omitempty"`
	// TimeoutSeconds is the hard wall-clock timeout for the check, enforced via
	// exec.CommandContext. Omitted/0 means the default 30; a set value must be
	// in [1, 300] (validation rejects negative and >300).
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
	// 0 is the omitted-field value (`omitempty`) and means "the default 30" —
	// the same reading EffectiveTimeoutSeconds and Normalized already give it.
	// Rejecting it here contradicted the schema and forced every client to
	// spell out the default.
	if r.TimeoutSeconds < 0 || r.TimeoutSeconds > 300 {
		return fmt.Errorf("run_if.timeout_seconds must be between 1 and 300 (omit or 0 for the default 30)")
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

// Normalized returns a canonical copy for change detection: defaultable fields
// resolve to their effective values, so a stored gate and a client echo that
// differ only in representation (OnError "" vs "run", an omitted timeout)
// compare equal. Used by the edit path's privilege check, where a raw
// DeepEqual would misread a lossy round-trip as an attempted run_if change.
// nil stays nil (no gate).
func (r *RunIf) Normalized() *RunIf {
	if r == nil {
		return nil
	}
	return &RunIf{
		Command:        strings.TrimSpace(r.Command),
		ExitCodeIs:     r.ExitCodeIs,
		TimeoutSeconds: r.EffectiveTimeoutSeconds(),
		OnError:        r.EffectiveOnError(),
	}
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
	// false the worktree is left in place for inspection / manual push. The
	// maintenance loop reclaims it once it ages past the prune window
	// (FLEET_WORKTREE_PRUNE_AGE, default 24h), so "keep for inspection" is a
	// window, not forever; `fleet worktree prune` reclaims it on demand.
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
	// BaseBranch is the trailing positional of `git worktree add -b <branch>
	// <path> <base>`, and that invocation carries no "--" end-of-options
	// separator, so a value beginning with "-" would be parsed by git as an
	// option rather than a commit-ish. Reject that shape here — worktree_config
	// is settable by any task creator (unlike run_if, which is admin-only), so
	// this is the boundary. The ref-name checks mirror BranchPrefix above; git
	// still makes the authoritative check at run time.
	if base := strings.TrimSpace(wc.BaseBranch); base != "" {
		if strings.HasPrefix(base, "-") {
			return fmt.Errorf("base_branch may not begin with '-'")
		}
		if strings.ContainsAny(base, " ~^:?*[\\") ||
			strings.Contains(base, "@{") ||
			strings.Contains(base, "..") ||
			strings.Contains(base, "//") ||
			strings.Contains(base, ".lock") {
			return fmt.Errorf("base_branch is not a valid git ref-name fragment")
		}
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
	FailureTransient     = "transient"                     // retry budget / stream blip — a fresh run may succeed
	FailureCostCeiling   = "cost_ceiling"                  // cost/token ceiling hit — will recur; default no-retry
	FailureContextBudget = "context_budget"                // context exhausted after compaction — default no-retry
	FailureOutputFormat  = "structured_output_format"      // model failed the declared machine-output contract
	FailureOutputPersist = "structured_output_persistence" // validated output could not commit under lease
	FailureTerminal      = "terminal"                      // unknown / deterministic — never retried
	// FailureConnectorUnavailable: a server the task's EXECUTION REQUIREMENTS
	// declare failed to connect transiently (agentcore.ErrConnectorUnavailable).
	// The runner re-runs the occurrence a few minutes later on its own infra
	// budget (Task.InfraRetryCount), not max_retries; once that is spent the
	// class follows the RetryPolicy like any other (default: no retry), and a
	// dead-letter it ends in does not count toward the recurrence park breaker
	// until three in a row, which park the chain on their own (ADR-0077).
	FailureConnectorUnavailable = "connector_unavailable"
)

// retryFailureClasses is the set a retry_on/no_retry_on list may name.
var retryFailureClasses = map[string]struct{}{
	FailureTransient: {}, FailureCostCeiling: {}, FailureContextBudget: {}, FailureTerminal: {},
	FailureOutputFormat: {}, FailureOutputPersist: {}, FailureConnectorUnavailable: {},
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
				return fmt.Errorf("unknown failure class %q (allowed: transient, cost_ceiling, context_budget, structured_output_format, structured_output_persistence, connector_unavailable, terminal)", c)
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

// User is an account with Operations Center access as this plane sees it: a
// row of the one identity table, core.users (#2049 M4, internal/identity).
// Username is the account's email; Role is the legacy Operations Center role
// its Fleet role and ops_scope grant — admin | client | readonly — so the
// permission checks (RolePermissions) read as they did over the old table.
type User struct {
	ID        uuid.UUID  `json:"id"`
	Username  string     `json:"username"`
	Role      string     `json:"role"`
	CreatedAt time.Time  `json:"created_at"`
	LastLogin *time.Time `json:"last_login,omitempty"`
}

// UserCreate represents the request to create a user.
type UserCreate struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

// UserResponse represents the public user data.
type UserResponse struct {
	ID        uuid.UUID `json:"id"`
	Username  string    `json:"username"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

// TaskStatus represents the status of a task in the system.
type TaskStatus string

const (
	TaskStatusPending   TaskStatus = "pending"
	TaskStatusScheduled TaskStatus = "scheduled"
	TaskStatusLeased    TaskStatus = "leased"
	TaskStatusRunning   TaskStatus = "running"
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
	// TaskStatusPausedAwaitingInput is the non-terminal pause a scheduled run
	// enters when its agent calls `ask` (#510): the run has ended and released
	// its sandbox/lease, and the task waits for a human answer that re-queues it.
	// NOT terminal (it resumes) — so IsTerminal excludes it and the paused row
	// holds no lease.
	TaskStatusPausedAwaitingInput TaskStatus = "paused_awaiting_input"
	// TaskStatusPausedAwaitingWake is the non-terminal pause a scheduled run
	// enters when its agent calls `sleep` or `wake_on_event` (self-wake,
	// docs/SELF-WAKE.md): the run has ended and released its sandbox/lease,
	// and the scheduler's wake sweep re-queues the task when wake_at passes
	// (or an event wake arrives first). NOT terminal — same lifecycle shape
	// as the ask pause (#510), keyed on a deadline/event instead of a human.
	TaskStatusPausedAwaitingWake TaskStatus = "paused_awaiting_wake"
)

// IsValidReportedStatus reports whether s is a status a worker is allowed to
// report for its own task. The orchestrator owns the rest of the lifecycle.
// TaskStatusDeadLettered is intentionally excluded: only the runner's terminal
// switch quarantines a task (#253), never a self-reporting worker. Legacy moc
// statuses (e.g. the retired 'analyzing', rewritten by migration 063) are
// unknown here and therefore rejected too.
func (s TaskStatus) IsValidReportedStatus() bool {
	switch s {
	case TaskStatusLeased, TaskStatusRunning, TaskStatusSuccess, TaskStatusError:
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

// TriggerKind distinguishes what SHAPE of inbound event a trigger row accepts
// (#511). Both kinds share the task_triggers row, the slug+HMAC credential, and
// the one spawn path — kind only selects the endpoint + the payload contract.
type TriggerKind string

const (
	// TriggerKindWebhook is the #177 generic JSON webhook (POST /triggers/{slug}):
	// any signed JSON payload spawns a run. The default for existing rows.
	TriggerKindWebhook TriggerKind = "webhook"
	// TriggerKindEmail is an email-ingress trigger (POST /triggers/email/{slug},
	// #511): a provider's inbound-parse webhook posts a normalized email payload,
	// gated by the row's EmailPolicy (approved senders, DKIM/SPF, attachments).
	TriggerKindEmail TriggerKind = "email"
)

// IsValid reports whether k is a recognized trigger kind.
func (k TriggerKind) IsValid() bool {
	switch k {
	case TriggerKindWebhook, TriggerKindEmail:
		return true
	default:
		return false
	}
}

// EmailTriggerPolicy holds the email-kind security controls (#511), persisted as
// the task_triggers.email_policy JSONB. A nil policy on an email trigger is
// treated as the most restrictive posture (no approved senders ⇒ reject all,
// DKIM required, no attachments) so a misconfigured trigger fails closed.
type EmailTriggerPolicy struct {
	// ApprovedSenders is the allowlist of permitted From values. Each entry is
	// either a full address ("alerts@corp.com") or a bare domain ("corp.com",
	// matching any address @corp.com). An inbound From matching NONE is rejected.
	// An empty list rejects every sender (an email trigger MUST name at least one).
	ApprovedSenders []string `json:"approved_senders,omitempty"`
	// RequireDKIM rejects an inbound email whose provider-reported DKIM result is
	// not "pass". Default true (set explicitly at create time).
	RequireDKIM bool `json:"require_dkim"`
	// RequireSPF rejects an inbound email whose provider-reported SPF result is
	// not "pass". Default false — SPF commonly breaks on legitimate forwarding, so
	// it is opt-in; DKIM is the stronger default gate.
	RequireSPF bool `json:"require_spf"`
	// MaxAttachments caps the number of attachments; an email exceeding it is
	// rejected. 0 (default) means NO attachments are permitted.
	MaxAttachments int `json:"max_attachments"`
	// MaxAttachmentBytes caps the size of any single attachment; an email with a
	// larger attachment is rejected. 0 (default) disallows sized attachments (any
	// attachment with size>0 is rejected unless a positive cap is set).
	MaxAttachmentBytes int64 `json:"max_attachment_bytes"`
}

// TaskTrigger binds a URL-safe slug + HMAC-SHA256 secret to a template task so
// external systems can spawn runs via POST /triggers/{slug} (#177). The secret
// is the per-trigger webhook credential — never the admin API key. Kind selects
// webhook vs email ingress (#511); EmailPolicy is set only for email kind.
type TaskTrigger struct {
	ID             uuid.UUID           `json:"id"`
	TaskID         uuid.UUID           `json:"task_id"`
	Slug           string              `json:"slug"`
	Secret         string              `json:"secret"`
	PromptTemplate string              `json:"prompt_template"`
	Kind           TriggerKind         `json:"kind"`
	EmailPolicy    *EmailTriggerPolicy `json:"email_policy,omitempty"`
	CreatedAt      time.Time           `json:"created_at"`
	UpdatedAt      time.Time           `json:"updated_at"`
}

// KindOrWebhook returns the trigger's kind, defaulting a blank/legacy value to
// webhook (rows created before #511 have no kind on the wire).
func (t *TaskTrigger) KindOrWebhook() TriggerKind {
	if t.Kind == "" {
		return TriggerKindWebhook
	}
	return t.Kind
}

// TriggerEvent is one ACCEPTED inbound event: the idempotency ledger row
// (UNIQUE(trigger_id, idempotency_key)) that also ties the spawned run back to
// its originating event and captures the reply target for reply-back (#511).
type TriggerEvent struct {
	ID             uuid.UUID  `json:"id"`
	TriggerID      uuid.UUID  `json:"trigger_id"`
	IdempotencyKey string     `json:"idempotency_key"`
	Sender         string     `json:"sender"`
	Subject        string     `json:"subject"`
	MessageID      string     `json:"message_id"`
	RunID          *uuid.UUID `json:"run_id,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
}

// Permission represents available permissions for API keys.
type Permission string

const (
	PermissionCreateTask Permission = "create_task"
	PermissionViewTasks  Permission = "view_tasks"
	PermissionCancelTask Permission = "cancel_task"
	// PermissionViewLogs admits a principal to the run-log read surface at all.
	// It is NOT a fleet-wide grant: since #980 it reads the transcripts of the
	// principal's OWN tasks only (the tasks it created). Reading another
	// principal's transcript additionally requires PermissionViewAllLogs.
	PermissionViewLogs Permission = "view_logs"
	// PermissionViewAllLogs is the explicit fleet-wide transcript grant (#980).
	// A run-log transcript carries connector query results, whatever PII the
	// agent handled, and cost data, so cross-principal reads are a deliberate,
	// separately-granted privilege rather than a side effect of holding
	// view_logs. PermissionAdmin implies it (see APIKey.hasPermission /
	// userHasPermission); no role grants it, so an operator who wants a
	// non-admin fleet-wide auditor mints a key with it explicitly.
	PermissionViewAllLogs Permission = "view_all_logs"
	PermissionManageKeys  Permission = "manage_keys"
	PermissionAdmin       Permission = "admin"
)

// AllPermissions is the permission vocabulary, used to validate an explicitly
// requested permission set on key creation. Keep it in sync with the constants
// above — an unknown string in a key's permission list would silently grant
// nothing, which is the failure mode this rejects up front.
var AllPermissions = []Permission{
	PermissionCreateTask,
	PermissionViewTasks,
	PermissionCancelTask,
	PermissionViewLogs,
	PermissionViewAllLogs,
	PermissionManageKeys,
	PermissionAdmin,
}

// Valid reports whether p is one of the known permissions.
func (p Permission) Valid() bool {
	for _, known := range AllPermissions {
		if p == known {
			return true
		}
	}
	return false
}

// RolePermissions maps role names to their permission sets. No role carries
// PermissionViewAllLogs except by way of PermissionAdmin: fleet-wide transcript
// reads are admin-only unless an operator mints a key with the permission
// explicitly (#980).
var RolePermissions = map[string][]Permission{
	"admin":    {PermissionAdmin},
	"client":   {PermissionCreateTask, PermissionViewTasks, PermissionViewLogs},
	"readonly": {PermissionViewTasks, PermissionViewLogs},
}

// PromptLibraryEntry is one UI-authored reusable prompt. Bundle/Git entries
// use clientconfig.Prompt and are merged by the HTTP handler; only mutable
// private/workspace rows live in the scheduler database.
type PromptLibraryEntry struct {
	ID            uuid.UUID `json:"id"`
	OwnerUsername string    `json:"owner_username,omitempty"`
	Name          string    `json:"name"`
	Description   string    `json:"description,omitempty"`
	Content       string    `json:"content"`
	Visibility    string    `json:"visibility"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// TaskCreate is the request model for creating a new task.
type TaskCreate struct {
	// Name is an optional, human-readable label for the task (#238). It is the
	// identity key used by the task-definition import/export endpoints
	// (GET /tasks/export, POST /tasks/import) for conflict detection on import.
	// Empty = unnamed (the historical default); a non-empty name must be unique
	// across tasks (enforced by a partial DB unique index). Not injected into the
	// agent prompt; purely an operator convenience + import/export key.
	Name string `json:"name,omitempty"`
	// Title is the short operator-facing label for the task — what the
	// Operations Center shows wherever the task is listed. Empty = untitled, and
	// clients fall back to the prompt's first line (which is why operators used
	// to write a title line at the head of the prompt).
	//
	// It is deliberately NOT Name: a title is a label, not an identity, so it
	// carries no unique index, and it IS carried by TaskToCreate — every
	// occurrence of a recurring task, every re-run and every clone keeps it,
	// whereas Name must be cleared on each copy to avoid colliding with the row
	// it was copied from. Not injected into the agent prompt.
	Title         string  `json:"title,omitempty"`
	Prompt        string  `json:"prompt"`
	Model         *string `json:"model,omitempty"`
	FallbackModel *string `json:"fallback_model,omitempty"`
	MaxIterations *int    `json:"max_iterations,omitempty"`
	// MaxCostUSD / MaxTotalTokens are this task's own per-run ceilings (#1533):
	// nil = inherit the deployment ceiling (FLEET_MAX_COST_USD /
	// FLEET_MAX_TOTAL_TOKENS or the admin settings); a value replaces it for
	// this task's runs. Anyone may lower a task's ceiling; a value ABOVE the
	// deployment ceiling needs admin permission (the same authority as editing
	// the deployment ceiling). Enforced through the one checkCeilings path.
	MaxCostUSD     *float64     `json:"max_cost_usd,omitempty"`
	MaxTotalTokens *int         `json:"max_total_tokens,omitempty"`
	MCPSelection   MCPSelection `json:"mcp_selection,omitempty"`
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
	// bounded draft-07 JSON Schema object the task's terminal result must conform
	// to. The governed run uses native strict generation when supported or one
	// forced terminal schema tool otherwise, validates locally, then commits the
	// JSON atomically with success. Any exhausted format/persistence failure is a
	// non-success outcome. nil = free-form text mode (the default).
	OutputSchema           json.RawMessage `json:"output_schema,omitempty"`
	Priority               int             `json:"priority"`
	InstructionSelfImprove bool            `json:"instruction_self_improve,omitempty"`
	ScheduledFor           *time.Time      `json:"scheduled_for,omitempty"`
	Recurrence             string          `json:"recurrence,omitempty"`
	// RecurrenceUntil ends the recurrence at an absolute instant: no occurrence
	// is spawned with a fire time after it. nil = repeat forever (the default).
	// Only meaningful with a non-empty Recurrence.
	RecurrenceUntil *time.Time `json:"recurrence_until,omitempty"`
	// RecurrenceRemaining ends the recurrence after a total number of runs: it
	// counts the runs still allowed INCLUDING this occurrence, so each spawned
	// occurrence carries remaining-1 and the spawn is skipped when it would hit
	// zero. nil = unbounded. Only meaningful with a non-empty Recurrence.
	RecurrenceRemaining *int     `json:"recurrence_remaining,omitempty"`
	Files               []string `json:"files,omitempty"`
	// FileNames optionally supplies the logical, prompt-visible name for each
	// stored file in Files. When present it must pair 1:1 with Files. The runner
	// materializes inputs under these names in the per-run connector workspace
	// while retaining collision-safe upload storage names.
	FileNames []string `json:"file_names,omitempty"`
	// Tags are user-defined labels for organizing and filtering tasks (#212):
	// lowercase alphanumeric + '-'/'.', ≤64 chars each, ≤20 per task. Normalized
	// and validated at create/edit. nil/empty = untagged.
	Tags []string `json:"tags,omitempty"`
	// MaxRetries is the number of ADDITIONAL whole-task attempts after the first
	// when a run fails cleanly with a transient error. nil = the deployment
	// default (DefaultMaxRetries, from FLEET_TASK_DEFAULT_MAX_RETRIES; 1 unless
	// configured); an explicit 0 = no retries.
	MaxRetries *int `json:"max_retries,omitempty"`
	// RetryPolicy customizes retry backoff + which failure classes retry (#201).
	// nil = legacy policy (transient-only, 30s→10m exponential). See RetryPolicy.
	RetryPolicy *RetryPolicy `json:"retry_policy,omitempty"`
	// LineageID is server-side only (never decoded from a request): the
	// TaskToCreate clone recipe sets it so a recurrence occurrence, re-run or
	// clone joins the source job's lineage (#1543). nil = a fresh job.
	LineageID *uuid.UUID `json:"-"`
	// AllowNetwork lets THIS scheduled task's bash/run_python execution sandbox
	// keep outbound egress. The default (false) seals the sandbox with
	// --network=none, matching the interactive lockdown path; egress is an
	// explicit opt-in for the tasks that genuinely need it.
	AllowNetwork bool `json:"allow_network,omitempty"`
	// CarryContext (#504): recurring runs carry a bounded handoff from the prior
	// run. Default false = fresh each run.
	CarryContext bool `json:"carry_context,omitempty"`
	// AllowEventTriggers (#511) is the security opt-in for event ingress: a run
	// spawned by an EMAIL (or other event) trigger inherits this template task's
	// write-capable MCP connectors ONLY when this is true. Default false ⇒ an
	// event-spawned run gets native tools only, so an untrusted inbound event can
	// never auto-escalate through a connector. (The #177 webhook path predates
	// this gate and always inherits — unchanged.)
	AllowEventTriggers bool `json:"allow_event_triggers,omitempty"`
	// AllowDelegation gates agent delegation for THIS task (#264, #1043): the
	// spawn_subagent native tool is registered so the run can fan out scoped
	// subtasks to governed child runs (sliced budget, depth/fan-out caps,
	// parent_task_id linkage). Tri-state: nil (field omitted) means the DEFAULT,
	// which since #1043 is TRUE — delegation is opt-OUT, the parent agent decides
	// whether to actually spawn. Explicit false is the per-task kill switch. It
	// composes with the fleet-wide FLEET_SUBAGENTS_ENABLED operator flag as AND
	// (both must be on for the tool to be registered). Resolve via
	// DelegationAllowed(), never by reading the pointer directly.
	AllowDelegation *bool `json:"allow_delegation,omitempty"`
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
	// ThinkingBudgetTokens is the per-task extended-thinking override (#220).
	// nil = inherit the global FLEET_DEFAULT_THINKING_BUDGET_TOKENS; 0 = force
	// thinking OFF for this task; N (>0) = this task's own budget (clamped to
	// the provider bounds at run time). Scheduled runs only.
	ThinkingBudgetTokens *int `json:"thinking_budget_tokens,omitempty"`
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
	// SerializationKey is an opaque mutual-exclusion token supplied by the
	// intake caller (#709). Fleet guarantees at most one task per key is in an
	// ACTIVE state (leased/running) at a time: a pending task whose
	// key matches an active task is not claimable and waits for a later claim
	// pass — it is skipped, never failed. Fleet NEVER interprets the key's
	// contents (coupling doctrine: the key's meaning is owned by the intake
	// side, e.g. "client:<id>"). nil / empty / whitespace-only = unserialized,
	// the historical behavior. Normalized (trimmed, empty → nil) in NewTask;
	// immutable after creation. When nil, NewTask takes the key the prompt's
	// EXECUTION REQUIREMENTS declare (serialization_key), if any.
	SerializationKey *string `json:"serialization_key,omitempty"`
	// A2ADelegationDepth is set ONLY by the inbound A2A server (#1368) from the
	// delegating peer's X-Fleet-A2A-Depth header; it is hidden from JSON so no
	// REST caller can claim a depth and skew the recursion guard.
	A2ADelegationDepth int `json:"-"`
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
	Name string `json:"name,omitempty"`
	// Title is the short human label shown wherever the task is listed. Empty =
	// untitled (clients fall back to the prompt's first line). Unlike Name it is
	// NOT unique and IS carried onto every occurrence / re-run / clone, so all
	// the runs of one job share it. See TaskCreate.Title.
	Title         string  `json:"title,omitempty"`
	Prompt        string  `json:"prompt"`
	Model         *string `json:"model,omitempty"`
	FallbackModel *string `json:"fallback_model,omitempty"`
	MaxIterations *int    `json:"max_iterations,omitempty"`
	// MaxCostUSD / MaxTotalTokens: this task's own per-run ceilings (#1533);
	// nil = inherit the deployment ceiling. See TaskCreate.
	MaxCostUSD     *float64     `json:"max_cost_usd,omitempty"`
	MaxTotalTokens *int         `json:"max_total_tokens,omitempty"`
	MCPSelection   MCPSelection `json:"mcp_selection"`
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
	// OutputJSON is the validated structured result when OutputSchema was set
	// (#244). It is non-empty for every successful structured task and committed
	// under the same lease/transaction as terminal success. nil when no schema was
	// declared or the structured task did not complete successfully.
	OutputJSON json.RawMessage `json:"output_json,omitempty"`
	// Artifacts is the manifest of named output files the run's agent explicitly
	// PUBLISHED via the publish_artifact tool (#204) — a curated list of
	// deliverables (a marshaled []TaskArtifact), distinct from the raw workspace
	// the file-browser endpoints expose. nil when the run published none. Each
	// entry's Path is a workspace-relative path downloadable via the workspace
	// file endpoint. Persisted on the run's success path; not client-settable.
	Artifacts json.RawMessage `json:"artifacts,omitempty"`
	// PendingQuestion / PendingAnswer back the ask/notify pause (#510): the
	// question the agent posed (set on pause), and the human answer that
	// re-queued it (injected + cleared at the resumed run's start).
	PendingQuestion string `json:"pending_question,omitempty"`
	PendingAnswer   string `json:"pending_answer,omitempty"`
	// Wake* back the self-wake pause (docs/SELF-WAKE.md). Runtime state, never
	// definition: WakeAt is the deadline the parked task wakes on (always set —
	// an event wait's timeout, a timer sleep's fire time); WakeEventKey names
	// the event an event wait listens for; WakeNote is the agent's message to
	// its future self, injected into the resumed run; WakeReason records why
	// the task woke (written by the wake transition, injected + cleared like
	// PendingAnswer); WakeCycles counts total parks so the runner can refuse a
	// runaway sleep loop.
	WakeAt       *time.Time `json:"wake_at,omitempty"`
	WakeEventKey string     `json:"wake_event_key,omitempty"`
	WakeNote     string     `json:"wake_note,omitempty"`
	WakeReason   string     `json:"wake_reason,omitempty"`
	WakeCycles   int        `json:"wake_cycles,omitempty"`
	// PausedAt is when the task last entered a paused state (#1116), stamped by
	// the dedicated pause transitions (PauseTaskForQuestion #510,
	// PauseTaskForWake docs/SELF-WAKE.md). The paused-expiry sweep counts the
	// ask window from it — previously it measured from StartedAt, which gave a
	// long-running run's question a near-zero TTL. Runtime state like the wake
	// columns: written only by the guarded pause writers, excluded from the
	// task insert/upsert, UpdateTaskTx, the TaskToCreate clone recipe, and the
	// export record. Not cleared on resume — it is only meaningful (and only
	// read) while Status is a paused state, and the next pause re-stamps it.
	PausedAt *time.Time `json:"paused_at,omitempty"`
	Priority int        `json:"priority"`
	// EffectivePriority is the value the scheduler actually orders the pending
	// queue by (#230). Equal to Priority at creation; only the anti-starvation
	// sweep lowers it (never Priority) so a long-waiting task is eventually
	// claimed without rewriting what the submitter requested. Lower = more urgent.
	EffectivePriority      int  `json:"effective_priority"`
	InstructionSelfImprove bool `json:"instruction_self_improve,omitempty"`
	// AllowNetwork controls whether this task's execution sandbox keeps outbound
	// egress. Default false seals it (--network=none); see TaskCreate.AllowNetwork.
	AllowNetwork bool `json:"allow_network,omitempty"`
	// CarryContext, when true on a RECURRING task, injects a bounded handoff
	// from the prior run (its final answer) into each run (#504). Default false
	// = start fresh each run.
	CarryContext bool `json:"carry_context,omitempty"`
	// AllowEventTriggers (#511) opts an event-triggered run into inheriting this
	// task's write-capable connectors. Default false ⇒ native tools only; see
	// TaskCreate.AllowEventTriggers.
	AllowEventTriggers bool `json:"allow_event_triggers,omitempty"`
	// AllowDelegation gates agent delegation for this task (#264, #1043). Default
	// TRUE since #1043 (delegation is opt-out); see TaskCreate.AllowDelegation.
	// Serialized WITHOUT omitempty so an explicit false survives the round trip
	// to clients now that false is the non-default value.
	AllowDelegation bool `json:"allow_delegation"`
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
	// RecurrenceUntil / RecurrenceRemaining are the recurrence end conditions
	// (see TaskCreate): repeat until an instant and/or for a total run count.
	// Definition fields, carried through the TaskToCreate clone recipe.
	RecurrenceUntil     *time.Time `json:"recurrence_until,omitempty"`
	RecurrenceRemaining *int       `json:"recurrence_remaining,omitempty"`
	// Timezone is the IANA timezone the cron Recurrence is evaluated in. Always
	// present in responses ("UTC" for legacy/unset tasks). See TaskCreate.Timezone.
	Timezone string `json:"timezone"`
	// CreatedByKeyID is the scoped API key (if any) that submitted this task, set
	// server-side at creation. It is the `key` bucket of the usage read model
	// (and so the principal a scope=key budget is evaluated against), and the
	// ownership check behind the transcript gate. Persisted; not settable by
	// clients.
	CreatedByKeyID *string `json:"created_by_key_id,omitempty"`
	// A2ADelegationDepth is how many fleet-to-fleet A2A delegation hops this
	// task sits at (#1368): 0 for anything a human or the REST API created,
	// 1 for a task another agent delegated here over A2A, and so on up the
	// chain. Stamped once at creation from the inbound X-Fleet-A2A-Depth
	// header (bounded by FLEET_A2A_MAX_DELEGATION_DEPTH) and carried forward
	// by the task's own outbound a2a peer sends, so a chain of deployments
	// cannot loop. Immutable provenance, never exported.
	A2ADelegationDepth int `json:"a2a_delegation_depth,omitempty"`
	// SourceTaskID is the task this one was re-run / cloned from (#270), set
	// server-side by POST /tasks/{id}/rerun|clone. nil for original tasks.
	// Persisted; not settable by clients.
	SourceTaskID *uuid.UUID `json:"source_task_id,omitempty"`
	// PreviousOccurrenceID is the recurring occurrence this row was spawned to
	// follow (storage.scheduleNextRecurrence stamps it on the successor). It is
	// what lets carry_context reach the previous occurrence's transcript: each
	// firing is a fresh row, so the run's own id has no prior log. nil for
	// non-recurring tasks and for the first occurrence. Persisted; immutable
	// lineage stamped at spawn, never exported, not settable by clients.
	PreviousOccurrenceID *uuid.UUID `json:"previous_occurrence_id,omitempty"`
	// RecurrenceParkedAt is when the consecutive-dead-letter breaker parked
	// this occurrence's chain (ADR-0070). nil means not parked: either the
	// DLQ path spawned a successor, or the row is unclaimed history. Replay
	// re-arms the spawn credit iff this is set or recurrence_spawned is still
	// FALSE. Persisted; never exported; not settable by clients.
	RecurrenceParkedAt *time.Time `json:"recurrence_parked_at,omitempty"`
	// RecurrenceParkedReason says why the breaker parked the chain, in words
	// the owner can act on (migration 073): the malformed EXECUTION
	// REQUIREMENTS message on a first-strike park (ADR-0073), or the
	// two-consecutive-dead-letters rule. nil when not parked, or parked
	// without a recorded reason (before the column, or by an import).
	// Persisted with RecurrenceParkedAt and cleared with it; never exported;
	// not settable by clients.
	RecurrenceParkedReason *string `json:"recurrence_parked_reason,omitempty"`
	// InfraRetryCount is how many times the runner re-ran this occurrence
	// because a declared connector was unavailable (FailureConnectorUnavailable,
	// migration 074): infrastructure weather, counted apart from AttemptCount so
	// it never spends the task's own max_retries. Bounded by the runner; reset
	// by a dead-letter replay. Persisted; never exported; not settable by
	// clients.
	InfraRetryCount int `json:"infra_retry_count,omitempty"`
	// RunOutcome qualifies how a terminal run ended, beyond its status
	// (migration 074): RunOutcomeBlocked on a success whose declared completion
	// clause recorded a blocked outcome (completion.blocked_when) — the run did
	// its job and decided not to publish — and RunOutcomeConnectorUnavailable on
	// a dead-letter caused by a connector outage, which the recurrence park
	// breaker exempts up to three in a row (ADR-0077). nil for every other
	// run. RunOutcomeDetail then names the failed connectors. Written with the
	// terminal transition, cleared by replay; never exported; not settable by
	// clients.
	RunOutcome *string `json:"run_outcome,omitempty"`
	// RunOutcomeDetail is the short reason that goes with RunOutcome — for a
	// blocked run, the declared outcome value and the call's own detail. nil
	// when RunOutcome is.
	RunOutcomeDetail *string `json:"run_outcome_detail,omitempty"`
	// LineageID is the key every run of one JOB shares — recurrence occurrences,
	// re-runs and clones all carry the original task's id here — and so the name
	// of the job's working directory under the workspace root
	// (<root>/tasks/<lineage_id>/, #1543). A task created fresh is its own
	// lineage (LineageID == ID). Persisted; carried by TaskToCreate; not
	// settable by clients; never exported. Read through WorkspaceLineage, which
	// treats the zero value (a Task built in code without the constructor) as
	// "own lineage".
	LineageID uuid.UUID `json:"lineage_id"`
	// NextRunAtLocal is ScheduledFor rendered in Timezone (RFC3339 with offset),
	// populated at query time for display so callers need no client-side tz math.
	// Not persisted; nil when the task has no scheduled_for.
	NextRunAtLocal *string    `json:"next_run_at_local,omitempty"`
	CreatedBy      *uuid.UUID `json:"created_by,omitempty"`
	Files          []string   `json:"files,omitempty"`
	FileNames      []string   `json:"file_names,omitempty"`
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
	// ThinkingBudgetTokens is the per-task extended-thinking override (#220).
	// See TaskCreate.ThinkingBudgetTokens. nil = inherit global.
	ThinkingBudgetTokens *int `json:"thinking_budget_tokens,omitempty"`
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
	// SerializationKey is the opaque mutual-exclusion token (#709). nil means
	// the task is not serialized. See TaskCreate.SerializationKey.
	SerializationKey *string `json:"serialization_key,omitempty"`
}

// DeriveDispatchState computes the (status, scheduled_for) pair that keeps a
// task on the correct dispatch path for its trigger type and gate. It is the
// SINGLE derivation rule, shared by NewTask and the task-edit recompute
// (storage.UpdateEditableTask); when the edit path hand-rolled a
// ScheduledFor-only version of it, an edit that echoed a gate but omitted
// scheduled_for flipped a gated task to pending — off the scheduler path,
// with run_if intact and never evaluated.
//
// The rules, in order:
//
//   - Plain tasks: a future scheduled_for means `scheduled`, anything else
//     (nil or past) means `pending` — immediate dispatch.
//   - A gated cron task is ALWAYS parked `scheduled` (see the RunIf
//     contract): only ProcessScheduledTasks evaluates run_if, so a gated task
//     headed for immediate dispatch — POST /tasks with no schedule,
//     rerun/clone, a trigger-spawned run, an import, an edit — is parked
//     scheduled-for-now (a nil scheduled_for defaults to now) instead of
//     pending. The gate is therefore evaluated before EVERY dispatch, at the
//     cost of up to one scheduler tick (~30s) of latency for an "immediate"
//     gated run.
//   - A webhook TEMPLATE is never run by the cron engine. It is parked inert
//     as `scheduled` with its scheduled_for left as-is (nil):
//     GetScheduledTasks requires scheduled_for IS NOT NULL, so it is never
//     promoted; firing the webhook spawns a fresh cron-type run cloned from
//     it, and the gate carried onto that run is what the previous rule parks.
func DeriveDispatchState(triggerType TriggerType, runIf *RunIf, scheduledFor *time.Time) (TaskStatus, *time.Time) {
	if triggerType == "" {
		triggerType = TriggerTypeCron
	}

	status := TaskStatusPending
	if scheduledFor != nil && scheduledFor.After(time.Now()) {
		status = TaskStatusScheduled
	}
	if runIf != nil && triggerType == TriggerTypeCron {
		status = TaskStatusScheduled
		if scheduledFor == nil {
			now := time.Now().UTC()
			scheduledFor = &now
		}
	}
	if triggerType == TriggerTypeWebhook {
		status = TaskStatusScheduled
	}
	return status, scheduledFor
}

// NewTask creates a new Task with defaults.
// DelegationAllowed resolves the tri-state AllowDelegation: nil (field omitted
// by the client / an older export / a bundle template) means the DEFAULT, which
// since #1043 is TRUE — sub-agent delegation is opt-OUT. An explicit pointer
// value wins either way. Every consumer of TaskCreate.AllowDelegation must go
// through this so the default lives in exactly one place.
func (tc TaskCreate) DelegationAllowed() bool {
	return tc.AllowDelegation == nil || *tc.AllowDelegation
}

// BoolPtr returns a pointer to v — for populating tri-state *bool fields
// (e.g. TaskCreate.AllowDelegation) from a concrete task's stored value.
func BoolPtr(v bool) *bool { return &v }

// defaultMaxRetries is the deployment-wide MaxRetries a task gets when its
// create request omits max_retries (#1538). Set once at boot from
// FLEET_TASK_DEFAULT_MAX_RETRIES via SetDefaultMaxRetries; a process that never
// sets it (tests, embedders) keeps the historical 0. It lives here rather than
// on a threaded parameter because NewTask has ten callers (handlers, batch,
// import/export, triggers, admin CLI) that must all agree on one value. A
// plain int under a mutex, not a fixed-width atomic: the value is written
// once and read per create, and a narrowing conversion here is exactly what
// CodeQL's go/incorrect-integer-conversion would (rightly) refuse to prove safe.
var (
	defaultMaxRetriesMu sync.RWMutex
	defaultMaxRetries   int
)

// SetDefaultMaxRetries installs the deployment default applied by NewTask when
// max_retries is omitted; values outside 0–10 are clamped to that range (the
// same bounds validateTaskLimits enforces on the per-task field).
func SetDefaultMaxRetries(n int) {
	if n < 0 {
		n = 0
	}
	if n > 10 {
		n = 10
	}
	defaultMaxRetriesMu.Lock()
	defaultMaxRetries = n
	defaultMaxRetriesMu.Unlock()
}

// DefaultMaxRetries reports the deployment default NewTask applies when
// max_retries is omitted.
func DefaultMaxRetries() int {
	defaultMaxRetriesMu.RLock()
	defer defaultMaxRetriesMu.RUnlock()
	return defaultMaxRetries
}

// WorkspaceLineage is the job lineage this task belongs to: LineageID when
// set, else the task's own id (a Task assembled without NewTask, or a row read
// from before migration 069). It names the job's working directory (#1543).
func (t *Task) WorkspaceLineage() uuid.UUID {
	if t == nil {
		return uuid.Nil
	}
	if t.LineageID != uuid.Nil {
		return t.LineageID
	}
	return t.ID
}

// IsEditable reports whether the task's definition may still be edited: only a
// task that has not started (pending or scheduled) can be. Leased, running and
// finished rows keep their execution state.
func (t *Task) IsEditable() bool {
	return t.Status == TaskStatusPending || t.Status == TaskStatusScheduled
}

func NewTask(tc TaskCreate) *Task {
	triggerType := tc.TriggerType
	if triggerType == "" {
		triggerType = TriggerTypeCron
	}

	status, scheduledFor := DeriveDispatchState(triggerType, tc.RunIf, tc.ScheduledFor)

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
	warnMul, failMul := ResolveSLAMultipliers(tc.SLAWarnMultiplier, tc.SLAFailMultiplier)

	// Resolve the scheduling priority once at creation (#230): the zero value
	// (unset) becomes Normal, and EffectivePriority starts equal to Priority —
	// the anti-starvation sweep is the only thing that later lowers it.
	priority := NormalizePriority(tc.Priority)

	// Normalize the serialization key (#709): an empty or whitespace-only key
	// must behave exactly like no key at all, so the claim gate never
	// serializes on "" and the column stores NULL for unserialized tasks.
	var serializationKey *string
	if tc.SerializationKey != nil {
		if trimmed := strings.TrimSpace(*tc.SerializationKey); trimmed != "" {
			serializationKey = &trimmed
		}
	}
	// A task created without an explicit key takes the one its prompt's
	// EXECUTION REQUIREMENTS declare, so every write path — the API, chat's
	// schedule_task, the task form, an import, a recurrence spawn — serializes
	// the same declaration the same way; production had two lineages for one
	// dashboard overlap because only API-created tasks carried the key. An
	// explicit key always wins. Creation only: the key stays immutable on an
	// existing row (docs/TASK-SERIALIZATION.md), so a prompt edit that adds a
	// declaration takes effect on the next occurrence, which is created here.
	if serializationKey == nil {
		if declared := DeclaredSerializationKey(tc.Prompt); declared != "" {
			serializationKey = &declared
		}
	}

	// Lineage (#1543): a fresh job is its own lineage; a copy joins its source's.
	id := uuid.New()
	lineage := id
	if tc.LineageID != nil && *tc.LineageID != uuid.Nil {
		lineage = *tc.LineageID
	}

	return &Task{
		ID:                         id,
		LineageID:                  lineage,
		Name:                       tc.Name,
		Title:                      tc.Title,
		Prompt:                     tc.Prompt,
		Model:                      tc.Model,
		FallbackModel:              tc.FallbackModel,
		MaxIterations:              tc.MaxIterations,
		MaxCostUSD:                 tc.MaxCostUSD,
		MaxTotalTokens:             tc.MaxTotalTokens,
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
		CarryContext:               tc.CarryContext,
		AllowEventTriggers:         tc.AllowEventTriggers,
		AllowDelegation:            tc.DelegationAllowed(),
		Persona:                    tc.Persona,
		Description:                tc.Description,
		Status:                     status,
		CreatedAt:                  time.Now().UTC(),
		ScheduledFor:               scheduledFor,
		Recurrence:                 tc.Recurrence,
		RecurrenceUntil:            tc.RecurrenceUntil,
		RecurrenceRemaining:        tc.RecurrenceRemaining,
		Timezone:                   tz,
		Files:                      tc.Files,
		FileNames:                  tc.FileNames,
		Tags:                       tc.Tags,
		MaxRetries:                 derefOr(tc.MaxRetries, DefaultMaxRetries()),
		RetryPolicy:                tc.RetryPolicy,
		TriggerType:                triggerType,
		AllowTaskCreation:          tc.AllowTaskCreation,
		AllowRecurringTaskCreation: tc.AllowRecurringTaskCreation,
		CreatedByTaskID:            tc.CreatedByTaskID,
		RunIf:                      tc.RunIf,
		ExpectedDurationMinutes:    tc.ExpectedDurationMinutes,
		ThinkingBudgetTokens:       tc.ThinkingBudgetTokens,
		SLAWarnMultiplier:          warnMul,
		SLAFailMultiplier:          failMul,
		SerializationKey:           serializationKey,
		A2ADelegationDepth:         tc.A2ADelegationDepth,
	}
}

// ResolveSLAMultipliers maps non-positive multipliers to the defaults — the
// shared rule applied at task creation (NewTask) and on edit
// (storage.UpdateEditableTask), so the persisted NOT NULL columns always hold
// usable thresholds regardless of which write path last touched the row.
func ResolveSLAMultipliers(warnMul, failMul float64) (float64, float64) {
	if warnMul <= 0 {
		warnMul = DefaultSLAWarnMultiplier
	}
	if failMul <= 0 {
		failMul = DefaultSLAFailMultiplier
	}
	return warnMul, failMul
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
	warnMul, failMul = ResolveSLAMultipliers(warnMul, failMul)
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
	lineage := t.WorkspaceLineage()
	return TaskCreate{
		// Every run of one job shares the job's lineage — and its working
		// directory (#1543).
		LineageID: &lineage,
		Name:      t.Name,
		// Title IS carried (unlike Name, which every copy path must clear): all
		// the runs of one job — occurrences, re-runs, clones — share its title.
		Title:                  t.Title,
		Prompt:                 t.Prompt,
		Model:                  t.Model,
		FallbackModel:          t.FallbackModel,
		MaxIterations:          t.MaxIterations,
		MaxCostUSD:             t.MaxCostUSD,
		MaxTotalTokens:         t.MaxTotalTokens,
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
		// CarryContext (#504) is part of the recipe so a recurrence/clone keeps the
		// prior-run handoff opt-in — without it a recurring carry_context task lost
		// the very feature after occurrence #1 (issue #565).
		CarryContext:       t.CarryContext,
		AllowEventTriggers: t.AllowEventTriggers,
		AllowDelegation:    BoolPtr(t.AllowDelegation),
		// Persona (#221) is part of the recipe so a recurrence/clone runs under the
		// same per-task persona override rather than falling back to the global
		// default on occurrence #2+ (issue #565).
		Persona:      t.Persona,
		ScheduledFor: t.ScheduledFor,
		Recurrence:   t.Recurrence,
		// The recurrence end conditions are part of the recipe so occurrence
		// #2+ keeps counting down / honoring the end date: RecurrenceRemaining
		// is decremented by scheduleNextRecurrence before the clone is minted.
		RecurrenceUntil:     t.RecurrenceUntil,
		RecurrenceRemaining: t.RecurrenceRemaining,
		Timezone:            t.Timezone,
		Files:               t.Files,
		FileNames:           t.FileNames,
		MaxRetries:          &maxRetries,
		TriggerType:         t.TriggerType,
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
		ThinkingBudgetTokens:    t.ThinkingBudgetTokens,
		SLAWarnMultiplier:       t.SLAWarnMultiplier,
		SLAFailMultiplier:       t.SLAFailMultiplier,
		// SerializationKey (#709) is part of the recipe so a recurrence keeps
		// successive occurrences mutually exclusive with same-key tasks, and a
		// re-run/clone keeps the caller's serialization intent.
		SerializationKey: t.SerializationKey,
	}
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
	// existing value untouched. The runner supplies it on the terminal success
	// update so output_json and success share one lease-checked transaction.
	OutputJSON json.RawMessage `json:"output_json,omitempty"`
	// Artifacts carries the published-artifact manifest (#204), a marshaled
	// []TaskArtifact. When non-empty the storage layer persists it on the task's
	// artifacts column; empty leaves the existing value untouched. Set on a
	// running-status update by the runner before terminal success, like OutputJSON.
	Artifacts json.RawMessage `json:"artifacts,omitempty"`
	// RunOutcome / RunOutcomeDetail qualify a terminal success (migration
	// 074): RunOutcomeBlocked when the run's declared completion clause
	// recorded a blocked outcome. The storage layer writes them with every
	// success or error transition, so a success without them clears a stale
	// value rather than inheriting it.
	RunOutcome       *string    `json:"run_outcome,omitempty"`
	RunOutcomeDetail *string    `json:"run_outcome_detail,omitempty"`
	Timestamp        *time.Time `json:"timestamp,omitempty"`
}

// Run outcomes (Task.RunOutcome, migration 074).
const (
	// RunOutcomeBlocked: the run succeeded through its declared completion
	// predicate, and the call that completed it recorded a blocked outcome
	// (EXECUTION REQUIREMENTS completion.blocked_when). Shown as "Blocked",
	// not plain success.
	RunOutcomeBlocked = "blocked"
	// RunOutcomeConnectorUnavailable: the run dead-lettered because a declared
	// connector was unavailable after the runner's infra re-runs. The
	// recurrence park breaker does not count it, up to three in a row
	// (ADR-0077).
	RunOutcomeConnectorUnavailable = "connector_unavailable"
)

// IsRunOutcome reports whether t's recorded run outcome is outcome.
func (t *Task) IsRunOutcome(outcome string) bool {
	return t != nil && t.RunOutcome != nil && *t.RunOutcome == outcome
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

// DashboardStats contains statistics for the dashboard.
type DashboardStats struct {
	PendingTasks        int `json:"pending_tasks"`
	RunningTasks        int `json:"running_tasks"`
	CompletedTasksToday int `json:"completed_tasks_today"`
	FailedTasksToday    int `json:"failed_tasks_today"`
	// Live agent-pool occupancy: agents executing scheduled tasks right now
	// and the pool's schedulable slot count. Attached by the handler from the
	// runner at response time (never cached — see GetDashboardStats), and only
	// when cmd/fleet wired the pool in, so standalone handlers omit them.
	// Pointers so a real zero still serializes.
	ActiveAgents *int `json:"active_agents,omitempty"`
	AgentSlots   *int `json:"agent_slots,omitempty"`
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
	ToolName    string        `json:"tool_name,omitempty"`
	IsError     bool          `json:"is_error,omitempty"`
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
	// OutputJSON carries the agentcore-validated terminal structured output
	// (#797) from the driver to the runner, redacted like every other session
	// field before it leaves the process boundary.
	OutputJSON string `json:"output_json,omitempty"`
	// RunOutcome / RunOutcomeDetail hand a successful run's declared outcome
	// (RunOutcomeBlocked, from completion.blocked_when) from the driver to the
	// runner, which writes them onto the task with the success transition.
	RunOutcome       string `json:"run_outcome,omitempty"`
	RunOutcomeDetail string `json:"run_outcome_detail,omitempty"`
	// AuxUsage mirrors agentcore.LogSession.AuxUsage (#1118): the labeled
	// ledger of host-side auxiliary model calls made on behalf of the run
	// (end-of-run verifier, phone-a-friend review, loop exit-condition
	// verifier). Deliberately NOT folded into the headline token/cost fields —
	// iteration cost accounting (#179) and the run ceilings exclude these by
	// design — but persisted so the spend is visible per call.
	AuxUsage []AuxUsageRecord `json:"aux_usage,omitempty"`
}

// AuxUsageRecord is one host-side auxiliary model call's metered spend
// (mirrors agentcore.AuxUsageRecord; #1118). PromptTokens includes cache
// reads, matching the session-log token convention.
type AuxUsageRecord struct {
	Label            string  `json:"label"`
	Model            string  `json:"model,omitempty"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// RunLogMeta identifies one superseded transcript in a task's per-attempt run
// log history: the history entry id plus when the newer transcript replaced
// it. The payload itself is fetched per-entry, never listed.
type RunLogMeta struct {
	ID           int64     `json:"id"`
	SupersededAt time.Time `json:"superseded_at"`
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
