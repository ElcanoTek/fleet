package db

// task_columns.go — the table-driven task-column registry (#1126).
//
// The ~76-column tasks row used to be enumerated by hand in seven places that
// had to stay mutually consistent (taskColumns, taskInsertColumns + a manual
// count, AddTask's inline upsert, taskInsertOnConflict, UpdateTaskTx's UPDATE
// list, scanTask's positional scan, and the export/import records). That
// pattern produced real incidents: #710 (the insert-column count drifted and
// broke every batch insert) and #1104 (the import-replace overlays silently
// dropped fields). taskColumnRegistry below is now the ONE source of truth:
//
//   - taskColumns / scanTask derive from the rows flagged `read`, in registry
//     order, so the SELECT list and the positional scan agree by construction.
//   - The INSERT column list, its placeholder count, the ON CONFLICT upsert
//     clause and UpdateTaskTx's UPDATE list derive from the `insert`, `upsert`
//     and `txUpdate` flags. There is no manual column count anywhere.
//   - The `export` flag pins the portable-definition set: a test asserts it
//     matches models.TaskExportRecord field-for-field, and the #1104
//     completeness tests chain the record to ExportRecordToTaskCreate /
//     OverlayTaskDefinition, so the whole export→import path is machine-checked.
//   - Every deliberate exclusion carries a REQUIRED reason string (the no*
//     fields), so the "result-like columns are excluded from the insert/upsert"
//     doctrine that used to live only in comments is now validated at init and
//     by TestTaskColumnRegistryConsistent.
//
// Adding a task column is now: one migration + one registry row (+ the
// models.Task field, and a models.TaskExportRecord field iff the column is
// flagged export). TestTaskRegistrySchemaAgreement fails on a migration
// without a registry row (and vice versa).
//
// The derived statements are built ONCE at package init — scanTask and the
// write paths are hot, so per-call work stays at "fill a slice of scan
// targets / arguments", never per-row string building.

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// taskScanBuf holds the raw per-row scan destinations for one tasks row —
// the same intermediate values scanTask used to declare as one big var block.
// Each read-flagged registry row points its `dest` at one field and its
// `assign` moves the converted value onto the models.Task.
type taskScanBuf struct {
	id                     uuid.UUID
	name                   string
	title                  string
	prompt                 string
	model                  sql.NullString
	fallbackModel          sql.NullString
	maxIterations          sql.NullInt64
	mcpSelection           sql.NullString
	priority               int
	instructionSelfImprove bool
	status                 string
	agentSessionID         sql.NullString
	createdAt              time.Time
	startedAt              sql.NullTime
	completedAt            sql.NullTime
	result                 sql.NullString
	errorMessage           sql.NullString
	scheduledFor           sql.NullTime
	recurrence             sql.NullString
	createdBy              *uuid.UUID
	files                  sql.NullString
	leaseOwner             sql.NullString
	leaseExpiresAt         sql.NullTime
	attemptCount           int
	maxRetries             int
	allowNetwork           bool
	timezone               sql.NullString
	createdByKeyID         sql.NullString
	triggerType            sql.NullString
	credentialAllowlist    sql.NullString
	loopConfig             sql.NullString
	worktreeConfig         sql.NullString
	description            sql.NullString
	tags                   sql.NullString
	retryPolicy            sql.NullString
	sourceTaskID           sql.NullString
	persona                sql.NullString
	workspacePath          sql.NullString
	allowTaskCreation      bool
	allowRecurringTaskCre  bool
	createdByTaskID        sql.NullString
	deadLetteredAt         sql.NullTime
	deadLetterReason       sql.NullString
	deadLetterAttempts     sql.NullInt64
	runIf                  sql.NullString
	skipCount              int
	lastSkipAt             sql.NullTime
	lastSkipReason         sql.NullString
	expectedDur            sql.NullInt64
	slaWarnMul             sql.NullFloat64
	slaFailMul             sql.NullFloat64
	slaBreached            bool
	actualDurSecs          sql.NullInt64
	effectivePriority      int
	sandboxLimits          sql.NullString
	allowDelegation        bool
	thinkingBudget         sql.NullInt64
	outputSchema           sql.NullString
	outputJSON             sql.NullString
	pendingQuestion        sql.NullString
	pendingAnswer          sql.NullString
	carryContext           bool
	allowEventTriggers     bool
	errorAnalysis          sql.NullString
	artifacts              sql.NullString
	fileNames              sql.NullString
	serializationKey       sql.NullString
	recurrenceUntil        sql.NullTime
	recurrenceRemaining    sql.NullInt64
	wakeAt                 sql.NullTime
	wakeEventKey           sql.NullString
	wakeNote               sql.NullString
	wakeReason             sql.NullString
	wakeCycles             int
	pausedAt               sql.NullTime
}

// taskColumn is one row of the task-column registry: one tasks-table column,
// which derived statement sets it belongs to, why it is excluded from the
// sets it is not in, and the functions that bind it on the write and read
// paths. validateTaskColumnRegistry enforces the structural rules (exactly
// one of flag/reason per set, value/dest/assign present iff the flags need
// them, upsert ⇒ insert).
type taskColumn struct {
	// name is the SQL column name in the tasks table.
	name string

	// Set membership. read: the SELECT list + scanTask. insert: the INSERT
	// column list shared by AddTask / AddTaskTx / AddTaskBatch. upsert: the
	// ON CONFLICT (id) DO UPDATE clause appended to every insert (UpdateTask
	// routes through it). txUpdate: UpdateTaskTx's UPDATE ... SET list.
	// export: the portable-definition set mirrored by models.TaskExportRecord.
	read     bool
	insert   bool
	upsert   bool
	txUpdate bool
	export   bool

	// Exclusion rationale: for every set the column is NOT in, the matching
	// no* field must say why — the machine-checked replacement for the
	// excluded-column policy that used to live only in comments.
	noRead     string
	noInsert   string
	noUpsert   string
	noTxUpdate string
	noExport   string

	// value produces the bound SQL argument for the insert/upsert/txUpdate
	// paths. Required iff insert || txUpdate (the upsert reuses the inserted
	// value via EXCLUDED). One function serves all write paths so the
	// single-row, batch and tx writers can never disagree on a conversion.
	value func(t *models.Task) any

	// dest returns this column's scan destination inside the per-row buffer;
	// assign converts the scanned value onto the Task. Required iff read.
	dest   func(b *taskScanBuf) any
	assign func(b *taskScanBuf, t *models.Task)
}

// Shared exclusion rationales for the column groups that follow one doctrine.
const (
	// reasonRuntimeState marks columns that are execution/runtime state — not
	// part of the portable task definition an export envelope carries (#238).
	reasonRuntimeState = "runtime/execution state, not part of the portable definition (#238)"
	// reasonWakeState marks the self-wake park state (docs/SELF-WAKE.md):
	// written only by the guarded park/wake transitions in storage, so no
	// generic write path may ever clobber a park.
	reasonWakeState = "self-wake runtime state (docs/SELF-WAKE.md): written only by the guarded park/wake transitions; a generic write could clobber a park"
	// reasonPauseState marks the ask/notify pause fields (#510): they are in
	// UpdateTaskTx (the pause/resume transitions write through it under the
	// row lock) but excluded from insert/upsert so a status write routed
	// through UpdateTask→AddTask can never fabricate or drop a pause.
	reasonPauseState = "ask/notify pause state (#510): written via UpdateTaskTx under the row lock; excluded here so an upsert status write can't fabricate or drop a pause"
)

// taskColumnRegistry is the single source of truth for the tasks-table row
// shape. Order matters: it is the SELECT/scan order (read set) and the INSERT
// column order (insert set), so append new columns at the end (before
// recurrence_spawned if the new column is read back; order within the list is
// otherwise only cosmetic for the write sets).
var taskColumnRegistry = []taskColumn{
	{
		name: "id",
		read: true, insert: true,
		noUpsert:   "primary key: the ON CONFLICT (id) conflict target, never a SET column",
		noTxUpdate: "immutable primary key; bound as the UPDATE's WHERE key, not a SET column",
		noExport:   "identity: the import target mints a fresh id (#238)",
		value:      func(t *models.Task) any { return t.ID },
		dest:       func(b *taskScanBuf) any { return &b.id },
		assign:     func(b *taskScanBuf, t *models.Task) { t.ID = b.id },
	},
	{
		name: "name",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		// name joined UpdateTaskTx for import conflict=replace (#1104), the only
		// tx write path that changes it: every other UpdateTaskTx caller writes
		// back the value it scanned under the same row lock.
		value:  func(t *models.Task) any { return t.Name },
		dest:   func(b *taskScanBuf) any { return &b.name },
		assign: func(b *taskScanBuf, t *models.Task) { t.Name = b.name },
	},
	{
		name: "prompt",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value:  func(t *models.Task) any { return t.Prompt },
		dest:   func(b *taskScanBuf) any { return &b.prompt },
		assign: func(b *taskScanBuf, t *models.Task) { t.Prompt = b.prompt },
	},
	{
		name: "model",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value: func(t *models.Task) any { return t.Model },
		dest:  func(b *taskScanBuf) any { return &b.model },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.model.Valid {
				t.Model = &b.model.String
			}
		},
	},
	{
		name: "fallback_model",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value: func(t *models.Task) any { return t.FallbackModel },
		dest:  func(b *taskScanBuf) any { return &b.fallbackModel },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.fallbackModel.Valid {
				t.FallbackModel = &b.fallbackModel.String
			}
		},
	},
	{
		name: "max_iterations",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value: func(t *models.Task) any { return t.MaxIterations },
		dest:  func(b *taskScanBuf) any { return &b.maxIterations },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.maxIterations.Valid {
				value := int(b.maxIterations.Int64)
				t.MaxIterations = &value
			}
		},
	},
	{
		name: "mcp_selection",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value: func(t *models.Task) any { return marshalJSON(mcpSelectionOrEmpty(t.MCPSelection)) },
		dest:  func(b *taskScanBuf) any { return &b.mcpSelection },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.mcpSelection.Valid {
				t.MCPSelection = unmarshalMCPSelection(b.mcpSelection.String)
			} else {
				t.MCPSelection = models.MCPSelection{}
			}
		},
	},
	{
		name: "priority",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value:  func(t *models.Task) any { return t.Priority },
		dest:   func(b *taskScanBuf) any { return &b.priority },
		assign: func(b *taskScanBuf, t *models.Task) { t.Priority = b.priority },
	},
	{
		name: "instruction_self_improve",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value:  func(t *models.Task) any { return t.InstructionSelfImprove },
		dest:   func(b *taskScanBuf) any { return &b.instructionSelfImprove },
		assign: func(b *taskScanBuf, t *models.Task) { t.InstructionSelfImprove = b.instructionSelfImprove },
	},
	{
		name: "status",
		read: true, insert: true, upsert: true, txUpdate: true,
		noExport: "runtime state: import re-derives status/scheduled_for via DeriveDispatchState (#238)",
		value:    func(t *models.Task) any { return string(t.Status) },
		dest:     func(b *taskScanBuf) any { return &b.status },
		assign:   func(b *taskScanBuf, t *models.Task) { t.Status = models.TaskStatus(b.status) },
	},
	{
		name: "agent_session_id",
		read: true, insert: true, upsert: true, txUpdate: true,
		noExport: reasonRuntimeState,
		value:    func(t *models.Task) any { return t.AgentSessionID },
		dest:     func(b *taskScanBuf) any { return &b.agentSessionID },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.agentSessionID.Valid {
				t.AgentSessionID = &b.agentSessionID.String
			}
		},
	},
	{
		name: "created_at",
		read: true, insert: true, upsert: true, txUpdate: true,
		noExport: reasonRuntimeState,
		value:    func(t *models.Task) any { return t.CreatedAt },
		dest:     func(b *taskScanBuf) any { return &b.createdAt },
		assign:   func(b *taskScanBuf, t *models.Task) { t.CreatedAt = b.createdAt },
	},
	{
		name: "started_at",
		read: true, insert: true, upsert: true, txUpdate: true,
		noExport: reasonRuntimeState,
		value:    func(t *models.Task) any { return t.StartedAt },
		dest:     func(b *taskScanBuf) any { return &b.startedAt },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.startedAt.Valid {
				t.StartedAt = &b.startedAt.Time
			}
		},
	},
	{
		name: "completed_at",
		read: true, insert: true, upsert: true, txUpdate: true,
		noExport: reasonRuntimeState,
		value:    func(t *models.Task) any { return t.CompletedAt },
		dest:     func(b *taskScanBuf) any { return &b.completedAt },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.completedAt.Valid {
				t.CompletedAt = &b.completedAt.Time
			}
		},
	},
	{
		name: "result",
		read: true, insert: true, upsert: true, txUpdate: true,
		noExport: reasonRuntimeState,
		value:    func(t *models.Task) any { return t.Result },
		dest:     func(b *taskScanBuf) any { return &b.result },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.result.Valid {
				t.Result = &b.result.String
			}
		},
	},
	{
		name: "error_message",
		read: true, insert: true, upsert: true, txUpdate: true,
		noExport: reasonRuntimeState,
		value:    func(t *models.Task) any { return t.ErrorMessage },
		dest:     func(b *taskScanBuf) any { return &b.errorMessage },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.errorMessage.Valid {
				t.ErrorMessage = &b.errorMessage.String
			}
		},
	},
	{
		name: "scheduled_for",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value: func(t *models.Task) any { return t.ScheduledFor },
		dest:  func(b *taskScanBuf) any { return &b.scheduledFor },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.scheduledFor.Valid {
				t.ScheduledFor = &b.scheduledFor.Time
			}
		},
	},
	{
		name: "recurrence",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value: func(t *models.Task) any { return nullableString(t.Recurrence) },
		dest:  func(b *taskScanBuf) any { return &b.recurrence },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.recurrence.Valid {
				t.Recurrence = b.recurrence.String
			}
		},
	},
	{
		name: "created_by",
		read: true, insert: true, upsert: true, txUpdate: true,
		noExport: "creation-time provenance; the import target records its own creator (#238)",
		value:    func(t *models.Task) any { return t.CreatedBy },
		dest:     func(b *taskScanBuf) any { return &b.createdBy },
		assign:   func(b *taskScanBuf, t *models.Task) { t.CreatedBy = b.createdBy },
	},
	{
		name: "files",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value: func(t *models.Task) any { return marshalJSON(t.Files) },
		dest:  func(b *taskScanBuf) any { return &b.files },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.files.Valid {
				t.Files = unmarshalStringSlice(b.files.String)
			}
		},
	},
	{
		name: "lease_owner",
		read: true, insert: true, upsert: true, txUpdate: true,
		noExport: reasonRuntimeState,
		value:    func(t *models.Task) any { return t.LeaseOwner },
		dest:     func(b *taskScanBuf) any { return &b.leaseOwner },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.leaseOwner.Valid {
				t.LeaseOwner = &b.leaseOwner.String
			}
		},
	},
	{
		name: "lease_expires_at",
		read: true, insert: true, upsert: true, txUpdate: true,
		noExport: reasonRuntimeState,
		value:    func(t *models.Task) any { return t.LeaseExpiresAt },
		dest:     func(b *taskScanBuf) any { return &b.leaseExpiresAt },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.leaseExpiresAt.Valid {
				t.LeaseExpiresAt = &b.leaseExpiresAt.Time
			}
		},
	},
	{
		name: "attempt_count",
		read: true, insert: true, upsert: true, txUpdate: true,
		noExport: reasonRuntimeState,
		value:    func(t *models.Task) any { return t.AttemptCount },
		dest:     func(b *taskScanBuf) any { return &b.attemptCount },
		assign:   func(b *taskScanBuf, t *models.Task) { t.AttemptCount = b.attemptCount },
	},
	{
		name: "max_retries",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value:  func(t *models.Task) any { return t.MaxRetries },
		dest:   func(b *taskScanBuf) any { return &b.maxRetries },
		assign: func(b *taskScanBuf, t *models.Task) { t.MaxRetries = b.maxRetries },
	},
	{
		name: "allow_network",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value:  func(t *models.Task) any { return t.AllowNetwork },
		dest:   func(b *taskScanBuf) any { return &b.allowNetwork },
		assign: func(b *taskScanBuf, t *models.Task) { t.AllowNetwork = b.allowNetwork },
	},
	{
		name: "timezone",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value:  func(t *models.Task) any { return taskTimezoneOrUTC(t.Timezone) },
		dest:   func(b *taskScanBuf) any { return &b.timezone },
		assign: func(b *taskScanBuf, t *models.Task) { t.Timezone = taskTimezoneOrUTC(b.timezone.String) },
	},
	{
		name: "created_by_key_id",
		read: true, insert: true,
		// Provenance is IMMUTABLE after creation (#1270). #1126 inherited an
		// asymmetry it could only preserve: the column was in the upsert set
		// (so UpdateTask → AddTask could rewrite a task's submitting API key)
		// but never in UpdateTaskTx, with no rationale recorded either side.
		// The write policy is now decided rather than inherited — the row's
		// provenance is stamped by the ONE insert that creates it and by
		// nothing afterwards, so a later generic write (an operator import
		// landing on an existing id, a spawn/edit round-trip through an
		// upsert) can neither re-attribute the row to another key nor clear
		// the attribution the authorization paths read (handlers/task_authz.go
		// + log_authz.go key their own-rows checks on it).
		noUpsert:   "creation-time provenance: stamped by the insert that creates the row and immutable afterwards, so no generic write path (import upsert included, #1267) can re-attribute or clear it (#1270)",
		noTxUpdate: "creation-time provenance: immutable after the creating insert (#1270) — the tx update path re-writes a scanned row, it never re-stamps who submitted it",
		noExport:   "creation-time provenance (the submitting API key); meaningless on the import target (#238)",
		value:      func(t *models.Task) any { return t.CreatedByKeyID },
		dest:       func(b *taskScanBuf) any { return &b.createdByKeyID },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.createdByKeyID.Valid {
				t.CreatedByKeyID = &b.createdByKeyID.String
			}
		},
	},
	{
		name: "trigger_type",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		// trigger_type joined UpdateTaskTx for import conflict=replace (#1104).
		value: func(t *models.Task) any { return triggerTypeOrCron(t.TriggerType) },
		dest:  func(b *taskScanBuf) any { return &b.triggerType },
		assign: func(b *taskScanBuf, t *models.Task) {
			t.TriggerType = models.TriggerType(triggerTypeOrCronStr(b.triggerType.String))
		},
	},
	{
		name: "credential_allowlist",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value: func(t *models.Task) any { return marshalCredentialAllowlist(t.CredentialAllowlist) },
		dest:  func(b *taskScanBuf) any { return &b.credentialAllowlist },
		// NULL → nil (inherit global); "[]" → non-nil empty (deny all). The
		// distinction is load-bearing for Gate-3, so do NOT coerce nil to empty.
		assign: func(b *taskScanBuf, t *models.Task) {
			t.CredentialAllowlist = unmarshalCredentialAllowlist(b.credentialAllowlist)
		},
	},
	{
		name: "loop_config",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value:  func(t *models.Task) any { return marshalLoopConfig(t.LoopConfig) },
		dest:   func(b *taskScanBuf) any { return &b.loopConfig },
		assign: func(b *taskScanBuf, t *models.Task) { t.LoopConfig = unmarshalLoopConfig(b.loopConfig) },
	},
	{
		name: "worktree_config",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value:  func(t *models.Task) any { return marshalWorktreeConfig(t.WorktreeConfig) },
		dest:   func(b *taskScanBuf) any { return &b.worktreeConfig },
		assign: func(b *taskScanBuf, t *models.Task) { t.WorktreeConfig = unmarshalWorktreeConfig(b.worktreeConfig) },
	},
	{
		name: "description",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value:  func(t *models.Task) any { return nullableString(t.Description) },
		dest:   func(b *taskScanBuf) any { return &b.description },
		assign: func(b *taskScanBuf, t *models.Task) { t.Description = b.description.String },
	},
	{
		name: "tags",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value: func(t *models.Task) any { return marshalTags(t.Tags) },
		dest:  func(b *taskScanBuf) any { return &b.tags },
		// tags is NOT NULL DEFAULT '[]', so it's always present; assign
		// unconditionally (unmarshalStringSlice maps ""/"null" → empty safely).
		assign: func(b *taskScanBuf, t *models.Task) { t.Tags = unmarshalStringSlice(b.tags.String) },
	},
	{
		name: "retry_policy",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value:  func(t *models.Task) any { return marshalRetryPolicy(t.RetryPolicy) },
		dest:   func(b *taskScanBuf) any { return &b.retryPolicy },
		assign: func(b *taskScanBuf, t *models.Task) { t.RetryPolicy = unmarshalRetryPolicy(b.retryPolicy) },
	},
	{
		name: "source_task_id",
		read: true, insert: true, upsert: true, txUpdate: true,
		noExport: "per-copy lineage (re-run/clone parent, #270), intentionally not portable",
		value:    func(t *models.Task) any { return sourceTaskIDValue(t.SourceTaskID) },
		dest:     func(b *taskScanBuf) any { return &b.sourceTaskID },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.sourceTaskID.Valid && b.sourceTaskID.String != "" {
				if sid, perr := uuid.Parse(b.sourceTaskID.String); perr == nil {
					t.SourceTaskID = &sid
				} else {
					log.Printf("Warning: invalid source_task_id %q: %v", b.sourceTaskID.String, perr)
				}
			}
		},
	},
	{
		name: "persona",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value:  func(t *models.Task) any { return nullableString(t.Persona) },
		dest:   func(b *taskScanBuf) any { return &b.persona },
		assign: func(b *taskScanBuf, t *models.Task) { t.Persona = b.persona.String },
	},
	{
		name: "workspace_path",
		read: true, insert: true, upsert: true, txUpdate: true,
		noExport: reasonRuntimeState,
		value:    func(t *models.Task) any { return workspacePathValue(t.WorkspacePath) },
		dest:     func(b *taskScanBuf) any { return &b.workspacePath },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.workspacePath.Valid && b.workspacePath.String != "" {
				t.WorkspacePath = &b.workspacePath.String
			}
		},
	},
	{
		name: "allow_task_creation",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value:  func(t *models.Task) any { return t.AllowTaskCreation },
		dest:   func(b *taskScanBuf) any { return &b.allowTaskCreation },
		assign: func(b *taskScanBuf, t *models.Task) { t.AllowTaskCreation = b.allowTaskCreation },
	},
	{
		name: "allow_recurring_task_creation",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value:  func(t *models.Task) any { return t.AllowRecurringTaskCreation },
		dest:   func(b *taskScanBuf) any { return &b.allowRecurringTaskCre },
		assign: func(b *taskScanBuf, t *models.Task) { t.AllowRecurringTaskCreation = b.allowRecurringTaskCre },
	},
	{
		name: "created_by_task_id",
		read: true, insert: true, upsert: true, txUpdate: true,
		noExport: "per-spawn lineage (create_task parent, #277), intentionally not portable",
		value:    func(t *models.Task) any { return createdByTaskIDValue(t.CreatedByTaskID) },
		dest:     func(b *taskScanBuf) any { return &b.createdByTaskID },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.createdByTaskID.Valid && b.createdByTaskID.String != "" {
				if cid, perr := uuid.Parse(b.createdByTaskID.String); perr == nil {
					t.CreatedByTaskID = &cid
				} else {
					log.Printf("Warning: invalid created_by_task_id %q: %v", b.createdByTaskID.String, perr)
				}
			}
		},
	},
	{
		name: "dead_lettered_at",
		read: true, insert: true, upsert: true, txUpdate: true,
		noExport: reasonRuntimeState,
		value:    func(t *models.Task) any { return t.DeadLetteredAt },
		dest:     func(b *taskScanBuf) any { return &b.deadLetteredAt },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.deadLetteredAt.Valid {
				t.DeadLetteredAt = &b.deadLetteredAt.Time
			}
		},
	},
	{
		name: "dead_letter_reason",
		read: true, insert: true, upsert: true, txUpdate: true,
		noExport: reasonRuntimeState,
		value:    func(t *models.Task) any { return nullableString(deref(t.DeadLetterReason)) },
		dest:     func(b *taskScanBuf) any { return &b.deadLetterReason },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.deadLetterReason.Valid {
				t.DeadLetterReason = &b.deadLetterReason.String
			}
		},
	},
	{
		name: "dead_letter_attempts",
		read: true, insert: true, upsert: true, txUpdate: true,
		noExport: reasonRuntimeState,
		value:    func(t *models.Task) any { return deadLetterAttemptsValue(t.DeadLetterAttempts) },
		dest:     func(b *taskScanBuf) any { return &b.deadLetterAttempts },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.deadLetterAttempts.Valid {
				t.DeadLetterAttempts = int(b.deadLetterAttempts.Int64)
			}
		},
	},
	{
		name: "run_if",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value:  func(t *models.Task) any { return marshalRunIf(t.RunIf) },
		dest:   func(b *taskScanBuf) any { return &b.runIf },
		assign: func(b *taskScanBuf, t *models.Task) { t.RunIf = unmarshalRunIf(b.runIf) },
	},
	{
		name: "skip_count",
		read: true, insert: true, upsert: true, txUpdate: true,
		noExport: "per-occurrence run_if telemetry (#269), not definition",
		value:    func(t *models.Task) any { return t.SkipCount },
		dest:     func(b *taskScanBuf) any { return &b.skipCount },
		assign:   func(b *taskScanBuf, t *models.Task) { t.SkipCount = b.skipCount },
	},
	{
		name: "last_skip_at",
		read: true, insert: true, upsert: true, txUpdate: true,
		noExport: "per-occurrence run_if telemetry (#269), not definition",
		value:    func(t *models.Task) any { return t.LastSkipAt },
		dest:     func(b *taskScanBuf) any { return &b.lastSkipAt },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.lastSkipAt.Valid {
				t.LastSkipAt = &b.lastSkipAt.Time
			}
		},
	},
	{
		name: "last_skip_reason",
		read: true, insert: true, upsert: true, txUpdate: true,
		noExport: "per-occurrence run_if telemetry (#269), not definition",
		value:    func(t *models.Task) any { return nullableString(deref(t.LastSkipReason)) },
		dest:     func(b *taskScanBuf) any { return &b.lastSkipReason },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.lastSkipReason.Valid {
				t.LastSkipReason = &b.lastSkipReason.String
			}
		},
	},
	{
		name: "expected_duration_minutes",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value: func(t *models.Task) any { return expectedDurationValue(t.ExpectedDurationMinutes) },
		dest:  func(b *taskScanBuf) any { return &b.expectedDur },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.expectedDur.Valid {
				v := int(b.expectedDur.Int64)
				t.ExpectedDurationMinutes = &v
			}
		},
	},
	{
		name: "sla_warn_multiplier",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value: func(t *models.Task) any {
			return slaMultiplierValue(t.SLAWarnMultiplier, models.DefaultSLAWarnMultiplier)
		},
		dest: func(b *taskScanBuf) any { return &b.slaWarnMul },
		// The multipliers are NOT NULL DEFAULT so they are always present —
		// normalize a stray zero to the default so a downstream monitor /
		// report never divides by zero (#274).
		assign: func(b *taskScanBuf, t *models.Task) {
			t.SLAWarnMultiplier = slaMultiplierValue(b.slaWarnMul.Float64, models.DefaultSLAWarnMultiplier)
		},
	},
	{
		name: "sla_fail_multiplier",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value: func(t *models.Task) any {
			return slaMultiplierValue(t.SLAFailMultiplier, models.DefaultSLAFailMultiplier)
		},
		dest: func(b *taskScanBuf) any { return &b.slaFailMul },
		assign: func(b *taskScanBuf, t *models.Task) {
			t.SLAFailMultiplier = slaMultiplierValue(b.slaFailMul.Float64, models.DefaultSLAFailMultiplier)
		},
	},
	{
		name: "sla_breached",
		read: true, insert: true, upsert: true, txUpdate: true,
		noExport: "runtime SLA state (#274): latched by the SLA monitor, cleared on replay",
		value:    func(t *models.Task) any { return t.SLABreached },
		dest:     func(b *taskScanBuf) any { return &b.slaBreached },
		assign:   func(b *taskScanBuf, t *models.Task) { t.SLABreached = b.slaBreached },
	},
	{
		name: "actual_duration_seconds",
		read: true, insert: true, upsert: true, txUpdate: true,
		noExport: "runtime SLA state (#274): derived on the terminal transition",
		value:    func(t *models.Task) any { return t.ActualDurationSeconds },
		dest:     func(b *taskScanBuf) any { return &b.actualDurSecs },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.actualDurSecs.Valid {
				v := int(b.actualDurSecs.Int64)
				t.ActualDurationSeconds = &v
			}
		},
	},
	{
		name: "effective_priority",
		read: true, insert: true,
		noUpsert:   "write-once at INSERT, thereafter mutated ONLY by the anti-starvation sweep (#230): in the upsert, a status update carrying a stale in-memory copy would silently un-promote a task",
		noTxUpdate: "same doctrine as the upsert exclusion (#230): only the anti-starvation sweep may change it after INSERT",
		noExport:   "runtime scheduling state (#230): NewTask re-derives it from priority on the import target",
		value:      func(t *models.Task) any { return effectivePriorityValue(t) },
		dest:       func(b *taskScanBuf) any { return &b.effectivePriority },
		assign:     func(b *taskScanBuf, t *models.Task) { t.EffectivePriority = b.effectivePriority },
	},
	{
		name: "sandbox_limits",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		// sandbox_limits (#205) IS in the upsert — it has no out-of-band mutator.
		value:  func(t *models.Task) any { return marshalSandboxLimits(t.SandboxLimits) },
		dest:   func(b *taskScanBuf) any { return &b.sandboxLimits },
		assign: func(b *taskScanBuf, t *models.Task) { t.SandboxLimits = unmarshalSandboxLimits(b.sandboxLimits) },
	},
	{
		name: "allow_delegation",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value:  func(t *models.Task) any { return t.AllowDelegation },
		dest:   func(b *taskScanBuf) any { return &b.allowDelegation },
		assign: func(b *taskScanBuf, t *models.Task) { t.AllowDelegation = b.allowDelegation },
	},
	{
		name: "output_schema",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		// output_schema (#244) is immutable post-create by convention, but it has
		// always been carried by every write path (harmless: writers echo the
		// scanned value); the registry preserves that verbatim.
		value:  func(t *models.Task) any { return marshalRawJSON(t.OutputSchema) },
		dest:   func(b *taskScanBuf) any { return &b.outputSchema },
		assign: func(b *taskScanBuf, t *models.Task) { t.OutputSchema = unmarshalRawJSON(b.outputSchema) },
	},
	{
		name: "output_json",
		read: true, insert: true, upsert: true, txUpdate: true,
		// output_json IS written post-run via UpdateTask→upsert, like result —
		// both belong in the upsert (#244).
		noExport: "run output (#244), not definition",
		value:    func(t *models.Task) any { return marshalRawJSON(t.OutputJSON) },
		dest:     func(b *taskScanBuf) any { return &b.outputJSON },
		assign:   func(b *taskScanBuf, t *models.Task) { t.OutputJSON = unmarshalRawJSON(b.outputJSON) },
	},
	{
		name:       "error_analysis",
		read:       true,
		noInsert:   "result-like (#317): written only by SetTaskErrorAnalysis after the terminal transition; a task write must never clobber a diagnosis",
		noUpsert:   "result-like (#317): a status write routed through the upsert must never clobber a diagnosis written after the terminal transition",
		noTxUpdate: "result-like (#317): written lease-free after the terminal transition, so no tx write path may carry a stale copy",
		noExport:   reasonRuntimeState,
		dest:       func(b *taskScanBuf) any { return &b.errorAnalysis },
		assign:     func(b *taskScanBuf, t *models.Task) { t.ErrorAnalysis = unmarshalRawJSON(b.errorAnalysis) },
	},
	{
		name: "artifacts",
		read: true, txUpdate: true,
		noInsert: "result-like (#204): persisted by the run via UpdateTaskTx under the row lock; a fresh insert never carries a published manifest",
		noUpsert: "result-like (#204): a status write routed through the upsert (UpdateTask→AddTask) must never clobber a published manifest with a stale in-memory copy",
		noExport: reasonRuntimeState,
		value:    func(t *models.Task) any { return marshalRawJSON(t.Artifacts) },
		dest:     func(b *taskScanBuf) any { return &b.artifacts },
		assign:   func(b *taskScanBuf, t *models.Task) { t.Artifacts = unmarshalRawJSON(b.artifacts) },
	},
	{
		name: "pending_question",
		read: true, txUpdate: true,
		noInsert: reasonPauseState,
		noUpsert: reasonPauseState,
		noExport: reasonRuntimeState,
		value:    func(t *models.Task) any { return nullableString(t.PendingQuestion) },
		dest:     func(b *taskScanBuf) any { return &b.pendingQuestion },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.pendingQuestion.Valid {
				t.PendingQuestion = b.pendingQuestion.String
			}
		},
	},
	{
		name: "pending_answer",
		read: true, txUpdate: true,
		noInsert: reasonPauseState,
		noUpsert: reasonPauseState,
		noExport: reasonRuntimeState,
		value:    func(t *models.Task) any { return nullableString(t.PendingAnswer) },
		dest:     func(b *taskScanBuf) any { return &b.pendingAnswer },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.pendingAnswer.Valid {
				t.PendingAnswer = b.pendingAnswer.String
			}
		},
	},
	{
		name: "carry_context",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value:  func(t *models.Task) any { return t.CarryContext },
		dest:   func(b *taskScanBuf) any { return &b.carryContext },
		assign: func(b *taskScanBuf, t *models.Task) { t.CarryContext = b.carryContext },
	},
	{
		name: "allow_event_triggers",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		// allow_event_triggers joined UpdateTaskTx for import conflict=replace (#1104).
		value:  func(t *models.Task) any { return t.AllowEventTriggers },
		dest:   func(b *taskScanBuf) any { return &b.allowEventTriggers },
		assign: func(b *taskScanBuf, t *models.Task) { t.AllowEventTriggers = b.allowEventTriggers },
	},
	{
		name: "thinking_budget_tokens",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value: func(t *models.Task) any { return thinkingBudgetValue(t.ThinkingBudgetTokens) },
		dest:  func(b *taskScanBuf) any { return &b.thinkingBudget },
		// Per-task thinking override (#220): NULL = inherit the global default.
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.thinkingBudget.Valid {
				v := int(b.thinkingBudget.Int64)
				t.ThinkingBudgetTokens = &v
			}
		},
	},
	{
		name: "file_names",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value: func(t *models.Task) any { return marshalJSON(t.FileNames) },
		dest:  func(b *taskScanBuf) any { return &b.fileNames },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.fileNames.Valid {
				t.FileNames = unmarshalStringSlice(b.fileNames.String)
			}
		},
	},
	{
		name: "serialization_key",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		// serialization_key joined UpdateTaskTx for import conflict=replace (#1104).
		value: func(t *models.Task) any { return serializationKeyValue(t.SerializationKey) },
		dest:  func(b *taskScanBuf) any { return &b.serializationKey },
		// NULL = unserialized (#709). The write path normalizes ""/whitespace to
		// NULL (serializationKeyValue), so a Valid value is always a real key.
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.serializationKey.Valid {
				t.SerializationKey = &b.serializationKey.String
			}
		},
	},
	{
		name: "recurrence_until",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value: func(t *models.Task) any { return t.RecurrenceUntil },
		dest:  func(b *taskScanBuf) any { return &b.recurrenceUntil },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.recurrenceUntil.Valid {
				v := b.recurrenceUntil.Time
				t.RecurrenceUntil = &v
			}
		},
	},
	{
		name: "recurrence_remaining",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value: func(t *models.Task) any { return recurrenceRemainingValue(t.RecurrenceRemaining) },
		dest:  func(b *taskScanBuf) any { return &b.recurrenceRemaining },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.recurrenceRemaining.Valid {
				v := int(b.recurrenceRemaining.Int64)
				t.RecurrenceRemaining = &v
			}
		},
	},
	{
		name:       "wake_at",
		read:       true,
		noInsert:   reasonWakeState,
		noUpsert:   reasonWakeState,
		noTxUpdate: reasonWakeState,
		noExport:   reasonRuntimeState,
		dest:       func(b *taskScanBuf) any { return &b.wakeAt },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.wakeAt.Valid {
				v := b.wakeAt.Time
				t.WakeAt = &v
			}
		},
	},
	{
		name:       "wake_event_key",
		read:       true,
		noInsert:   reasonWakeState,
		noUpsert:   reasonWakeState,
		noTxUpdate: reasonWakeState,
		noExport:   reasonRuntimeState,
		dest:       func(b *taskScanBuf) any { return &b.wakeEventKey },
		assign:     func(b *taskScanBuf, t *models.Task) { t.WakeEventKey = b.wakeEventKey.String },
	},
	{
		name:       "wake_note",
		read:       true,
		noInsert:   reasonWakeState,
		noUpsert:   reasonWakeState,
		noTxUpdate: reasonWakeState,
		noExport:   reasonRuntimeState,
		dest:       func(b *taskScanBuf) any { return &b.wakeNote },
		assign:     func(b *taskScanBuf, t *models.Task) { t.WakeNote = b.wakeNote.String },
	},
	{
		name:       "wake_reason",
		read:       true,
		noInsert:   reasonWakeState,
		noUpsert:   reasonWakeState,
		noTxUpdate: reasonWakeState,
		noExport:   reasonRuntimeState,
		dest:       func(b *taskScanBuf) any { return &b.wakeReason },
		assign:     func(b *taskScanBuf, t *models.Task) { t.WakeReason = b.wakeReason.String },
	},
	{
		name:       "wake_cycles",
		read:       true,
		noInsert:   reasonWakeState,
		noUpsert:   reasonWakeState,
		noTxUpdate: reasonWakeState,
		noExport:   reasonRuntimeState,
		dest:       func(b *taskScanBuf) any { return &b.wakeCycles },
		assign:     func(b *taskScanBuf, t *models.Task) { t.WakeCycles = b.wakeCycles },
	},
	{
		name: "title",
		read: true, insert: true, upsert: true, txUpdate: true, export: true,
		value:  func(t *models.Task) any { return t.Title },
		dest:   func(b *taskScanBuf) any { return &b.title },
		assign: func(b *taskScanBuf, t *models.Task) { t.Title = b.title },
	},
	{
		name:       "paused_at",
		read:       true,
		noInsert:   "pause clock (#1116): stamped only by the guarded pause transitions (PauseTaskForQuestion / PauseTaskForWake); a task write must never re-stamp or clear it",
		noUpsert:   "pause clock (#1116): a status write routed through the upsert must never clobber the paused-expiry baseline",
		noTxUpdate: "pause clock (#1116): written only by the guarded pause transitions, never by the generic tx update",
		noExport:   reasonRuntimeState,
		dest:       func(b *taskScanBuf) any { return &b.pausedAt },
		assign: func(b *taskScanBuf, t *models.Task) {
			if b.pausedAt.Valid {
				v := b.pausedAt.Time
				t.PausedAt = &v
			}
		},
	},
	{
		name:       "recurrence_spawned",
		insert:     true,
		noRead:     "no Task field: consumed only by the guarded spawn/settle SQL predicates in storage (#1116)",
		noUpsert:   "insert-only (#1116): after INSERT it is owned by the guarded spawn/settle statements; an upsert write could clobber a claimed spawn credit",
		noTxUpdate: "insert-only (#1116): same doctrine as the upsert exclusion — only the guarded spawn/settle statements may change it",
		noExport:   "runtime settlement marker (#1116): derived at insert (recurrenceSpawnedInsertValue) so restored terminal rows land settled",
		value:      func(t *models.Task) any { return recurrenceSpawnedInsertValue(t) },
	},
}

// Derived column sets and SQL fragments/statements, built once at package
// init from taskColumnRegistry. These are on hot paths (scanTask runs per
// row), so nothing here is rebuilt per call.
var (
	// taskReadSet / taskInsertSet / taskTxUpdateSet are the registry rows in
	// each derived set, in registry order. scanTask iterates taskReadSet for
	// both the SELECT list and the positional scan, so the two agree by
	// construction.
	taskReadSet     []*taskColumn
	taskInsertSet   []*taskColumn
	taskTxUpdateSet []*taskColumn

	// taskColumns is the SELECT column list every task read uses.
	taskColumns string
	// taskInsertColumns is the ordered column list for the tasks INSERT,
	// shared by the single-row, multi-row and in-tx builders.
	taskInsertColumns string
	// taskInsertOnConflict is the ON CONFLICT (id) DO UPDATE clause appended
	// to every tasks INSERT, derived from the upsert flags so the deliberate
	// exclusions (effective_priority, recurrence_spawned, result-like
	// columns) can never be re-added by hand in one path only.
	taskInsertOnConflict string
	// taskInsertStatement is the complete single-row upsert used by AddTask
	// and AddTaskTx.
	taskInsertStatement string
	// taskUpdateStatement is UpdateTaskTx's UPDATE: id is bound as $1 (the
	// WHERE key), the txUpdate-flagged columns as $2..$N in registry order —
	// the same order updateTaskArgs binds them.
	taskUpdateStatement string
)

func init() {
	if err := validateTaskColumnRegistry(); err != nil {
		panic("sched/db: invalid task column registry: " + err.Error())
	}

	readNames := make([]string, 0, len(taskColumnRegistry))
	insertNames := make([]string, 0, len(taskColumnRegistry))
	for i := range taskColumnRegistry {
		c := &taskColumnRegistry[i]
		if c.read {
			taskReadSet = append(taskReadSet, c)
			readNames = append(readNames, c.name)
		}
		if c.insert {
			taskInsertSet = append(taskInsertSet, c)
			insertNames = append(insertNames, c.name)
		}
		if c.txUpdate {
			taskTxUpdateSet = append(taskTxUpdateSet, c)
		}
	}
	taskColumns = strings.Join(readNames, ", ")
	taskInsertColumns = strings.Join(insertNames, ", ")

	var conflict strings.Builder
	conflict.WriteString(" ON CONFLICT (id) DO UPDATE SET ")
	first := true
	for i := range taskColumnRegistry {
		c := &taskColumnRegistry[i]
		if !c.upsert {
			continue
		}
		if !first {
			conflict.WriteString(", ")
		}
		first = false
		fmt.Fprintf(&conflict, "%s = EXCLUDED.%s", c.name, c.name)
	}
	taskInsertOnConflict = conflict.String()

	var ins strings.Builder
	ins.WriteString("INSERT INTO tasks (")
	ins.WriteString(taskInsertColumns)
	ins.WriteString(") VALUES ($1")
	for i := 2; i <= len(taskInsertSet); i++ {
		fmt.Fprintf(&ins, ",$%d", i)
	}
	ins.WriteByte(')')
	ins.WriteString(taskInsertOnConflict)
	taskInsertStatement = ins.String()

	var upd strings.Builder
	upd.WriteString("UPDATE tasks SET ")
	for i, c := range taskTxUpdateSet {
		if i > 0 {
			upd.WriteString(", ")
		}
		fmt.Fprintf(&upd, "%s = $%d", c.name, i+2)
	}
	upd.WriteString(" WHERE id = $1")
	taskUpdateStatement = upd.String()
}

// validateTaskColumnRegistry enforces the registry's structural rules. It is
// run at package init (a malformed registry must fail loudly at boot, not
// emit subtly wrong SQL) and again by TestTaskColumnRegistryConsistent so a
// violation surfaces as a readable test failure.
func validateTaskColumnRegistry() error {
	seen := make(map[string]bool, len(taskColumnRegistry))
	hasID := false
	for i := range taskColumnRegistry {
		c := &taskColumnRegistry[i]
		if c.name == "" {
			return fmt.Errorf("registry row %d has an empty column name", i)
		}
		if seen[c.name] {
			return fmt.Errorf("duplicate registry row for column %q", c.name)
		}
		seen[c.name] = true
		if c.name == "id" {
			hasID = true
			if !c.insert || !c.read {
				return fmt.Errorf("column id must be in the read and insert sets")
			}
		}

		// Exactly one of (membership flag, exclusion reason) per derived set:
		// a column is either in the set, or documents why it is not.
		for _, set := range []struct {
			label  string
			in     bool
			reason string
		}{
			{"read", c.read, c.noRead},
			{"insert", c.insert, c.noInsert},
			{"upsert", c.upsert, c.noUpsert},
			{"txUpdate", c.txUpdate, c.noTxUpdate},
			{"export", c.export, c.noExport},
		} {
			if set.in && set.reason != "" {
				return fmt.Errorf("column %q is in the %s set but also carries a %s-exclusion reason", c.name, set.label, set.label)
			}
			if !set.in && set.reason == "" {
				return fmt.Errorf("column %q is excluded from the %s set without a reason — document why (the exclusion policy is machine-checked, #1126)", c.name, set.label)
			}
		}

		// The upsert reuses the INSERT's bound value via EXCLUDED.<col>, so an
		// upsert-only column is impossible.
		if c.upsert && !c.insert {
			return fmt.Errorf("column %q is flagged upsert but not insert — the upsert can only reference EXCLUDED values of inserted columns", c.name)
		}
		// export describes the portable definition; a column no write path
		// carries cannot round-trip an import, so export requires insert.
		if c.export && !c.insert {
			return fmt.Errorf("column %q is flagged export but not insert — an exported field must be persistable at import", c.name)
		}

		needsValue := c.insert || c.txUpdate
		if needsValue && c.value == nil {
			return fmt.Errorf("column %q is in a write set but has no value function", c.name)
		}
		if !needsValue && c.value != nil {
			return fmt.Errorf("column %q has a value function but is in no write set", c.name)
		}
		if c.read && (c.dest == nil || c.assign == nil) {
			return fmt.Errorf("column %q is in the read set but lacks dest/assign functions", c.name)
		}
		if !c.read && (c.dest != nil || c.assign != nil) {
			return fmt.Errorf("column %q is not in the read set but has dest/assign functions", c.name)
		}
	}
	if !hasID {
		return fmt.Errorf("registry has no id column")
	}
	return nil
}

// taskInsertArgs returns the positional INSERT values for a task, in the
// exact column order of taskInsertColumns — both derive from taskInsertSet,
// so the statement and its arguments can never disagree (#710's drift class
// is structurally gone; there is no manual column count to bump). It derives
// actual_duration_seconds (#274) up front so the batch/tx paths persist it
// identically to the single-row AddTask path.
func taskInsertArgs(t *models.Task) []any {
	maybeComputeActualDuration(t)
	args := make([]any, len(taskInsertSet))
	for i, c := range taskInsertSet {
		args[i] = c.value(t)
	}
	return args
}

// updateTaskArgs returns UpdateTaskTx's positional arguments: the id (the
// WHERE key, $1) followed by the txUpdate-set values in registry order,
// matching taskUpdateStatement's placeholders by construction.
func updateTaskArgs(t *models.Task) []any {
	args := make([]any, 0, len(taskTxUpdateSet)+1)
	args = append(args, t.ID)
	for _, c := range taskTxUpdateSet {
		args = append(args, c.value(t))
	}
	return args
}
