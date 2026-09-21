package storage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"

	"github.com/ElcanoTek/fleet/internal/sched"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// nextCronTick returns the first fire of spec strictly after `after`,
// evaluated in tz — the same math scheduleNextRecurrence applies when it
// stamps the successor's scheduled_for.
func nextCronTick(t *testing.T, spec, tz string, after time.Time) time.Time {
	t.Helper()
	schedule, err := cron.ParseStandard(spec)
	if err != nil {
		t.Fatalf("parse recurrence %q: %v", spec, err)
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		t.Fatalf("load location %q: %v", tz, err)
	}
	return schedule.Next(after.In(loc)).UTC()
}

// seedUnclaimedDeadLettered mirrors the row a crash in the
// dead-letter-commit→spawn window leaves behind: the occurrence INSERTED live
// (pending) and only then TRANSITIONED dead_lettered with its completed_at
// backdated past the reconciliation grace — so the row keeps an UNCLAIMED
// spawn credit (like seedTerminalRecurring does for success/error; inserting
// the row already-terminal would land settled via the born-terminal path).
func seedUnclaimedDeadLettered(t *testing.T, store *Storage, task *models.Task) {
	t.Helper()
	ctx := context.Background()
	started := time.Now().Add(-11 * time.Minute).UTC()
	completed := time.Now().Add(-10 * time.Minute).UTC()
	reason := "non-retryable failure: boom"
	task.Status = models.TaskStatusDeadLettered
	task.StartedAt = &started
	task.CompletedAt = &completed
	task.DeadLetteredAt = &completed
	task.DeadLetterReason = &reason
	task.DeadLetterAttempts = 1
	if err := store.DB().UpdateTask(ctx, task); err != nil {
		t.Fatalf("UpdateTask(dead_lettered): %v", err)
	}
}

// TestDeadLetteredOccurrenceSpawnsSuccessor locks in the core change: a
// dead-lettered recurring occurrence spawns its successor exactly like a
// success/error transition does (the production incident: two daily tasks
// dead-lettered once and their schedules silently stopped for days). The
// successor is the same idempotent spawn-credit contract as the normal path —
// previous_occurrence_id + lineage stamped, scheduled_for at the next cron
// tick in the task's timezone, task memory carried, and the dead-lettered
// row's spawn credit settled so the reconciliation sweep can never duplicate.
func TestDeadLetteredOccurrenceSpawnsSuccessor(t *testing.T) {
	store, database := newTestStore(t)
	store.SetTimezone("UTC")
	ctx := context.Background()
	mem := sched.NewStore(database)

	orig := &models.Task{
		ID:                     uuid.New(),
		Prompt:                 "daily digest",
		Status:                 models.TaskStatusPending,
		Priority:               10,
		Recurrence:             "@daily",
		Timezone:               "UTC",
		InstructionSelfImprove: true,
		CreatedAt:              time.Now().UTC(),
	}
	if _, err := store.AddTask(orig); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	if err := mem.UpsertTaskMemory(ctx, orig.ID, "last_price", "42.17", 100, 4096); err != nil {
		t.Fatalf("UpsertTaskMemory: %v", err)
	}
	owner := uuid.New()
	if _, err := store.leaseTaskToOwner(orig.ID, owner); err != nil {
		t.Fatalf("leaseTaskToOwner: %v", err)
	}

	before := time.Now().UTC()
	updated, err := store.DeadLetterTaskWithContext(ctx, orig.ID, owner, "non-retryable failure: boom", 1)
	if err != nil {
		t.Fatalf("DeadLetterTaskWithContext: %v", err)
	}
	after := time.Now().UTC()
	if updated.Status != models.TaskStatusDeadLettered {
		t.Fatalf("status = %s, want dead_lettered", updated.Status)
	}

	succ := successorsOf(t, store, map[uuid.UUID]bool{orig.ID: true})
	if len(succ) != 1 {
		t.Fatalf("successors = %d, want exactly 1 — a dead-lettered occurrence must not end its schedule", len(succ))
	}
	next := succ[0]
	if next.Recurrence != "@daily" {
		t.Errorf("successor recurrence = %q, want @daily", next.Recurrence)
	}
	if next.Status != models.TaskStatusScheduled {
		t.Errorf("successor status = %s, want scheduled (next cron tick is in the future, matching a success/error spawn)", next.Status)
	}
	if next.PreviousOccurrenceID == nil || *next.PreviousOccurrenceID != orig.ID {
		t.Errorf("successor previous_occurrence_id = %v, want %s", next.PreviousOccurrenceID, orig.ID)
	}
	if next.LineageID != orig.ID {
		t.Errorf("successor lineage_id = %s, want %s (the job's lineage)", next.LineageID, orig.ID)
	}
	// scheduled_for is the NEXT cron tick in the task's own timezone. The spawn
	// evaluated `now` somewhere inside [before, after], so its tick is one of
	// the two candidates — asserting both keeps the check exact without racing
	// a fire instant.
	wantBefore := nextCronTick(t, "@daily", "UTC", before)
	wantAfter := nextCronTick(t, "@daily", "UTC", after)
	if next.ScheduledFor == nil || (!next.ScheduledFor.Equal(wantBefore) && !next.ScheduledFor.Equal(wantAfter)) {
		t.Errorf("successor scheduled_for = %v, want next @daily UTC tick (%v or %v)", next.ScheduledFor, wantBefore, wantAfter)
	}
	if !next.InstructionSelfImprove {
		t.Error("successor must keep instruction_self_improve (the spawn clones the full definition)")
	}
	// Task memory is carried to the successor — a recurring occurrence must not
	// start cold (#285 parity with the success/error spawn).
	got, err := mem.GetTaskMemory(ctx, next.ID, "last_price")
	if err != nil {
		t.Fatalf("memory not carried to the successor: %v", err)
	}
	if got != "42.17" {
		t.Errorf("carried-forward value = %q, want 42.17", got)
	}

	// The dead-lettered row's spawn credit is settled, so the reconciliation
	// sweep can never re-drive (and duplicate) the spawn.
	if !recurrenceSpawned(t, store, orig.ID) {
		t.Error("the dead-lettered occurrence's spawn credit must be settled")
	}
	repaired, err := store.ReconcileRecurrences(ctx)
	if err != nil {
		t.Fatalf("ReconcileRecurrences: %v", err)
	}
	if repaired != 0 {
		t.Fatalf("repaired %d, want 0 — the spawn already claimed the credit", repaired)
	}
	if n := len(successorsOf(t, store, map[uuid.UUID]bool{orig.ID: true})); n != 1 {
		t.Fatalf("successors after sweep = %d, want still 1 (no duplicate spawn)", n)
	}
}

// TestDeadLetterBreakerParksChainAfterTwoConsecutive locks in the breaker: one
// dead-lettered occurrence is a bad day (the successor spawns), but the
// immediate successor dead-lettering too means the chain is systemically
// broken — spawning a third occurrence would just burn the next cron tick on
// the same failure. The second dead-letter must NOT spawn; it settles its
// spawn credit (park) so the reconciliation sweep never re-evaluates it, and
// replay remains the way the chain continues.
func TestDeadLetterBreakerParksChainAfterTwoConsecutive(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetTimezone("UTC")
	ctx := context.Background()
	owner := uuid.New()

	first := &models.Task{
		ID:         uuid.New(),
		Prompt:     "daily digest",
		Status:     models.TaskStatusPending,
		Priority:   10,
		Recurrence: "@daily",
		Timezone:   "UTC",
		CreatedAt:  time.Now().UTC(),
	}
	if _, err := store.AddTask(first); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	if _, err := store.leaseTaskToOwner(first.ID, owner); err != nil {
		t.Fatalf("leaseTaskToOwner: %v", err)
	}
	if _, err := store.DeadLetterTaskWithContext(ctx, first.ID, owner, "non-retryable failure: boom", 1); err != nil {
		t.Fatalf("DeadLetterTaskWithContext(first): %v", err)
	}
	succ := successorsOf(t, store, map[uuid.UUID]bool{first.ID: true})
	if len(succ) != 1 {
		t.Fatalf("successors after first dead-letter = %d, want 1 (the first dead-letter spawns)", len(succ))
	}
	second := succ[0]

	// The immediate successor dead-letters too → the breaker parks the chain.
	owner2 := uuid.New()
	if _, err := store.leaseTaskToOwner(second.ID, owner2); err != nil {
		t.Fatalf("leaseTaskToOwner(second): %v", err)
	}
	if _, err := store.DeadLetterTaskWithContext(ctx, second.ID, owner2, "non-retryable failure: boom again", 1); err != nil {
		t.Fatalf("DeadLetterTaskWithContext(second): %v", err)
	}

	if n := len(successorsOf(t, store, map[uuid.UUID]bool{first.ID: true, second.ID: true})); n != 0 {
		t.Fatalf("successors of the second dead-letter = %d, want 0 — two consecutive dead-letters must park the chain", n)
	}
	if !recurrenceSpawned(t, store, second.ID) {
		t.Error("the parked occurrence's spawn credit must be settled (park), or the sweep re-evaluates it forever")
	}
	if !recurrenceSpawned(t, store, first.ID) {
		t.Error("the first occurrence's credit stays settled from its own spawn")
	}

	// The sweep finds nothing: both credits are claimed, no phantom third row.
	repaired, err := store.ReconcileRecurrences(ctx)
	if err != nil {
		t.Fatalf("ReconcileRecurrences: %v", err)
	}
	if repaired != 0 {
		t.Fatalf("repaired %d, want 0 — the parked chain must not be re-driven", repaired)
	}
	if n := len(successorsOf(t, store, map[uuid.UUID]bool{first.ID: true, second.ID: true})); n != 0 {
		t.Fatalf("successors after sweep = %d, want still 0", n)
	}
}

// TestReconcileRecurrencesSpawnsUnclaimedDeadLetteredOccurrence is the #1116
// repair seam extended to the DLQ path: the dead-letter committed but its
// post-commit spawn never landed (transient error / crash in the window), so
// the occurrence sits dead_lettered with an UNCLAIMED credit. The sweep must
// re-drive the spawn — predecessor healthy (success), so no breaker — landing
// exactly one successor and settling the credit.
func TestReconcileRecurrencesSpawnsUnclaimedDeadLetteredOccurrence(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetTimezone("UTC")
	ctx := context.Background()
	owner := uuid.New()

	// A healthy chain: yesterday's occurrence completed normally and spawned
	// today's, the one that is about to be dead-lettered.
	pred := &models.Task{
		ID:         uuid.New(),
		Prompt:     "daily digest",
		Status:     models.TaskStatusPending,
		Priority:   10,
		Recurrence: "@daily",
		Timezone:   "UTC",
		CreatedAt:  time.Now().UTC(),
	}
	if _, err := store.AddTask(pred); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	if _, err := store.leaseTaskToOwner(pred.ID, owner); err != nil {
		t.Fatalf("leaseTaskToOwner: %v", err)
	}
	if _, err := store.UpdateTaskStatusAtomic(pred.ID, owner, &models.StatusUpdate{Status: models.TaskStatusSuccess, Message: strPtr("done")}); err != nil {
		t.Fatalf("UpdateTaskStatusAtomic: %v", err)
	}
	orphan := successorsOf(t, store, map[uuid.UUID]bool{pred.ID: true})
	if len(orphan) != 1 {
		t.Fatalf("successors of pred = %d, want 1", len(orphan))
	}
	dl := orphan[0]

	// The dead-letter commit lands; the spawn is lost. No successor of dl yet,
	// and its credit is unclaimed — exactly what the sweep must select.
	seedUnclaimedDeadLettered(t, store, dl)
	if n := len(successorsOf(t, store, map[uuid.UUID]bool{pred.ID: true, dl.ID: true})); n != 0 {
		t.Fatalf("successors of dl = %d, want 0 before the sweep", n)
	}

	repaired, err := store.ReconcileRecurrences(ctx)
	if err != nil {
		t.Fatalf("ReconcileRecurrences: %v", err)
	}
	if repaired != 1 {
		t.Fatalf("repaired %d, want 1 — the sweep must respawn the dead-lettered occurrence", repaired)
	}
	succ := successorsOf(t, store, map[uuid.UUID]bool{pred.ID: true, dl.ID: true})
	if len(succ) != 1 {
		t.Fatalf("successors after sweep = %d, want exactly 1", len(succ))
	}
	next := succ[0]
	if next.PreviousOccurrenceID == nil || *next.PreviousOccurrenceID != dl.ID {
		t.Errorf("successor previous_occurrence_id = %v, want %s", next.PreviousOccurrenceID, dl.ID)
	}
	if next.LineageID != dl.LineageID {
		t.Errorf("successor lineage_id = %s, want %s", next.LineageID, dl.LineageID)
	}
	if next.Recurrence != "@daily" || next.Status.IsTerminal() {
		t.Errorf("successor must be a live @daily occurrence; got recurrence=%q status=%s", next.Recurrence, next.Status)
	}
	if next.ScheduledFor == nil || !next.ScheduledFor.After(time.Now()) {
		t.Errorf("successor scheduled_for = %v, want the next future cron tick", next.ScheduledFor)
	}
	if !recurrenceSpawned(t, store, dl.ID) {
		t.Error("the repaired occurrence's spawn credit must be settled")
	}

	// Idempotent: the credit is claimed, so the sweep never duplicates.
	repaired, err = store.ReconcileRecurrences(ctx)
	if err != nil {
		t.Fatalf("second ReconcileRecurrences: %v", err)
	}
	if repaired != 0 {
		t.Fatalf("second sweep repaired %d, want 0", repaired)
	}
	if n := len(successorsOf(t, store, map[uuid.UUID]bool{pred.ID: true, dl.ID: true})); n != 1 {
		t.Fatalf("successors after second sweep = %d, want still 1", n)
	}
}

// TestReconcileRecurrencesAppliesDeadLetterBreaker pins that the SWEEP applies
// the same breaker as the post-dead-letter path (the decision lives in ONE
// place — scheduleNextRecurrence — so both entry points agree): a
// dead-lettered occurrence whose immediate predecessor is ALSO dead_lettered
// is parked, not spawned, and its credit is settled so the sweep does not
// re-evaluate it every tick.
func TestReconcileRecurrencesAppliesDeadLetterBreaker(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetTimezone("UTC")
	ctx := context.Background()
	owner := uuid.New()

	// First dead-letter spawns its successor through the real path.
	first := &models.Task{
		ID:         uuid.New(),
		Prompt:     "daily digest",
		Status:     models.TaskStatusPending,
		Priority:   10,
		Recurrence: "@daily",
		Timezone:   "UTC",
		CreatedAt:  time.Now().UTC(),
	}
	if _, err := store.AddTask(first); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	if _, err := store.leaseTaskToOwner(first.ID, owner); err != nil {
		t.Fatalf("leaseTaskToOwner: %v", err)
	}
	if _, err := store.DeadLetterTaskWithContext(ctx, first.ID, owner, "non-retryable failure: boom", 1); err != nil {
		t.Fatalf("DeadLetterTaskWithContext: %v", err)
	}
	succ := successorsOf(t, store, map[uuid.UUID]bool{first.ID: true})
	if len(succ) != 1 {
		t.Fatalf("successors of first = %d, want 1", len(succ))
	}
	second := succ[0]
	if second.PreviousOccurrenceID == nil || *second.PreviousOccurrenceID != first.ID {
		t.Fatalf("second.previous_occurrence_id = %v, want %s", second.PreviousOccurrenceID, first.ID)
	}

	// The successor's own dead-letter committed but its spawn crashed — it
	// reaches the sweep with an unclaimed credit while its predecessor is
	// dead_lettered: the breaker must park it (no third occurrence).
	seedUnclaimedDeadLettered(t, store, second)

	repaired, err := store.ReconcileRecurrences(ctx)
	if err != nil {
		t.Fatalf("ReconcileRecurrences: %v", err)
	}
	if repaired != 0 {
		t.Fatalf("repaired %d, want 0 — the breaker-parked chain must not spawn", repaired)
	}
	if n := len(successorsOf(t, store, map[uuid.UUID]bool{first.ID: true, second.ID: true})); n != 0 {
		t.Fatalf("successors = %d, want 0 — the breaker must not spawn a third occurrence", n)
	}
	if !recurrenceSpawned(t, store, second.ID) {
		t.Error("the breaker's park must settle the credit, or the sweep re-evaluates the chain forever")
	}

	// And the parked row stays settled: the next sweep is a no-op too.
	repaired, err = store.ReconcileRecurrences(ctx)
	if err != nil {
		t.Fatalf("second ReconcileRecurrences: %v", err)
	}
	if repaired != 0 {
		t.Fatalf("second sweep repaired %d, want 0", repaired)
	}
	if n := len(successorsOf(t, store, map[uuid.UUID]bool{first.ID: true, second.ID: true})); n != 0 {
		t.Fatalf("successors after second sweep = %d, want still 0", n)
	}
}

// TestReplayDeadLetteredWithSuccessorKeepsSpawnSettled pins the replay half of
// the new contract where the DLQ path already spawned: the successor exists,
// so replaying the dead-lettered occurrence must NOT re-arm its spawn credit —
// the replayed run's own success would otherwise fork a second parallel chain.
func TestReplayDeadLetteredWithSuccessorKeepsSpawnSettled(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetTimezone("UTC")
	ctx := context.Background()
	owner := uuid.New()

	orig := &models.Task{
		ID:         uuid.New(),
		Prompt:     "daily digest",
		Status:     models.TaskStatusPending,
		Priority:   10,
		Recurrence: "@daily",
		Timezone:   "UTC",
		CreatedAt:  time.Now().UTC(),
	}
	if _, err := store.AddTask(orig); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	if _, err := store.leaseTaskToOwner(orig.ID, owner); err != nil {
		t.Fatalf("leaseTaskToOwner: %v", err)
	}
	if _, err := store.DeadLetterTaskWithContext(ctx, orig.ID, owner, "non-retryable failure: boom", 1); err != nil {
		t.Fatalf("DeadLetterTaskWithContext: %v", err)
	}
	succ := successorsOf(t, store, map[uuid.UUID]bool{orig.ID: true})
	if len(succ) != 1 {
		t.Fatalf("successors = %d, want 1 (the DLQ path spawned)", len(succ))
	}
	successor := succ[0]
	if !recurrenceSpawned(t, store, orig.ID) {
		t.Fatal("setup: the DLQ path must settle the credit")
	}
	if recurrenceParked(t, store, orig.ID) {
		t.Fatal("setup: a spawned successor means the chain is not parked")
	}

	replayed, err := store.ReplayDeadLetteredTask(ctx, orig.ID)
	if err != nil {
		t.Fatalf("ReplayDeadLetteredTask: %v", err)
	}
	if replayed.Status != models.TaskStatusPending {
		t.Fatalf("replayed status = %s, want pending", replayed.Status)
	}
	if !recurrenceSpawned(t, store, orig.ID) {
		t.Fatal("replay with an existing successor must KEEP recurrence_spawned TRUE — re-arming would let the replayed run fork a second chain")
	}

	// The replayed run succeeds: no second successor, no fork.
	owner2 := uuid.New()
	if _, err := store.leaseTaskToOwner(orig.ID, owner2); err != nil {
		t.Fatalf("re-lease: %v", err)
	}
	if _, err := store.UpdateTaskStatusAtomic(orig.ID, owner2, &models.StatusUpdate{Status: models.TaskStatusSuccess, Message: strPtr("done")}); err != nil {
		t.Fatalf("UpdateTaskStatusAtomic: %v", err)
	}
	rest := successorsOf(t, store, map[uuid.UUID]bool{orig.ID: true})
	if len(rest) != 1 || rest[0].ID != successor.ID {
		t.Fatalf("rows beyond orig = %d, want exactly the pre-existing successor %s — the replayed run must not fork a parallel chain", len(rest), successor.ID)
	}
	if got, err := store.GetTask(successor.ID); err != nil || got.Status != models.TaskStatusScheduled || got.ID != successor.ID {
		t.Errorf("the pre-existing successor must be untouched and still scheduled; status=%v err=%v", got.Status, err)
	}
	if repaired, err := store.ReconcileRecurrences(ctx); err != nil || repaired != 0 {
		t.Fatalf("post-replay sweep: repaired=%d err=%v, want 0", repaired, err)
	}
	if n := len(successorsOf(t, store, map[uuid.UUID]bool{orig.ID: true})); n != 1 {
		t.Fatalf("successors after sweep = %d, want still 1", n)
	}
}

// TestReplayDeadLetteredWithoutSuccessorReArmsAndContinues: the breaker
// parked the chain (recurrence_parked_at set). Replay re-arms the spawn
// credit and the replayed run's success then spawns exactly one successor.
func TestReplayDeadLetteredWithoutSuccessorReArmsAndContinues(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetTimezone("UTC")
	ctx := context.Background()
	owner := uuid.New()

	// Drive the chain into the parked state: first dead-letter spawns, the
	// successor's dead-letter trips the breaker (predecessor also
	// dead_lettered) → parked with no successor and a settled credit.
	first := &models.Task{
		ID:         uuid.New(),
		Prompt:     "daily digest",
		Status:     models.TaskStatusPending,
		Priority:   10,
		Recurrence: "@daily",
		Timezone:   "UTC",
		CreatedAt:  time.Now().UTC(),
	}
	if _, err := store.AddTask(first); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	if _, err := store.leaseTaskToOwner(first.ID, owner); err != nil {
		t.Fatalf("leaseTaskToOwner: %v", err)
	}
	if _, err := store.DeadLetterTaskWithContext(ctx, first.ID, owner, "non-retryable failure: boom", 1); err != nil {
		t.Fatalf("DeadLetterTaskWithContext(first): %v", err)
	}
	succ := successorsOf(t, store, map[uuid.UUID]bool{first.ID: true})
	if len(succ) != 1 {
		t.Fatalf("successors of first = %d, want 1", len(succ))
	}
	parked := succ[0]
	owner2 := uuid.New()
	if _, err := store.leaseTaskToOwner(parked.ID, owner2); err != nil {
		t.Fatalf("leaseTaskToOwner(parked): %v", err)
	}
	if _, err := store.DeadLetterTaskWithContext(ctx, parked.ID, owner2, "non-retryable failure: boom again", 1); err != nil {
		t.Fatalf("DeadLetterTaskWithContext(parked): %v", err)
	}
	if n := len(successorsOf(t, store, map[uuid.UUID]bool{first.ID: true, parked.ID: true})); n != 0 {
		t.Fatalf("setup: parked chain must have no successor, got %d", n)
	}
	if !recurrenceSpawned(t, store, parked.ID) {
		t.Fatal("setup: the breaker's park must settle the credit")
	}
	if !recurrenceParked(t, store, parked.ID) {
		t.Fatal("setup: the breaker must stamp recurrence_parked_at")
	}

	// Replay re-arms: no successor exists, so the parked chain continues here.
	replayed, err := store.ReplayDeadLetteredTask(ctx, parked.ID)
	if err != nil {
		t.Fatalf("ReplayDeadLetteredTask: %v", err)
	}
	if replayed.Status != models.TaskStatusPending {
		t.Fatalf("replayed status = %s, want pending", replayed.Status)
	}
	if recurrenceSpawned(t, store, parked.ID) {
		t.Fatal("replay of a parked row must re-arm recurrence_spawned to FALSE — otherwise the replayed run cannot continue the chain")
	}
	if recurrenceParked(t, store, parked.ID) {
		t.Fatal("replay must clear recurrence_parked_at")
	}

	// The replayed run completes: the chain continues with exactly one successor.
	owner3 := uuid.New()
	if _, err := store.leaseTaskToOwner(parked.ID, owner3); err != nil {
		t.Fatalf("re-lease: %v", err)
	}
	if _, err := store.UpdateTaskStatusAtomic(parked.ID, owner3, &models.StatusUpdate{Status: models.TaskStatusSuccess, Message: strPtr("done")}); err != nil {
		t.Fatalf("UpdateTaskStatusAtomic: %v", err)
	}
	rest := successorsOf(t, store, map[uuid.UUID]bool{first.ID: true, parked.ID: true})
	if len(rest) != 1 {
		t.Fatalf("successors after replayed completion = %d, want exactly 1 (the chain continues once)", len(rest))
	}
	next := rest[0]
	if next.PreviousOccurrenceID == nil || *next.PreviousOccurrenceID != parked.ID {
		t.Errorf("successor previous_occurrence_id = %v, want %s", next.PreviousOccurrenceID, parked.ID)
	}
	if next.LineageID != parked.LineageID {
		t.Errorf("successor lineage_id = %s, want %s", next.LineageID, parked.LineageID)
	}
	if next.ScheduledFor == nil || !next.ScheduledFor.After(time.Now()) {
		t.Errorf("successor scheduled_for = %v, want the next future cron tick", next.ScheduledFor)
	}
	if !recurrenceSpawned(t, store, parked.ID) {
		t.Error("the replayed run's spawn must settle the credit again")
	}

	// Exactly once: the sweep finds nothing more even past the grace window.
	old := time.Now().Add(-time.Hour).UTC()
	if _, err := store.DB().Conn().ExecContext(ctx,
		`UPDATE tasks SET completed_at = $1 WHERE id = $2`, old, parked.ID); err != nil {
		t.Fatalf("backdate completed_at: %v", err)
	}
	if repaired, err := store.ReconcileRecurrences(ctx); err != nil || repaired != 0 {
		t.Fatalf("post-replay sweep: repaired=%d err=%v, want 0 (no double-spawn)", repaired, err)
	}
	if n := len(successorsOf(t, store, map[uuid.UUID]bool{first.ID: true, parked.ID: true})); n != 1 {
		t.Fatalf("successors after sweep = %d, want still 1", n)
	}
}

// deadLetterRecurringOccurrence leases and dead-letters a pending recurring
// task, returning the updated row. The caller must have already inserted it.
func deadLetterRecurringOccurrence(t *testing.T, store *Storage, id uuid.UUID) {
	t.Helper()
	owner := uuid.New()
	if _, err := store.leaseTaskToOwner(id, owner); err != nil {
		t.Fatalf("leaseTaskToOwner: %v", err)
	}
	if _, err := store.DeadLetterTaskWithContext(context.Background(), id, owner, "non-retryable failure: boom", 1); err != nil {
		t.Fatalf("DeadLetterTaskWithContext: %v", err)
	}
}

// TestReplayDeadLetteredUnclaimedRearms: an unclaimed dead-letter (crash
// window, or a seed that never spawned) has recurrence_spawned FALSE and
// is not parked. Replay keeps the credit unclaimed so the replayed run
// can spawn.
func TestReplayDeadLetteredUnclaimedRearms(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetTimezone("UTC")
	ctx := context.Background()

	dl := seedTerminalRecurring(t, store, models.TaskStatusDeadLettered, 10*time.Minute, nil)
	if recurrenceSpawned(t, store, dl.ID) {
		t.Fatal("setup: unclaimed")
	}
	if recurrenceParked(t, store, dl.ID) {
		t.Fatal("setup: unclaimed seed is not parked")
	}
	if _, err := store.ReplayDeadLetteredTask(ctx, dl.ID); err != nil {
		t.Fatalf("ReplayDeadLetteredTask: %v", err)
	}
	if recurrenceSpawned(t, store, dl.ID) {
		t.Fatal("replay of an unclaimed row must leave recurrence_spawned FALSE")
	}
	if recurrenceParked(t, store, dl.ID) {
		t.Fatal("replay must leave recurrence_parked_at NULL")
	}
}

// TestSettleDeadLetteredRecurrenceSpawnDoesNotClobberReplay: if the operator
// replay commits before the breaker settle, the guarded UPDATE must not flip
// the re-armed flag back to TRUE.
func TestSettleDeadLetteredRecurrenceSpawnDoesNotClobberReplay(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetTimezone("UTC")
	ctx := context.Background()

	first := &models.Task{
		ID:         uuid.New(),
		Prompt:     "daily digest",
		Status:     models.TaskStatusPending,
		Priority:   10,
		Recurrence: "@daily",
		Timezone:   "UTC",
		CreatedAt:  time.Now().UTC(),
	}
	if _, err := store.AddTask(first); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	deadLetterRecurringOccurrence(t, store, first.ID)
	succ := successorsOf(t, store, map[uuid.UUID]bool{first.ID: true})
	if len(succ) != 1 {
		t.Fatalf("successors = %d, want 1", len(succ))
	}
	parked := succ[0]
	deadLetterRecurringOccurrence(t, store, parked.ID)

	if _, err := store.ReplayDeadLetteredTask(ctx, parked.ID); err != nil {
		t.Fatalf("ReplayDeadLetteredTask: %v", err)
	}
	if recurrenceSpawned(t, store, parked.ID) {
		t.Fatal("setup: replay of a parked row with nothing newer must re-arm")
	}

	store.settleDeadLetteredRecurrenceSpawn(ctx, parked.ID)
	if recurrenceSpawned(t, store, parked.ID) {
		t.Fatal("breaker settle after replay must leave the re-armed flag FALSE (row is no longer dead_lettered)")
	}
}

// TestScheduleNextRecurrenceIgnoresStaleRowAfterReplay: the sweep may hold a
// dead_lettered snapshot; if replay commits first, the status-gated claim
// must not consume the re-armed credit or mint a successor from the stale row.
func TestScheduleNextRecurrenceIgnoresStaleRowAfterReplay(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetTimezone("UTC")
	ctx := context.Background()

	stale := seedTerminalRecurring(t, store, models.TaskStatusDeadLettered, 10*time.Minute, nil)
	if recurrenceSpawned(t, store, stale.ID) {
		t.Fatal("setup: unclaimed dead-letter")
	}
	if _, err := store.ReplayDeadLetteredTask(ctx, stale.ID); err != nil {
		t.Fatalf("ReplayDeadLetteredTask: %v", err)
	}
	if recurrenceSpawned(t, store, stale.ID) {
		t.Fatal("setup: replay of a row with no successor must re-arm")
	}

	if spawned := store.scheduleNextRecurrence(ctx, stale); spawned {
		t.Fatal("stale dead_lettered snapshot must not spawn after replay")
	}
	if recurrenceSpawned(t, store, stale.ID) {
		t.Fatal("stale spawn must leave the re-armed credit untouched")
	}
	if n := len(successorsOf(t, store, map[uuid.UUID]bool{stale.ID: true})); n != 0 {
		t.Fatalf("successors = %d, want 0", n)
	}
}
