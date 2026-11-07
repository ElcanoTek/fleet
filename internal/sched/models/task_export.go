package models

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
)

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
	Name string `json:"name,omitempty"                       yaml:"name,omitempty"`
	// Title is the operator-facing display label (non-unique, carried onto every
	// occurrence/copy). Exported so a task definition keeps its title when it is
	// moved between deployments. It maps to TaskCreate.Title.
	Title                  string              `json:"title,omitempty"                      yaml:"title,omitempty"`
	Prompt                 string              `json:"prompt"                               yaml:"prompt"`
	Model                  *string             `json:"model,omitempty"                      yaml:"model,omitempty"`
	FallbackModel          *string             `json:"fallback_model,omitempty"             yaml:"fallback_model,omitempty"`
	MaxIterations          *int                `json:"max_iterations,omitempty"             yaml:"max_iterations,omitempty"`
	MaxCostUSD             *float64            `json:"max_cost_usd,omitempty"               yaml:"max_cost_usd,omitempty"`
	MaxTotalTokens         *int                `json:"max_total_tokens,omitempty"           yaml:"max_total_tokens,omitempty"`
	MCPSelection           MCPSelection        `json:"mcp_selection,omitempty"              yaml:"mcp_selection,omitempty"`
	CredentialAllowlist    CredentialAllowlist `json:"credential_allowlist,omitempty" yaml:"credential_allowlist,omitempty"`
	LoopConfig             *LoopConfig         `json:"loop_config,omitempty"                yaml:"loop_config,omitempty"`
	WorktreeConfig         *WorktreeConfig     `json:"worktree_config,omitempty"         yaml:"worktree_config,omitempty"`
	SandboxLimits          *TaskSandboxLimits  `json:"sandbox_limits,omitempty"          yaml:"sandbox_limits,omitempty"`
	OutputSchema           json.RawMessage     `json:"output_schema,omitempty"           yaml:"output_schema,omitempty"`
	Priority               int                 `json:"priority,omitempty"                   yaml:"priority,omitempty"`
	InstructionSelfImprove bool                `json:"instruction_self_improve,omitempty"  yaml:"instruction_self_improve,omitempty"`
	AllowNetwork           bool                `json:"allow_network,omitempty"              yaml:"allow_network,omitempty"`
	CarryContext           bool                `json:"carry_context,omitempty"              yaml:"carry_context,omitempty"`
	AllowEventTriggers     bool                `json:"allow_event_triggers,omitempty"       yaml:"allow_event_triggers,omitempty"`
	// AllowDelegation mirrors TaskCreate.AllowDelegation (tri-state, #1043): a
	// pointer so exports written by this version always carry the explicit value
	// (TaskToExportRecord sets it) while an older export that omits the key
	// imports as nil → the default (true) rather than silently opting out.
	AllowDelegation            *bool        `json:"allow_delegation,omitempty"           yaml:"allow_delegation,omitempty"`
	ThinkingBudgetTokens       *int         `json:"thinking_budget_tokens,omitempty"     yaml:"thinking_budget_tokens,omitempty"`
	Persona                    string       `json:"persona,omitempty"                    yaml:"persona,omitempty"`
	Description                string       `json:"description,omitempty"                yaml:"description,omitempty"`
	ScheduledFor               *time.Time   `json:"scheduled_for,omitempty"              yaml:"scheduled_for,omitempty"`
	Recurrence                 string       `json:"recurrence,omitempty"                 yaml:"recurrence,omitempty"`
	RecurrenceUntil            *time.Time   `json:"recurrence_until,omitempty"           yaml:"recurrence_until,omitempty"`
	RecurrenceRemaining        *int         `json:"recurrence_remaining,omitempty"       yaml:"recurrence_remaining,omitempty"`
	Timezone                   string       `json:"timezone,omitempty"                   yaml:"timezone,omitempty"`
	Files                      []string     `json:"files,omitempty"                      yaml:"files,omitempty"`
	FileNames                  []string     `json:"file_names,omitempty"                 yaml:"file_names,omitempty"`
	Tags                       []string     `json:"tags,omitempty"                       yaml:"tags,omitempty"`
	MaxRetries                 *int         `json:"max_retries,omitempty"                yaml:"max_retries,omitempty"`
	RetryPolicy                *RetryPolicy `json:"retry_policy,omitempty"               yaml:"retry_policy,omitempty"`
	TriggerType                TriggerType  `json:"trigger_type,omitempty"               yaml:"trigger_type,omitempty"`
	AllowTaskCreation          bool         `json:"allow_task_creation,omitempty"        yaml:"allow_task_creation,omitempty"`
	AllowRecurringTaskCreation bool         `json:"allow_recurring_task_creation,omitempty" yaml:"allow_recurring_task_creation,omitempty"`
	// RunIf is the pre-run host-side gate (#269), part of the portable
	// definition: dropping it on export turned every gated task unconditional
	// on the target box, silently. Because the gate executes on the host as
	// the fleet user, importing a record that carries one requires admin
	// permission — the same boundary as authoring one (requireAdminForRunIf).
	// It maps to TaskCreate.RunIf.
	RunIf *RunIf `json:"run_if,omitempty" yaml:"run_if,omitempty"`
	// SLA monitoring config (#274) is part of the portable definition so an
	// exported task keeps its expected-duration + multiplier posture on reimport,
	// mirroring the clone-recipe (TaskToTaskCreate). Runtime SLA state
	// (sla_breached, actual_duration_seconds) is runtime-only and excluded, like
	// id/status/lease. The multipliers are carried only alongside an expected
	// duration (see TaskToExportRecord) since they are meaningless without it.
	ExpectedDurationMinutes *int    `json:"expected_duration_minutes,omitempty" yaml:"expected_duration_minutes,omitempty"`
	SLAWarnMultiplier       float64 `json:"sla_warn_multiplier,omitempty"       yaml:"sla_warn_multiplier,omitempty"`
	SLAFailMultiplier       float64 `json:"sla_fail_multiplier,omitempty"       yaml:"sla_fail_multiplier,omitempty"`
	// SerializationKey is the opaque mutual-exclusion token (#709), part of the
	// portable definition so an exported task keeps its serialization posture on
	// reimport. It maps to TaskCreate.SerializationKey.
	SerializationKey *string `json:"serialization_key,omitempty" yaml:"serialization_key,omitempty"`
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
		Title:                      rec.Title,
		Prompt:                     rec.Prompt,
		Model:                      rec.Model,
		FallbackModel:              rec.FallbackModel,
		MaxIterations:              rec.MaxIterations,
		MaxCostUSD:                 rec.MaxCostUSD,
		MaxTotalTokens:             rec.MaxTotalTokens,
		MCPSelection:               rec.MCPSelection,
		CredentialAllowlist:        rec.CredentialAllowlist,
		LoopConfig:                 rec.LoopConfig,
		WorktreeConfig:             rec.WorktreeConfig,
		SandboxLimits:              rec.SandboxLimits,
		OutputSchema:               rec.OutputSchema,
		Priority:                   rec.Priority,
		InstructionSelfImprove:     rec.InstructionSelfImprove,
		AllowNetwork:               rec.AllowNetwork,
		CarryContext:               rec.CarryContext,
		AllowEventTriggers:         rec.AllowEventTriggers,
		AllowDelegation:            rec.AllowDelegation,
		ThinkingBudgetTokens:       rec.ThinkingBudgetTokens,
		Persona:                    rec.Persona,
		Description:                rec.Description,
		ScheduledFor:               rec.ScheduledFor,
		Recurrence:                 rec.Recurrence,
		RecurrenceUntil:            rec.RecurrenceUntil,
		RecurrenceRemaining:        rec.RecurrenceRemaining,
		Timezone:                   rec.Timezone,
		Files:                      rec.Files,
		FileNames:                  rec.FileNames,
		Tags:                       rec.Tags,
		MaxRetries:                 maxRetries,
		RetryPolicy:                rec.RetryPolicy,
		TriggerType:                rec.TriggerType,
		AllowTaskCreation:          rec.AllowTaskCreation,
		AllowRecurringTaskCreation: rec.AllowRecurringTaskCreation,
		// RunIf (#269): the import handler has already enforced the admin
		// authoring boundary; NewTask parks the gated task on the scheduler
		// path exactly as the public create path does.
		RunIf: rec.RunIf,
		// SLA config (#274). Passed straight through; NewTask normalizes a
		// zero/absent multiplier to the default exactly as the public create path
		// does, so an imported definition resolves identically to one created via
		// POST /tasks.
		ExpectedDurationMinutes: rec.ExpectedDurationMinutes,
		SLAWarnMultiplier:       rec.SLAWarnMultiplier,
		SLAFailMultiplier:       rec.SLAFailMultiplier,
		SerializationKey:        rec.SerializationKey,
	}
}

// TaskToExportRecord extracts the portable definition of a Task — the inverse of
// ExportRecordToTaskCreate. Runtime fields (id, status, attempt_count, lease,
// results, created_by, timestamps, dead-letter, lineage) are dropped so an
// export envelope carries only the configuration needed to recreate the task.
func TaskToExportRecord(t *Task) TaskExportRecord {
	// MaxRetries is an int on Task but a *int on the export record. Drop only
	// the value equal to the deployment default (omitempty) so it round-trips
	// as "unset" and follows the default on import; any other value — including
	// an explicit 0 when the default is higher — is preserved verbatim, or a
	// task deliberately pinned to "no retries" would come back with one (#1538).
	var maxRetries *int
	if t.MaxRetries != DefaultMaxRetries() {
		v := t.MaxRetries
		maxRetries = &v
	}
	rec := TaskExportRecord{
		Name:                       t.Name,
		Title:                      t.Title,
		Prompt:                     t.Prompt,
		Model:                      t.Model,
		FallbackModel:              t.FallbackModel,
		MaxIterations:              t.MaxIterations,
		MaxCostUSD:                 t.MaxCostUSD,
		MaxTotalTokens:             t.MaxTotalTokens,
		MCPSelection:               t.MCPSelection,
		CredentialAllowlist:        t.CredentialAllowlist,
		LoopConfig:                 t.LoopConfig,
		WorktreeConfig:             t.WorktreeConfig,
		SandboxLimits:              t.SandboxLimits,
		OutputSchema:               t.OutputSchema,
		Priority:                   t.Priority,
		InstructionSelfImprove:     t.InstructionSelfImprove,
		AllowNetwork:               t.AllowNetwork,
		CarryContext:               t.CarryContext,
		AllowEventTriggers:         t.AllowEventTriggers,
		AllowDelegation:            BoolPtr(t.AllowDelegation),
		ThinkingBudgetTokens:       t.ThinkingBudgetTokens,
		Persona:                    t.Persona,
		Description:                t.Description,
		ScheduledFor:               t.ScheduledFor,
		Recurrence:                 t.Recurrence,
		RecurrenceUntil:            t.RecurrenceUntil,
		RecurrenceRemaining:        t.RecurrenceRemaining,
		Timezone:                   t.Timezone,
		Files:                      t.Files,
		FileNames:                  t.FileNames,
		Tags:                       t.Tags,
		MaxRetries:                 maxRetries,
		RetryPolicy:                t.RetryPolicy,
		TriggerType:                t.TriggerType,
		AllowTaskCreation:          t.AllowTaskCreation,
		AllowRecurringTaskCreation: t.AllowRecurringTaskCreation,
		RunIf:                      t.RunIf,
		SerializationKey:           t.SerializationKey,
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

// OverlayTaskDefinition applies a full imported definition (a TaskCreate
// materialized from a TaskExportRecord) onto an existing task IN PLACE — the
// import conflict=replace overlay (#238, #1104). It is the SINGLE overlay
// shared by the HTTP import handler and the admin CLI (both via
// storage.ReplaceTaskDefinition), so the two replace paths cannot drift
// field-by-field; before it existed each path hand-rolled its own field list
// and silently dropped the fields the other carried (issue #1104).
// TestOverlayTaskDefinitionCarriesEveryExportField pins it against every
// TaskExportRecord field, so a future export-record field cannot silently
// miss the overlay.
//
// Only DEFINITION fields are written. Execution state — id, status, lease,
// attempt_count, started_at/completed_at, result/error, skip/pause/wake/
// dead-letter state, created_by/lineage — is never copied from the record: a
// TaskExportRecord cannot even express it, and the caller re-derives
// status/scheduled_for with DeriveDispatchState after the overlay.
// Normalizations mirror NewTask exactly (priority, timezone, trigger type,
// SLA multipliers, serialization key), so a replaced definition resolves the
// same way a freshly created one would.
func OverlayTaskDefinition(task *Task, tc TaskCreate) error {
	// run_if is a full-definition field like the rest (replace already requires
	// admin, matching the authoring boundary) — but a PENDING task is past the
	// scheduled→pending promotion where a gate is evaluated (RunIf's enforcement
	// contract), so attaching or changing one there would be dead config for the
	// imminent dispatch; refuse, mirroring the PUT /tasks edit path. Removal
	// (record omits run_if) stays allowed: absence needs no evaluation point.
	// Checked first so a refusal leaves the task entirely unmodified.
	if !reflect.DeepEqual(tc.RunIf.Normalized(), task.RunIf.Normalized()) {
		if tc.RunIf != nil && task.Status == TaskStatusPending {
			return fmt.Errorf("run_if: task is already pending dispatch, so its gate can no longer be evaluated for this run")
		}
		task.RunIf = tc.RunIf
	}

	task.Name = tc.Name
	task.Title = tc.Title
	task.Prompt = tc.Prompt
	task.Model = tc.Model
	task.FallbackModel = tc.FallbackModel
	task.MaxIterations = tc.MaxIterations
	task.MaxCostUSD = tc.MaxCostUSD
	task.MaxTotalTokens = tc.MaxTotalTokens
	task.MCPSelection = tc.MCPSelection
	task.CredentialAllowlist = tc.CredentialAllowlist
	task.LoopConfig = tc.LoopConfig
	task.WorktreeConfig = tc.WorktreeConfig
	task.SandboxLimits = tc.SandboxLimits
	task.OutputSchema = tc.OutputSchema
	task.Priority = NormalizePriority(tc.Priority)
	task.InstructionSelfImprove = tc.InstructionSelfImprove
	task.AllowNetwork = tc.AllowNetwork
	task.CarryContext = tc.CarryContext
	task.AllowEventTriggers = tc.AllowEventTriggers
	task.AllowDelegation = tc.DelegationAllowed()
	task.ThinkingBudgetTokens = tc.ThinkingBudgetTokens
	task.Persona = tc.Persona
	task.Description = tc.Description
	task.ScheduledFor = tc.ScheduledFor
	task.Recurrence = tc.Recurrence
	task.RecurrenceUntil = tc.RecurrenceUntil
	task.RecurrenceRemaining = tc.RecurrenceRemaining
	if tc.Timezone != "" {
		task.Timezone = tc.Timezone
	} else {
		task.Timezone = "UTC"
	}
	task.Files = tc.Files
	task.FileNames = tc.FileNames
	task.Tags = tc.Tags
	task.MaxRetries = derefOr(tc.MaxRetries, 0)
	task.RetryPolicy = tc.RetryPolicy
	if tc.TriggerType != "" {
		task.TriggerType = tc.TriggerType
	} else {
		task.TriggerType = TriggerTypeCron
	}
	task.AllowTaskCreation = tc.AllowTaskCreation
	task.AllowRecurringTaskCreation = tc.AllowRecurringTaskCreation
	task.ExpectedDurationMinutes = tc.ExpectedDurationMinutes
	task.SLAWarnMultiplier, task.SLAFailMultiplier = ResolveSLAMultipliers(tc.SLAWarnMultiplier, tc.SLAFailMultiplier)
	// Serialization key (#709): normalized the same way NewTask does, so a
	// replaced definition can never carry a whitespace-only key the claim gate
	// would treat as a real key.
	task.SerializationKey = nil
	if tc.SerializationKey != nil {
		if trimmed := strings.TrimSpace(*tc.SerializationKey); trimmed != "" {
			task.SerializationKey = &trimmed
		}
	}
	return nil
}
