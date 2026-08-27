package admincli

// import_policy.go — the collision policy shared by the two operator import
// paths that write task rows verbatim (#1267).
//
// db.AddTask is an unconditional full-column upsert: its ON CONFLICT (id) DO
// UPDATE carries status, lease_owner and lease_expires_at, so an import that
// lands on an EXISTING row writes over live state and bypasses every
// transition guard above it (the claim gate, lease fencing, the terminal
// refusal lists, the #1116 recovery predicates, the #1127 lifecycle table).
// The upsert's verbatim semantics are load-bearing for same-generation
// re-import idempotency, so the policy lives HERE, at the import seam:
//
//   - importableTaskStatus (import.go) rejects the transient statuses at
//     envelope/bundle validation for BOTH paths: no import may birth or leave
//     a row in leased/running/paused, states only a live worker or a guarded
//     pause transition owns.
//   - a target row that currently holds a LIVE lease (leased/running) is never
//     written over, under any flag — the in-flight run owns those columns and
//     its next fenced write must not bounce off an imported snapshot.
//   - the lease columns are never importable ONTO an existing row: whatever
//     the target holds is preserved and the envelope's values are dropped, so
//     an import can neither fabricate nor clear a lease.
//   - rewriting an existing row's status needs an explicit operator opt-in.
//     For `sched task import` that is the new --replace-status; for the legacy
//     importer it is the pre-existing --overwrite, whose documented meaning is
//     already "the bundle snapshot wins" (#713, docs/LEGACY-IMPORT.md).
//
// What remains deliberately outside the guarded lifecycle model, and is
// recorded as such on models.TaskLifecycle: with the opt-in flag passed, an
// operator CAN still move a terminal row back to a schedulable status. That is
// restore surgery, and re-queueing a completed task re-runs its side effects —
// so it takes a flag, a warning, and the operator's judgement.

import (
	"fmt"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// taskHoldsLiveLease reports whether an existing target row is mid-run, i.e.
// holds a lease a worker may still fence a write against. The status set comes
// from models.ActiveTaskStatuses, which is DERIVED from the lifecycle table's
// lease-recovery edges, so a future lease-holding status is covered here the
// moment the table learns it.
func taskHoldsLiveLease(t *models.Task) bool {
	if t == nil {
		return false
	}
	for _, s := range models.ActiveTaskStatuses {
		if t.Status == s {
			return true
		}
	}
	return false
}

// importCollisionError reports why imported may NOT be written over the
// existing target row, or nil when the write is allowed. existing == nil (no
// such row on this box) is always allowed — that is a create, not a collision.
// statusOptIn is the operator's explicit consent to rewrite runtime status
// (--replace-status for `sched task import`, --overwrite for the legacy
// importer); it never licenses writing over a live lease.
func importCollisionError(imported, existing *models.Task, statusOptIn bool) error {
	if existing == nil {
		return nil
	}
	if taskHoldsLiveLease(existing) {
		return fmt.Errorf("task is %s on this box: refusing to import over a live lease — wait for the run to finish or cancel it first",
			existing.Status)
	}
	if !statusOptIn && imported.Status != existing.Status {
		return fmt.Errorf("task already exists here with status %q but the import carries %q: refusing to rewrite runtime status — pass --replace-status to overwrite it (re-queueing a finished task re-runs its side effects), or drop the task from the import",
			existing.Status, imported.Status)
	}
	return nil
}

// preserveTargetLease drops the imported snapshot's lease columns in favour of
// whatever the target row holds, so an import onto an existing row can never
// write lease_owner / lease_expires_at (#1267). A no-op for a create.
//
// Belt-and-braces next to the live-lease refusal above: that refusal covers
// the rows whose STATUS says a run is in flight, while this covers the columns
// directly, including a row left holding a stale lease in some other status.
func preserveTargetLease(imported, existing *models.Task) {
	if imported == nil || existing == nil {
		return
	}
	imported.LeaseOwner = existing.LeaseOwner
	imported.LeaseExpiresAt = existing.LeaseExpiresAt
}
