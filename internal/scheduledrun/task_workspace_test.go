// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scheduledrun

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// A non-worktree scheduled run works in <root>/tasks/<lineage>/ — one directory
// per job, shared by every occurrence of that job and by nothing else (#1543).
// FLEET_SCHEDULED_SHARED_WORKSPACE restores the shared root.
func TestTaskWorkspaceDirIsPerJobLineage(t *testing.T) {
	root := t.TempDir()
	r := &Runner{cfg: &config.Config{WorkspaceRoot: root}}

	job := models.NewTask(models.TaskCreate{Prompt: "daily scan", Recurrence: "0 9 * * *"})
	dir := r.taskWorkspaceDir(job)
	want := filepath.Join(root, "tasks", job.ID.String())
	if dir != want {
		t.Fatalf("dir = %q, want %q", dir, want)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("per-job dir must be created on first use: %v", err)
	}

	// The next occurrence of the same job lands in the SAME directory …
	next := models.NewTask(models.TaskToCreate(job))
	if got := r.taskWorkspaceDir(next); got != want {
		t.Fatalf("occurrence dir = %q, want the job's %q", got, want)
	}
	// … and an unrelated job in its own.
	other := models.NewTask(models.TaskCreate{Prompt: "weekly report"})
	if got := r.taskWorkspaceDir(other); got == want || got != filepath.Join(root, "tasks", other.ID.String()) {
		t.Fatalf("unrelated job dir = %q, must be its own", got)
	}

	shared := &Runner{cfg: &config.Config{WorkspaceRoot: root, ScheduledSharedWorkspace: true}}
	if got := shared.taskWorkspaceDir(job); got != root {
		t.Fatalf("kill switch must restore the shared root, got %q", got)
	}
	if got := r.taskWorkspaceDir(nil); got != root {
		t.Fatalf("nil task must fall back to the root, got %q", got)
	}
}
