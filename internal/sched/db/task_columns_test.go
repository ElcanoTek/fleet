package db

// Tests for the table-driven task-column registry (#1126). These replace
// TestTaskInsertColumnsCount: the manual taskInsertColumnsCount (whose drift
// broke every batch insert in #710) no longer exists — the statement text and
// the bound arguments now derive from the same taskColumnRegistry slice, so
// that drift class is structurally impossible. What still CAN drift is pinned
// here instead:
//
//   - the registry's internal structure (flags, exclusion reasons, functions),
//   - registry ↔ models.TaskExportRecord (the portable-definition set),
//   - registry ↔ the migrated database schema (a migration without a registry
//     row, or a registry row without a migration, fails loudly),
//   - and the actual write/read round trip through the derived statements.

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestTaskColumnRegistryConsistent is the DB-free structural gate: every
// registry row has a legal flag combination, every exclusion carries a
// reason, and the derived statements/argument builders agree with the sets
// they were built from — positional agreement by construction, verified once
// more here so a regression in the builders is a readable test failure.
func TestTaskColumnRegistryConsistent(t *testing.T) {
	if err := validateTaskColumnRegistry(); err != nil {
		t.Fatalf("registry invalid: %v", err)
	}

	// The SELECT list and the scan-destination list come from the same slice;
	// pin the counts and the one-dest-per-column property anyway so a
	// copy-paste error in a dest closure (two rows scanning into the same
	// buffer field) cannot silently shift every following column.
	if got := len(strings.Split(taskColumns, ", ")); got != len(taskReadSet) {
		t.Errorf("taskColumns lists %d columns but the read set has %d", got, len(taskReadSet))
	}
	var buf taskScanBuf
	seen := make(map[any]string, len(taskReadSet))
	for _, c := range taskReadSet {
		d := c.dest(&buf)
		if prev, dup := seen[d]; dup {
			t.Errorf("columns %q and %q scan into the same taskScanBuf field", prev, c.name)
		}
		seen[d] = c.name
	}

	// Insert statement: placeholder count == argument count == insert set.
	args := taskInsertArgs(&models.Task{Status: models.TaskStatusPending})
	if len(args) != len(taskInsertSet) {
		t.Errorf("taskInsertArgs returns %d values but the insert set has %d columns", len(args), len(taskInsertSet))
	}
	if got := strings.Count(taskInsertStatement, "$"); got != len(taskInsertSet) {
		t.Errorf("taskInsertStatement binds %d placeholders but the insert set has %d columns", got, len(taskInsertSet))
	}
	if got := len(strings.Split(taskInsertColumns, ", ")); got != len(taskInsertSet) {
		t.Errorf("taskInsertColumns lists %d columns but the insert set has %d", got, len(taskInsertSet))
	}

	// Upsert clause: exactly the upsert-flagged columns, nothing else.
	for i := range taskColumnRegistry {
		c := &taskColumnRegistry[i]
		clause := fmt.Sprintf("%s = EXCLUDED.%s", c.name, c.name)
		if c.upsert != strings.Contains(taskInsertOnConflict, clause) {
			t.Errorf("column %q: upsert flag %v disagrees with the derived ON CONFLICT clause", c.name, c.upsert)
		}
	}

	// Update statement: id as $1, one sequential placeholder per txUpdate
	// column, and one argument for each.
	if !strings.HasSuffix(taskUpdateStatement, "WHERE id = $1") {
		t.Errorf("taskUpdateStatement must key on id = $1, got: %s", taskUpdateStatement)
	}
	if got := strings.Count(taskUpdateStatement, "$"); got != len(taskTxUpdateSet)+1 {
		t.Errorf("taskUpdateStatement binds %d placeholders but the txUpdate set has %d columns (+1 for id)", got, len(taskTxUpdateSet))
	}
	if got := len(updateTaskArgs(&models.Task{Status: models.TaskStatusPending})); got != len(taskTxUpdateSet)+1 {
		t.Errorf("updateTaskArgs returns %d values but the txUpdate set has %d columns (+1 for id)", got, len(taskTxUpdateSet))
	}
	for i, c := range taskTxUpdateSet {
		want := fmt.Sprintf("%s = $%d", c.name, i+2)
		if !strings.Contains(taskUpdateStatement, want) {
			t.Errorf("taskUpdateStatement is missing %q — SET order must be registry order", want)
		}
	}
}

// TestTaskRegistryExportSetMatchesExportRecord pins the registry's export
// flag to models.TaskExportRecord: the record's JSON tags ARE the column
// names, so the two sets must match exactly. Together with the #1104
// completeness tests in models (record ↔ ExportRecordToTaskCreate ↔
// OverlayTaskDefinition), this closes the chain: a new column flagged export
// fails here until the record carries it, and the record cannot carry a
// field the overlay/create recipe drop.
func TestTaskRegistryExportSetMatchesExportRecord(t *testing.T) {
	recordCols := make(map[string]bool)
	rt := reflect.TypeOf(models.TaskExportRecord{})
	for i := 0; i < rt.NumField(); i++ {
		tag := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
		if tag == "" || tag == "-" {
			t.Fatalf("TaskExportRecord field %q has no usable json tag — the tag is the column-name mapping this test relies on", rt.Field(i).Name)
		}
		recordCols[tag] = true
	}

	exportCols := make(map[string]bool)
	for i := range taskColumnRegistry {
		c := &taskColumnRegistry[i]
		if c.export {
			exportCols[c.name] = true
		}
		if c.export && !recordCols[c.name] {
			t.Errorf("column %q is flagged export but models.TaskExportRecord has no field tagged %q — add it (and the #1104 completeness tests will walk it through the overlay/create recipe)", c.name, c.name)
		}
	}
	for tag := range recordCols {
		if !exportCols[tag] {
			t.Errorf("TaskExportRecord carries %q but the registry does not flag that column export — either flag it or drop the record field", tag)
		}
	}
}

// TestTaskRegistrySchemaAgreement is the schema↔registry drift gate: the
// migrated database and taskColumnRegistry must describe the same tasks-table
// columns. A migration that adds a column without a registry row fails here,
// as does a registry row whose column was never migrated (or was dropped).
func TestTaskRegistrySchemaAgreement(t *testing.T) {
	d := setupTestDB(t)
	defer d.Close()

	rows, err := d.conn.QueryContext(context.Background(),
		`SELECT column_name FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'tasks'`)
	if err != nil {
		t.Fatalf("information_schema query: %v", err)
	}
	defer rows.Close()

	schemaCols := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan column name: %v", err)
		}
		schemaCols[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate columns: %v", err)
	}
	if len(schemaCols) == 0 {
		t.Fatal("no tasks columns found — migrations did not run?")
	}

	registryCols := make(map[string]bool, len(taskColumnRegistry))
	for i := range taskColumnRegistry {
		registryCols[taskColumnRegistry[i].name] = true
	}

	for name := range schemaCols {
		if !registryCols[name] {
			t.Errorf("tasks column %q exists in the migrated schema but has no taskColumnRegistry row — a new column is one migration + one registry row (#1126)", name)
		}
	}
	for name := range registryCols {
		if !schemaCols[name] {
			t.Errorf("taskColumnRegistry row %q has no column in the migrated schema — missing migration, or the column was dropped without removing the row", name)
		}
	}
}

// TestTaskRegistryRoundTrip exercises the derived statements end to end:
// a fully-populated task goes in through the derived insert, comes back
// through the derived SELECT/scan, the txUpdate-only columns are written
// through the derived UPDATE, and the export set survives an
// export→import→re-export cycle unchanged. It also proves the exclusion
// doctrine in SQL, not just in flags, in BOTH directions: values seeded into
// excluded columns' Task fields do NOT persist through the insert, and
// non-NULL values seeded directly into those columns SURVIVE a repeat upsert
// and an UpdateTaskTx whose in-memory task carries zeroes for them — so
// flipping an exclusion flag (with its reason deleted) turns this test red
// instead of silently arming the stale-copy clobber the registry exists to
// prevent.
func TestTaskRegistryRoundTrip(t *testing.T) {
	d := setupTestDB(t)
	defer d.Close()
	ctx := context.Background()

	// Postgres stores microseconds; seed truncated UTC instants so equality
	// is exact rather than lossy.
	now := time.Now().UTC().Truncate(time.Microsecond)
	future := now.Add(45 * time.Minute)
	later := now.Add(90 * time.Minute)

	maxRetries := 3
	maxIter := 7
	expectedDur := 30
	thinking := 2048
	remaining := 5
	actualSecs := 61
	model := "openai/gpt-5"
	fallback := "anthropic/claude"
	serKey := "client:acme"

	task := models.NewTask(models.TaskCreate{
		Name:          "registry-roundtrip",
		Title:         "Registry round trip",
		Prompt:        "do the thing",
		Model:         &model,
		FallbackModel: &fallback,
		MaxIterations: &maxIter,
		MCPSelection:  models.MCPSelection{{Server: "github", Account: "acme"}},
		CredentialAllowlist: models.CredentialAllowlist{
			{Server: "github", Account: "acme"},
		},
		LoopConfig:                 &models.LoopConfig{MaxIterations: 4, ExitCondition: "done"},
		WorktreeConfig:             &models.WorktreeConfig{Enabled: true, BaseBranch: "dev", AutoCleanup: true},
		SandboxLimits:              &models.TaskSandboxLimits{MemoryMB: 2048, CPUs: 1.5, Pids: 256},
		OutputSchema:               json.RawMessage(`{"type":"object"}`),
		Priority:                   models.PriorityHigh,
		InstructionSelfImprove:     true,
		ScheduledFor:               &future,
		Recurrence:                 "0 12 * * *",
		RecurrenceUntil:            &later,
		RecurrenceRemaining:        &remaining,
		Files:                      []string{"stored/a.csv"},
		FileNames:                  []string{"a.csv"},
		Tags:                       []string{"ops", "roundtrip"},
		MaxRetries:                 &maxRetries,
		RetryPolicy:                &models.RetryPolicy{Backoff: "exponential", InitialDelaySeconds: 30},
		AllowNetwork:               true,
		CarryContext:               true,
		AllowEventTriggers:         true,
		AllowDelegation:            models.BoolPtr(true),
		Persona:                    "security-auditor",
		Description:                "round-trip fixture",
		Timezone:                   "America/New_York",
		TriggerType:                models.TriggerTypeCron,
		AllowTaskCreation:          true,
		AllowRecurringTaskCreation: true,
		CreatedByTaskID:            uuidPtr(uuid.New()),
		RunIf:                      &models.RunIf{Command: "true", TimeoutSeconds: 30},
		ExpectedDurationMinutes:    &expectedDur,
		ThinkingBudgetTokens:       &thinking,
		SLAWarnMultiplier:          1.75,
		SLAFailMultiplier:          2.5,
		SerializationKey:           &serKey,
	})

	// Runtime state the insert set carries: seed every remaining insertable
	// field non-zero so the round trip covers each column.
	task.CreatedAt = now
	task.CreatedBy = uuidPtr(uuid.New())
	task.CreatedByKeyID = strPtr("key-1")
	task.SourceTaskID = uuidPtr(uuid.New())
	task.AgentSessionID = strPtr("sess-1")
	task.WorkspacePath = strPtr("/data/fleet/workspaces/run-1")
	task.StartedAt = &now
	task.CompletedAt = &future
	task.ActualDurationSeconds = &actualSecs // pre-set: maybeComputeActualDuration must not overwrite
	task.Result = strPtr("result text")
	task.ErrorMessage = strPtr("error text")
	task.LeaseOwner = strPtr("scheduler-1")
	task.LeaseExpiresAt = &future
	task.AttemptCount = 2
	task.SLABreached = true
	task.OutputJSON = json.RawMessage(`{"ok":true}`)
	task.DeadLetteredAt = &now
	task.DeadLetterReason = strPtr("gave up")
	task.DeadLetterAttempts = 3
	task.SkipCount = 4
	task.LastSkipAt = &now
	task.LastSkipReason = strPtr("gate failed")

	// Fields whose columns are deliberately EXCLUDED from the insert: seed
	// them non-zero to prove the derived insert cannot write them.
	task.ErrorAnalysis = json.RawMessage(`{"category":"bug"}`)
	task.Artifacts = json.RawMessage(`[{"name":"a","path":"a","size":1}]`)
	task.PendingQuestion = "seeded question"
	task.PendingAnswer = "seeded answer"
	task.WakeAt = &future
	task.WakeEventKey = "seeded-event"
	task.WakeNote = "seeded note"
	task.WakeReason = "seeded reason"
	task.WakeCycles = 5
	task.PausedAt = &now

	if err := d.AddTask(ctx, task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	got, err := d.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}

	// Insert-excluded columns must come back zero: the doctrine ("a task
	// write can never clobber result-like/pause/wake state") held in SQL.
	insertExcluded := []string{
		"ErrorAnalysis", "Artifacts", "PendingQuestion", "PendingAnswer",
		"WakeAt", "WakeEventKey", "WakeNote", "WakeReason", "WakeCycles", "PausedAt",
	}
	assertTaskFieldsEqual(t, "insert round trip", task, got, insertExcluded)

	// The exclusion doctrine must hold against REAL values, not just NULLs:
	// seed every write-excluded column non-NULL directly (standing in for the
	// guarded writers — the park/pause transitions, SetTaskErrorAnalysis, the
	// anti-starvation sweep, the spawn/settle statements), then prove both
	// generic write paths leave the seeded values untouched even though the
	// in-memory task carries zeroes for them. Without non-NULL seeds,
	// "excluded from the statement" and "included but overwritten with the
	// same NULL" are indistinguishable — the exact flag-flip regression the
	// registry exists to make loud.
	seedTime := now.Add(10 * time.Minute)
	seededAnalysis := json.RawMessage(`{"category":"seeded"}`)
	seededArtifacts := json.RawMessage(`[{"name":"seeded","path":"seeded","size":9}]`)
	if _, err := d.conn.ExecContext(ctx, `
		UPDATE tasks SET
			error_analysis = $2, artifacts = $3,
			pending_question = 'seeded question', pending_answer = 'seeded answer',
			wake_at = $4, wake_event_key = 'seeded-event', wake_note = 'seeded note',
			wake_reason = 'seeded reason', wake_cycles = 7,
			paused_at = $4, effective_priority = 7, recurrence_spawned = TRUE
		WHERE id = $1`,
		task.ID, []byte(seededAnalysis), []byte(seededArtifacts), seedTime); err != nil {
		t.Fatalf("seed excluded columns: %v", err)
	}

	// Upsert survival: a repeat AddTask is the ON CONFLICT path every
	// UpdateTask routes through. got's in-memory copies of the seeded columns
	// are zero (and its EffectivePriority is the stale pre-sweep 25), so any
	// seeded value coming back changed means the upsert clobbered a column it
	// is excluded from.
	if err := d.AddTask(ctx, got); err != nil {
		t.Fatalf("AddTask upsert: %v", err)
	}
	afterUpsert, err := d.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask after upsert: %v", err)
	}
	expUpsert := *got
	expUpsert.ErrorAnalysis = seededAnalysis
	expUpsert.Artifacts = seededArtifacts
	expUpsert.PendingQuestion = "seeded question"
	expUpsert.PendingAnswer = "seeded answer"
	expUpsert.WakeAt = &seedTime
	expUpsert.WakeEventKey = "seeded-event"
	expUpsert.WakeNote = "seeded note"
	expUpsert.WakeReason = "seeded reason"
	expUpsert.WakeCycles = 7
	expUpsert.PausedAt = &seedTime
	expUpsert.EffectivePriority = 7 // the in-memory task carried 25; the upsert must not write it back
	assertTaskFieldsEqual(t, "upsert exclusion", &expUpsert, afterUpsert, nil)
	assertRecurrenceSpawned(t, d, task.ID, true, "upsert exclusion")

	// UpdateTaskTx: the txUpdate-writable trio (artifacts, pending_question,
	// pending_answer) must go through, while the txUpdate-excluded columns —
	// wake state, paused_at, error_analysis, effective_priority,
	// created_by_key_id, recurrence_spawned — keep their stored values even
	// though the in-memory copy zeroes every one of them.
	upd := *afterUpsert
	upd.Artifacts = json.RawMessage(`[{"name":"b.txt","path":"out/b.txt","size":42}]`)
	upd.PendingQuestion = "which env?"
	upd.PendingAnswer = "prod"
	upd.ErrorAnalysis = nil
	upd.WakeAt = nil
	upd.WakeEventKey = ""
	upd.WakeNote = ""
	upd.WakeReason = ""
	upd.WakeCycles = 0
	upd.PausedAt = nil
	upd.EffectivePriority = 0
	upd.CreatedByKeyID = nil
	tx, err := d.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if err := d.UpdateTaskTx(ctx, tx, &upd); err != nil {
		t.Fatalf("UpdateTaskTx: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	afterTx, err := d.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask after UpdateTaskTx: %v", err)
	}
	expTx := upd
	expTx.ErrorAnalysis = seededAnalysis
	expTx.WakeAt = &seedTime
	expTx.WakeEventKey = "seeded-event"
	expTx.WakeNote = "seeded note"
	expTx.WakeReason = "seeded reason"
	expTx.WakeCycles = 7
	expTx.PausedAt = &seedTime
	expTx.EffectivePriority = 7
	expTx.CreatedByKeyID = strPtr("key-1") // txUpdate-excluded (historical asymmetry): must keep the stored key
	assertTaskFieldsEqual(t, "txUpdate exclusion", &expTx, afterTx, nil)
	assertRecurrenceSpawned(t, d, task.ID, true, "txUpdate exclusion")

	// Export set round trip: export → TaskCreate → NewTask → re-export must
	// be lossless over every TaskExportRecord field.
	rec := models.TaskToExportRecord(afterTx)
	imported := models.NewTask(models.ExportRecordToTaskCreate(rec))
	rec2 := models.TaskToExportRecord(imported)
	rv, rv2 := reflect.ValueOf(rec), reflect.ValueOf(rec2)
	rt := reflect.TypeOf(rec)
	for i := 0; i < rt.NumField(); i++ {
		name := rt.Field(i).Name
		if !valuesEquivalent(rv.Field(i).Interface(), rv2.Field(i).Interface()) {
			t.Errorf("export round trip lost field %q: %#v -> %#v", name, rv.Field(i).Interface(), rv2.Field(i).Interface())
		}
	}
}

// assertTaskFieldsEqual compares every models.Task field between want and
// got, except that the fields named in zeroExpected must be ZERO in got
// (their columns are excluded from the write path under test) and the
// query-time display fields (never persisted) are always expected zero.
func assertTaskFieldsEqual(t *testing.T, phase string, want, got *models.Task, zeroExpected []string) {
	t.Helper()
	zero := map[string]bool{
		// Populated at query time by the handlers, never persisted.
		"NextRunAtLocal":    true,
		"CreatedByUsername": true,
	}
	for _, name := range zeroExpected {
		zero[name] = true
	}

	wv := reflect.ValueOf(*want)
	gv := reflect.ValueOf(*got)
	tt := reflect.TypeOf(*want)
	for i := 0; i < tt.NumField(); i++ {
		name := tt.Field(i).Name
		if zero[name] {
			if !gv.Field(i).IsZero() {
				t.Errorf("%s: field %q must not persist through this write path, but came back %#v", phase, name, gv.Field(i).Interface())
			}
			continue
		}
		if !valuesEquivalent(wv.Field(i).Interface(), gv.Field(i).Interface()) {
			t.Errorf("%s: field %q did not round-trip: %#v -> %#v", phase, name, wv.Field(i).Interface(), gv.Field(i).Interface())
		}
	}
}

// valuesEquivalent compares two field values, treating time instants by
// instant (Postgres returns a different wall-clock location) and raw JSON by
// value (JSONB canonicalizes formatting); everything else is DeepEqual.
func valuesEquivalent(a, b any) bool {
	switch av := a.(type) {
	case time.Time:
		return av.Equal(b.(time.Time))
	case *time.Time:
		bv := b.(*time.Time)
		if av == nil || bv == nil {
			return av == nil && bv == nil
		}
		return av.Equal(*bv)
	case json.RawMessage:
		bv := b.(json.RawMessage)
		if len(av) == 0 || len(bv) == 0 {
			return len(av) == 0 && len(bv) == 0
		}
		var aj, bj any
		if json.Unmarshal(av, &aj) != nil || json.Unmarshal(bv, &bj) != nil {
			return false
		}
		return reflect.DeepEqual(aj, bj)
	default:
		return reflect.DeepEqual(a, b)
	}
}

// assertRecurrenceSpawned reads the insert-only recurrence_spawned column
// (it has no Task field, so scanTask cannot surface it) and asserts the
// generic write path under test left it at the seeded value.
func assertRecurrenceSpawned(t *testing.T, d *Database, id uuid.UUID, want bool, phase string) {
	t.Helper()
	var got bool
	if err := d.conn.QueryRowContext(context.Background(),
		"SELECT recurrence_spawned FROM tasks WHERE id = $1", id).Scan(&got); err != nil {
		t.Fatalf("%s: read recurrence_spawned: %v", phase, err)
	}
	if got != want {
		t.Errorf("%s: recurrence_spawned = %v, want %v — the insert-only settlement marker must survive generic writes (#1116)", phase, got, want)
	}
}

func uuidPtr(u uuid.UUID) *uuid.UUID { return &u }

func strPtr(s string) *string { return &s }
