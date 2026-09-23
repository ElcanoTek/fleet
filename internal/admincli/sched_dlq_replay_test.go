package admincli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	scheddb "github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// `fleet sched dlq replay` refuses a prompt with a malformed EXECUTION
// REQUIREMENTS line (it would dead-letter again, exit 1: a refused write), and --prompt-file
// replays the same row with a corrected prompt (ADR-0073). Gated on
// DATABASE_URL, the sched-suite convention.
func TestSchedDLQReplayRefusesMalformedAndTakesAPromptFile_DB(t *testing.T) {
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
	clean := func() { database.Conn().ExecContext(ctx, "DELETE FROM tasks") }
	clean()
	t.Cleanup(func() {
		clean()
		conn.ExecContext(ctx, "SELECT pg_advisory_unlock(1)")
		conn.Close()
		database.Close()
	})

	st := storage.New()
	st.SetDatabase(database)
	now := time.Now().UTC()
	reason := "non-retryable failure (terminal): execution requirements: …"
	task := &models.Task{
		ID: uuid.New(), Status: models.TaskStatusDeadLettered, Priority: 1, CreatedAt: now,
		Prompt:           "Refresh.\n" + models.ExecutionRequirementsMarker + "\n" + `{"mcp_servers":["fast_io + fastio_helpers"]}`,
		DeadLetteredAt:   &now,
		DeadLetterReason: &reason,
	}
	if _, err := st.AddTaskWithContext(ctx, task); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	id := task.ID.String()

	if code := schedDLQReplay([]string{id}); code != 1 {
		t.Fatalf("replay of a malformed prompt exit = %d, want 1", code)
	}
	if got, _ := st.GetTask(task.ID); got == nil || got.Status != models.TaskStatusDeadLettered {
		t.Fatalf("a refused replay must leave the row dead-lettered, got %+v", got)
	}

	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := schedDLQReplay([]string{"--prompt-file", empty, id}); code != 1 {
		t.Fatalf("replay with an empty --prompt-file exit = %d, want 1", code)
	}

	corrected := "Refresh.\n" + models.ExecutionRequirementsMarker + "\n" + `{"mcp_servers":["fast_io","fastio_helpers"]}`
	file := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(file, []byte(corrected+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := schedDLQReplay([]string{"--prompt-file", file, id}); code != 0 {
		t.Fatalf("replay with a corrected --prompt-file exit = %d, want 0", code)
	}
	got, err := st.GetTask(task.ID)
	if err != nil || got.Status != models.TaskStatusPending || got.Prompt != corrected {
		t.Fatalf("replayed row = %+v %v, want pending with the corrected prompt", got, err)
	}
}
