package sched

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// newSelfImproveStore mirrors newNotesStore but also cleans the self-improvement
// tables and exposes the underlying *db.Database so a test can create the tasks
// that task_memories FK-references. It serializes on the same advisory lock the
// notes tests use (the sched suite runs -p 1).
func newSelfImproveStore(t *testing.T) (*Store, *db.Database) {
	t.Helper()
	database := db.New()
	if err := database.Init("", db.DefaultPoolConfig()); err != nil {
		t.Skipf("Skipping self-improve test because DB init failed: %v", err)
	}
	ctx := context.Background()
	conn, err := database.Conn().Conn(ctx)
	if err != nil {
		t.Fatalf("get DB connection for lock: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock(1)"); err != nil {
		conn.Close()
		t.Fatalf("acquire test lock: %v", err)
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
	return NewStore(database), database
}

func makeTask(t *testing.T, database *db.Database) uuid.UUID {
	t.Helper()
	task := &models.Task{ID: uuid.New(), Prompt: "do x", Status: models.TaskStatusPending, Priority: 10, CreatedAt: time.Now().UTC()}
	if err := database.AddTask(context.Background(), task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	return task.ID
}

func TestTaskMemory_UpsertGetList(t *testing.T) {
	s, database := newSelfImproveStore(t)
	ctx := context.Background()
	taskID := makeTask(t, database)

	if err := s.UpsertTaskMemory(ctx, taskID, "price", "42", 100, 4096); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	// Overwrite same key (no eviction, value replaced).
	if err := s.UpsertTaskMemory(ctx, taskID, "price", "43", 100, 4096); err != nil {
		t.Fatalf("Upsert overwrite: %v", err)
	}
	if err := s.UpsertTaskMemory(ctx, taskID, "seen", `["a"]`, 100, 4096); err != nil {
		t.Fatalf("Upsert second: %v", err)
	}

	got, err := s.GetTaskMemory(ctx, taskID, "price")
	if err != nil || got != "43" {
		t.Fatalf("GetTaskMemory price = %q, %v; want 43", got, err)
	}
	if _, err := s.GetTaskMemory(ctx, taskID, "missing"); !errors.Is(err, ErrTaskMemoryNotFound) {
		t.Fatalf("expected ErrTaskMemoryNotFound, got %v", err)
	}

	mems, err := s.ListTaskMemories(ctx, taskID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(mems) != 2 {
		t.Fatalf("expected 2 keys, got %d: %+v", len(mems), mems)
	}
	n, err := s.CountTaskMemories(ctx, taskID)
	if err != nil || n != 2 {
		t.Fatalf("Count = %d, %v; want 2", n, err)
	}
}

func TestTaskMemory_ValueCapAndKeyValidation(t *testing.T) {
	s, database := newSelfImproveStore(t)
	ctx := context.Background()
	taskID := makeTask(t, database)

	// Oversized value is a hard error.
	big := strings.Repeat("x", 4097)
	if err := s.UpsertTaskMemory(ctx, taskID, "k", big, 100, 4096); !errors.Is(err, ErrTaskMemoryValueTooLarge) {
		t.Fatalf("expected ErrTaskMemoryValueTooLarge, got %v", err)
	}
	// 0 cap disables the value check.
	if err := s.UpsertTaskMemory(ctx, taskID, "k", big, 100, 0); err != nil {
		t.Fatalf("value cap disabled should accept, got %v", err)
	}
	// Bad keys.
	for _, bad := range []string{"", strings.Repeat("k", 129)} {
		if err := s.UpsertTaskMemory(ctx, taskID, bad, "v", 100, 4096); !errors.Is(err, ErrTaskMemoryKeyInvalid) {
			t.Errorf("key %q: expected ErrTaskMemoryKeyInvalid, got %v", bad, err)
		}
	}
}

func TestTaskMemory_LRUEviction(t *testing.T) {
	s, database := newSelfImproveStore(t)
	ctx := context.Background()
	taskID := makeTask(t, database)

	// Insert 3 keys, then stamp their updated_at to distinct, increasing values
	// directly (updated_at is unix-second resolution, so a wall-clock sleep would
	// be needed to disambiguate them otherwise — this keeps the test deterministic
	// and fast). k1 is oldest, k3 newest.
	for i, k := range []string{"k1", "k2", "k3"} {
		if err := s.UpsertTaskMemory(ctx, taskID, k, "v", 3, 4096); err != nil {
			t.Fatalf("Upsert %s: %v", k, err)
		}
		if _, err := database.Conn().ExecContext(ctx,
			"UPDATE task_memories SET updated_at = $1 WHERE task_id = $2 AND key = $3",
			int64(1000+i), taskID, k); err != nil {
			t.Fatalf("stamp updated_at for %s: %v", k, err)
		}
	}

	// Inserting a 4th key at maxKeys=3 evicts the oldest by updated_at (k1).
	if err := s.UpsertTaskMemory(ctx, taskID, "k4", "v", 3, 4096); err != nil {
		t.Fatalf("Upsert k4: %v", err)
	}
	n, _ := s.CountTaskMemories(ctx, taskID)
	if n != 3 {
		t.Fatalf("expected cap of 3 keys after eviction, got %d", n)
	}
	if _, err := s.GetTaskMemory(ctx, taskID, "k1"); !errors.Is(err, ErrTaskMemoryNotFound) {
		t.Errorf("oldest key k1 should have been evicted, got %v", err)
	}
	for _, k := range []string{"k2", "k3", "k4"} {
		if _, err := s.GetTaskMemory(ctx, taskID, k); err != nil {
			t.Errorf("key %s should still be present, got %v", k, err)
		}
	}
}

func TestTaskMemory_DeleteAndClear(t *testing.T) {
	s, database := newSelfImproveStore(t)
	ctx := context.Background()
	taskID := makeTask(t, database)

	_ = s.UpsertTaskMemory(ctx, taskID, "a", "1", 100, 4096)
	_ = s.UpsertTaskMemory(ctx, taskID, "b", "2", 100, 4096)

	if err := s.DeleteTaskMemory(ctx, taskID, "a"); err != nil {
		t.Fatalf("DeleteTaskMemory: %v", err)
	}
	if err := s.DeleteTaskMemory(ctx, taskID, "a"); !errors.Is(err, ErrTaskMemoryNotFound) {
		t.Fatalf("re-delete should be not-found, got %v", err)
	}
	cleared, err := s.DeleteAllTaskMemories(ctx, taskID)
	if err != nil || cleared != 1 {
		t.Fatalf("DeleteAll = %d, %v; want 1 (only b left)", cleared, err)
	}
}

// TestTaskMemory_CascadeOnTaskDelete proves the FK ON DELETE CASCADE: deleting a
// task removes its memories (they are the task's own state).
func TestTaskMemory_CascadeOnTaskDelete(t *testing.T) {
	s, database := newSelfImproveStore(t)
	ctx := context.Background()
	taskID := makeTask(t, database)
	_ = s.UpsertTaskMemory(ctx, taskID, "a", "1", 100, 4096)

	if _, err := database.Conn().ExecContext(ctx, "DELETE FROM tasks WHERE id = $1", taskID); err != nil {
		t.Fatalf("delete task: %v", err)
	}
	n, err := s.CountTaskMemories(ctx, taskID)
	if err != nil || n != 0 {
		t.Fatalf("memories should cascade-delete with the task, got count=%d err=%v", n, err)
	}
}
