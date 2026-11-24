package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/reclaim"
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

	// Dead-lettered task (replayable — its mcp-runs ledger and lineage workspace must survive)
	dlqID := uuid.New()
	dlqAt := time.Now().Add(-10 * 24 * time.Hour).UTC()
	dlqTask := &models.Task{
		ID:             dlqID,
		LineageID:      dlqID,
		Prompt:         "replayable dead-lettered run",
		Status:         models.TaskStatusDeadLettered,
		Priority:       models.PriorityNormal,
		Timezone:       "UTC",
		CreatedAt:      dlqAt,
		DeadLetteredAt: &dlqAt,
	}
	if _, err := s.AddTaskWithContext(ctx, dlqTask); err != nil {
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
		filepath.Join(mcpRuns, "task-"+dlqTask.ID.String()),
		filepath.Join(mcpRuns, "task-"+doneTask.ID.String()),
		filepath.Join(mcpRuns, "task-"+orphanID),
		filepath.Join(tasksDir, activeTask.WorkspaceLineage().String()),
		filepath.Join(tasksDir, dlqTask.WorkspaceLineage().String()),
		filepath.Join(tasksDir, doneTask.WorkspaceLineage().String()),
		filepath.Join(tasksDir, orphanID),
	} {
		// Made long ago: a directory changed within the grace period is
		// never swept (TestSweepTaskWorkspacesLeavesNewDirectoriesAlone).
		makeTaskWorkspaceDir(t, p, longAgo)
	}

	removed, err := s.SweepTaskWorkspaces(ctx, wsRoot, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("SweepTaskWorkspaces: %v", err)
	}
	if removed != 4 {
		t.Fatalf("removed = %d, want 4 (2 mcp-runs + 2 tasks)", removed)
	}

	// Active and dead-lettered tasks' mcp-runs and tasks dirs survive.
	for _, keep := range []string{
		filepath.Join(mcpRuns, "task-"+activeTask.ID.String()),
		filepath.Join(mcpRuns, "task-"+dlqTask.ID.String()),
		filepath.Join(tasksDir, activeTask.WorkspaceLineage().String()),
		filepath.Join(tasksDir, dlqTask.WorkspaceLineage().String()),
	} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("expected %s to survive: %v", keep, err)
		}
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

// longAgo is a modification time well past the sweep's grace period: the
// age of a directory a run made days ago.
var longAgo = time.Now().Add(-48 * time.Hour)

// makeTaskWorkspaceDir creates dir holding one file and sets the directory's
// modification time to mtime.
func makeTaskWorkspaceDir(t *testing.T, dir string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "marker.txt"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dir, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func onDisk(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("lstat %s: %v", path, err)
	}
	return err == nil
}

// TestSweepTaskWorkspacesKeepsWorkStartedDuringTheSweep: the sweep reads the
// tasks once and lists the directories after. A task created in between —
// a new job, or a rerun of a job that finished longer ago than the retention
// window — has a live row by the time its run makes or reuses a directory,
// so that directory is kept, while a directory nothing owns is still removed.
func TestSweepTaskWorkspacesKeepsWorkStartedDuringTheSweep(t *testing.T) {
	// Not parallel: it sets a package-level hook.
	s, _ := newTestStore(t)
	ctx := context.Background()

	doneAt := time.Now().Add(-10 * 24 * time.Hour).UTC()
	done := &models.Task{
		ID: uuid.New(), Prompt: "finished long ago", Status: models.TaskStatusSuccess,
		Priority: models.PriorityNormal, Timezone: "UTC", CreatedAt: doneAt, CompletedAt: &doneAt,
	}
	done.LineageID = done.ID
	if _, err := s.AddTaskWithContext(ctx, done); err != nil {
		t.Fatal(err)
	}

	wsRoot := t.TempDir()
	mcpRuns := filepath.Join(wsRoot, "mcp-runs")
	tasksDir := filepath.Join(wsRoot, "tasks")
	doneDir := filepath.Join(tasksDir, done.ID.String())
	makeTaskWorkspaceDir(t, doneDir, longAgo)
	orphan := uuid.NewString()
	orphanDirs := []string{filepath.Join(tasksDir, orphan), filepath.Join(mcpRuns, "task-"+orphan)}
	for _, p := range orphanDirs {
		makeTaskWorkspaceDir(t, p, longAgo)
	}

	var started []string
	afterTaskWorkspaceSnapshot = func() {
		fresh := &models.Task{
			ID: uuid.New(), Prompt: "new job", Status: models.TaskStatusPending,
			Priority: models.PriorityNormal, Timezone: "UTC", CreatedAt: time.Now().UTC(),
		}
		fresh.LineageID = fresh.ID
		rerun := &models.Task{
			ID: uuid.New(), LineageID: done.ID, Prompt: "rerun", Status: models.TaskStatusPending,
			Priority: models.PriorityNormal, Timezone: "UTC", CreatedAt: time.Now().UTC(),
		}
		for _, task := range []*models.Task{fresh, rerun} {
			if _, err := s.AddTaskWithContext(ctx, task); err != nil {
				t.Error(err)
			}
		}
		// The runs make their directories; the rerun reuses its job's
		// (doneDir). They are dated long ago so that what keeps them is the
		// owning row, not the grace period: the rerun's job directory really
		// is that old.
		started = []string{
			filepath.Join(tasksDir, fresh.ID.String()),
			filepath.Join(mcpRuns, "task-"+fresh.ID.String()),
			filepath.Join(mcpRuns, "task-"+rerun.ID.String()),
			doneDir,
		}
		for _, p := range started[:3] {
			makeTaskWorkspaceDir(t, p, longAgo)
		}
	}
	t.Cleanup(func() { afterTaskWorkspaceSnapshot = nil })

	removed, err := s.SweepTaskWorkspaces(ctx, wsRoot, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("SweepTaskWorkspaces: %v", err)
	}
	if len(started) == 0 {
		t.Fatal("the hook did not run")
	}
	for _, p := range started {
		if !onDisk(t, p) {
			t.Errorf("%s belongs to a task created during the sweep and was removed", p)
		}
	}
	for _, p := range orphanDirs {
		if onDisk(t, p) {
			t.Errorf("orphan %s was not removed", p)
		}
	}
	if removed != len(orphanDirs) {
		t.Errorf("removed = %d, want %d", removed, len(orphanDirs))
	}
}

// TestSweepTaskWorkspacesLeavesNewDirectoriesAlone: a directory changed within
// the grace period is never removed, even with no task row naming it — a
// directory can exist before the row that owns it is visible to the sweep.
// Once it is older than that, the same directory is an orphan, and so is one
// dated far in the future (a sandbox can set its job directory's time).
func TestSweepTaskWorkspacesLeavesNewDirectoriesAlone(t *testing.T) {
	t.Parallel()
	s, _ := newTestStore(t)
	ctx := context.Background()

	wsRoot := t.TempDir()
	mcpRuns := filepath.Join(wsRoot, "mcp-runs")
	tasksDir := filepath.Join(wsRoot, "tasks")
	fresh := []string{
		filepath.Join(tasksDir, uuid.NewString()),
		filepath.Join(mcpRuns, "task-"+uuid.NewString()),
	}
	future := filepath.Join(tasksDir, uuid.NewString())
	old := []string{
		filepath.Join(tasksDir, uuid.NewString()),
		filepath.Join(mcpRuns, "task-"+uuid.NewString()),
		future,
	}
	for _, p := range fresh {
		makeTaskWorkspaceDir(t, p, time.Now())
	}
	for _, p := range old[:2] {
		makeTaskWorkspaceDir(t, p, longAgo)
	}
	makeTaskWorkspaceDir(t, future, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))

	removed, err := s.SweepTaskWorkspaces(ctx, wsRoot, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("SweepTaskWorkspaces: %v", err)
	}
	for _, p := range fresh {
		if !onDisk(t, p) {
			t.Errorf("%s was made just now and was removed", p)
		}
	}
	for _, p := range old {
		if onDisk(t, p) {
			t.Errorf("%s is an old orphan and was kept", p)
		}
	}
	if removed != len(old) {
		t.Errorf("removed = %d, want %d", removed, len(old))
	}
}

// TestSweepTaskWorkspacesRechecksUnderTheRunLock: the second look at a
// candidate's tasks, and its removal, hold the reclaim.TaskDirs lock a run
// holds while it makes the directory. A task whose row is written before the
// run asks for that lock — as every run's is — is seen by that look.
func TestSweepTaskWorkspacesRechecksUnderTheRunLock(t *testing.T) {
	// Not parallel: it sets a package-level hook.
	s, _ := newTestStore(t)
	ctx := context.Background()

	wsRoot := t.TempDir()
	lineage := uuid.New()
	occurrence := uuid.New()
	jobDir := filepath.Join(wsRoot, "tasks", lineage.String())
	runDir := filepath.Join(wsRoot, "mcp-runs", "task-"+occurrence.String())
	makeTaskWorkspaceDir(t, jobDir, longAgo)
	makeTaskWorkspaceDir(t, runDir, longAgo)
	owners := map[string]*models.Task{
		reclaim.TaskWorkspaceKey(lineage.String()):       {ID: uuid.New(), LineageID: lineage},
		reclaim.MCPRunKey("task-" + occurrence.String()): {ID: occurrence, LineageID: occurrence},
	}

	seen := map[string]bool{}
	beforeTaskWorkspaceRecheck = func(key string) {
		seen[key] = true
		if unlock, ok := reclaim.TaskDirs.TryLock(key); ok {
			unlock()
			t.Errorf("%s: the tasks are read again without the run lock held", key)
		}
		task := owners[key]
		if task == nil {
			t.Errorf("unexpected candidate %s", key)
			return
		}
		task.Prompt, task.Status, task.Priority, task.Timezone, task.CreatedAt = "run", models.TaskStatusPending, models.PriorityNormal, "UTC", time.Now().UTC()
		if _, err := s.AddTaskWithContext(ctx, task); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { beforeTaskWorkspaceRecheck = nil })

	removed, err := s.SweepTaskWorkspaces(ctx, wsRoot, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("SweepTaskWorkspaces: %v", err)
	}
	if len(seen) != len(owners) {
		t.Fatalf("rechecked %v, want both candidates", seen)
	}
	if removed != 0 || !onDisk(t, jobDir) || !onDisk(t, runDir) {
		t.Errorf("removed = %d (job dir on disk %v, run dir on disk %v); a task live at the second look keeps its directory",
			removed, onDisk(t, jobDir), onDisk(t, runDir))
	}
}
