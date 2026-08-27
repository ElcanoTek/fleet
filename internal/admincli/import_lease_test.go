package admincli

// The legacy bundle importer's half of the #1267 acceptance, DB-backed:
// --overwrite is consent to restore a snapshot over an already-present task,
// but never consent to write over a row a worker is running.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/db"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// TestImportBundle_SchedOverwriteRefusesLiveLease seeds a row that is running
// under a live lease, then imports a bundle carrying that id with
// --overwrite. The row must keep its status AND its lease, the record must be
// reported as an error (exit 3), and --dry-run must refuse identically because
// the check is a read.
func TestImportBundle_SchedOverwriteRefusesLiveLease(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set — skipping sched-DB import test")
	}
	database := db.New()
	if err := database.Init("", db.DefaultPoolConfig()); err != nil {
		t.Skipf("sched DB unavailable: %v", err)
	}
	ctx := context.Background()
	conn, err := database.Conn().Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock(5)"); err != nil {
		conn.Close()
		t.Fatalf("lock: %v", err)
	}
	clean := func() {
		for _, q := range []string{"DELETE FROM logs", "DELETE FROM tasks", "DELETE FROM users"} {
			if _, err := database.Conn().ExecContext(ctx, q); err != nil {
				t.Fatalf("clean %q: %v", q, err)
			}
		}
	}
	clean()
	t.Cleanup(func() {
		clean()
		conn.ExecContext(ctx, "SELECT pg_advisory_unlock(5)")
		conn.Close()
		database.Close()
	})

	st := storage.New()
	st.SetDatabase(database)

	taskID := uuid.New()
	now := time.Now().UTC()
	seed := &models.Task{
		ID: taskID, Prompt: "long job", Status: models.TaskStatusPending,
		CreatedAt: now, Priority: 5,
	}
	if _, err := st.AddTaskWithContext(ctx, seed); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	// Stand in for the claim + run-start transitions: the row is running under
	// a live lease this box's worker owns.
	leaseExpiry := now.Add(5 * time.Minute)
	if _, err := database.Conn().ExecContext(ctx,
		"UPDATE tasks SET status = $2, lease_owner = $3, lease_expires_at = $4, started_at = $5 WHERE id = $1",
		taskID, string(models.TaskStatusRunning), "scheduler-1", leaseExpiry, now); err != nil {
		t.Fatalf("lease the row: %v", err)
	}

	bundle := migrationBundle{
		Format: migrationBundleFormat, Version: 1,
		Sched: &schedSection{Tasks: []bundleTask{{
			ID: taskID, Prompt: "long job", Status: "pending",
			Priority: 5, CreatedAt: now,
		}}},
	}
	path := writeBundle(t, bundle)
	dsn := os.Getenv("DATABASE_URL")

	for _, args := range [][]string{
		{path, "--overwrite", "--dry-run", "--sched-database-url", dsn},
		{path, "--overwrite", "--sched-database-url", dsn},
	} {
		if code := cmdImport(args); code != 3 {
			t.Errorf("import %v: exit %d, want 3 (the live-lease record must be reported as an error)", args, code)
		}
	}

	after, err := st.GetTask(taskID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.Status != models.TaskStatusRunning {
		t.Errorf("--overwrite wrote over a running row: status is %q", after.Status)
	}
	if after.LeaseOwner == nil || *after.LeaseOwner != "scheduler-1" {
		t.Errorf("live lease owner lost: %v", after.LeaseOwner)
	}
	if after.LeaseExpiresAt == nil {
		t.Error("live lease expiry cleared")
	}
}
