package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func TestParseMCPRunTaskID(t *testing.T) {
	t.Parallel()

	id := "c8bd6051-8f8b-4999-bb84-175d8a26a068"
	if got, ok := parseMCPRunTaskID("task-" + id); !ok || got != id {
		t.Fatalf("parseMCPRunTaskID(task-id) = %q, %v", got, ok)
	}
	if got, ok := parseMCPRunTaskID("task-" + id + "-sub1"); !ok || got != id {
		t.Fatalf("parseMCPRunTaskID(task-id-sub1) = %q, %v", got, ok)
	}
	if _, ok := parseMCPRunTaskID("chat-" + id); ok {
		t.Fatal("chat- prefix must not match")
	}
	if _, ok := parseMCPRunTaskID("task-not-a-uuid"); ok {
		t.Fatal("non-uuid must not match")
	}
}

func TestIsStagedAttachmentFileName(t *testing.T) {
	t.Parallel()

	if !isStagedAttachmentFileName("Husqvarna__8693a41c.csv") {
		t.Error("expected Husqvarna__8693a41c.csv to match staged attachment pattern")
	}
	if !isStagedAttachmentFileName("Working_with_Cancer_Report__91f81777.zip") {
		t.Error("expected Working_with_Cancer_Report__91f81777.zip to match staged attachment pattern")
	}
	if isStagedAttachmentFileName("OptLog_WorkingWithCancer_ZIAO91552.csv") {
		t.Error("canonical output CSV without __<8hex> must not match")
	}
	if isStagedAttachmentFileName("canonical_template_ref.html") {
		t.Error("template HTML must not match")
	}
}

func TestPruneAgedLineageScratch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	oldTime := time.Now().Add(-10 * 24 * time.Hour)

	// 1. Old staged attachment (small, but matches __<8hex>) -> pruned
	stagedOld := filepath.Join(dir, "Report__a1b2c3d4.csv")
	if err := os.WriteFile(stagedOld, []byte("staged"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(stagedOld, oldTime, oldTime)

	// 2. Old large scratch dump (>= 1 MiB) in a subdir -> pruned, and empty subdir removed
	subDir := filepath.Join(dir, "run1020")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bigOld := filepath.Join(subDir, "dump.pkl")
	if err := os.WriteFile(bigOld, make([]byte, activeLineageScratchMinBytes+16), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(bigOld, oldTime, oldTime)

	// 3. Old small reference/template file (< 1 MiB, not __<8hex>) -> preserved!
	smallRef := filepath.Join(dir, "canonical_template_ref.html")
	if err := os.WriteFile(smallRef, []byte("<html>ref</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(smallRef, oldTime, oldTime)

	// 4. Fresh large file -> preserved!
	bigFresh := filepath.Join(dir, "fresh.zip")
	if err := os.WriteFile(bigFresh, make([]byte, activeLineageScratchMinBytes+16), 0o600); err != nil {
		t.Fatal(err)
	}

	pruned, err := pruneAgedLineageScratch(dir, cutoff)
	if err != nil {
		t.Fatalf("pruneAgedLineageScratch: %v", err)
	}
	if pruned != 2 {
		t.Fatalf("pruned = %d, want 2", pruned)
	}
	if _, err := os.Stat(stagedOld); !os.IsNotExist(err) {
		t.Error("old staged attachment should be removed")
	}
	if _, err := os.Stat(bigOld); !os.IsNotExist(err) {
		t.Error("old large scratch file should be removed")
	}
	if _, err := os.Stat(subDir); !os.IsNotExist(err) {
		t.Error("empty subdirectory should be removed")
	}
	if _, err := os.Stat(smallRef); err != nil {
		t.Error("small reference file must be preserved")
	}
	if _, err := os.Stat(bigFresh); err != nil {
		t.Error("fresh large file must be preserved")
	}
}

func TestSweepTaskWorkspaces(t *testing.T) {
	t.Parallel()

	s, _ := newTestStore(t)
	ctx := context.Background()

	// Active scheduled task
	future := time.Now().Add(time.Hour).UTC()
	activeID := uuid.New()
	activeTask := &models.Task{
		ID:           activeID,
		LineageID:    activeID,
		Prompt:       "active recurring",
		Status:       models.TaskStatusScheduled,
		ScheduledFor: &future,
		Priority:     models.PriorityNormal,
		Recurrence:   "@daily",
		Timezone:     "UTC",
		CreatedAt:    time.Now().UTC(),
	}
	if _, err := s.AddTaskWithContext(ctx, activeTask); err != nil {
		t.Fatal(err)
	}

	// Completed old task (completed 10 days ago)
	doneID := uuid.New()
	doneAt := time.Now().Add(-10 * 24 * time.Hour).UTC()
	doneTask := &models.Task{
		ID:          doneID,
		LineageID:   doneID,
		Prompt:      "finished one-off",
		Status:      models.TaskStatusSuccess,
		Priority:    models.PriorityNormal,
		Timezone:    "UTC",
		CreatedAt:   doneAt,
		CompletedAt: &doneAt,
	}
	if _, err := s.AddTaskWithContext(ctx, doneTask); err != nil {
		t.Fatal(err)
	}

	orphanID := uuid.New().String()

	wsRoot := t.TempDir()
	mcpRuns := filepath.Join(wsRoot, "mcp-runs")
	tasksDir := filepath.Join(wsRoot, "tasks")

	for _, p := range []string{
		filepath.Join(mcpRuns, "task-"+activeTask.ID.String()),
		filepath.Join(mcpRuns, "task-"+doneTask.ID.String()),
		filepath.Join(mcpRuns, "task-"+orphanID),
		filepath.Join(tasksDir, activeTask.WorkspaceLineage().String()),
		filepath.Join(tasksDir, doneTask.WorkspaceLineage().String()),
		filepath.Join(tasksDir, orphanID),
	} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "marker.txt"), []byte("ok"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := s.SweepTaskWorkspaces(ctx, wsRoot, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("SweepTaskWorkspaces: %v", err)
	}
	if removed != 4 {
		t.Fatalf("removed = %d, want 4 (2 mcp-runs + 2 tasks)", removed)
	}

	// Active task's mcp-runs and tasks dirs survive.
	if _, err := os.Stat(filepath.Join(mcpRuns, "task-"+activeTask.ID.String())); err != nil {
		t.Error("active task mcp-runs dir must survive")
	}
	if _, err := os.Stat(filepath.Join(tasksDir, activeTask.WorkspaceLineage().String())); err != nil {
		t.Error("active task lineage workspace dir must survive")
	}

	// Done and orphan mcp-runs + tasks dirs are gone.
	for _, gone := range []string{
		filepath.Join(mcpRuns, "task-"+doneTask.ID.String()),
		filepath.Join(mcpRuns, "task-"+orphanID),
		filepath.Join(tasksDir, doneTask.WorkspaceLineage().String()),
		filepath.Join(tasksDir, orphanID),
	} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("expected %s to be removed, stat err = %v", gone, err)
		}
	}
}
