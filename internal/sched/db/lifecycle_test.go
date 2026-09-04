package db

// Coupling tests between the task-lifecycle table
// (internal/sched/models/task_lifecycle.go, #1127) and this package's
// status-guarded SQL. Two strengths of check, called out per test:
//
//   - BEHAVIORAL (strong): TestLifecycleDBWriterMatrix seeds a task row in
//     every status and drives each transition-writing function against it,
//     asserting the row moves exactly when the table has that writer's edge.
//     Removing a table row makes the matrix expect "unchanged" while the
//     writer still transitions — a loud red failure (and vice versa for a
//     guard change without a table edit).
//   - TEXTUAL (weaker, used where no behavioral seam exists): the SQL-literal
//     drift scan (#1126's schema↔registry precedent applied to source text)
//     and the scheduler caller pins, which read Go source and assert the
//     status literals/identifiers it mentions are edges/statuses the table
//     knows. These catch drift in query text that parameters would hide from
//     a behavioral run (e.g. a literal 'analyzing' resurrected in a WHERE).

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// taskStatusIdentifiers maps the models constant names (as they appear in
// source text) to their values, for the textual caller pins.
var taskStatusIdentifiers = map[string]models.TaskStatus{
	"TaskStatusPending":             models.TaskStatusPending,
	"TaskStatusScheduled":           models.TaskStatusScheduled,
	"TaskStatusLeased":              models.TaskStatusLeased,
	"TaskStatusRunning":             models.TaskStatusRunning,
	"TaskStatusPausedAwaitingInput": models.TaskStatusPausedAwaitingInput,
	"TaskStatusPausedAwaitingWake":  models.TaskStatusPausedAwaitingWake,
	"TaskStatusSuccess":             models.TaskStatusSuccess,
	"TaskStatusError":               models.TaskStatusError,
	"TaskStatusCancelled":           models.TaskStatusCancelled,
	"TaskStatusDeadLettered":        models.TaskStatusDeadLettered,
}

func statusIn(s models.TaskStatus, set []models.TaskStatus) bool {
	for _, v := range set {
		if v == s {
			return true
		}
	}
	return false
}

// stringLiteralsInDir parses every non-test .go file in dir and returns the
// values of all string literals (raw and interpreted).
func stringLiteralsInDir(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	if len(files) == 0 {
		t.Fatalf("no Go files found in %s — scan path wrong?", dir)
	}
	var out []string
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, uerr := strconv.Unquote(lit.Value)
			if uerr != nil {
				return true
			}
			out = append(out, s)
			return true
		})
	}
	return out
}

var (
	tasksTableRe   = regexp.MustCompile(`\btasks\b`)
	statusEqRe     = regexp.MustCompile(`(?i)\bstatus\s*(?:=|<>|!=)\s*'([a-z_]+)'`)
	statusInListRe = regexp.MustCompile(`(?i)\bstatus\s+IN\s*\(([^)]*)\)`)
	quotedWordRe   = regexp.MustCompile(`'([a-z_]+)'`)
)

// taskStatusLiteralsIn extracts every status literal used in a tasks-table
// status predicate/assignment within the given SQL text.
func taskStatusLiteralsIn(sql string) []string {
	if !tasksTableRe.MatchString(sql) {
		return nil
	}
	var out []string
	for _, m := range statusEqRe.FindAllStringSubmatch(sql, -1) {
		out = append(out, m[1])
	}
	for _, m := range statusInListRe.FindAllStringSubmatch(sql, -1) {
		for _, q := range quotedWordRe.FindAllStringSubmatch(m[1], -1) {
			out = append(out, q[1])
		}
	}
	return out
}

// TestLifecycleSQLStatusLiteralsKnown is the SQL-literal drift scan
// (TEXTUAL): every status literal appearing in a tasks-table status clause in
// this package's, storage's, or the scheduler's source must be a status the
// lifecycle table knows. A typo, or a resurrected retired status
// ('analyzing', rewritten away by migration 063), fails here. Statuses bound
// as parameters via string(models.TaskStatusX) are compile-checked and
// covered by the behavioral matrix instead. Migration .sql files are NOT
// scanned: they are immutable history and legitimately mention retired
// statuses (063 itself does).
func TestLifecycleSQLStatusLiteralsKnown(t *testing.T) {
	dirs := []string{".", "../storage", "../scheduler"}
	found := 0
	for _, dir := range dirs {
		for _, lit := range stringLiteralsInDir(t, dir) {
			for _, status := range taskStatusLiteralsIn(lit) {
				found++
				if !statusIn(models.TaskStatus(status), models.AllTaskStatuses) {
					t.Errorf("%s: SQL mentions task status literal %q, which the lifecycle table does not know (retired or typo?): %q", dir, status, lit)
				}
			}
		}
	}
	// The scan must actually be seeing the known literal-guarded queries
	// (pause.go / wake.go / cleanup.go); zero hits means the extraction broke.
	if found == 0 {
		t.Fatal("SQL scan extracted no status literals — the extraction regexes or scan paths have rotted")
	}
}

// TestLifecycleCleanupSetPinned pins cleanup.go's literal terminal-status
// lists to models.CleanupEligibleTaskStatuses, exactly (TEXTUAL): the sweeps
// must prune success/error/cancelled and must NOT prune dead_lettered (a
// quarantined row awaits operator review). cleanupEligibleSubquery is checked
// directly; DeleteOldHistory / archiveCandidatesPage bind the same set as
// parameters and are covered by the source scan + compile.
func TestLifecycleCleanupSetPinned(t *testing.T) {
	got := taskStatusLiteralsIn(cleanupEligibleSubquery)
	if len(got) != len(models.CleanupEligibleTaskStatuses) {
		t.Fatalf("cleanupEligibleSubquery lists statuses %v; models.CleanupEligibleTaskStatuses has %v", got, models.CleanupEligibleTaskStatuses)
	}
	for _, s := range got {
		if !statusIn(models.TaskStatus(s), models.CleanupEligibleTaskStatuses) {
			t.Errorf("cleanupEligibleSubquery prunes status %q, which is not in models.CleanupEligibleTaskStatuses", s)
		}
	}
}

// TestLifecycleSerializationPlaceholders pins serializationNotBlockedSQL's
// hard-coded IN-list placeholders to len(models.ActiveTaskStatuses)
// (TEXTUAL): taskActiveStatuses derives from that set at runtime, so if the
// set ever grows, this fails until the $2..$N list (and the claim query's
// argument splice) grows with it.
func TestLifecycleSerializationPlaceholders(t *testing.T) {
	m := regexp.MustCompile(`(?i)status\s+IN\s*\(([^)]*)\)`).FindStringSubmatch(serializationNotBlockedSQL)
	if m == nil {
		t.Fatalf("serializationNotBlockedSQL no longer contains a status IN (...) clause: %q", serializationNotBlockedSQL)
	}
	if got := strings.Count(m[1], "$"); got != len(models.ActiveTaskStatuses) {
		t.Errorf("serializationNotBlockedSQL binds %d active statuses but models.ActiveTaskStatuses has %d — grow the placeholder list with the set", got, len(models.ActiveTaskStatuses))
	}
}

// TestLifecycleRecurrenceSpawnPredicate pins the Go-side spawn-settlement
// predicate to the lifecycle set (STRONG, set-derivation): a row is born
// settled exactly when its status is one that would have spawned.
func TestLifecycleRecurrenceSpawnPredicate(t *testing.T) {
	for _, s := range models.AllTaskStatuses {
		want := statusIn(s, models.RecurrenceSpawnTaskStatuses)
		if got := recurrenceSpawnedInsertValue(&models.Task{Status: s}); got != want {
			t.Errorf("recurrenceSpawnedInsertValue(%q) = %v, want %v (models.RecurrenceSpawnTaskStatuses)", s, got, want)
		}
	}
}

// TestLifecycleSchedulerCallerPins asserts the scheduler's parametric
// transition calls pass exactly the from/to statuses the lifecycle table
// lists for those writers (TEXTUAL: db.UpdateTasksStatusBatch and
// db.SettleGatedTask transition wherever their arguments say, so the
// caller's compile-time constants are the guard — this pin makes a new
// caller edge fail until the table learns it, and vice versa).
//
// SCAN SCOPE: internal/sched/scheduler only — the writer constants' docs
// assert the scheduler is the only caller today. If either function ever
// gains a call site outside that package, extend the scanned dirs here or
// the new caller's edges go unpinned.
func TestLifecycleSchedulerCallerPins(t *testing.T) {
	var source strings.Builder
	for _, lit := range sourceTextOfDir(t, "../scheduler") {
		source.WriteString(lit)
	}
	text := source.String()

	// db.SettleGatedTask: last argument is the target status.
	settleRe := regexp.MustCompile(`SettleGatedTask\([^)]*models\.(TaskStatus\w+)\s*\)`)
	gotSettle := map[models.TaskStatus]bool{}
	for _, m := range settleRe.FindAllStringSubmatch(text, -1) {
		status, ok := taskStatusIdentifiers[m[1]]
		if !ok {
			t.Fatalf("SettleGatedTask caller uses unknown status identifier %q", m[1])
		}
		gotSettle[status] = true
	}
	wantSettle := map[models.TaskStatus]bool{}
	for _, tr := range models.TaskTransitionsByWriter(models.TaskWriterSettleGatedTask) {
		wantSettle[tr.To] = true
		if tr.From != models.TaskStatusScheduled {
			t.Errorf("table lists a SettleGatedTask edge from %q, but the function is guarded on scheduled only", tr.From)
		}
	}
	for s := range gotSettle {
		if !wantSettle[s] {
			t.Errorf("scheduler settles gated tasks to %q but the lifecycle table has no scheduled→%q edge for %s", s, s, models.TaskWriterSettleGatedTask)
		}
	}
	for s := range wantSettle {
		if !gotSettle[s] {
			t.Errorf("lifecycle table lists scheduled→%q for %s but no scheduler call site passes it", s, models.TaskWriterSettleGatedTask)
		}
	}

	// db.UpdateTasksStatusBatch: (from, to) argument pairs.
	batchRe := regexp.MustCompile(`UpdateTasksStatusBatch\([^)]*models\.(TaskStatus\w+),\s*models\.(TaskStatus\w+)\s*\)`)
	matches := batchRe.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		t.Fatal("no UpdateTasksStatusBatch call sites found in the scheduler — pin scan rotted?")
	}
	gotBatch := map[[2]models.TaskStatus]bool{}
	for _, m := range matches {
		from, okF := taskStatusIdentifiers[m[1]]
		to, okT := taskStatusIdentifiers[m[2]]
		if !okF || !okT {
			t.Fatalf("UpdateTasksStatusBatch caller uses unknown status identifiers %q/%q", m[1], m[2])
		}
		gotBatch[[2]models.TaskStatus{from, to}] = true
		if !models.TaskTransitionExists(from, to, models.TaskWriterScheduledPromotion) {
			t.Errorf("scheduler batches %q→%q but the lifecycle table has no such edge for %s", from, to, models.TaskWriterScheduledPromotion)
		}
	}
	for _, tr := range models.TaskTransitionsByWriter(models.TaskWriterScheduledPromotion) {
		if !gotBatch[[2]models.TaskStatus{tr.From, tr.To}] {
			t.Errorf("lifecycle table lists %q→%q for %s but no scheduler call site passes that pair", tr.From, tr.To, models.TaskWriterScheduledPromotion)
		}
	}
}

// sourceTextOfDir returns the raw text of every non-test .go file in dir
// (whole files, not just literals — the caller pins match call expressions).
func sourceTextOfDir(t *testing.T, dir string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob %s: %v (files: %d)", dir, err, len(files))
	}
	var out []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, rerr := os.ReadFile(f)
		if rerr != nil {
			t.Fatalf("read %s: %v", f, rerr)
		}
		out = append(out, string(b))
	}
	return out
}

// ── Behavioral writer matrix ────────────────────────────────────────────────

// lifecycleSeed is one seeded task row plus the ancillary identity the
// writers need (the lease owner used, if any).
type lifecycleSeed struct {
	id    uuid.UUID
	owner uuid.UUID
}

// lifecycleSeedOpts shapes the ancillary columns so the STATUS predicate is
// the discriminating part of each writer's guard wherever the guard is pure
// SQL: e.g. the pause writers get a valid lease on EVERY row, so only the
// status='running' clause decides. (The lease-guarded storage writers are
// exercised production-shaped in sched/storage/lifecycle_test.go instead —
// see the note there.)
type lifecycleSeedOpts struct {
	withLease    bool          // lease_owner + lease_expires_at now+5m
	leaseExpired bool          // lease_expires_at in the past instead
	attemptSpent bool          // attempt_count = max_retries (else 0 of 2)
	pausedAgo    time.Duration // paused_at = now-… (0 = unset)
	wakeAgo      time.Duration // wake_at = now-…, wake_event_key set (0 = unset)
	scheduledFor *time.Time
}

// seedLifecycleTask inserts one row in the given status. paused_at /
// wake_at / wake_event_key are insert-excluded columns (the #1126 registry
// doctrine), so they are stamped by a direct UPDATE, standing in for the
// guarded pause/park writers.
func seedLifecycleTask(t *testing.T, d *Database, status models.TaskStatus, opts lifecycleSeedOpts) lifecycleSeed {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	owner := uuid.New()
	task := &models.Task{
		ID:         uuid.New(),
		Prompt:     "lifecycle matrix " + string(status),
		Status:     status,
		Priority:   10,
		MaxRetries: 2,
		CreatedAt:  now,
	}
	if opts.attemptSpent {
		task.AttemptCount = task.MaxRetries
	}
	if opts.withLease || opts.leaseExpired {
		o := owner.String()
		task.LeaseOwner = &o
		exp := now.Add(5 * time.Minute)
		if opts.leaseExpired {
			exp = now.Add(-5 * time.Minute)
		}
		task.LeaseExpiresAt = &exp
	}
	if opts.scheduledFor != nil {
		task.ScheduledFor = opts.scheduledFor
	}
	if err := d.AddTask(ctx, task); err != nil {
		t.Fatalf("seed %s: %v", status, err)
	}
	if opts.pausedAgo > 0 {
		if _, err := d.conn.ExecContext(ctx,
			`UPDATE tasks SET paused_at = $2 WHERE id = $1`,
			task.ID, now.Add(-opts.pausedAgo)); err != nil {
			t.Fatalf("seed paused_at: %v", err)
		}
	}
	if opts.wakeAgo > 0 {
		if _, err := d.conn.ExecContext(ctx,
			`UPDATE tasks SET wake_at = $2, wake_event_key = 'lifecycle-evt' WHERE id = $1`,
			task.ID, now.Add(-opts.wakeAgo)); err != nil {
			t.Fatalf("seed wake state: %v", err)
		}
	}
	return lifecycleSeed{id: task.ID, owner: owner}
}

// assertLifecycleOutcome checks one seeded row against the table: it must be
// in target iff the table lists (seeded → target) for the writer, and
// otherwise unchanged.
func assertLifecycleOutcome(t *testing.T, d *Database, writer string, target models.TaskStatus, seeded models.TaskStatus, id uuid.UUID) {
	t.Helper()
	got, err := d.GetTask(context.Background(), id)
	if err != nil {
		t.Fatalf("re-read %s row: %v", seeded, err)
	}
	want := seeded
	if models.TaskTransitionExists(seeded, target, writer) {
		want = target
	}
	if got.Status != want {
		t.Errorf("%s from %q: row ended %q, want %q (lifecycle table edge %q→%q exists: %v)",
			writer, seeded, got.Status, want, seeded, target,
			models.TaskTransitionExists(seeded, target, writer))
	}
}

// TestLifecycleDBWriterMatrix drives every db-level transition writer against
// a row seeded in EVERY status and asserts the outcomes match the lifecycle
// table exactly (BEHAVIORAL, the strongest seam: the guard that runs is the
// production SQL). Each subtest starts from a clean tasks table because the
// sweep writers (wake/expiry/recovery) act globally.
func TestLifecycleDBWriterMatrix(t *testing.T) {
	d := setupTestDB(t)
	defer d.Close()
	ctx := context.Background()

	reset := func() {
		if _, err := d.conn.ExecContext(ctx, "DELETE FROM tasks"); err != nil {
			t.Fatalf("reset tasks: %v", err)
		}
	}
	seedAll := func(opts lifecycleSeedOpts) map[models.TaskStatus]lifecycleSeed {
		rows := make(map[models.TaskStatus]lifecycleSeed, len(models.AllTaskStatuses))
		for _, s := range models.AllTaskStatuses {
			rows[s] = seedLifecycleTask(t, d, s, opts)
		}
		return rows
	}
	assertAll := func(writer string, target models.TaskStatus, rows map[models.TaskStatus]lifecycleSeed) {
		t.Helper()
		for _, s := range models.AllTaskStatuses {
			assertLifecycleOutcome(t, d, writer, target, s, rows[s].id)
		}
	}

	t.Run("db.ClaimNextPendingTask", func(t *testing.T) {
		reset()
		rows := seedAll(lifecycleSeedOpts{})
		claimed, err := d.ClaimNextPendingTask(ctx, "lifecycle-owner", 5*time.Minute)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if claimed == nil || claimed.ID != rows[models.TaskStatusPending].id {
			t.Fatalf("claim leased %v, want the pending row", claimed)
		}
		assertAll(models.TaskWriterClaim, models.TaskStatusLeased, rows)
		// Nothing else is claimable: the pending row was the only edge source.
		if again, err := d.ClaimNextPendingTask(ctx, "lifecycle-owner", 5*time.Minute); err != nil || again != nil {
			t.Fatalf("second claim = (%v, %v), want (nil, nil)", again, err)
		}
	})

	t.Run("db.UpdateTasksStatusBatch scheduled→pending", func(t *testing.T) {
		reset()
		due := time.Now().UTC().Add(-time.Minute)
		rows := seedAll(lifecycleSeedOpts{scheduledFor: &due})
		ids := make([]uuid.UUID, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.id)
		}
		// The from-status is the caller's compile-time constant (pinned to the
		// table by TestLifecycleSchedulerCallerPins); passing every id proves
		// the guard skips rows that left it.
		n, err := d.UpdateTasksStatusBatch(ctx, ids, models.TaskStatusScheduled, models.TaskStatusPending)
		if err != nil {
			t.Fatalf("batch: %v", err)
		}
		if n != 1 {
			t.Errorf("batch transitioned %d rows, want 1 (only the scheduled row)", n)
		}
		assertAll(models.TaskWriterScheduledPromotion, models.TaskStatusPending, rows)
	})

	for _, target := range []models.TaskStatus{models.TaskStatusPending, models.TaskStatusCancelled} {
		t.Run("db.SettleGatedTask →"+string(target), func(t *testing.T) {
			reset()
			sf := time.Now().UTC().Truncate(time.Microsecond)
			rows := seedAll(lifecycleSeedOpts{scheduledFor: &sf})
			for _, s := range models.AllTaskStatuses {
				// Same scheduled_for the rows were seeded with: the reschedule
				// compare passes, so status is the deciding clause.
				if _, err := d.SettleGatedTask(ctx, rows[s].id, &sf, target); err != nil {
					t.Fatalf("settle %s: %v", s, err)
				}
			}
			assertAll(models.TaskWriterSettleGatedTask, target, rows)
		})
	}

	t.Run("db.PauseTaskForQuestion", func(t *testing.T) {
		reset()
		// Every row gets a valid lease so status='running' is the deciding
		// clause of the guard, not lease possession.
		rows := seedAll(lifecycleSeedOpts{withLease: true})
		for _, s := range models.AllTaskStatuses {
			if _, err := d.PauseTaskForQuestion(ctx, rows[s].id, rows[s].owner, "which env?"); err != nil {
				t.Fatalf("pause %s: %v", s, err)
			}
		}
		assertAll(models.TaskWriterPauseForQuestion, models.TaskStatusPausedAwaitingInput, rows)
	})

	t.Run("db.PauseTaskForWake", func(t *testing.T) {
		reset()
		rows := seedAll(lifecycleSeedOpts{withLease: true})
		wakeAt := time.Now().UTC().Add(time.Hour)
		for _, s := range models.AllTaskStatuses {
			if _, err := d.PauseTaskForWake(ctx, rows[s].id, rows[s].owner, wakeAt, "", "sleeping"); err != nil {
				t.Fatalf("park %s: %v", s, err)
			}
		}
		assertAll(models.TaskWriterPauseForWake, models.TaskStatusPausedAwaitingWake, rows)
	})

	t.Run("db.ResumeTask", func(t *testing.T) {
		reset()
		rows := seedAll(lifecycleSeedOpts{})
		for _, s := range models.AllTaskStatuses {
			if _, err := d.ResumeTask(ctx, rows[s].id, "prod"); err != nil {
				t.Fatalf("resume %s: %v", s, err)
			}
		}
		assertAll(models.TaskWriterResume, models.TaskStatusPending, rows)
	})

	t.Run("db.WakeDueTasks", func(t *testing.T) {
		reset()
		// wake_at is stamped past on EVERY row so the sweep's status clause
		// is the deciding predicate.
		rows := seedAll(lifecycleSeedOpts{wakeAgo: 2 * time.Hour})
		if _, err := d.WakeDueTasks(ctx); err != nil {
			t.Fatalf("wake sweep: %v", err)
		}
		assertAll(models.TaskWriterWakeDue, models.TaskStatusPending, rows)
	})

	t.Run("db.WakeTaskByEvent", func(t *testing.T) {
		reset()
		rows := seedAll(lifecycleSeedOpts{wakeAgo: 2 * time.Hour}) // seeds wake_event_key too
		for _, s := range models.AllTaskStatuses {
			if _, err := d.WakeTaskByEvent(ctx, rows[s].id, "lifecycle-evt", "it fired"); err != nil {
				t.Fatalf("event wake %s: %v", s, err)
			}
		}
		assertAll(models.TaskWriterWakeByEvent, models.TaskStatusPending, rows)
	})

	t.Run("db.ExpirePausedTasks", func(t *testing.T) {
		reset()
		rows := seedAll(lifecycleSeedOpts{pausedAgo: 2 * time.Hour})
		if _, err := d.ExpirePausedTasks(ctx, 60); err != nil {
			t.Fatalf("paused expiry: %v", err)
		}
		assertAll(models.TaskWriterPausedExpiry, models.TaskStatusError, rows)
	})

	t.Run("db.ExpireStrandedWakeTasks", func(t *testing.T) {
		reset()
		// Both clocks past the 24h grace so status decides.
		rows := seedAll(lifecycleSeedOpts{pausedAgo: 25 * time.Hour, wakeAgo: 25 * time.Hour})
		if _, err := d.ExpireStrandedWakeTasks(ctx, StrandedWakeGrace); err != nil {
			t.Fatalf("stranded expiry: %v", err)
		}
		assertAll(models.TaskWriterStrandedWakeExpiry, models.TaskStatusError, rows)
	})

	t.Run("db.RecoverExpiredLeases requeue", func(t *testing.T) {
		reset()
		// Expired lease on EVERY row (status decides); attempt budget intact
		// → the requeue branch's edges.
		rows := seedAll(lifecycleSeedOpts{leaseExpired: true})
		requeued, deadLettered, err := d.RecoverExpiredLeases(ctx, time.Now().UTC())
		if err != nil {
			t.Fatalf("recover: %v", err)
		}
		if requeued != 2 || deadLettered != 0 {
			t.Errorf("recover = (%d requeued, %d dead-lettered), want (2, 0)", requeued, deadLettered)
		}
		assertAll(models.TaskWriterLeaseRecovery, models.TaskStatusPending, rows)
	})

	t.Run("db.RecoverExpiredLeases crash-loop dead-letter", func(t *testing.T) {
		reset()
		// Attempt budget spent → the quarantine branch's edges.
		rows := seedAll(lifecycleSeedOpts{leaseExpired: true, attemptSpent: true})
		requeued, deadLettered, err := d.RecoverExpiredLeases(ctx, time.Now().UTC())
		if err != nil {
			t.Fatalf("recover: %v", err)
		}
		if requeued != 0 || deadLettered != 2 {
			t.Errorf("recover = (%d requeued, %d dead-lettered), want (0, 2)", requeued, deadLettered)
		}
		assertAll(models.TaskWriterLeaseRecovery, models.TaskStatusDeadLettered, rows)
	})
}
