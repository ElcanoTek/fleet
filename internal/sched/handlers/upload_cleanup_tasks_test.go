package handlers

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/testdb/fleetdb"
)

// TestCleanupTempFilesKeepsFilesAKeptJobNames: a task copies its input files
// from temp_uploads at the start of every run, so the hourly cleanup leaves a
// file (and its checksum) while a kept job names it — a weekly job whose next
// occurrence is days away (named also by its finished earlier occurrence), a
// task scheduled weeks ahead, a dead-lettered task awaiting replay, a
// recurring occurrence whose successor is not spawned yet, and a job that
// failed two days ago (so it can be fixed and rerun) although its file was
// uploaded weeks before. The file of a job that finished long ago (a
// cancelled recurring one too), and an upload no task names, are removed by
// age: rerunning that finished job would find its input gone.
func TestCleanupTempFilesKeepsFilesAKeptJobNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := storage.New()
	if err := store.Initialize(fleetdb.New(t).SchedDSN(), fleetdb.SchedPool()); err != nil {
		t.Fatalf("init storage: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	dataDir := t.TempDir()
	tempDir := filepath.Join(dataDir, "temp_uploads")
	old := time.Now().Add(-30 * 24 * time.Hour)
	write := func(rel string) string {
		p := filepath.Join(tempDir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		return p
	}
	add := func(task *models.Task) {
		t.Helper()
		task.Prompt, task.Priority, task.CreatedAt = "report from the brief", models.PriorityNormal, time.Now().UTC()
		if task.LineageID == uuid.Nil {
			task.LineageID = task.ID
		}
		if _, err := store.AddTaskWithContext(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	next := time.Now().Add(72 * time.Hour).UTC()
	later := time.Now().Add(40 * 24 * time.Hour).UTC()
	done := time.Now().Add(-60 * 24 * time.Hour).UTC()

	// A weekly job: the finished first occurrence named the brief, the next
	// one (same lineage) is scheduled and names it too.
	weekly := uuid.New()
	add(&models.Task{ID: weekly, Status: models.TaskStatusSuccess, CompletedAt: &done, Files: []string{"weekly_1a2b3c4d.txt"}})
	add(&models.Task{ID: uuid.New(), LineageID: weekly, Status: models.TaskStatusScheduled, ScheduledFor: &next,
		Recurrence: "@weekly", Timezone: "UTC", Files: []string{"weekly_1a2b3c4d.txt"}})
	// A job whose finished occurrence alone names a file, while the lineage
	// is still live: kept by the lineage rule.
	lineage := uuid.New()
	add(&models.Task{ID: lineage, Status: models.TaskStatusSuccess, CompletedAt: &done, Files: []string{"lineage_6f7a8b9c.txt"}})
	add(&models.Task{ID: uuid.New(), LineageID: lineage, Status: models.TaskStatusPending})
	// Scheduled further out than the retention window.
	add(&models.Task{ID: uuid.New(), Status: models.TaskStatusScheduled, ScheduledFor: &later, Timezone: "UTC", Files: []string{"later_2b3c4d5e.txt"}})
	// Dead-lettered, awaiting replay.
	add(&models.Task{ID: uuid.New(), Status: models.TaskStatusDeadLettered, DeadLetteredAt: &done, Files: []string{"dlq_3c4d5e6f.txt"}})
	// A one-off job that finished long ago.
	add(&models.Task{ID: uuid.New(), Status: models.TaskStatusSuccess, CompletedAt: &done, Files: []string{"finished_4d5e6f7a.txt"}})
	// A one-off job that failed two days ago; its file is a month old.
	failed := time.Now().Add(-48 * time.Hour).UTC()
	add(&models.Task{ID: uuid.New(), Status: models.TaskStatusError, CompletedAt: &failed, Files: []string{"failed_7a8b9c0d.txt"}})
	// A recurring occurrence that finished long ago whose successor has not
	// been spawned yet (the scheduler's sweep spawns it). A terminal row is
	// inserted settled, so the spawn credit is reopened the way a terminal
	// write leaves it.
	unspawned := uuid.New()
	add(&models.Task{ID: unspawned, Status: models.TaskStatusSuccess, CompletedAt: &done, Recurrence: "@daily", Timezone: "UTC",
		Files: []string{"unspawned_8b9c0d1e.txt"}})
	if _, err := store.DB().Conn().ExecContext(ctx, `UPDATE tasks SET recurrence_spawned = FALSE WHERE id = $1`, unspawned); err != nil {
		t.Fatal(err)
	}
	// A recurring job someone cancelled long ago: it spawns nothing more.
	add(&models.Task{ID: uuid.New(), Status: models.TaskStatusCancelled, CompletedAt: &done, Recurrence: "@daily", Timezone: "UTC",
		Files: []string{"cancelled_9c0d1e2f.txt"}})

	kept := []string{
		write("weekly_1a2b3c4d.txt"), write(".checksums/weekly_1a2b3c4d.txt.sha256"),
		write("lineage_6f7a8b9c.txt"), write("later_2b3c4d5e.txt"), write("dlq_3c4d5e6f.txt"),
		write("failed_7a8b9c0d.txt"), write("unspawned_8b9c0d1e.txt"),
	}
	removed := []string{
		write("finished_4d5e6f7a.txt"), write("cancelled_9c0d1e2f.txt"),
		write("unused_5e6f7a8b.txt"), write(".checksums/unused_5e6f7a8b.txt.sha256"),
	}

	h := &Handlers{config: Config{DataDir: dataDir}, storage: store}
	h.CleanupTempFiles(14 * 24 * time.Hour)
	for _, p := range kept {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed while a kept job names it: %v", filepath.Base(p), err)
		}
	}
	for _, p := range removed {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s outlived the retention window (stat err %v)", filepath.Base(p), err)
		}
	}
}
