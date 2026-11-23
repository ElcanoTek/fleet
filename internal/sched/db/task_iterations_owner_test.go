package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestIterationFirstWriteFollowsAConcurrentOwnerChange: an iteration first
// written while an owner reassignment of its task is in flight (the task row
// locked and rewritten, its iterations re-attributed, not yet committed — the
// shape of storage.SetTaskOwner) waits for it and records the new owner, so
// the reassignment cannot leave one iteration under the owner it replaced.
func TestIterationFirstWriteFollowsAConcurrentOwnerChange(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	ctx := context.Background()
	oldOwner, newOwner := uuid.New(), uuid.New()
	task := &models.Task{ID: uuid.New(), Prompt: "running", Status: models.TaskStatusRunning,
		CreatedAt: time.Now().UTC(), CreatedBy: &oldOwner}
	if err := db.AddTask(ctx, task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		`SELECT 1 FROM tasks WHERE id = $1 FOR UPDATE`,
		`UPDATE tasks SET created_by = $2 WHERE id = $1`,
		`UPDATE task_iterations SET created_by = $2 WHERE task_id = $1`,
	} {
		args := []any{task.ID}
		if q != `SELECT 1 FROM tasks WHERE id = $1 FOR UPDATE` {
			args = append(args, newOwner)
		}
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	it := &models.TaskIteration{ID: uuid.New(), TaskID: task.ID, IterationNumber: 1,
		StartedAt: time.Now().UTC(), CostUSD: 1, Status: models.IterationStatusPassed}
	done := make(chan error, 1)
	go func() { done <- db.AddTaskIteration(ctx, it) }()

	// The write must be waiting on the task row before the reassignment
	// commits; one that finishes first copied the owner being replaced.
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("the iteration was written (err %v) while the owner change was still in flight, instead of waiting for it", err)
		default:
		}
		var waiting int
		if err := db.conn.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the iteration write never waited on the task row")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("AddTaskIteration: %v", err)
	}
	var got uuid.UUID
	if err := db.conn.QueryRowContext(ctx, `SELECT created_by FROM task_iterations WHERE id = $1`, it.ID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != newOwner {
		t.Fatalf("iteration created_by = %s, want the new owner %s (old owner %s)", got, newOwner, oldOwner)
	}
}

// TestIterationOfADeletedTaskKeepsTheRunnersAttribution: a task stopped and
// deleted while its last run is still finishing has no row when that run's
// iteration is first written. The iteration then records the attribution the
// runner carries for the task, so its spend still counts for the creator, the
// key and the model. While the task row exists, the row's values win over the
// runner's copy.
func TestIterationOfADeletedTaskKeepsTheRunnersAttribution(t *testing.T) {
	t.Parallel()

	db := setupTestDB(t)
	ctx := context.Background()
	owner := uuid.New()
	seedAccount(t, db, owner, "rita@example.com", "client")
	key, model := "key-rita", "model-r"
	ran := time.Now().UTC().Add(-time.Minute)

	gone := &models.Task{ID: uuid.New(), Prompt: "stopped then deleted", Status: models.TaskStatusCancelled,
		CreatedAt: ran, CreatedBy: &owner, CreatedByKeyID: &key, Model: &model}
	if err := db.AddTask(ctx, gone); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	if deleted, err := db.DeleteTask(ctx, gone.ID); err != nil || !deleted {
		t.Fatalf("DeleteTask = %v, %v", deleted, err)
	}
	if err := db.AddTaskIteration(ctx, &models.TaskIteration{ID: uuid.New(), TaskID: gone.ID, IterationNumber: 1,
		StartedAt: ran, CostUSD: 4, PromptTokens: 40, Status: models.IterationStatusFailed,
		CreatedBy: &owner, CreatedByKeyID: &key, Model: &model}); err != nil {
		t.Fatalf("AddTaskIteration: %v", err)
	}

	from, to := ran.Add(-time.Minute), time.Now().UTC().Add(time.Minute)
	for groupBy, want := range map[string]string{"user": "rita@example.com", "key": key, "model": model} {
		rows, err := db.TaskUsage(ctx, from, to, groupBy)
		if err != nil {
			t.Fatalf("TaskUsage(%s): %v", groupBy, err)
		}
		if b := bucketMap(rows)[want]; !almostEqual(b.TaskCostUSD, 4) || b.TaskIterations != 1 {
			t.Errorf("group_by=%s bucket %q = %+v, want the deleted task's run (all: %+v)", groupBy, want, b, rows)
		}
	}

	// A live task row wins over a stale runner copy.
	other, otherKey, otherModel := uuid.New(), "key-stale", "model-stale"
	live := &models.Task{ID: uuid.New(), Prompt: "still here", Status: models.TaskStatusRunning,
		CreatedAt: ran, CreatedBy: &owner, CreatedByKeyID: &key, Model: &model}
	if err := db.AddTask(ctx, live); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	it := &models.TaskIteration{ID: uuid.New(), TaskID: live.ID, IterationNumber: 1, StartedAt: ran, CostUSD: 1,
		Status: models.IterationStatusPassed, CreatedBy: &other, CreatedByKeyID: &otherKey, Model: &otherModel}
	if err := db.AddTaskIteration(ctx, it); err != nil {
		t.Fatalf("AddTaskIteration: %v", err)
	}
	var gotOwner uuid.UUID
	var gotKey, gotModel string
	if err := db.conn.QueryRowContext(ctx, `SELECT created_by, created_by_key_id, model FROM task_iterations WHERE id = $1`, it.ID).
		Scan(&gotOwner, &gotKey, &gotModel); err != nil {
		t.Fatal(err)
	}
	if gotOwner != owner || gotKey != key || gotModel != model {
		t.Errorf("live task's iteration = %s/%s/%s, want the task row's %s/%s/%s", gotOwner, gotKey, gotModel, owner, key, model)
	}

	// A present task row wins even where its values are NULL: a task with no
	// creator, key or pinned model records none, whatever the runner's copy
	// says.
	bare := &models.Task{ID: uuid.New(), Prompt: "no attribution", Status: models.TaskStatusRunning, CreatedAt: ran}
	if err := db.AddTask(ctx, bare); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	bareIt := &models.TaskIteration{ID: uuid.New(), TaskID: bare.ID, IterationNumber: 1, StartedAt: ran, CostUSD: 1,
		Status: models.IterationStatusPassed, CreatedBy: &other, CreatedByKeyID: &otherKey, Model: &otherModel}
	if err := db.AddTaskIteration(ctx, bareIt); err != nil {
		t.Fatalf("AddTaskIteration: %v", err)
	}
	var nullOwner, nullKey, nullModel bool
	if err := db.conn.QueryRowContext(ctx, `SELECT created_by IS NULL, created_by_key_id IS NULL, model IS NULL
		FROM task_iterations WHERE id = $1`, bareIt.ID).Scan(&nullOwner, &nullKey, &nullModel); err != nil {
		t.Fatal(err)
	}
	if !nullOwner || !nullKey || !nullModel {
		t.Errorf("iteration of a task with no attribution: created_by/created_by_key_id/model NULL = %v/%v/%v, want all NULL (the runner's copy must not fill them)",
			nullOwner, nullKey, nullModel)
	}
}
