package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched"
	scheddb "github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

func TestParseTaskID(t *testing.T) {
	if _, ok := parseTaskID(uuid.New().String()); !ok {
		t.Error("a valid UUID must parse")
	}
	for _, bad := range []string{"", "not-a-uuid", "1234"} {
		if _, ok := parseTaskID(bad); ok {
			t.Errorf("%q must not parse as a task id", bad)
		}
	}
}

// TestTaskMemoriesCLI_DB exercises the list/delete/clear dispatch + exit codes
// against the sched test DB (gated on DATABASE_URL, the sched-suite convention).
func TestTaskMemoriesCLI_DB(t *testing.T) {
	database := scheddb.New()
	if err := database.Init("", scheddb.DefaultPoolConfig()); err != nil {
		t.Skipf("sched DB unavailable: %v", err)
	}
	ctx := context.Background()
	conn, err := database.Conn().Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock(1)"); err != nil {
		conn.Close()
		t.Fatalf("lock: %v", err)
	}
	clean := func() {
		database.Conn().ExecContext(ctx, "DELETE FROM task_memories")
		database.Conn().ExecContext(ctx, "DELETE FROM tasks")
	}
	clean()
	t.Cleanup(func() {
		clean()
		conn.ExecContext(ctx, "SELECT pg_advisory_unlock(1)")
		conn.Close()
		database.Close()
	})

	st := storage.New()
	st.SetDatabase(database)
	task := &models.Task{ID: uuid.New(), Prompt: "p", Status: models.TaskStatusPending, Priority: 1, CreatedAt: time.Now().UTC()}
	if _, err := st.AddTaskWithContext(ctx, task); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	mem := sched.NewStore(database)
	_ = mem.UpsertTaskMemory(ctx, task.ID, "a", "1", 100, 4096)
	_ = mem.UpsertTaskMemory(ctx, task.ID, "b", "2", 100, 4096)

	id := task.ID.String()
	// list ok
	if code := taskMemories([]string{"list", id}); code != 0 {
		t.Errorf("list exit = %d, want 0", code)
	}
	// invalid id → usage error
	if code := taskMemories([]string{"list", "not-a-uuid"}); code != 1 {
		t.Errorf("list bad id exit = %d, want 1", code)
	}
	// delete one key
	if code := taskMemories([]string{"delete", id, "a"}); code != 0 {
		t.Errorf("delete exit = %d, want 0", code)
	}
	if _, err := mem.GetTaskMemory(ctx, task.ID, "a"); err == nil {
		t.Error("key a should be gone after delete")
	}
	// delete missing key → not found exit 2
	if code := taskMemories([]string{"delete", id, "nope"}); code != 2 {
		t.Errorf("delete missing exit = %d, want 2", code)
	}
	// clear all
	if code := taskMemories([]string{"clear", id}); code != 0 {
		t.Errorf("clear exit = %d, want 0", code)
	}
	if n, _ := mem.CountTaskMemories(ctx, task.ID); n != 0 {
		t.Errorf("after clear, count = %d, want 0", n)
	}
	// unknown subcommand → usage error
	if code := taskMemories([]string{"bogus"}); code != 1 {
		t.Errorf("unknown subcommand exit = %d, want 1", code)
	}
}
