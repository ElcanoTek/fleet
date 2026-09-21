package storage

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// recurrenceSpawned reads the internal spawn-settlement flag (migration 065)
// straight from the row — it is deliberately not surfaced on models.Task.
func recurrenceSpawned(t *testing.T, store *Storage, id uuid.UUID) bool {
	t.Helper()
	var settled bool
	if err := store.DB().Conn().QueryRowContext(context.Background(),
		`SELECT recurrence_spawned FROM tasks WHERE id = $1`, id).Scan(&settled); err != nil {
		t.Fatalf("read recurrence_spawned: %v", err)
	}
	return settled
}

// seedTerminalRecurring builds a terminal recurring occurrence whose
// post-completion spawn never happened — the exact row a transient spawn
// error or a crash in the terminal-commit→spawn window leaves behind. It
// mirrors the production sequence: the row is INSERTED live (pending) and only
// then TRANSITIONED terminal (the upsert preserves the unclaimed FALSE spawn
// flag, like UpdateTaskTx does for a real terminal write). Inserting the row
// already-terminal would instead take the born-terminal import path, which
// deliberately lands settled — see TestImportedTerminalRecurringHistoryIsInert.
func seedTerminalRecurring(t *testing.T, store *Storage, status models.TaskStatus, completedAgo time.Duration, remaining *int) *models.Task {
	t.Helper()
	started := time.Now().Add(-completedAgo - time.Minute).UTC()
	completed := time.Now().Add(-completedAgo).UTC()
	task := &models.Task{
		ID:                  uuid.New(),
		Prompt:              "daily digest",
		Status:              models.TaskStatusPending,
		Priority:            10,
		Recurrence:          "@daily",
		Timezone:            "UTC",
		CreatedAt:           time.Now().UTC(),
		RecurrenceRemaining: remaining,
	}
	if _, err := store.AddTask(task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	task.Status = status
	task.StartedAt = &started
	task.CompletedAt = &completed
	if err := store.DB().UpdateTask(context.Background(), task); err != nil {
		t.Fatalf("UpdateTask(terminal): %v", err)
	}
	return task
}

// successorsOf returns every task row other than the given ids.
func successorsOf(t *testing.T, store *Storage, exclude map[uuid.UUID]bool) []*models.Task {
	t.Helper()
	all, err := store.GetAllTasks()
	if err != nil {
		t.Fatalf("GetAllTasks: %v", err)
	}
	var out []*models.Task
	for _, tk := range all {
		if !exclude[tk.ID] {
			out = append(out, tk)
		}
	}
	return out
}

// TestReconcileRecurrencesRepairsLostSpawn locks in the #1116 fix: a terminal
// recurring occurrence whose post-completion spawn was lost (transient DB
// error, or a crash between the terminal commit and the spawn) must be
// repaired by the reconciliation sweep — the schedule no longer dies silently.
// The repair is idempotent: a second sweep spawns nothing further.
func TestReconcileRecurrencesRepairsLostSpawn(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetTimezone("UTC")
	ctx := context.Background()

	orphan := seedTerminalRecurring(t, store, models.TaskStatusSuccess, 10*time.Minute, nil)

	repaired, err := store.ReconcileRecurrences(ctx)
	if err != nil {
		t.Fatalf("ReconcileRecurrences: %v", err)
	}
	if repaired != 1 {
		t.Fatalf("repaired %d, want 1 — the orphaned chain was not respawned", repaired)
	}

	succ := successorsOf(t, store, map[uuid.UUID]bool{orphan.ID: true})
	if len(succ) != 1 {
		t.Fatalf("successors = %d, want exactly 1", len(succ))
	}
	if succ[0].Recurrence != "@daily" || succ[0].Status.IsTerminal() {
		t.Errorf("successor must be a live @daily occurrence; got recurrence=%q status=%s", succ[0].Recurrence, succ[0].Status)
	}
	if succ[0].ScheduledFor == nil || !succ[0].ScheduledFor.After(time.Now()) {
		t.Errorf("successor scheduled_for = %v, want the next future cron tick", succ[0].ScheduledFor)
	}
	if !recurrenceSpawned(t, store, orphan.ID) {
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
	if n := len(successorsOf(t, store, map[uuid.UUID]bool{orphan.ID: true})); n != 1 {
		t.Fatalf("successors after second sweep = %d, want still 1 (no duplicate spawn)", n)
	}
}

// TestReconcileRecurrencesRespectsGraceAndStatus pins the sweep's selection:
// a row completed inside the grace window is left for the normal post-commit
// spawn, and cancelled rows never spawn (a deliberate stop must not resurrect
// the chain). Dead-lettered rows DO spawn (ADR-0070); that path is covered by
// the dedicated dead-letter recurrence tests, not this grace/status pin.
func TestReconcileRecurrencesRespectsGraceAndStatus(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetTimezone("UTC")
	ctx := context.Background()

	fresh := seedTerminalRecurring(t, store, models.TaskStatusSuccess, 0, nil) // just completed
	cancelled := seedTerminalRecurring(t, store, models.TaskStatusCancelled, 10*time.Minute, nil)

	repaired, err := store.ReconcileRecurrences(ctx)
	if err != nil {
		t.Fatalf("ReconcileRecurrences: %v", err)
	}
	if repaired != 0 {
		t.Fatalf("repaired %d, want 0 (inside grace / cancelled)", repaired)
	}
	exclude := map[uuid.UUID]bool{fresh.ID: true, cancelled.ID: true}
	if n := len(successorsOf(t, store, exclude)); n != 0 {
		t.Fatalf("successors = %d, want 0", n)
	}
}

// TestReconcileRecurrencesSettlesEndedChain: a chain whose end condition is
// already met (run budget exhausted) is SETTLED without spawning, so the sweep
// does not re-evaluate it forever — and it never gains a phantom successor.
func TestReconcileRecurrencesSettlesEndedChain(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetTimezone("UTC")
	ctx := context.Background()

	one := 1
	ended := seedTerminalRecurring(t, store, models.TaskStatusSuccess, 10*time.Minute, &one)

	repaired, err := store.ReconcileRecurrences(ctx)
	if err != nil {
		t.Fatalf("ReconcileRecurrences: %v", err)
	}
	if repaired != 0 {
		t.Fatalf("repaired %d, want 0 (the chain legitimately ended)", repaired)
	}
	if n := len(successorsOf(t, store, map[uuid.UUID]bool{ended.ID: true})); n != 0 {
		t.Fatalf("successors = %d, want 0 — an ended chain must not respawn", n)
	}
	if !recurrenceSpawned(t, store, ended.ID) {
		t.Error("an ended chain must be settled so the sweep stops selecting it")
	}
}

// TestImportedTerminalRecurringHistoryIsInert is the disaster-recovery guard
// (#1116 review): `fleet import` restores terminal recurring HISTORY rows
// verbatim (status/recurrence/completed_at preserved, the #713 flow) through
// AddTaskWithContext — the exact insert exercised here. Those rows' spawns
// happened (or were deliberately not made) in the source deployment, so they
// must land SETTLED: if a fresh insert of a born-terminal row defaulted the
// spawn flag to FALSE, a restored daily chain with 90 terminal occurrences
// would get 90 duplicate successors within two sweep ticks, each perpetuating
// its own chain.
func TestImportedTerminalRecurringHistoryIsInert(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetTimezone("UTC")
	ctx := context.Background()

	started := time.Now().Add(-11 * time.Minute).UTC()
	completed := time.Now().Add(-10 * time.Minute).UTC()
	imported := &models.Task{
		ID:          uuid.New(),
		Prompt:      "daily digest (restored history)",
		Status:      models.TaskStatusSuccess,
		Priority:    10,
		Recurrence:  "@daily",
		Timezone:    "UTC",
		CreatedAt:   time.Now().Add(-24 * time.Hour).UTC(),
		StartedAt:   &started,
		CompletedAt: &completed,
	}
	// The import seam: `fleet sched task import` → AddTaskWithContext (a fresh INSERT).
	if _, err := store.AddTaskWithContext(ctx, imported); err != nil {
		t.Fatalf("AddTaskWithContext: %v", err)
	}

	repaired, err := store.ReconcileRecurrences(ctx)
	if err != nil {
		t.Fatalf("ReconcileRecurrences: %v", err)
	}
	if repaired != 0 {
		t.Fatalf("repaired %d, want 0 — imported terminal history must be inert, not treated as a lost spawn", repaired)
	}
	if n := len(successorsOf(t, store, map[uuid.UUID]bool{imported.ID: true})); n != 0 {
		t.Fatalf("successors = %d, want 0 — the sweep resurrected restored history", n)
	}
	if !recurrenceSpawned(t, store, imported.ID) {
		t.Error("a row inserted already-terminal must land with its spawn credit settled")
	}
}

// TestReconcileRecurrencesDoesNotRespawnBackfilledDeadLetter pins migration
// 071's backfill: every organic pre-upgrade dead_lettered row had
// recurrence_spawned=FALSE (only success/error claimed the credit), including
// rows whose lineage ALREADY continued. The consecutive-DLQ breaker does not
// catch that (the predecessor was a success), so an unsettled flag would make
// the first sweep insert a second successor — a duplicate chain. After the
// backfill the sweep must leave the lineage alone.
func TestReconcileRecurrencesDoesNotRespawnBackfilledDeadLetter(t *testing.T) {
	store, _ := newTestStore(t)
	store.SetTimezone("UTC")
	ctx := context.Background()

	// Pre-upgrade organic DLQ: inserted live, then transitioned dead_lettered
	// (UpdateTaskTx does not claim the spawn credit). Age it past the grace
	// window so the sweep WOULD select it if the flag stayed FALSE.
	dl := seedTerminalRecurring(t, store, models.TaskStatusDeadLettered, 10*time.Minute, nil)
	if recurrenceSpawned(t, store, dl.ID) {
		t.Fatal("setup: organic dead-letter must land with an unclaimed spawn credit")
	}

	// Migration 071 backfill: settle existing dead_lettered rows.
	if _, err := store.DB().Conn().ExecContext(ctx,
		`UPDATE tasks SET recurrence_spawned = TRUE WHERE id = $1`, dl.ID); err != nil {
		t.Fatalf("backfill settle: %v", err)
	}
	if !recurrenceSpawned(t, store, dl.ID) {
		t.Fatal("setup: backfill must settle the credit")
	}

	// The lineage already continued: a later success (production shape:
	// lineage ef49b641 — dead-lettered occurrence, then successes and a live
	// head). Born-terminal so ITS spawn credit is settled too.
	started := time.Now().Add(-11 * time.Minute).UTC()
	completed := time.Now().Add(-10 * time.Minute).UTC()
	later := &models.Task{
		ID:                   uuid.New(),
		Prompt:               "daily digest",
		Status:               models.TaskStatusSuccess,
		Priority:             10,
		Recurrence:           "@daily",
		Timezone:             "UTC",
		LineageID:            dl.ID,
		PreviousOccurrenceID: &dl.ID,
		CreatedAt:            time.Now().Add(-24 * time.Hour).UTC(),
		StartedAt:            &started,
		CompletedAt:          &completed,
	}
	if _, err := store.AddTaskWithContext(ctx, later); err != nil {
		t.Fatalf("AddTaskWithContext(later success): %v", err)
	}

	repaired, err := store.ReconcileRecurrences(ctx)
	if err != nil {
		t.Fatalf("ReconcileRecurrences: %v", err)
	}
	if repaired != 0 {
		t.Fatalf("repaired %d, want 0 — a backfilled pre-upgrade dead-letter must not grow a duplicate chain", repaired)
	}
	got := successorsOf(t, store, map[uuid.UUID]bool{dl.ID: true})
	if len(got) != 1 || got[0].ID != later.ID {
		t.Fatalf("rows besides the dead-letter = %d, want exactly the later success %s (no duplicate spawn)", len(got), later.ID)
	}
}

// TestReplayedDeadLetterContinuesChainOnce pins the DLQ↔recurrence contract
// (ADR-0070): dead-lettering a recurring occurrence spawns the successor, and
// replaying that quarantined row must NOT fork a second chain when the
// replayed run completes — the spawn credit stays claimed because a successor
// already exists.
func TestReplayedDeadLetterContinuesChainOnce(t *testing.T) {
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
	if _, err := store.DeadLetterTaskWithContext(ctx, orig.ID, owner, "boom", 1); err != nil {
		t.Fatalf("DeadLetterTaskWithContext: %v", err)
	}
	// Dead-lettering continues the chain: exactly one successor, spawn credit claimed.
	if n := len(successorsOf(t, store, map[uuid.UUID]bool{orig.ID: true})); n != 1 {
		t.Fatalf("successors after dead-letter = %d, want 1 (quarantine spawns the next occurrence)", n)
	}
	if !recurrenceSpawned(t, store, orig.ID) {
		t.Fatal("dead-lettering a recurring occurrence must settle the spawn credit")
	}

	if _, err := store.ReplayDeadLetteredTask(ctx, orig.ID); err != nil {
		t.Fatalf("ReplayDeadLetteredTask: %v", err)
	}
	if !recurrenceSpawned(t, store, orig.ID) {
		t.Fatal("replay must keep the spawn credit claimed when a successor already exists")
	}

	// The replayed run completes: it must not mint a second successor.
	owner2 := uuid.New()
	if _, err := store.leaseTaskToOwner(orig.ID, owner2); err != nil {
		t.Fatalf("re-lease: %v", err)
	}
	if _, err := store.UpdateTaskStatusAtomic(orig.ID, owner2, &models.StatusUpdate{
		Status: models.TaskStatusSuccess, Message: strPtr("done"),
	}); err != nil {
		t.Fatalf("UpdateTaskStatusAtomic: %v", err)
	}
	succ := successorsOf(t, store, map[uuid.UUID]bool{orig.ID: true})
	if len(succ) != 1 {
		t.Fatalf("successors after replayed completion = %d, want exactly 1 (no forked chain)", len(succ))
	}

	// And only once: the sweep finds nothing more even past the grace window.
	old := time.Now().Add(-time.Hour).UTC()
	if _, err := store.DB().Conn().ExecContext(ctx,
		`UPDATE tasks SET completed_at = $1 WHERE id = $2`, old, orig.ID); err != nil {
		t.Fatalf("backdate completed_at: %v", err)
	}
	if repaired, err := store.ReconcileRecurrences(ctx); err != nil || repaired != 0 {
		t.Fatalf("post-replay sweep: repaired=%d err=%v, want 0 (no double-spawn)", repaired, err)
	}
	if n := len(successorsOf(t, store, map[uuid.UUID]bool{orig.ID: true})); n != 1 {
		t.Fatalf("successors after sweep = %d, want still 1", n)
	}
}

// TestTerminalSpawnSettlesCreditAgainstReconciliation drives the NORMAL path —
// claim → terminal success → post-commit spawn — and proves the spawn credit
// it claims keeps the reconciliation sweep from ever double-spawning the same
// occurrence, even once the row ages past the grace window.
func TestTerminalSpawnSettlesCreditAgainstReconciliation(t *testing.T) {
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
	if _, err := store.UpdateTaskStatusAtomic(orig.ID, owner, &models.StatusUpdate{
		Status: models.TaskStatusSuccess, Message: strPtr("done"),
	}); err != nil {
		t.Fatalf("UpdateTaskStatusAtomic: %v", err)
	}
	if !recurrenceSpawned(t, store, orig.ID) {
		t.Fatal("the terminal transition's spawn must settle the credit")
	}
	if n := len(successorsOf(t, store, map[uuid.UUID]bool{orig.ID: true})); n != 1 {
		t.Fatalf("successors = %d, want 1 from the normal spawn", n)
	}

	// Age the row past the grace window; the sweep must still find nothing.
	old := time.Now().Add(-time.Hour).UTC()
	if _, err := store.DB().Conn().ExecContext(ctx,
		`UPDATE tasks SET completed_at = $1 WHERE id = $2`, old, orig.ID); err != nil {
		t.Fatalf("backdate completed_at: %v", err)
	}
	repaired, err := store.ReconcileRecurrences(ctx)
	if err != nil {
		t.Fatalf("ReconcileRecurrences: %v", err)
	}
	if repaired != 0 {
		t.Fatalf("repaired %d, want 0 — the normal spawn already claimed the credit", repaired)
	}
	if n := len(successorsOf(t, store, map[uuid.UUID]bool{orig.ID: true})); n != 1 {
		t.Fatalf("successors = %d, want still 1 (no duplicate)", n)
	}
}
