package db

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func setupTestDB(t *testing.T) *Database {
	connStr := os.Getenv("DATABASE_URL")
	if connStr == "" {
		t.Skip("DATABASE_URL not set, skipping integration tests")
	}

	db := New()
	if err := db.Init(connStr); err != nil {
		if isDatabaseUnavailable(err) {
			t.Skipf("Database unavailable, skipping integration tests: %v", err)
		}
		t.Fatalf("Failed to init db: %v", err)
	}

	ctx := context.Background()
	queries := []string{
		"DELETE FROM logs",
		"DELETE FROM tasks",
		"DELETE FROM nodes",
		"DELETE FROM users",
	}
	for _, q := range queries {
		if _, err := db.conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("Failed to clean up tables: %v", err)
		}
	}
	return db
}

func isDatabaseUnavailable(err error) bool {
	errMsg := err.Error()
	return strings.Contains(errMsg, "failed to connect to database") ||
		strings.Contains(errMsg, "connection refused") ||
		strings.Contains(errMsg, "no such host")
}

func TestNodeOperations(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	plainAPIKey := "test-key"
	node := &models.Node{ID: uuid.New(), Hostname: "test-host", Name: "test-node", APIKey: models.HashToken(plainAPIKey), OSType: "linux", Status: models.NodeStatusIdle, LastHeartbeat: time.Now().UTC(), RegisteredAt: time.Now().UTC()}

	if err := db.AddNode(ctx, node); err != nil {
		t.Fatalf("Failed to add node: %v", err)
	}
	retrieved, err := db.GetNode(ctx, node.ID)
	if err != nil {
		t.Fatalf("Failed to get node: %v", err)
	}
	if retrieved.Name != node.Name {
		t.Errorf("Expected name %s, got %s", node.Name, retrieved.Name)
	}

	node.Status = models.NodeStatusBusy
	if err := db.UpdateNode(ctx, node); err != nil {
		t.Fatalf("Failed to update node: %v", err)
	}
	retrieved, err = db.GetNode(ctx, node.ID)
	if err != nil {
		t.Fatalf("Failed to get updated node: %v", err)
	}
	if retrieved.Status != models.NodeStatusBusy {
		t.Errorf("Expected status %s, got %s", models.NodeStatusBusy, retrieved.Status)
	}

	retrieved, err = db.GetNodeByAPIKey(ctx, plainAPIKey)
	if err != nil {
		t.Fatalf("Failed to get node by api key: %v", err)
	}
	if retrieved.ID != node.ID {
		t.Errorf("Expected ID %s, got %s", node.ID, retrieved.ID)
	}

	nodes, err := db.GetAllNodes(ctx)
	if err != nil {
		t.Fatalf("Failed to get all nodes: %v", err)
	}
	if len(nodes) != 1 {
		t.Errorf("Expected 1 node, got %d", len(nodes))
	}

	deleted, err := db.RemoveNode(ctx, node.ID)
	if err != nil {
		t.Fatalf("Failed to remove node: %v", err)
	}
	if !deleted {
		t.Error("RemoveNode returned false, expected true")
	}
	if _, err = db.GetNode(ctx, node.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Expected ErrNoRows, got %v", err)
	}
}

func TestTaskOperations(t *testing.T) {
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

	running, err := db.GetRunningTasks(ctx)
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

// TestTaskRuntimeFlavorRoundTrip proves the per-task runtime-flavor override
// (#158, the Operations Center agent picker) persists: a task with an explicit
// flavor round-trips it, and the default is the empty string (use the bundle's
// global scheduled runtime).
func TestTaskRuntimeFlavorRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	picked := &models.Task{ID: uuid.New(), Prompt: "acp", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC(), RuntimeFlavor: "native-acp"}
	if err := db.AddTask(ctx, picked); err != nil {
		t.Fatalf("Failed to add task: %v", err)
	}
	gotPicked, err := db.GetTask(ctx, picked.ID)
	if err != nil {
		t.Fatalf("Failed to get task: %v", err)
	}
	if gotPicked.RuntimeFlavor != "native-acp" {
		t.Errorf("RuntimeFlavor = %q, want %q (explicit pick must persist)", gotPicked.RuntimeFlavor, "native-acp")
	}

	// The default is empty: no per-task override, fall back to the global runtime.
	def := &models.Task{ID: uuid.New(), Prompt: "default", Status: models.TaskStatusPending, CreatedAt: time.Now().UTC()}
	if err := db.AddTask(ctx, def); err != nil {
		t.Fatalf("Failed to add default task: %v", err)
	}
	gotDef, err := db.GetTask(ctx, def.ID)
	if err != nil {
		t.Fatalf("Failed to get default task: %v", err)
	}
	if gotDef.RuntimeFlavor != "" {
		t.Errorf("RuntimeFlavor = %q, want empty (default = global runtime)", gotDef.RuntimeFlavor)
	}
}

func TestTaskTimezoneRoundTrip(t *testing.T) {
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

func TestGetTasksCompletedToday(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	now := time.Now().UTC()
	taskToday := &models.Task{ID: uuid.New(), Prompt: "task completed today", Status: models.TaskStatusSuccess, Priority: 10, CreatedAt: now.Add(-2 * time.Hour), StartedAt: timePtr(now.Add(-1 * time.Hour)), CompletedAt: timePtr(now)}
	if err := db.AddTask(ctx, taskToday); err != nil {
		t.Fatalf("Failed to add taskToday: %v", err)
	}
	taskYesterday := &models.Task{ID: uuid.New(), Prompt: "task completed yesterday", Status: models.TaskStatusSuccess, Priority: 10, CreatedAt: now.AddDate(0, 0, -1).Add(-2 * time.Hour), StartedAt: timePtr(now.AddDate(0, 0, -1).Add(-1 * time.Hour)), CompletedAt: timePtr(now.AddDate(0, 0, -1))}
	if err := db.AddTask(ctx, taskYesterday); err != nil {
		t.Fatalf("Failed to add taskYesterday: %v", err)
	}
	taskPending := &models.Task{ID: uuid.New(), Prompt: "task pending", Status: models.TaskStatusPending, Priority: 10, CreatedAt: now}
	if err := db.AddTask(ctx, taskPending); err != nil {
		t.Fatalf("Failed to add taskPending: %v", err)
	}

	tasks, err := db.GetTasksCompletedToday(ctx)
	if err != nil {
		t.Fatalf("GetTasksCompletedToday failed: %v", err)
	}
	if len(tasks) != 1 {
		t.Fatalf("Expected 1 task completed today, got %d", len(tasks))
	}
	if tasks[0].ID != taskToday.ID {
		t.Errorf("Expected task ID %s, got %s", taskToday.ID, tasks[0].ID)
	}
}

func TestUserOperations(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	user := &models.User{ID: uuid.New(), Username: "testuser", PasswordHash: "hash", Role: "admin", Scopes: []string{"*"}, CreatedAt: time.Now().UTC()}
	if err := db.AddUser(ctx, user); err != nil {
		t.Fatalf("Failed to add user: %v", err)
	}
	retrieved, err := db.GetUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("Failed to get user: %v", err)
	}
	if retrieved.Username != user.Username {
		t.Errorf("Expected username %s, got %s", user.Username, retrieved.Username)
	}

	retrieved, err = db.GetUserByUsername(ctx, user.Username)
	if err != nil {
		t.Fatalf("Failed to get user by username: %v", err)
	}
	if retrieved.ID != user.ID {
		t.Errorf("Expected ID %s, got %s", user.ID, retrieved.ID)
	}

	token := "session_token_123"
	tokenHash := models.HashToken(token)
	user.SessionToken = &tokenHash
	if err := db.AddUser(ctx, user); err != nil {
		t.Fatalf("Failed to update user: %v", err)
	}
	retrieved, err = db.GetUserByToken(ctx, token)
	if err != nil {
		t.Fatalf("Failed to get user by token: %v", err)
	}
	if retrieved.ID != user.ID {
		t.Errorf("Expected ID %s, got %s", user.ID, retrieved.ID)
	}

	if err := db.UpdateUserRole(ctx, user.ID, "client"); err != nil {
		t.Fatalf("Failed to update user role: %v", err)
	}
	retrieved, err = db.GetUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("Failed to get user after role update: %v", err)
	}
	if retrieved.Role != "client" {
		t.Errorf("Expected role client, got %s", retrieved.Role)
	}

	if err := db.UpdateUserRole(ctx, uuid.New(), "admin"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Expected sql.ErrNoRows updating unknown user, got %v", err)
	}

	if err := db.RenameUser(ctx, user.ID, "renameduser"); err != nil {
		t.Fatalf("Failed to rename user: %v", err)
	}
	retrieved, err = db.GetUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("Failed to get user after rename: %v", err)
	}
	if retrieved.Username != "renameduser" {
		t.Errorf("Expected username renameduser, got %s", retrieved.Username)
	}
	if _, err := db.GetUserByUsername(ctx, "testuser"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Expected sql.ErrNoRows fetching old username, got %v", err)
	}
	if byName, err := db.GetUserByUsername(ctx, "renameduser"); err != nil || byName.ID != user.ID {
		t.Errorf("Expected new username to resolve to user %s, got %v (err %v)", user.ID, byName, err)
	}
	if err := db.RenameUser(ctx, uuid.New(), "ghost"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Expected sql.ErrNoRows renaming unknown user, got %v", err)
	}

	other := &models.User{ID: uuid.New(), Username: "otheruser", PasswordHash: "hash", Role: "client", CreatedAt: time.Now().UTC()}
	if err := db.AddUser(ctx, other); err != nil {
		t.Fatalf("Failed to add second user: %v", err)
	}
	if err := db.RenameUser(ctx, other.ID, "renameduser"); err == nil {
		t.Error("Expected error renaming onto a taken username, got nil")
	}
	if err := db.DeleteUser(ctx, other.ID); err != nil {
		t.Fatalf("Failed to delete second user: %v", err)
	}

	if err := db.DeleteUser(ctx, user.ID); err != nil {
		t.Fatalf("Failed to delete user: %v", err)
	}
	if _, err := db.GetUser(ctx, user.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Expected sql.ErrNoRows fetching deleted user, got %v", err)
	}
	if err := db.DeleteUser(ctx, user.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Expected sql.ErrNoRows deleting unknown user, got %v", err)
	}
}

// TestGetTasksFilteredCreatorVisibility verifies created_by filtering. (moc's
// scoped node-targeting visibility is gone with target_node_*; scoped users now
// see their own tasks plus all untargeted tasks, which is every task.)
func TestGetTasksFilteredCreatorVisibility(t *testing.T) {
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
	logs, err := db.GetAllLogs(ctx)
	if err != nil {
		t.Fatalf("Failed to get all logs: %v", err)
	}
	if len(logs) != 1 {
		t.Errorf("Expected 1 log entry, got %d", len(logs))
	}
}

func timePtr(t time.Time) *time.Time { return &t }

func TestGetNodesScopedPaginated(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	ctx := context.Background()

	nodes := []*models.Node{
		{ID: uuid.New(), Hostname: "host1", Name: "prod-web-1", APIKey: models.HashToken("key1"), OSType: "linux", Status: models.NodeStatusIdle, LastHeartbeat: time.Now().UTC(), RegisteredAt: time.Now().UTC().Add(-10 * time.Minute)},
		{ID: uuid.New(), Hostname: "host2", Name: "prod-db-1", APIKey: models.HashToken("key2"), OSType: "linux", Status: models.NodeStatusIdle, LastHeartbeat: time.Now().UTC(), RegisteredAt: time.Now().UTC().Add(-5 * time.Minute)},
		{ID: uuid.New(), Hostname: "host3", Name: "dev-web-1", APIKey: models.HashToken("key3"), OSType: "linux", Status: models.NodeStatusIdle, LastHeartbeat: time.Now().UTC(), RegisteredAt: time.Now().UTC()},
		{ID: uuid.New(), Hostname: "host4", Name: "prod-web-2", APIKey: models.HashToken("key4"), OSType: "linux", Status: models.NodeStatusIdle, LastHeartbeat: time.Now().UTC(), RegisteredAt: time.Now().UTC().Add(-1 * time.Minute)},
	}
	for _, node := range nodes {
		if err := db.AddNode(ctx, node); err != nil {
			t.Fatalf("Failed to add node: %v", err)
		}
	}

	tests := []struct {
		name          string
		scopes        []string
		limit, offset int
		expectedTotal int
		expectedNames []string
	}{
		{"Empty scopes", []string{}, 10, 0, 0, []string{}},
		{"Exact match", []string{"prod-web-1"}, 10, 0, 1, []string{"prod-web-1"}},
		{"Wildcard suffix", []string{"prod-*"}, 10, 0, 3, []string{"prod-web-2", "prod-db-1", "prod-web-1"}},
		{"Wildcard middle", []string{"prod-w?b-*"}, 10, 0, 2, []string{"prod-web-2", "prod-web-1"}},
		{"Global wildcard", []string{"*"}, 10, 0, 4, []string{"dev-web-1", "prod-web-2", "prod-db-1", "prod-web-1"}},
		{"Multiple scopes", []string{"dev-*", "prod-db-*"}, 10, 0, 2, []string{"dev-web-1", "prod-db-1"}},
		{"Pagination limit", []string{"*"}, 2, 0, 4, []string{"dev-web-1", "prod-web-2"}},
		{"Pagination offset", []string{"*"}, 2, 2, 4, []string{"prod-db-1", "prod-web-1"}},
		{"No match", []string{"staging-*"}, 10, 0, 0, []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resNodes, total, err := db.GetNodesScopedPaginated(ctx, tc.limit, tc.offset, tc.scopes)
			if err != nil {
				t.Fatalf("Unexpected error: %v", err)
			}
			if total != tc.expectedTotal {
				t.Errorf("Expected total %d, got %d", tc.expectedTotal, total)
			}
			if len(resNodes) != len(tc.expectedNames) {
				t.Errorf("Expected %d nodes, got %d", len(tc.expectedNames), len(resNodes))
			} else {
				for i, expectedName := range tc.expectedNames {
					if resNodes[i].Name != expectedName {
						t.Errorf("Expected node at index %d to be %s, got %s", i, expectedName, resNodes[i].Name)
					}
				}
			}
		})
	}
}

func TestUpdateTasksStatusBatch(t *testing.T) {
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

	// First claim: highest priority first (a).
	claimed1, err := db.ClaimNextPendingTask(ctx, "worker-1", time.Minute)
	if err != nil {
		t.Fatalf("Claim 1 failed: %v", err)
	}
	if claimed1 == nil || claimed1.ID != a.ID {
		t.Fatalf("Expected first claim to be task a, got %v", claimed1)
	}
	if claimed1.Status != models.TaskStatusLeased || claimed1.LeaseOwner == nil || *claimed1.LeaseOwner != "worker-1" {
		t.Fatalf("Expected leased to worker-1, got status=%s owner=%v", claimed1.Status, claimed1.LeaseOwner)
	}
	if claimed1.LeaseExpiresAt == nil || !claimed1.LeaseExpiresAt.After(time.Now().UTC()) {
		t.Fatalf("Expected a future lease expiry, got %v", claimed1.LeaseExpiresAt)
	}

	// Second claim gets the other task (a is no longer pending).
	claimed2, err := db.ClaimNextPendingTask(ctx, "worker-2", time.Minute)
	if err != nil {
		t.Fatalf("Claim 2 failed: %v", err)
	}
	if claimed2 == nil || claimed2.ID != b.ID {
		t.Fatalf("Expected second claim to be task b, got %v", claimed2)
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
	row := tx1.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE status = $1 ORDER BY priority DESC, created_at ASC LIMIT 1 FOR UPDATE SKIP LOCKED", string(models.TaskStatusPending))
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

// TestUpdateTasksModelBatch covers the bulk model re-assignment primitive (#44):
// the from_model filter (NULL-model tasks excluded), the affected count, and the
// fix that an empty fallback persists as NULL (not "") so scanTask reads nil.
func TestUpdateTasksModelBatch(t *testing.T) {
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

	// from_model filter: only the two "old/model" rows change; "" fallback → NULL.
	n, err := db.UpdateTasksModelBatch(ctx, "new/model", "", "old/model")
	if err != nil {
		t.Fatalf("UpdateTasksModelBatch: %v", err)
	}
	if n != 2 {
		t.Fatalf("from_model filter updated %d, want 2 (other + nil-model excluded)", n)
	}
	for _, id := range []uuid.UUID{old1.ID, old2.ID} {
		got, _ := db.GetTask(ctx, id)
		if got.Model == nil || *got.Model != "new/model" {
			t.Errorf("task %s model = %v, want new/model", id, got.Model)
		}
		if got.FallbackModel != nil {
			t.Errorf("task %s empty fallback must persist as NULL (nil), got %q", id, *got.FallbackModel)
		}
	}
	// Untouched rows.
	if got, _ := db.GetTask(ctx, other.ID); got.Model == nil || *got.Model != "other/model" {
		t.Errorf("other-model task should be untouched, got %v", got.Model)
	}
	if got, _ := db.GetTask(ctx, nilModel.ID); got.Model != nil {
		t.Errorf("nil-model task should be untouched (nil), got %v", got.Model)
	}

	// No from_model: all scheduled tasks re-assigned.
	all, err := db.UpdateTasksModelBatch(ctx, "final/model", "fb/final", "")
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
