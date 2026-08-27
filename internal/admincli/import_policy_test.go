package admincli

// Tests for the import collision policy (#1267): what an operator import may
// and may not do to a task row that ALREADY exists on the target box. DB-free —
// the envelope path runs against fakeTaskStore, and the shared policy helpers
// (which the legacy `fleet import --overwrite` path also calls) are exercised
// directly.

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// importEnvelope encodes an envelope the production exporter could emit.
func importEnvelope(t *testing.T, tasks ...*models.Task) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	if _, err := exportTasksEnvelope(&buf, taskExportEnvelope{Version: taskExportVersion, Tasks: tasks}); err != nil {
		t.Fatalf("encode envelope: %v", err)
	}
	return &buf
}

// TestImportTasks_RefusesResurrectingFinishedTask is the #1267 headline case: a
// task was exported while scheduled, then ran to success on this box. Importing
// that envelope again used to rewrite success→scheduled with the stale
// scheduled_for, so the due sweep re-queued a completed task and its external
// side effects (emails, MCP writes) ran a second time — the #1104
// double-execution shape through a supported operator flow.
func TestImportTasks_RefusesResurrectingFinishedTask(t *testing.T) {
	id := uuid.New()
	stale := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond)
	envelopeTask := &models.Task{
		ID: id, Prompt: "weekly report", Status: models.TaskStatusScheduled,
		ScheduledFor: &stale, Recurrence: "0 9 * * 1",
	}
	finished := &models.Task{
		ID: id, Prompt: "weekly report", Status: models.TaskStatusSuccess,
		Recurrence: "0 9 * * 1", Result: ptr("sent"),
	}
	f := &fakeTaskStore{existing: map[uuid.UUID]*models.Task{id: finished}}

	_, err := importTasks(context.Background(), f, importEnvelope(t, envelopeTask), taskImportOpts{})
	if err == nil {
		t.Fatal("import moved a finished task back to a schedulable status without an operator opt-in")
	}
	if !strings.Contains(err.Error(), "--replace-status") {
		t.Errorf("refusal should name the opt-in flag, got: %v", err)
	}
	if len(f.added) != 0 {
		t.Fatalf("a refused collision must write nothing; got %d adds", len(f.added))
	}

	// With the explicit opt-in it is restore surgery the operator asked for.
	f = &fakeTaskStore{existing: map[uuid.UUID]*models.Task{id: finished}}
	envelopeTask = &models.Task{
		ID: id, Prompt: "weekly report", Status: models.TaskStatusScheduled,
		ScheduledFor: &stale, Recurrence: "0 9 * * 1",
	}
	n, err := importTasks(context.Background(), f, importEnvelope(t, envelopeTask), taskImportOpts{replaceStatus: true})
	if err != nil || n != 1 {
		t.Fatalf("--replace-status import n=%d err=%v", n, err)
	}
	if len(f.added) != 1 || f.added[0].Status != models.TaskStatusScheduled {
		t.Fatalf("--replace-status should write the envelope's status verbatim; got %+v", f.added)
	}
}

// TestImportTasks_RefusesOverwritingLiveLease is the second #1267 case: an
// import landing on a row a worker is running used to overwrite status,
// lease_owner and lease_expires_at mid-run, so the in-flight worker's next
// write bounced off the fence while the row sat in the envelope's state.
// Refused under BOTH flag settings — --replace-status is consent to rewrite a
// status, never to null a live lease.
func TestImportTasks_RefusesOverwritingLiveLease(t *testing.T) {
	for _, replaceStatus := range []bool{false, true} {
		for _, live := range models.ActiveTaskStatuses {
			id := uuid.New()
			expires := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Microsecond)
			running := &models.Task{
				ID: id, Prompt: "long job", Status: live,
				LeaseOwner: ptr("scheduler-1"), LeaseExpiresAt: &expires,
			}
			f := &fakeTaskStore{existing: map[uuid.UUID]*models.Task{id: running}}
			envelopeTask := &models.Task{ID: id, Prompt: "long job", Status: models.TaskStatusScheduled}

			_, err := importTasks(context.Background(), f, importEnvelope(t, envelopeTask),
				taskImportOpts{replaceStatus: replaceStatus})
			if err == nil {
				t.Fatalf("status %q, --replace-status=%v: import overwrote a live lease", live, replaceStatus)
			}
			if !strings.Contains(err.Error(), "live lease") {
				t.Errorf("status %q: refusal should say the lease is live, got: %v", live, err)
			}
			if len(f.added) != 0 {
				t.Fatalf("status %q: a refused collision must write nothing; got %d adds", live, len(f.added))
			}
		}
	}
}

// TestImportTasks_NeverImportsLeaseColumns pins the "lease columns are not
// importable onto an existing row" half of the policy, which holds even for a
// collision the policy otherwise allows (same status, no live-lease status):
// whatever the target row holds wins, so an envelope can neither fabricate a
// lease nor clear one.
func TestImportTasks_NeverImportsLeaseColumns(t *testing.T) {
	id := uuid.New()
	targetExpiry := time.Now().UTC().Add(time.Minute).Truncate(time.Microsecond)
	target := &models.Task{
		ID: id, Prompt: "p", Status: models.TaskStatusScheduled,
		LeaseOwner: ptr("target-owner"), LeaseExpiresAt: &targetExpiry,
	}
	envelopeExpiry := time.Now().UTC().Add(99 * time.Hour).Truncate(time.Microsecond)
	envelopeTask := &models.Task{
		ID: id, Prompt: "p", Status: models.TaskStatusScheduled,
		LeaseOwner: ptr("envelope-owner"), LeaseExpiresAt: &envelopeExpiry,
	}
	f := &fakeTaskStore{existing: map[uuid.UUID]*models.Task{id: target}}

	if n, err := importTasks(context.Background(), f, importEnvelope(t, envelopeTask), taskImportOpts{}); err != nil || n != 1 {
		t.Fatalf("import n=%d err=%v", n, err)
	}
	got := f.added[0]
	if got.LeaseOwner == nil || *got.LeaseOwner != "target-owner" {
		t.Errorf("lease_owner came from the envelope: %v", got.LeaseOwner)
	}
	if got.LeaseExpiresAt == nil || !got.LeaseExpiresAt.Equal(targetExpiry) {
		t.Errorf("lease_expires_at came from the envelope: %v", got.LeaseExpiresAt)
	}
}

// TestImportTasks_SameGenerationReimportIsIdempotent guards the behavior the
// verbatim upsert exists for: re-importing an envelope onto rows that have not
// progressed is still a clean no-op-shaped write, and a row this box does not
// have at all is still inserted verbatim (the cross-box migration case).
func TestImportTasks_SameGenerationReimportIsIdempotent(t *testing.T) {
	unchangedID, freshID := uuid.New(), uuid.New()
	when := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	unchanged := &models.Task{ID: unchangedID, Prompt: "a", Status: models.TaskStatusScheduled, ScheduledFor: &when}
	fresh := &models.Task{ID: freshID, Prompt: "b", Status: models.TaskStatusSuccess}
	f := &fakeTaskStore{existing: map[uuid.UUID]*models.Task{
		unchangedID: {ID: unchangedID, Prompt: "a", Status: models.TaskStatusScheduled, ScheduledFor: &when},
	}}

	n, err := importTasks(context.Background(), f, importEnvelope(t, unchanged, fresh), taskImportOpts{})
	if err != nil || n != 2 {
		t.Fatalf("re-import n=%d err=%v", n, err)
	}
	if len(f.added) != 2 {
		t.Fatalf("added %d, want 2", len(f.added))
	}
}

// TestImportCollisionPolicyHelpers exercises the shared helpers directly — the
// legacy `fleet import --overwrite` path calls the same two functions, and its
// own behavior is DB-gated (TestImportBundle_SchedRerunPreservesLiveState), so
// this is where its policy is pinned DB-free.
func TestImportCollisionPolicyHelpers(t *testing.T) {
	scheduled := &models.Task{Status: models.TaskStatusScheduled}
	success := &models.Task{Status: models.TaskStatusSuccess}

	// A create (no target row) is never a collision.
	if err := importCollisionError(scheduled, nil, false); err != nil {
		t.Errorf("create refused: %v", err)
	}
	// Same status needs no opt-in.
	if err := importCollisionError(scheduled, &models.Task{Status: models.TaskStatusScheduled}, false); err != nil {
		t.Errorf("same-status re-import refused: %v", err)
	}
	// The legacy importer passes statusOptIn=true (--overwrite is its
	// documented "the bundle snapshot wins" switch, #713), so a cross-status
	// restore is allowed there...
	if err := importCollisionError(scheduled, success, true); err != nil {
		t.Errorf("opted-in status restore refused: %v", err)
	}
	// ...but never over a live lease, whatever the opt-in says.
	expires := time.Now().UTC().Add(time.Minute)
	for _, live := range models.ActiveTaskStatuses {
		target := &models.Task{Status: live, LeaseOwner: ptr("scheduler-1"), LeaseExpiresAt: &expires}
		if err := importCollisionError(scheduled, target, true); err == nil {
			t.Errorf("status %q: write over a live lease allowed", live)
		}
		if !taskHoldsLiveLease(target) {
			t.Errorf("taskHoldsLiveLease(%q) = false", live)
		}
	}
	if taskHoldsLiveLease(success) {
		t.Error("a terminal row must not read as holding a live lease")
	}
	if taskHoldsLiveLease(nil) {
		t.Error("a missing row must not read as holding a live lease")
	}

	// preserveTargetLease always takes the target's columns, including nil
	// (an import cannot fabricate a lease on a lease-free row).
	imported := &models.Task{LeaseOwner: ptr("envelope"), LeaseExpiresAt: &expires}
	preserveTargetLease(imported, success)
	if imported.LeaseOwner != nil || imported.LeaseExpiresAt != nil {
		t.Errorf("lease not cleared to the target's NULLs: %v %v", imported.LeaseOwner, imported.LeaseExpiresAt)
	}
}
