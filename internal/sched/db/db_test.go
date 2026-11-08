package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/identity"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/testdb"
	"github.com/ElcanoTek/fleet/internal/testdb/onedb"
)

// schedTemplate is the one-database template (internal/testdb/onedb): the
// whole database migrated by the one runner, chat, core and sched alike. This
// package cannot use internal/testdb/fleetdb (it imports this package), so
// its tests clone the template through onedb — the same one fleetdb clones.
var schedTemplate = onedb.Spec

// seedAccount creates an account in the identity table with the legacy
// Operations Center role (admin → Fleet admin; client/readonly → a viewer with
// ops_scope run/view).
func seedAccount(t testing.TB, db *Database, id uuid.UUID, email, legacyRole string) {
	t.Helper()
	acct := identity.NewAccount{ID: id, Email: email, Role: identity.RoleViewer}
	switch legacyRole {
	case "admin":
		acct.Role = identity.RoleAdmin
	case "client":
		acct.OpsScope = identity.OpsRun
	case "readonly":
		acct.OpsScope = identity.OpsView
	}
	if _, err := db.Identity().CreateAccount(context.Background(), acct); err != nil {
		t.Fatalf("seed account %s: %v", email, err)
	}
}

// setupTestDB opens the scheduler on a fresh, migrated database of the test's
// own, closed (and the database dropped) when the test ends. It skips only
// when FLEET_TEST_DATABASE_URL is unset (internal/testdb).
func setupTestDB(t testing.TB) *Database {
	t.Helper()
	db := New()
	pool := DefaultPoolConfig()
	pool.MaxOpenConns, pool.MaxIdleConns = testdb.PoolMaxOpen, testdb.PoolMaxIdle
	if err := db.Init(config.WithSchema(testdb.New(t, schedTemplate), config.SchedSchema), pool); err != nil {
		t.Fatalf("Failed to init db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestTaskOperations(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	task := &models.Task{ID: uuid.New(), Prompt: "do something", Status: models.TaskStatusPending, Priority: 10, CreatedAt: time.Now().UTC()}
	if err := db.AddTask(ctx, task); err != nil {
		t.Fatalf("Failed to add task: %v", err)
	}
	retrieved, err := db.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("Failed to get task: %v", err)
	}
	if retrieved.Prompt != task.Prompt {
		t.Errorf("Expected prompt %s, got %s", task.Prompt, retrieved.Prompt)
	}

	pending, err := db.GetPendingTasks(ctx)
	if err != nil {
		t.Fatalf("Failed to get pending tasks: %v", err)
	}
	if len(pending) != 1 {
		t.Errorf("Expected 1 pending task, got %d", len(pending))
	}

	task.Status = models.TaskStatusRunning
	task.StartedAt = timePtr(time.Now().UTC())
	if err := db.UpdateTask(ctx, task); err != nil {
		t.Fatalf("Failed to update task: %v", err)
	}

	running, err := db.GetTasksByStatus(ctx, models.TaskStatusRunning)
	if err != nil {
		t.Fatalf("Failed to get running tasks: %v", err)
	}
	if len(running) != 1 {
		t.Errorf("Expected 1 running task, got %d", len(running))
	}

	oldTask := &models.Task{ID: uuid.New(), Prompt: "old task", Status: models.TaskStatusSuccess, CompletedAt: timePtr(time.Now().UTC().AddDate(0, 0, -31)), CreatedAt: time.Now().UTC().AddDate(0, 0, -32)}
	if err := db.AddTask(ctx, oldTask); err != nil {
		t.Fatalf("Failed to add old task: %v", err)
	}
	deletedCount, err := db.DeleteOldHistory(ctx, 30)
	if err != nil {
		t.Fatalf("Failed to delete old history: %v", err)
	}
	if deletedCount != 1 {
		t.Errorf("Expected 1 deleted task, got %d", deletedCount)
	}
}

// TestTaskMCPSelectionRoundTrip verifies the new mcp_selection column round-trips
// through AddTask/GetTask (the schema change that replaced target_node_*).
func TestTaskMCPSelectionRoundTrip(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	sel := models.MCPSelection{{Server: "xandr", Account: "client_a"}, {Server: "magnite"}}
	task := &models.Task{ID: uuid.New(), Prompt: "sel task", Status: models.TaskStatusPending, Priority: 1, CreatedAt: time.Now().UTC(), MCPSelection: sel}
	if err := db.AddTask(ctx, task); err != nil {
		t.Fatalf("Failed to add task: %v", err)
	}
	got, err := db.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("Failed to get task: %v", err)
	}
	if len(got.MCPSelection) != 2 {
		t.Fatalf("Expected 2 mcp choices, got %d (%+v)", len(got.MCPSelection), got.MCPSelection)
	}
	if got.MCPSelection[0].Server != "xandr" || got.MCPSelection[0].Account != "client_a" {
		t.Errorf("choice[0] = %+v, want {xandr client_a}", got.MCPSelection[0])
	}
	if got.MCPSelection[1].Server != "magnite" || got.MCPSelection[1].Account != "" {
		t.Errorf("choice[1] = %+v, want {magnite }", got.MCPSelection[1])
	}

	// A task with no selection round-trips as an empty (non-nil) slice.
	bare := &models.Task{ID: uuid.New(), Prompt: "bare", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC()}
	if err := db.AddTask(ctx, bare); err != nil {
		t.Fatalf("Failed to add bare task: %v", err)
	}
	gotBare, err := db.GetTask(ctx, bare.ID)
	if err != nil {
		t.Fatalf("Failed to get bare task: %v", err)
	}
	if len(gotBare.MCPSelection) != 0 {
		t.Errorf("Expected empty selection, got %+v", gotBare.MCPSelection)
	}
}

// TestTaskAllowNetworkRoundTrip proves the per-task network-egress toggle (#145)
// persists: an opt-in task round-trips AllowNetwork=true, and the default seals
// to false (the --network=none posture).
func TestTaskAllowNetworkRoundTrip(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	open := &models.Task{ID: uuid.New(), Prompt: "egress", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC(), AllowNetwork: true}
	if err := db.AddTask(ctx, open); err != nil {
		t.Fatalf("Failed to add task: %v", err)
	}
	gotOpen, err := db.GetTask(ctx, open.ID)
	if err != nil {
		t.Fatalf("Failed to get task: %v", err)
	}
	if !gotOpen.AllowNetwork {
		t.Errorf("AllowNetwork = false, want true (opt-in must persist)")
	}

	// The default is the sealed posture: AllowNetwork=false survives the round-trip.
	sealed := &models.Task{ID: uuid.New(), Prompt: "sealed", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC()}
	if err := db.AddTask(ctx, sealed); err != nil {
		t.Fatalf("Failed to add sealed task: %v", err)
	}
	gotSealed, err := db.GetTask(ctx, sealed.ID)
	if err != nil {
		t.Fatalf("Failed to get sealed task: %v", err)
	}
	if gotSealed.AllowNetwork {
		t.Errorf("AllowNetwork = true, want false (default must be sealed)")
	}
}

// TestTaskAllowDelegationRoundTrip proves the per-task delegation opt-in (#264)
// persists: an opted-in task round-trips AllowDelegation=true, and the default is
// false (delegation off) so an existing task's behaviour is unchanged.
func TestTaskAllowDelegationRoundTrip(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	on := &models.Task{ID: uuid.New(), Prompt: "delegate", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC(), AllowDelegation: true}
	if err := db.AddTask(ctx, on); err != nil {
		t.Fatalf("Failed to add task: %v", err)
	}
	gotOn, err := db.GetTask(ctx, on.ID)
	if err != nil {
		t.Fatalf("Failed to get task: %v", err)
	}
	if !gotOn.AllowDelegation {
		t.Errorf("AllowDelegation = false, want true (opt-in must persist)")
	}

	// The default is off: AllowDelegation=false survives the round-trip, so an
	// existing task (no field) keeps delegation disabled.
	off := &models.Task{ID: uuid.New(), Prompt: "no delegate", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC()}
	if err := db.AddTask(ctx, off); err != nil {
		t.Fatalf("Failed to add task: %v", err)
	}
	gotOff, err := db.GetTask(ctx, off.ID)
	if err != nil {
		t.Fatalf("Failed to get task: %v", err)
	}
	if gotOff.AllowDelegation {
		t.Errorf("AllowDelegation = true, want false (default must be off)")
	}
}

func TestTaskTimezoneRoundTrip(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	picked := &models.Task{ID: uuid.New(), Prompt: "tz", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC(), Timezone: "America/New_York"}
	if err := db.AddTask(ctx, picked); err != nil {
		t.Fatalf("Failed to add task: %v", err)
	}
	gotPicked, err := db.GetTask(ctx, picked.ID)
	if err != nil {
		t.Fatalf("Failed to get task: %v", err)
	}
	if gotPicked.Timezone != "America/New_York" {
		t.Errorf("Timezone = %q, want %q (explicit timezone must persist)", gotPicked.Timezone, "America/New_York")
	}

	// A task constructed without a timezone (bypassing NewTask) defends to UTC so
	// the NOT NULL column never sees an empty string and reads back as "UTC".
	def := &models.Task{ID: uuid.New(), Prompt: "default", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC()}
	if err := db.AddTask(ctx, def); err != nil {
		t.Fatalf("Failed to add default task: %v", err)
	}
	gotDef, err := db.GetTask(ctx, def.ID)
	if err != nil {
		t.Fatalf("Failed to get default task: %v", err)
	}
	if gotDef.Timezone != "UTC" {
		t.Errorf("Timezone = %q, want %q (missing timezone defaults to UTC)", gotDef.Timezone, "UTC")
	}
}

func TestTaskCreatedByKeyIDRoundTrip(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	keyID := "key_abc123"
	withKey := &models.Task{ID: uuid.New(), Prompt: "k", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC(), CreatedByKeyID: &keyID}
	if err := db.AddTask(ctx, withKey); err != nil {
		t.Fatalf("Failed to add task: %v", err)
	}
	got, err := db.GetTask(ctx, withKey.ID)
	if err != nil {
		t.Fatalf("Failed to get task: %v", err)
	}
	if got.CreatedByKeyID == nil || *got.CreatedByKeyID != keyID {
		t.Errorf("CreatedByKeyID = %v, want %q", got.CreatedByKeyID, keyID)
	}

	// A task created without an API key leaves it NULL.
	noKey := &models.Task{ID: uuid.New(), Prompt: "nokey", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC()}
	if err := db.AddTask(ctx, noKey); err != nil {
		t.Fatalf("Failed to add no-key task: %v", err)
	}
	gotNoKey, err := db.GetTask(ctx, noKey.ID)
	if err != nil {
		t.Fatalf("Failed to get no-key task: %v", err)
	}
	if gotNoKey.CreatedByKeyID != nil {
		t.Errorf("CreatedByKeyID = %v, want nil", *gotNoKey.CreatedByKeyID)
	}
}

// TestUserOperations pins the scheduler's view of the one identity table
// (#2049 M4): an Operations Center user is an account that is enabled and is
// an admin or has a scope; everything else answers sql.ErrNoRows, as the old
// "enabled row" lookup did. Role is the legacy name the row grants.
func TestUserOperations(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	admin, client, viewer, none := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	seedAccount(t, db, admin, "admin@x.test", "admin")
	seedAccount(t, db, client, "client@x.test", "client")
	seedAccount(t, db, viewer, "viewer@x.test", "readonly")
	seedAccount(t, db, none, "chatonly@x.test", "")

	for _, c := range []struct {
		id    uuid.UUID
		email string
		role  string
	}{{admin, "admin@x.test", "admin"}, {client, "client@x.test", "client"}, {viewer, "viewer@x.test", "readonly"}} {
		byID, err := db.GetUser(ctx, c.id)
		if err != nil || byID.Username != c.email || byID.Role != c.role {
			t.Errorf("GetUser(%s) = %+v, %v; want %s/%s", c.id, byID, err, c.email, c.role)
		}
		byName, err := db.GetUserByUsername(ctx, "  "+strings.ToUpper(c.email)+" ")
		if err != nil || byName.ID != c.id || byName.Role != c.role {
			t.Errorf("GetUserByUsername(%s) = %+v, %v", c.email, byName, err)
		}
	}
	// A chat-only account (scope none) is not an Operations Center user.
	if _, err := db.GetUserByUsername(ctx, "chatonly@x.test"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("chat-only lookup err = %v, want sql.ErrNoRows", err)
	}
	if _, err := db.GetUserByUsername(ctx, "nobody@x.test"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("unknown lookup err = %v, want sql.ErrNoRows", err)
	}
	// A disabled account (central Auth revoked it) has no access, but keeps
	// its UUID, role and scope for a re-grant.
	if _, err := db.conn.ExecContext(ctx, `UPDATE core.users SET enabled = FALSE WHERE email = 'client@x.test'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetUserByUsername(ctx, "client@x.test"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("disabled lookup err = %v, want sql.ErrNoRows", err)
	}
	if acct, err := db.GetIdentity(ctx, "client@x.test"); err != nil || acct.ID != client || acct.OpsScope != identity.OpsRun {
		t.Errorf("disabled row not retained: %+v %v", acct, err)
	}

	// ListUsers / CountUsers: the accounts holding access in their stored
	// role and scope, the disabled one included (with its stored role).
	list, err := db.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, u := range list {
		got[u.Username] = u.Role
	}
	want := map[string]string{"admin@x.test": "admin", "client@x.test": "client", "viewer@x.test": "readonly"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("ListUsers = %v, want %v", got, want)
	}
	if n, err := db.CountUsers(ctx); err != nil || n != 3 {
		t.Errorf("CountUsers = %d, %v; want 3", n, err)
	}
	// Created By resolves any account, access or not.
	names, err := db.GetUsersByIDs(ctx, []uuid.UUID{none, client, uuid.New()})
	if err != nil || len(names) != 2 || names[none] != "chatonly@x.test" || names[client] != "client@x.test" {
		t.Errorf("GetUsersByIDs = %v, %v", names, err)
	}
}

// TestGetTasksFilteredCreatorVisibility verifies created_by filtering. (moc's
// scoped node-targeting visibility is gone with target_node_*; scoped users now
// see their own tasks plus all untargeted tasks, which is every task.)
func TestGetTasksFilteredCreatorVisibility(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	userID := uuid.New()
	otherID := uuid.New()

	addTask := func(prompt string, createdBy *uuid.UUID) {
		t.Helper()
		task := &models.Task{ID: uuid.New(), Prompt: prompt, Status: models.TaskStatusPending, CreatedBy: createdBy, CreatedAt: time.Now().UTC()}
		if err := db.AddTask(ctx, task); err != nil {
			t.Fatalf("Failed to add task %q: %v", prompt, err)
		}
	}
	addTask("mine-1", &userID)
	addTask("mine-2", &userID)
	addTask("theirs", &otherID)
	addTask("untargeted", nil)

	tasks, total, err := db.GetTasksFiltered(ctx, TaskFilter{CreatedBy: &userID}, 100, 0)
	if err != nil {
		t.Fatalf("GetTasksFiltered failed: %v", err)
	}
	if total != 2 {
		t.Errorf("Expected total 2 for created_by filter, got %d", total)
	}
	for _, task := range tasks {
		if task.CreatedBy == nil || *task.CreatedBy != userID {
			t.Errorf("Unexpected task in created_by filter: %q", task.Prompt)
		}
	}
}

func TestLogOperations(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	taskID := uuid.New()
	session := &models.LogSession{ID: "sess-1", Messages: []models.LogMessage{{CreatedAt: time.Now().Unix(), Content: "log message"}}}
	if err := db.AddLog(ctx, taskID, session); err != nil {
		t.Fatalf("Failed to add log: %v", err)
	}
	retrieved, err := db.GetLog(ctx, taskID)
	if err != nil {
		t.Fatalf("Failed to get log: %v", err)
	}
	if retrieved.ID != session.ID {
		t.Errorf("Expected session ID %s, got %s", session.ID, retrieved.ID)
	}
	logs, err := collectLogs(ctx, db)
	if err != nil {
		t.Fatalf("Failed to get all logs: %v", err)
	}
	if len(logs) != 1 {
		t.Errorf("Expected 1 log entry, got %d", len(logs))
	}
}

// TestRunLogHistory covers the copy-on-overwrite per-attempt history: the
// first write archives nothing, every re-write archives the transcript it
// supersedes, the per-task cap trims oldest-first, and entries are scoped to
// their task id.
func TestRunLogHistory(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	taskID := uuid.New()
	write := func(id string) {
		t.Helper()
		if err := db.AddLog(ctx, taskID, &models.LogSession{ID: id}); err != nil {
			t.Fatalf("AddLog(%s): %v", id, err)
		}
	}

	write("attempt-1")
	entries, err := db.ListRunLogHistory(ctx, taskID)
	if err != nil {
		t.Fatalf("ListRunLogHistory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("first write must archive nothing, got %d entries", len(entries))
	}

	write("attempt-2")
	write("attempt-3")
	entries, err = db.ListRunLogHistory(ctx, taskID)
	if err != nil {
		t.Fatalf("ListRunLogHistory: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 superseded transcripts, got %d", len(entries))
	}
	// Newest-first: entries[0] is the transcript attempt-3 superseded.
	got, err := db.GetRunLogEntry(ctx, taskID, entries[0].ID)
	if err != nil {
		t.Fatalf("GetRunLogEntry: %v", err)
	}
	if got.ID != "attempt-2" {
		t.Errorf("newest history entry: want attempt-2, got %s", got.ID)
	}
	got, err = db.GetRunLogEntry(ctx, taskID, entries[1].ID)
	if err != nil {
		t.Fatalf("GetRunLogEntry: %v", err)
	}
	if got.ID != "attempt-1" {
		t.Errorf("oldest history entry: want attempt-1, got %s", got.ID)
	}

	// Task scoping: another task cannot read this task's entry by id.
	if _, err := db.GetRunLogEntry(ctx, uuid.New(), entries[0].ID); err == nil {
		t.Error("GetRunLogEntry must not return an entry for a different task id")
	}
	// The latest transcript stays in logs, untouched.
	latest, err := db.GetLog(ctx, taskID)
	if err != nil || latest.ID != "attempt-3" {
		t.Errorf("GetLog: want attempt-3, got %v (err %v)", latest, err)
	}

	// Cap: drive well past runLogHistoryKeep and confirm the trim held, oldest
	// dropped first.
	for i := 0; i < runLogHistoryKeep+5; i++ {
		write(fmt.Sprintf("attempt-%d", i+4))
	}
	entries, err = db.ListRunLogHistory(ctx, taskID)
	if err != nil {
		t.Fatalf("ListRunLogHistory: %v", err)
	}
	if len(entries) != runLogHistoryKeep {
		t.Fatalf("expected history capped at %d, got %d", runLogHistoryKeep, len(entries))
	}
	got, err = db.GetRunLogEntry(ctx, taskID, entries[len(entries)-1].ID)
	if err != nil {
		t.Fatalf("GetRunLogEntry(oldest kept): %v", err)
	}
	if got.ID == "attempt-1" || got.ID == "attempt-2" {
		t.Errorf("trim must drop oldest first; oldest kept is %s", got.ID)
	}
}

// TestRunLogHistoryPrunedWithTask confirms both retention paths delete a
// task's run_logs alongside its logs row.
func TestRunLogHistoryPrunedWithTask(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	old := time.Now().UTC().AddDate(0, 0, -60)
	task := &models.Task{ID: uuid.New(), Prompt: "history prune", Status: models.TaskStatusSuccess,
		CreatedAt: old, CompletedAt: timePtr(old)}
	if err := db.AddTask(ctx, task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	if err := db.AddLog(ctx, task.ID, &models.LogSession{ID: "a1"}); err != nil {
		t.Fatalf("AddLog: %v", err)
	}
	if err := db.AddLog(ctx, task.ID, &models.LogSession{ID: "a2"}); err != nil {
		t.Fatalf("AddLog: %v", err)
	}

	if _, err := db.DeleteOldHistory(ctx, 30); err != nil {
		t.Fatalf("DeleteOldHistory: %v", err)
	}
	entries, err := db.ListRunLogHistory(ctx, task.ID)
	if err != nil {
		t.Fatalf("ListRunLogHistory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("DeleteOldHistory left %d run_logs rows behind", len(entries))
	}
}

// collectLogs gathers every stored log session through ForEachLog, keyed by task.
func collectLogs(ctx context.Context, db *Database) (map[uuid.UUID]*models.LogSession, error) {
	logs := make(map[uuid.UUID]*models.LogSession)
	err := db.ForEachLog(ctx, func(id uuid.UUID, session *models.LogSession) error {
		logs[id] = session
		return nil
	})
	return logs, err
}

func timePtr(t time.Time) *time.Time { return &t }

// bigSession builds a log session whose JSON is large and repetitive so gzip
// measurably shrinks it, exercising the real archival code path.
func bigSession(id string) *models.LogSession {
	msgs := make([]models.LogMessage, 0, 40)
	for i := 0; i < 40; i++ {
		msgs = append(msgs, models.LogMessage{
			CreatedAt: time.Now().Unix(),
			Content:   strings.Repeat("the agent reasoned at length about the task and called tools. ", 20),
		})
	}
	return &models.LogSession{ID: id, Title: "archival test", Messages: msgs}
}

// fetchLogRow reads the raw archival columns for a task so tests can assert the
// on-disk shape (live vs. archived) directly.
func fetchLogRow(t *testing.T, db *Database, taskID uuid.UUID) (sessionData *string, gzLen int, codec string) {
	t.Helper()
	var sd *string
	var gz []byte
	var c sql.NullString
	err := db.conn.QueryRowContext(context.Background(),
		"SELECT session_data, session_data_gz, session_compression FROM logs WHERE task_id = $1", taskID).
		Scan(&sd, &gz, &c)
	if err != nil {
		t.Fatalf("fetch log row: %v", err)
	}
	return sd, len(gz), c.String
}

// TestArchiveOldLogsRoundtripAndAgeThreshold covers the compress->store->read
// roundtrip AND the age-threshold selection (#272): only terminal tasks older
// than the threshold are archived; a recent terminal task and a non-terminal
// task are left live; GetLog returns identical content for an archived row.
func TestArchiveOldLogsRoundtripAndAgeThreshold(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// 1) old + terminal -> should be archived.
	oldTask := &models.Task{ID: uuid.New(), Prompt: "old terminal", Status: models.TaskStatusSuccess,
		CreatedAt: time.Now().UTC().AddDate(0, 0, -40), CompletedAt: timePtr(time.Now().UTC().AddDate(0, 0, -40))}
	// 2) recent + terminal -> too new, left live.
	recentTask := &models.Task{ID: uuid.New(), Prompt: "recent terminal", Status: models.TaskStatusError,
		CreatedAt: time.Now().UTC().AddDate(0, 0, -2), CompletedAt: timePtr(time.Now().UTC().AddDate(0, 0, -2))}
	// 3) old but non-terminal -> never archived.
	runningTask := &models.Task{ID: uuid.New(), Prompt: "old running", Status: models.TaskStatusRunning,
		CreatedAt: time.Now().UTC().AddDate(0, 0, -40)}
	for _, tk := range []*models.Task{oldTask, recentTask, runningTask} {
		if err := db.AddTask(ctx, tk); err != nil {
			t.Fatalf("add task: %v", err)
		}
	}

	oldSession := bigSession("old-sess")
	for _, tc := range []struct {
		id      uuid.UUID
		session *models.LogSession
	}{
		{oldTask.ID, oldSession},
		{recentTask.ID, bigSession("recent-sess")},
		{runningTask.ID, bigSession("running-sess")},
	} {
		if err := db.AddLog(ctx, tc.id, tc.session); err != nil {
			t.Fatalf("add log: %v", err)
		}
	}

	// Threshold 30d: only the 40d-old terminal task qualifies.
	n, bytesSaved, err := db.ArchiveOldLogs(ctx, 30)
	if err != nil {
		t.Fatalf("ArchiveOldLogs: %v", err)
	}
	if n != 1 {
		t.Fatalf("archived %d rows, want 1 (age-threshold selection)", n)
	}
	if bytesSaved <= 0 {
		t.Fatalf("bytesSaved = %d, want > 0 (gzip should shrink the payload)", bytesSaved)
	}

	// Old task is now archived: session_data NULL, gz populated, codec = gzip.
	sd, gzLen, codec := fetchLogRow(t, db, oldTask.ID)
	if sd != nil {
		t.Fatal("archived row still has live session_data")
	}
	if gzLen == 0 {
		t.Fatal("archived row has empty session_data_gz")
	}
	if codec != compressionGzip {
		t.Fatalf("codec = %q, want %q", codec, compressionGzip)
	}

	// Recent + running rows untouched: still live, no codec.
	for _, id := range []uuid.UUID{recentTask.ID, runningTask.ID} {
		sd, _, codec := fetchLogRow(t, db, id)
		if sd == nil || codec != "" {
			t.Fatalf("task %s was archived but should have been left live", id)
		}
	}

	// Roundtrip: GetLog transparently inflates the archived payload.
	got, err := db.GetLog(ctx, oldTask.ID)
	if err != nil {
		t.Fatalf("GetLog(archived): %v", err)
	}
	if got.ID != oldSession.ID || len(got.Messages) != len(oldSession.Messages) {
		t.Fatalf("archived roundtrip mismatch: got id=%q msgs=%d, want id=%q msgs=%d",
			got.ID, len(got.Messages), oldSession.ID, len(oldSession.Messages))
	}
	if got.Messages[0].Content != oldSession.Messages[0].Content {
		t.Fatal("archived message content mismatch after inflate")
	}

	// ForEachLog also inflates transparently and still visits all three.
	all, err := collectLogs(ctx, db)
	if err != nil {
		t.Fatalf("ForEachLog: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("ForEachLog visited %d, want 3", len(all))
	}
	if all[oldTask.ID].ID != oldSession.ID {
		t.Fatal("ForEachLog did not inflate the archived row correctly")
	}

	// Idempotent: a second sweep archives nothing (already-archived skipped).
	n2, _, err := db.ArchiveOldLogs(ctx, 30)
	if err != nil {
		t.Fatalf("second ArchiveOldLogs: %v", err)
	}
	if n2 != 0 {
		t.Fatalf("second sweep archived %d rows, want 0 (idempotent)", n2)
	}
}

// TestArchiveOldLogsEncryptedRoundtrip covers the encrypted archive path: with a
// 32-byte key configured, the stored bytes are encrypted and GetLog still
// transparently inflates+decrypts to the original session.
func TestArchiveOldLogsEncryptedRoundtrip(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	key := make([]byte, aesKeyLen)
	for i := range key {
		key[i] = byte(i + 1)
	}
	db.SetLogArchiveKey(key)

	task := &models.Task{ID: uuid.New(), Prompt: "enc", Status: models.TaskStatusSuccess,
		CreatedAt: time.Now().UTC().AddDate(0, 0, -40), CompletedAt: timePtr(time.Now().UTC().AddDate(0, 0, -40))}
	if err := db.AddTask(ctx, task); err != nil {
		t.Fatalf("add task: %v", err)
	}
	session := bigSession("enc-sess")
	if err := db.AddLog(ctx, task.ID, session); err != nil {
		t.Fatalf("add log: %v", err)
	}

	n, _, err := db.ArchiveOldLogs(ctx, 30)
	if err != nil {
		t.Fatalf("ArchiveOldLogs: %v", err)
	}
	if n != 1 {
		t.Fatalf("archived %d, want 1", n)
	}
	_, _, codec := fetchLogRow(t, db, task.ID)
	if codec != compressionGzipAES {
		t.Fatalf("codec = %q, want %q", codec, compressionGzipAES)
	}

	got, err := db.GetLog(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetLog(encrypted archive): %v", err)
	}
	if got.ID != session.ID || len(got.Messages) != len(session.Messages) {
		t.Fatal("encrypted archive roundtrip mismatch")
	}

	// Without the key, reading the encrypted archive must fail closed.
	db.SetLogArchiveKey(nil)
	if _, err := db.GetLog(ctx, task.ID); err == nil {
		t.Fatal("GetLog returned an encrypted archive without a key; want error")
	}
}

// TestArchiveOldLogsDisabled confirms days<=0 is inert (no scan, no writes).
func TestArchiveOldLogsDisabled(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	n, saved, err := db.ArchiveOldLogs(ctx, 0)
	if err != nil || n != 0 || saved != 0 {
		t.Fatalf("ArchiveOldLogs(0) = (%d,%d,%v), want (0,0,nil)", n, saved, err)
	}
	n, saved, err = db.ArchiveOldLogs(ctx, -5)
	if err != nil || n != 0 || saved != 0 {
		t.Fatalf("ArchiveOldLogs(-5) = (%d,%d,%v), want (0,0,nil)", n, saved, err)
	}
}

// TestAddLogResetsArchiveColumns confirms re-writing a previously archived row
// returns it to the live, uncompressed state (AddLog clears the archive columns).
func TestAddLogResetsArchiveColumns(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	task := &models.Task{ID: uuid.New(), Prompt: "rewrite", Status: models.TaskStatusSuccess,
		CreatedAt: time.Now().UTC().AddDate(0, 0, -40), CompletedAt: timePtr(time.Now().UTC().AddDate(0, 0, -40))}
	if err := db.AddTask(ctx, task); err != nil {
		t.Fatalf("add task: %v", err)
	}
	if err := db.AddLog(ctx, task.ID, bigSession("v1")); err != nil {
		t.Fatalf("add log v1: %v", err)
	}
	if _, _, err := db.ArchiveOldLogs(ctx, 30); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, _, codec := fetchLogRow(t, db, task.ID); codec == "" {
		t.Fatal("row should be archived before rewrite")
	}

	if err := db.AddLog(ctx, task.ID, bigSession("v2")); err != nil {
		t.Fatalf("add log v2: %v", err)
	}
	sd, gzLen, codec := fetchLogRow(t, db, task.ID)
	if sd == nil || gzLen != 0 || codec != "" {
		t.Fatalf("rewrite did not reset archive columns: sd=%v gzLen=%d codec=%q", sd != nil, gzLen, codec)
	}
	got, err := db.GetLog(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetLog after rewrite: %v", err)
	}
	if got.ID != "v2" {
		t.Fatalf("GetLog returned id %q, want v2", got.ID)
	}
}

func TestUpdateTasksStatusBatch(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	task1 := &models.Task{ID: uuid.New(), Prompt: "task 1", Status: models.TaskStatusPending, Priority: 10, CreatedAt: time.Now().UTC()}
	task2 := &models.Task{ID: uuid.New(), Prompt: "task 2", Status: models.TaskStatusPending, Priority: 10, CreatedAt: time.Now().UTC()}
	task3 := &models.Task{ID: uuid.New(), Prompt: "task 3", Status: models.TaskStatusPending, Priority: 10, CreatedAt: time.Now().UTC()}
	for _, tk := range []*models.Task{task1, task2, task3} {
		if err := db.AddTask(ctx, tk); err != nil {
			t.Fatalf("Failed to add task: %v", err)
		}
	}

	if n, err := db.UpdateTasksStatusBatch(ctx, []uuid.UUID{}, models.TaskStatusPending, models.TaskStatusRunning); err != nil || n != 0 {
		t.Fatalf("UpdateTasksStatusBatch with empty list = (%d, %v), want (0, nil)", n, err)
	}

	taskIDs := []uuid.UUID{task1.ID, task2.ID}
	if n, err := db.UpdateTasksStatusBatch(ctx, taskIDs, models.TaskStatusPending, models.TaskStatusRunning); err != nil {
		t.Fatalf("UpdateTasksStatusBatch failed: %v", err)
	} else if n != 2 {
		t.Fatalf("expected 2 tasks transitioned, got %d", n)
	}
	if n, err := db.UpdateTasksStatusBatch(ctx, taskIDs, models.TaskStatusPending, models.TaskStatusRunning); err != nil {
		t.Fatalf("UpdateTasksStatusBatch (guard) failed: %v", err)
	} else if n != 0 {
		t.Fatalf("expected 0 tasks transitioned on second pass, got %d", n)
	}

	r1, _ := db.GetTask(ctx, task1.ID)
	if r1.Status != models.TaskStatusRunning {
		t.Errorf("Expected task 1 status %s, got %s", models.TaskStatusRunning, r1.Status)
	}
	r3, _ := db.GetTask(ctx, task3.ID)
	if r3.Status != models.TaskStatusPending {
		t.Errorf("Expected task 3 status %s, got %s", models.TaskStatusPending, r3.Status)
	}
}

// TestClaimNextPendingTask verifies FOR UPDATE SKIP LOCKED claims exactly one
// task and leases it to the given owner; a second concurrent claim in a parallel
// transaction must skip the locked row and pick a different task (never double-
// lease the same one).
func TestClaimNextPendingTask(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	// Two pending tasks; two concurrent claims must each get a distinct one.
	a := &models.Task{ID: uuid.New(), Prompt: "a", Status: models.TaskStatusPending, Priority: 10, CreatedAt: time.Now().UTC()}
	b := &models.Task{ID: uuid.New(), Prompt: "b", Status: models.TaskStatusPending, Priority: 5, CreatedAt: time.Now().UTC()}
	for _, tk := range []*models.Task{a, b} {
		if err := db.AddTask(ctx, tk); err != nil {
			t.Fatalf("Failed to add task: %v", err)
		}
	}

	// First claim: lowest integer = most urgent first (#230), so b (priority 5)
	// beats a (priority 10). EffectivePriority is unset on these directly-built
	// tasks, so the insert falls back to Priority — exactly the ordering tested.
	claimed1, err := db.ClaimNextPendingTask(ctx, "worker-1", time.Minute)
	if err != nil {
		t.Fatalf("Claim 1 failed: %v", err)
	}
	if claimed1 == nil || claimed1.ID != b.ID {
		t.Fatalf("Expected first claim to be task b (priority 5, more urgent), got %v", claimed1)
	}
	if claimed1.Status != models.TaskStatusLeased || claimed1.LeaseOwner == nil || *claimed1.LeaseOwner != "worker-1" {
		t.Fatalf("Expected leased to worker-1, got status=%s owner=%v", claimed1.Status, claimed1.LeaseOwner)
	}
	if claimed1.LeaseExpiresAt == nil || !claimed1.LeaseExpiresAt.After(time.Now().UTC()) {
		t.Fatalf("Expected a future lease expiry, got %v", claimed1.LeaseExpiresAt)
	}

	// Second claim gets the other task (b is no longer pending).
	claimed2, err := db.ClaimNextPendingTask(ctx, "worker-2", time.Minute)
	if err != nil {
		t.Fatalf("Claim 2 failed: %v", err)
	}
	if claimed2 == nil || claimed2.ID != a.ID {
		t.Fatalf("Expected second claim to be task a (priority 10), got %v", claimed2)
	}

	// Third claim: nothing left.
	claimed3, err := db.ClaimNextPendingTask(ctx, "worker-3", time.Minute)
	if err != nil {
		t.Fatalf("Claim 3 failed: %v", err)
	}
	if claimed3 != nil {
		t.Fatalf("Expected no task left, got %v", claimed3)
	}

	// Concurrent SKIP LOCKED: with one pending task and two simultaneous open
	// transactions, exactly one claims it and the other skips to nil.
	c := &models.Task{ID: uuid.New(), Prompt: "c", Status: models.TaskStatusPending, Priority: 1, CreatedAt: time.Now().UTC()}
	if err := db.AddTask(ctx, c); err != nil {
		t.Fatalf("Failed to add task c: %v", err)
	}

	tx1, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx1: %v", err)
	}
	defer tx1.Rollback()
	row := tx1.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE status = $1 ORDER BY effective_priority ASC, created_at ASC LIMIT 1 FOR UPDATE SKIP LOCKED", string(models.TaskStatusPending))
	locked, err := db.scanTask(row)
	if err != nil {
		t.Fatalf("tx1 lock claim failed: %v", err)
	}
	if locked.ID != c.ID {
		t.Fatalf("tx1 expected to lock task c, got %s", locked.ID)
	}

	// While tx1 holds the row lock, a fresh claim must skip it and find nothing.
	skipped, err := db.ClaimNextPendingTask(ctx, "worker-x", time.Minute)
	if err != nil {
		t.Fatalf("concurrent claim failed: %v", err)
	}
	if skipped != nil {
		t.Fatalf("Expected concurrent claim to skip the locked row (nil), got %s", skipped.ID)
	}
	tx1.Rollback()
}

// TestUpdateTasksModelBatch covers the bulk model re-assignment primitive (#44)
// and the #1120 omit-vs-clear contract: nil fallback leaves existing values,
// empty string clears them to NULL.
func TestUpdateTasksModelBatch(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()
	sp := func(s string) *string { return &s }

	old1 := &models.Task{ID: uuid.New(), Prompt: "a", Status: models.TaskStatusScheduled, Model: sp("old/model"), FallbackModel: sp("fb/old"), CreatedAt: time.Now().UTC()}
	old2 := &models.Task{ID: uuid.New(), Prompt: "b", Status: models.TaskStatusScheduled, Model: sp("old/model"), CreatedAt: time.Now().UTC()}
	other := &models.Task{ID: uuid.New(), Prompt: "c", Status: models.TaskStatusScheduled, Model: sp("other/model"), CreatedAt: time.Now().UTC()}
	nilModel := &models.Task{ID: uuid.New(), Prompt: "d", Status: models.TaskStatusScheduled, CreatedAt: time.Now().UTC()} // Model == nil
	for _, tk := range []*models.Task{old1, old2, other, nilModel} {
		if err := db.AddTask(ctx, tk); err != nil {
			t.Fatalf("AddTask: %v", err)
		}
	}

	// from_model filter + omitted fallback: model changes, fallback_model kept.
	n, err := db.UpdateTasksModelBatch(ctx, "new/model", nil, "old/model")
	if err != nil {
		t.Fatalf("UpdateTasksModelBatch: %v", err)
	}
	if n != 2 {
		t.Fatalf("from_model filter updated %d, want 2 (other + nil-model excluded)", n)
	}
	got1, _ := db.GetTask(ctx, old1.ID)
	if got1.Model == nil || *got1.Model != "new/model" {
		t.Errorf("task %s model = %v, want new/model", old1.ID, got1.Model)
	}
	if got1.FallbackModel == nil || *got1.FallbackModel != "fb/old" {
		t.Errorf("omitted fallback must leave fb/old in place, got %v", got1.FallbackModel)
	}
	got2, _ := db.GetTask(ctx, old2.ID)
	if got2.FallbackModel != nil {
		t.Errorf("task with no fallback should stay nil, got %q", *got2.FallbackModel)
	}

	// Explicit empty fallback → NULL. The first call re-pinned these rows to
	// new/model, so filter on that — and assert the count so a filter that
	// matches nothing cannot fake a pass.
	n, err = db.UpdateTasksModelBatch(ctx, "new/model", sp(""), "new/model")
	if err != nil {
		t.Fatalf("UpdateTasksModelBatch clear: %v", err)
	}
	if n != 2 {
		t.Fatalf("clear updated %d, want 2", n)
	}
	if got, _ := db.GetTask(ctx, old1.ID); got.FallbackModel != nil {
		t.Errorf("explicit empty fallback must persist as NULL, got %q", *got.FallbackModel)
	}

	// Untouched rows.
	if got, _ := db.GetTask(ctx, other.ID); got.Model == nil || *got.Model != "other/model" {
		t.Errorf("other-model task should be untouched, got %v", got.Model)
	}
	if got, _ := db.GetTask(ctx, nilModel.ID); got.Model != nil {
		t.Errorf("nil-model task should be untouched (nil), got %v", got.Model)
	}

	// No from_model + explicit fallback: all scheduled tasks re-assigned.
	all, err := db.UpdateTasksModelBatch(ctx, "final/model", sp("fb/final"), "")
	if err != nil {
		t.Fatalf("UpdateTasksModelBatch (all): %v", err)
	}
	if all != 4 {
		t.Fatalf("unfiltered update touched %d, want 4 (all scheduled)", all)
	}
	if got, _ := db.GetTask(ctx, nilModel.ID); got.Model == nil || *got.Model != "final/model" || got.FallbackModel == nil || *got.FallbackModel != "fb/final" {
		t.Errorf("unfiltered update should set model+fallback on every scheduled task; got model=%v fb=%v", got.Model, got.FallbackModel)
	}
}

// TestCarryContextRoundTrip pins #504's carry_context threading through the
// full insert/scan path (the fragile 58-column seam).
func TestCarryContextRoundTrip(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	on := &models.Task{ID: uuid.New(), Prompt: "daily digest", Recurrence: "0 9 * * *", CarryContext: true, Status: models.TaskStatusScheduled, CreatedAt: time.Now().UTC()}
	off := &models.Task{ID: uuid.New(), Prompt: "one-off", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC()}
	for _, tk := range []*models.Task{on, off} {
		if err := db.AddTask(ctx, tk); err != nil {
			t.Fatalf("AddTask: %v", err)
		}
	}
	gotOn, _ := db.GetTask(ctx, on.ID)
	gotOff, _ := db.GetTask(ctx, off.ID)
	if !gotOn.CarryContext {
		t.Fatal("carry_context=true did not round-trip")
	}
	if gotOff.CarryContext {
		t.Fatal("carry_context should default false")
	}
	// Batch insert path preserves it too.
	b := &models.Task{ID: uuid.New(), Prompt: "batch", Recurrence: "0 * * * *", CarryContext: true, Status: models.TaskStatusScheduled, CreatedAt: time.Now().UTC()}
	if err := db.AddTaskBatch(ctx, []*models.Task{b}); err != nil {
		t.Fatalf("AddTaskBatch: %v", err)
	}
	gotB, _ := db.GetTask(ctx, b.ID)
	if !gotB.CarryContext {
		t.Fatal("carry_context lost through the batch insert")
	}
}

func TestTaskFileNamesRoundTrip(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	defer db.Close()
	task := &models.Task{
		ID: uuid.New(), Prompt: "use domains.csv", Status: models.TaskStatusPending,
		CreatedAt: time.Now().UTC(), Files: []string{"domains_deadbeef.csv"}, FileNames: []string{"domains.csv"},
	}
	if err := db.AddTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.FileNames) != 1 || got.FileNames[0] != "domains.csv" {
		t.Fatalf("file_names = %v", got.FileNames)
	}
}
