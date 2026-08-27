package models

// task_lifecycle.go — the task-lifecycle transition table (#1127).
//
// Until this table existed, the lifecycle's transition rules lived only in
// scattered `WHERE status = …` clauses across the claim / recovery /
// serialization / pause / wake / reporting queries (internal/sched/db) and
// the Go-side guards in internal/sched/storage — every new status touched
// them all by hand, and nothing enumerated which edges were legal. This file
// encodes the CURRENT reality as a tested constant. It is deliberately NOT a
// runtime framework: no query routes through the table except sets that were
// already named (db's taskActiveStatuses now derives from
// ActiveTaskStatuses); every writer keeps its own guarded SQL / row-locked
// transaction, and the table plus its coupling tests
// (models/task_lifecycle_test.go, sched/db/lifecycle_test.go,
// sched/storage/lifecycle_test.go) make drift between the two fail loudly —
// the #1126 column-registry treatment, applied to status edges.
//
// The lifecycle, in one narrative:
//
//	           (create: DeriveDispatchState)          (legacy restore)
//	                 │            │                  born terminal, settled
//	                 ▼            ▼                          │
//	              pending ◄── scheduled ──► cancelled        ▼
//	                 │   ▲  (due/gate pass) (chain end) success/error/
//	          claim  │   │                              cancelled/dead_lettered
//	                 ▼   │ (resume/wake/recovery/replay)
//	              leased ─► running ─► success | error          (terminal)
//	                 │          │  ▲
//	   (crash        │          │  └─ retry requeue ─► scheduled
//	    recovery /   │          ├───► paused_awaiting_input ─► pending | error
//	    crash-loop   │          ├───► paused_awaiting_wake  ─► pending | error
//	    dead-letter) ▼          ▼
//	              pending   dead_lettered ─► pending (replay)
//
//   - Birth: models.DeriveDispatchState (NewTask, POST /tasks, edits, import
//     conflict=replace, trigger/webhook spawns, re-run/clone) yields exactly
//     `pending` (immediate dispatch) or `scheduled` (future fire time, gated
//     cron parked for the gate pass, inert webhook template). The legacy
//     importer (internal/admincli/import.go, #713) additionally restores
//     history rows born directly in a terminal status, settled.
//   - Dispatch: the scheduler promotes due ungated rows scheduled→pending in
//     batch (db.UpdateTasksStatusBatch) and settles run_if-gated rows
//     asynchronously (db.SettleGatedTask: promote on pass, cancel when the
//     recurrence ends at a skip; a skip itself keeps the row `scheduled` and
//     only advances scheduled_for — db.RecordSkip, a status-preserving write).
//   - Execution: db.ClaimNextPendingTask is the ONLY pending→active edge
//     (serialization gate #709); the runner reports leased→running and the
//     terminal success/error through storage.UpdateTaskStatusAtomicWithContext
//     under its lease. The from-side of every runner-driven edge is
//     {leased, running} — exactly the statuses that can hold a lease (every
//     other writer clears the lease as it leaves them), which is also why
//     those writers' "guards" are lease-possession checks rather than status
//     predicates. The leased→success/error edges are reachable because a
//     failed running-report only logs (runner.go) and the run proceeds.
//   - Failure: the runner requeues a retryable failure as `scheduled` with a
//     backoff (same occurrence, storage.RequeueTaskForRetryWithContext) or
//     quarantines to `dead_lettered` (storage.DeadLetterTaskWithContext).
//     Crash recovery (db.RecoverExpiredLeases, #1116) requeues expired-lease
//     rows to `pending` while the attempt budget lasts and dead-letters
//     crash-loopers past it.
//   - Pauses (#510 / docs/SELF-WAKE.md): a RUNNING task may park lease-free in
//     paused_awaiting_input (ask) or paused_awaiting_wake (sleep/event);
//     resume/wake re-queue it as `pending` and the expiry sweeps fail an
//     abandoned pause to terminal `error` (never dead_lettered — that status
//     is reserved for the runner's lease-guarded quarantine).
//   - Operator recovery: cancel reaches every NON-TERMINAL status (#1268/#1269
//     harmonized all four writers' from-side refusal onto the one terminal set,
//     so a quarantined row is no longer cancellable — replay it or delete it),
//     and a DLQ replay resets dead_lettered→pending. That replay is now the ONLY
//     GUARDED edge out of a terminal status; success/error/cancelled/dead_lettered
//     have no cancel edge (the verbatim-upsert restore paths below sit outside the
//     guarded model entirely).
//   - Edits (storage.UpdateEditableTask / storage.ReplaceTaskDefinition) touch
//     only pending/scheduled rows and re-derive the dispatch state, so they
//     move rows within {pending, scheduled} and never off the scheduler path.
//
// Retired: 'analyzing' (moc leftover) was rewritten to 'running' by migration
// 063 and must never reappear — RetiredTaskStatuses backs the SQL-literal
// drift scan in sched/db/lifecycle_test.go.
//
// Adding a status or edge is now: the constant (for a status), the table
// row(s) here, membership in the derived sets that apply — and the coupling
// tests walk you through every guarded query that must learn it.

import "fmt"

// TaskLifecycleStart is the birth pseudo-status: the From of every edge a row
// can be CREATED with. It is the TaskStatus zero value and never stored.
const TaskLifecycleStart TaskStatus = ""

// errf keeps the validator's rule list readable.
func errf(format string, args ...any) error { return fmt.Errorf(format, args...) }

// Authoritative writer names for the lifecycle table. Each constant names the
// ONE function that owns that edge's guarded write; the per-writer matrix
// tests key on these exact strings.
const (
	// TaskWriterDeriveDispatchState — models.DeriveDispatchState (this
	// package): the single create/edit/import dispatch-state rule.
	TaskWriterDeriveDispatchState = "models.DeriveDispatchState"
	// TaskWriterLegacyImport — the legacy history importer
	// (internal/admincli/import.go, #713): restores rows verbatim, born in
	// pending/scheduled or settled terminal statuses (validSchedTaskStatus
	// rejects transient ones).
	TaskWriterLegacyImport = "admincli legacy import"
	// TaskWriterScheduledPromotion — db.UpdateTasksStatusBatch
	// (internal/sched/db/scheduling.go), called by the scheduler's due sweep
	// with (scheduled, pending) — the only caller today.
	TaskWriterScheduledPromotion = "db.UpdateTasksStatusBatch"
	// TaskWriterSettleGatedTask — db.SettleGatedTask
	// (internal/sched/db/scheduling.go): async run_if verdict settlement,
	// guarded on `scheduled` + an unchanged scheduled_for.
	TaskWriterSettleGatedTask = "db.SettleGatedTask"
	// TaskWriterClaim — db.ClaimNextPendingTask (internal/sched/db/claim.go):
	// the ONLY pending→active edge; every resume/recovery re-passes it.
	TaskWriterClaim = "db.ClaimNextPendingTask"
	// TaskWriterWorkerReport — storage.UpdateTaskStatusAtomicWithContext
	// (internal/sched/storage/storage.go): the runner's lease-guarded status
	// reports (start, renewal rides, terminal success/error). Since #1269 the
	// to-side is a GUARD, not caller discipline: the writer refuses any target
	// outside WorkerReportableTaskStatuses / IsValidReportedStatus before it
	// opens a transaction, so a caller passing e.g. cancelled from a leased row
	// is rejected loudly instead of leaving a cancelled row holding a live
	// lease. The reportable set is one status WIDER than this writer's target
	// column (it still admits `leased`, the re-lease report no production call
	// site sends), so the table stays the tighter statement of what actually
	// happens and the matrix pins both.
	TaskWriterWorkerReport = "storage.UpdateTaskStatusAtomicWithContext"
	// TaskWriterRetryRequeue — storage.RequeueTaskForRetryWithContext:
	// lease-guarded retry of the SAME occurrence with a backoff.
	TaskWriterRetryRequeue = "storage.RequeueTaskForRetryWithContext"
	// TaskWriterRunnerDeadLetter — storage.DeadLetterTaskWithContext (#253):
	// the runner's lease-guarded quarantine.
	TaskWriterRunnerDeadLetter = "storage.DeadLetterTaskWithContext"
	// TaskWriterLeaseRecovery — db.RecoverExpiredLeases
	// (internal/sched/db/claim.go): crash-safe requeue of expired-lease rows,
	// attempt-bounded into the DLQ (#1116).
	TaskWriterLeaseRecovery = "db.RecoverExpiredLeases"
	// TaskWriterPauseForQuestion — db.PauseTaskForQuestion
	// (internal/sched/db/pause.go, #510): ask-pause, running only, lease
	// released.
	TaskWriterPauseForQuestion = "db.PauseTaskForQuestion"
	// TaskWriterResume — db.ResumeTask (internal/sched/db/pause.go): a human
	// answer re-queues the ask-paused task.
	TaskWriterResume = "db.ResumeTask"
	// TaskWriterPausedExpiry — db.ExpirePausedTasks
	// (internal/sched/db/pause.go): unattended ask-pauses fail terminally
	// after the window, measured from paused_at (#1116).
	TaskWriterPausedExpiry = "db.ExpirePausedTasks"
	// TaskWriterPauseForWake — db.PauseTaskForWake
	// (internal/sched/db/wake.go, docs/SELF-WAKE.md): self-wake park, running
	// only, lease released.
	TaskWriterPauseForWake = "db.PauseTaskForWake"
	// TaskWriterWakeDue — db.WakeDueTasks (internal/sched/db/wake.go): the
	// wake sweep re-queues parked rows whose deadline passed.
	TaskWriterWakeDue = "db.WakeDueTasks"
	// TaskWriterWakeByEvent — db.WakeTaskByEvent (internal/sched/db/wake.go):
	// an event wake re-queues one parked row early, keyed on its event.
	TaskWriterWakeByEvent = "db.WakeTaskByEvent"
	// TaskWriterStrandedWakeExpiry — db.ExpireStrandedWakeTasks
	// (internal/sched/db/wake.go): the terminal backstop for parked rows no
	// wake can reach.
	TaskWriterStrandedWakeExpiry = "db.ExpireStrandedWakeTasks"
	// TaskWriterCancel — storage.CancelTaskAtomic
	// (internal/sched/storage/storage.go, #508): operator cancel.
	TaskWriterCancel = "storage.CancelTaskAtomic"
	// TaskWriterDLQReplay — storage.ReplayDeadLetteredTask (#253): resets a
	// quarantined row to a fresh pending slate.
	TaskWriterDLQReplay = "storage.ReplayDeadLetteredTask"
	// TaskWriterEdit — storage.UpdateEditableTask: the full-task edit,
	// re-deriving the dispatch state (guarded on pending/scheduled).
	TaskWriterEdit = "storage.UpdateEditableTask"
	// TaskWriterImportReplace — storage.ReplaceTaskDefinition (#238 import
	// conflict=replace): same guard + re-derivation as the edit.
	TaskWriterImportReplace = "storage.ReplaceTaskDefinition"
)

// TaskTransition is one legal status edge: From→To, owned by exactly one
// authoritative Writer (a TaskWriter* constant). Note carries the one-line
// context that would otherwise live only at the writer.
type TaskTransition struct {
	From   TaskStatus
	To     TaskStatus
	Writer string
	Note   string
}

// TaskLifecycle is the transition table: every task-status edge the GUARDED
// runtime writers can produce, as written today. The coupling tests assert
// both directions — every guarded writer transitions exactly the edges
// listed for it, and a status/edge added in SQL without a row here fails the
// drift scans.
//
// Self-edges (running→running lease renewals and artifact/output rides;
// pending→pending / scheduled→scheduled re-derivations on edit) are listed
// because they are real guarded status writes; status-preserving writes that
// never SET a different status (db.RecordSkip, db.PromoteStarvedTasks,
// db.MarkSLABreached, db.SetErrorAnalysis, db.UpdateTasksModelBatch, the
// targeted pending/scheduled field edits) are documented at their writers
// and deliberately not edges.
//
// Deliberately OUTSIDE the model — out-of-model restore surgery: two
// operator import paths write a task's status verbatim through db.AddTask's
// unconditional full-column ON CONFLICT upsert (status, lease_owner and
// lease_expires_at included), so they can produce any→imported-status and
// bypass every guard in this table. They are `fleet-admin sched task import`
// (internal/admincli/sched_task.go importTasks — validateImportedTask checks
// prompt/MCP/recurrence, never status) and `fleet legacy import --overwrite`
// (internal/admincli/import.go; without --overwrite, skip-by-default #713
// protects progressed rows). Example: re-importing an envelope exported
// while a task was scheduled, after that task ran to success, rewrites
// success→scheduled with the stale scheduled_for and the scheduler re-runs
// the completed task's external side effects (over a running row it also
// nulls the live lease — the #1104 shape). Pre-existing behavior, recorded
// here rather than changed; the validator below validates the GUARDED model
// only.
var TaskLifecycle = []TaskTransition{
	// ── Birth ───────────────────────────────────────────────────────────
	{TaskLifecycleStart, TaskStatusPending, TaskWriterDeriveDispatchState, "immediate dispatch: no future scheduled_for, ungated, non-webhook"},
	{TaskLifecycleStart, TaskStatusScheduled, TaskWriterDeriveDispatchState, "future scheduled_for, OR gated cron parked for the gate pass, OR inert webhook template"},
	{TaskLifecycleStart, TaskStatusPending, TaskWriterLegacyImport, "live restore (#713)"},
	{TaskLifecycleStart, TaskStatusScheduled, TaskWriterLegacyImport, "live restore; a past recurring fire time is recomputed"},
	{TaskLifecycleStart, TaskStatusSuccess, TaskWriterLegacyImport, "history restore, born settled (recurrence_spawned=TRUE at insert)"},
	{TaskLifecycleStart, TaskStatusError, TaskWriterLegacyImport, "history restore, born settled"},
	{TaskLifecycleStart, TaskStatusCancelled, TaskWriterLegacyImport, "history restore"},
	{TaskLifecycleStart, TaskStatusDeadLettered, TaskWriterLegacyImport, "history restore; replayable (#253)"},

	// ── Scheduler dispatch ──────────────────────────────────────────────
	{TaskStatusScheduled, TaskStatusPending, TaskWriterScheduledPromotion, "due sweep promotes ungated rows in batch; guarded WHERE status=scheduled"},
	{TaskStatusScheduled, TaskStatusPending, TaskWriterSettleGatedTask, "run_if gate passed (#269); stale verdicts fail the scheduled_for compare"},
	{TaskStatusScheduled, TaskStatusCancelled, TaskWriterSettleGatedTask, "recurrence_until passed at a gate skip: no future occurrence to advance to"},

	// ── Claim ───────────────────────────────────────────────────────────
	{TaskStatusPending, TaskStatusLeased, TaskWriterClaim, "FOR UPDATE SKIP LOCKED + serialization gate (#709); the only pending→active edge"},

	// ── Runner status reports (lease-guarded; from-side = the lease-holding
	//    statuses; refuses success/error/cancelled) ──────────────────────
	{TaskStatusLeased, TaskStatusRunning, TaskWriterWorkerReport, "run start: stamps StartedAt, renews the lease"},
	{TaskStatusRunning, TaskStatusRunning, TaskWriterWorkerReport, "lease renewal + workspace/output/artifact rides"},
	{TaskStatusLeased, TaskStatusSuccess, TaskWriterWorkerReport, "reachable when the running report failed (runner logs and proceeds)"},
	{TaskStatusRunning, TaskStatusSuccess, TaskWriterWorkerReport, "terminal success; clears lease + pending Q&A, spawns the next recurrence post-commit"},
	{TaskStatusLeased, TaskStatusError, TaskWriterWorkerReport, "panic/failure before the running report landed"},
	{TaskStatusRunning, TaskStatusError, TaskWriterWorkerReport, "per-attempt terminal failure (still replay-/retry-visible via error, not DLQ)"},

	// ── Retry / dead-letter (runner-driven, lease-guarded) ──────────────
	{TaskStatusLeased, TaskStatusScheduled, TaskWriterRetryRequeue, "retryable failure before the running report landed"},
	{TaskStatusRunning, TaskStatusScheduled, TaskWriterRetryRequeue, "retryable failure: same occurrence, backoff scheduled_for, attempt++"},
	{TaskStatusLeased, TaskStatusDeadLettered, TaskWriterRunnerDeadLetter, "non-retryable/exhausted before the running report landed"},
	{TaskStatusRunning, TaskStatusDeadLettered, TaskWriterRunnerDeadLetter, "retries exhausted or non-retryable class (#253)"},

	// ── Crash recovery (#1116) ──────────────────────────────────────────
	{TaskStatusLeased, TaskStatusPending, TaskWriterLeaseRecovery, "expired lease, attempt budget remains: requeue (attempt++)"},
	{TaskStatusRunning, TaskStatusPending, TaskWriterLeaseRecovery, "expired lease, attempt budget remains: requeue (attempt++)"},
	{TaskStatusLeased, TaskStatusDeadLettered, TaskWriterLeaseRecovery, "crash-loop guard: attempt budget spent"},
	{TaskStatusRunning, TaskStatusDeadLettered, TaskWriterLeaseRecovery, "crash-loop guard: attempt budget spent"},

	// ── Ask pause (#510) ────────────────────────────────────────────────
	{TaskStatusRunning, TaskStatusPausedAwaitingInput, TaskWriterPauseForQuestion, "agent asked a human; lease released, paused_at stamped"},
	{TaskStatusPausedAwaitingInput, TaskStatusPending, TaskWriterResume, "answer arrived: re-queued immediately (re-passes the claim gate)"},
	{TaskStatusPausedAwaitingInput, TaskStatusError, TaskWriterPausedExpiry, "no answer within the window (from paused_at); error, never dead_lettered (no lease)"},

	// ── Self-wake park (docs/SELF-WAKE.md) ──────────────────────────────
	{TaskStatusRunning, TaskStatusPausedAwaitingWake, TaskWriterPauseForWake, "sleep/wake_on_event; lease released, wake_at always set, wake_cycles++"},
	{TaskStatusPausedAwaitingWake, TaskStatusPending, TaskWriterWakeDue, "deadline passed (timer fired, or event wait timed out)"},
	{TaskStatusPausedAwaitingWake, TaskStatusPending, TaskWriterWakeByEvent, "named event arrived early (keyed on wake_event_key)"},
	{TaskStatusPausedAwaitingWake, TaskStatusError, TaskWriterStrandedWakeExpiry, "no wake can reach the row (NULL or day-stale wake_at)"},

	// ── Operator cancel (#508): every NON-TERMINAL status. The guard is
	//    IsTerminal (the one shared terminal set, #1269), so all four terminal
	//    statuses — dead_lettered included since #1268 — refuse the cancel. ──
	{TaskStatusPending, TaskStatusCancelled, TaskWriterCancel, ""},
	{TaskStatusScheduled, TaskStatusCancelled, TaskWriterCancel, ""},
	{TaskStatusLeased, TaskStatusCancelled, TaskWriterCancel, ""},
	{TaskStatusRunning, TaskStatusCancelled, TaskWriterCancel, "the in-flight run observes the cancel and stops without a second terminal write"},
	{TaskStatusPausedAwaitingInput, TaskStatusCancelled, TaskWriterCancel, ""},
	{TaskStatusPausedAwaitingWake, TaskStatusCancelled, TaskWriterCancel, ""},

	// ── DLQ replay (#253) ───────────────────────────────────────────────
	{TaskStatusDeadLettered, TaskStatusPending, TaskWriterDLQReplay, "fresh slate: attempts/DLQ columns/SLA artifacts cleared, spawn credit re-armed"},

	// ── Edits: re-derive the dispatch state within {pending, scheduled} ─
	{TaskStatusPending, TaskStatusPending, TaskWriterEdit, ""},
	{TaskStatusPending, TaskStatusScheduled, TaskWriterEdit, "edit added a future schedule / a run_if gate / webhook trigger"},
	{TaskStatusScheduled, TaskStatusPending, TaskWriterEdit, "edit removed the schedule and the row is ungated"},
	{TaskStatusScheduled, TaskStatusScheduled, TaskWriterEdit, ""},
	{TaskStatusPending, TaskStatusPending, TaskWriterImportReplace, ""},
	{TaskStatusPending, TaskStatusScheduled, TaskWriterImportReplace, "replacement definition carries a schedule/gate/webhook trigger"},
	{TaskStatusScheduled, TaskStatusPending, TaskWriterImportReplace, "replacement definition is an ungated one-shot"},
	{TaskStatusScheduled, TaskStatusScheduled, TaskWriterImportReplace, ""},
}

// AllTaskStatuses is every status a task row can hold, in lifecycle order.
var AllTaskStatuses = []TaskStatus{
	TaskStatusPending,
	TaskStatusScheduled,
	TaskStatusLeased,
	TaskStatusRunning,
	TaskStatusPausedAwaitingInput,
	TaskStatusPausedAwaitingWake,
	TaskStatusSuccess,
	TaskStatusError,
	TaskStatusCancelled,
	TaskStatusDeadLettered,
}

// TerminalTaskStatuses are the final states a task will not leave on its own.
// Declared (not derived) so validateTaskLifecycle can cross-check it against
// IsTerminal — the two must always agree, and neither can drift silently.
//
// This is the ONE from-side refusal set every transition writer shares
// (#1269): storage's CancelTaskAtomic, UpdateTaskStatusAtomicWithContext,
// RequeueTaskForRetryWithContext and DeadLetterTaskWithContext each guard on
// TaskStatus.IsTerminal() rather than hand-listing three or four statuses, so
// a new terminal status cannot be forgotten in one writer and honored in the
// next. Only the REFUSAL STYLE differs, by design: cancel is an operator
// request and errors, while the three lease-guarded runner writers return the
// row unchanged (an idempotent late report must not fail a run).
var TerminalTaskStatuses = []TaskStatus{
	TaskStatusSuccess,
	TaskStatusError,
	TaskStatusCancelled,
	TaskStatusDeadLettered,
}

// WorkerReportableTaskStatuses are the statuses a worker may self-report
// (mirrors IsValidReportedStatus, cross-checked by validateTaskLifecycle):
// dead_lettered is excluded — only the runner's terminal switch quarantines.
//
// Since #1269 this is an ENFORCED to-side guard, not documentation: storage's
// UpdateTaskStatusAtomicWithContext refuses a target outside this set. It is
// deliberately one status wider than that writer's table edges (`leased` has
// no production call site), so the guard narrows and the table describes.
var WorkerReportableTaskStatuses = []TaskStatus{
	TaskStatusLeased,
	TaskStatusRunning,
	TaskStatusSuccess,
	TaskStatusError,
}

// CleanupEligibleTaskStatuses are the terminal statuses the retention sweeps
// (db.CleanupOldRuns / db.DeleteOldHistory / db.ArchiveOldLogs) may prune or
// archive. dead_lettered is deliberately absent: a quarantined row awaits
// operator review/replay and must never age out silently. The SQL literals in
// sched/db/cleanup.go are pinned to this set by the lifecycle drift test.
var CleanupEligibleTaskStatuses = []TaskStatus{
	TaskStatusSuccess,
	TaskStatusError,
	TaskStatusCancelled,
}

// RecurrenceSpawnTaskStatuses are the terminal statuses that spawn the next
// occurrence of a recurring task (#1116): cancel ends the chain, dead-letter
// parks it for replay. Pinned to db.recurrenceSpawnedInsertValue and
// db.GetUnspawnedRecurringTasks by the lifecycle drift test.
var RecurrenceSpawnTaskStatuses = []TaskStatus{
	TaskStatusSuccess,
	TaskStatusError,
}

// RetiredTaskStatuses are statuses that once existed and must never reappear
// in a query or a table row: 'analyzing' (moc leftover) was rewritten to
// 'running' by migration 063 (#1077).
var RetiredTaskStatuses = []TaskStatus{"analyzing"}

// Sets derived from the table at init — the edges ARE the definition, so
// these cannot drift from it.
var (
	// ClaimableTaskStatuses are the statuses the claim path may lease:
	// the From-side of db.ClaimNextPendingTask's edges ({pending}).
	ClaimableTaskStatuses []TaskStatus

	// ActiveTaskStatuses are the in-flight statuses that hold a lease — and
	// with it a serialization key (#709) and an SLA watch (#274): the
	// From-side of db.RecoverExpiredLeases' edges ({leased, running}).
	// db.taskActiveStatuses derives from this at runtime.
	ActiveTaskStatuses []TaskStatus

	// PausedTaskStatuses are the lease-free parked states (#510 +
	// docs/SELF-WAKE.md): the From-side of the two expiry sweeps' edges.
	PausedTaskStatuses []TaskStatus
)

// TaskTransitionExists reports whether the table contains the exact edge
// (from → to) owned by writer.
func TaskTransitionExists(from, to TaskStatus, writer string) bool {
	for _, tr := range TaskLifecycle {
		if tr.From == from && tr.To == to && tr.Writer == writer {
			return true
		}
	}
	return false
}

// TaskTransitionsByWriter returns the table rows owned by writer, in table
// order. The per-writer coupling matrices iterate this.
func TaskTransitionsByWriter(writer string) []TaskTransition {
	var out []TaskTransition
	for _, tr := range TaskLifecycle {
		if tr.Writer == writer {
			out = append(out, tr)
		}
	}
	return out
}

// taskStatusIn reports membership of s in set.
func taskStatusIn(s TaskStatus, set []TaskStatus) bool {
	for _, v := range set {
		if v == s {
			return true
		}
	}
	return false
}

// derivedFromSet collects the distinct From-statuses of the given writers'
// edges, in AllTaskStatuses order (deterministic regardless of table order).
func derivedFromSet(writers ...string) []TaskStatus {
	seen := make(map[TaskStatus]bool)
	for _, w := range writers {
		for _, tr := range TaskTransitionsByWriter(w) {
			seen[tr.From] = true
		}
	}
	out := make([]TaskStatus, 0, len(seen))
	for _, s := range AllTaskStatuses {
		if seen[s] {
			out = append(out, s)
		}
	}
	return out
}

func init() {
	ClaimableTaskStatuses = derivedFromSet(TaskWriterClaim)
	ActiveTaskStatuses = derivedFromSet(TaskWriterLeaseRecovery)
	PausedTaskStatuses = derivedFromSet(TaskWriterPausedExpiry, TaskWriterStrandedWakeExpiry)

	if err := validateTaskLifecycle(); err != nil {
		panic("sched/models: invalid task lifecycle table: " + err.Error())
	}
}

// validateTaskLifecycle enforces the table's structural rules. It validates
// the GUARDED model only — the verbatim-upsert restore paths documented on
// TaskLifecycle sit outside it by design. Run at package init (a malformed
// table must fail loudly at boot) and again by TestTaskLifecycleValid so a
// violation surfaces as a readable test failure:
//
//   - every edge references known statuses and a non-empty writer, no dupes;
//   - every status is reachable from birth (a status added without table
//     rows fails loudly here — the #1127 completeness requirement);
//   - every non-terminal status has an outgoing edge to a DIFFERENT status
//     (nothing non-terminal can be a dead end);
//   - the only GUARDED edges out of a terminal status are the
//     operator-recovery writers (DLQ replay, cancel) —
//     success/error/cancelled have none;
//   - the declared sets agree with the Go predicates they mirror
//     (IsTerminal, IsValidReportedStatus) and with each other.
func validateTaskLifecycle() error {
	known, err := lifecycleKnownStatuses()
	if err != nil {
		return err
	}
	if err := lifecycleValidateEdges(known); err != nil {
		return err
	}
	if err := lifecycleValidateReachability(); err != nil {
		return err
	}
	if err := lifecycleValidateStatusRoles(); err != nil {
		return err
	}
	return lifecycleValidateSets(known)
}

// lifecycleKnownStatuses builds the known-status set: AllTaskStatuses must be
// duplicate-free, never contain the birth sentinel, and never re-list a
// retired status.
func lifecycleKnownStatuses() (map[TaskStatus]bool, error) {
	known := make(map[TaskStatus]bool, len(AllTaskStatuses))
	for _, s := range AllTaskStatuses {
		if s == TaskLifecycleStart {
			return nil, errf("AllTaskStatuses contains the empty birth sentinel")
		}
		if known[s] {
			return nil, errf("duplicate status %q in AllTaskStatuses", s)
		}
		known[s] = true
	}
	for _, s := range RetiredTaskStatuses {
		if known[s] {
			return nil, errf("retired status %q is still listed in AllTaskStatuses", s)
		}
	}
	return known, nil
}

// lifecycleValidateEdges checks every table row references known statuses and
// a writer, with no duplicate (from, to, writer) rows.
func lifecycleValidateEdges(known map[TaskStatus]bool) error {
	seen := make(map[TaskTransition]bool, len(TaskLifecycle))
	for _, tr := range TaskLifecycle {
		if tr.From != TaskLifecycleStart && !known[tr.From] {
			return errf("edge %q→%q: unknown From status", tr.From, tr.To)
		}
		if !known[tr.To] {
			return errf("edge %q→%q: unknown To status", tr.From, tr.To)
		}
		if tr.Writer == "" {
			return errf("edge %q→%q has no writer", tr.From, tr.To)
		}
		key := TaskTransition{From: tr.From, To: tr.To, Writer: tr.Writer}
		if seen[key] {
			return errf("duplicate edge %q→%q for writer %s", tr.From, tr.To, tr.Writer)
		}
		seen[key] = true
	}
	return nil
}

// lifecycleValidateReachability walks the edge graph from birth to a fixpoint
// and requires every status to be reached — a status added without table rows
// fails here (the #1127 completeness requirement).
func lifecycleValidateReachability() error {
	reachable := map[TaskStatus]bool{TaskLifecycleStart: true}
	for changed := true; changed; {
		changed = false
		for _, tr := range TaskLifecycle {
			if reachable[tr.From] && !reachable[tr.To] {
				reachable[tr.To] = true
				changed = true
			}
		}
	}
	for _, s := range AllTaskStatuses {
		if !reachable[s] {
			return errf("status %q is unreachable from birth — a status without table rows cannot exist", s)
		}
	}
	return nil
}

// lifecycleValidateStatusRoles cross-checks the declared role sets against
// the Go predicates they mirror, requires terminal exits to be operator
// recovery only, and refuses non-terminal dead ends.
func lifecycleValidateStatusRoles() error {
	for _, s := range AllTaskStatuses {
		if s.IsTerminal() != taskStatusIn(s, TerminalTaskStatuses) {
			return errf("TerminalTaskStatuses disagrees with IsTerminal on %q", s)
		}
		if s.IsValidReportedStatus() != taskStatusIn(s, WorkerReportableTaskStatuses) {
			return errf("WorkerReportableTaskStatuses disagrees with IsValidReportedStatus on %q", s)
		}
		if s.IsTerminal() {
			// Terminal exits are operator recovery only.
			for _, tr := range TaskLifecycle {
				if tr.From != s {
					continue
				}
				if tr.Writer != TaskWriterDLQReplay && tr.Writer != TaskWriterCancel {
					return errf("terminal status %q has a non-operator outgoing edge to %q (writer %s)", s, tr.To, tr.Writer)
				}
			}
			continue
		}
		// Every non-terminal status must be able to leave (self-edges don't count).
		exits := false
		for _, tr := range TaskLifecycle {
			if tr.From == s && tr.To != s {
				exits = true
				break
			}
		}
		if !exits {
			return errf("non-terminal status %q has no outgoing edge — it would be a dead end", s)
		}
	}
	return nil
}

// lifecycleValidateSets checks the role sets are non-empty, reference known
// statuses, and sit on the right side of the terminal divide.
func lifecycleValidateSets(known map[TaskStatus]bool) error {
	for _, set := range []struct {
		name    string
		members []TaskStatus
	}{
		{"ClaimableTaskStatuses", ClaimableTaskStatuses},
		{"ActiveTaskStatuses", ActiveTaskStatuses},
		{"PausedTaskStatuses", PausedTaskStatuses},
		{"CleanupEligibleTaskStatuses", CleanupEligibleTaskStatuses},
		{"RecurrenceSpawnTaskStatuses", RecurrenceSpawnTaskStatuses},
	} {
		if len(set.members) == 0 {
			return errf("%s is empty", set.name)
		}
		for _, s := range set.members {
			if !known[s] {
				return errf("%s contains unknown status %q", set.name, s)
			}
		}
	}
	// The cleanup and spawn sets describe settled rows only.
	for _, s := range CleanupEligibleTaskStatuses {
		if !s.IsTerminal() {
			return errf("CleanupEligibleTaskStatuses contains non-terminal %q", s)
		}
	}
	for _, s := range RecurrenceSpawnTaskStatuses {
		if !s.IsTerminal() {
			return errf("RecurrenceSpawnTaskStatuses contains non-terminal %q", s)
		}
	}
	// The claimable/active/paused sets are live rows by definition.
	for _, s := range append(append(append([]TaskStatus{}, ClaimableTaskStatuses...), ActiveTaskStatuses...), PausedTaskStatuses...) {
		if s.IsTerminal() {
			return errf("live status set contains terminal %q", s)
		}
	}
	return nil
}
