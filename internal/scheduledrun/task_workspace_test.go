// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scheduledrun

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// A non-worktree scheduled run works in <root>/tasks/<lineage>/ — one directory
// per job, shared by every occurrence of that job and by nothing else (#1543).
// FLEET_SCHEDULED_SHARED_WORKSPACE restores the shared root.
func TestTaskWorkspaceDirIsPerJobLineage(t *testing.T) {
	root := t.TempDir()
	r := &Runner{cfg: &config.Config{WorkspaceRoot: root}}

	job := models.NewTask(models.TaskCreate{Prompt: "daily scan", Recurrence: "0 9 * * *"})
	dir := r.taskWorkspaceDir(context.Background(), job)
	want := filepath.Join(root, "tasks", job.ID.String())
	if dir != want {
		t.Fatalf("dir = %q, want %q", dir, want)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		t.Fatalf("per-job dir must be created on first use: %v", err)
	}

	// The next occurrence of the same job lands in the SAME directory …
	next := models.NewTask(models.TaskToCreate(job))
	if got := r.taskWorkspaceDir(context.Background(), next); got != want {
		t.Fatalf("occurrence dir = %q, want the job's %q", got, want)
	}
	// … and an unrelated job in its own.
	other := models.NewTask(models.TaskCreate{Prompt: "weekly report"})
	if got := r.taskWorkspaceDir(context.Background(), other); got == want || got != filepath.Join(root, "tasks", other.ID.String()) {
		t.Fatalf("unrelated job dir = %q, must be its own", got)
	}

	shared := &Runner{cfg: &config.Config{WorkspaceRoot: root, ScheduledSharedWorkspace: true}}
	if got := shared.taskWorkspaceDir(context.Background(), job); got != root {
		t.Fatalf("kill switch must restore the shared root, got %q", got)
	}
	if got := r.taskWorkspaceDir(context.Background(), nil); got != root {
		t.Fatalf("nil task must fall back to the root, got %q", got)
	}
}

// A job-directory run gets a sandbox scoped to that directory (#1897): its
// bash and run_python must not see other jobs' or conversations' files. The
// whole mcp-runs/ tree is withheld, and this occurrence's own run dir is
// mounted only when one of its MCP servers uses ${FLEET_WORKSPACE}.
func TestRunSandboxScope(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
	r := &Runner{cfg: &config.Config{WorkspaceRoot: root}}
	job := models.NewTask(models.TaskCreate{Prompt: "daily scan"})
	dir := r.taskWorkspaceDir(context.Background(), job)
	ctx := tools.WithForcedWorkingDir(context.Background(), dir)

	scope, err := r.runSandboxScope(ctx, job, "")
	if err != nil {
		t.Fatalf("runSandboxScope: %v", err)
	}
	if scope == nil || scope.Dir != dir {
		t.Fatalf("scope = %+v, want Dir %q", scope, dir)
	}
	if want := filepath.Join(root, "mcp-runs"); len(scope.Withhold) != 1 || scope.Withhold[0] != want {
		t.Errorf("Withhold = %v, want the whole mcp-runs tree %q", scope.Withhold, want)
	}
	if len(scope.ReadOnly) != 0 {
		t.Errorf("ReadOnly = %v; a task with no ${FLEET_WORKSPACE} server has no run dir to mount", scope.ReadOnly)
	}
	if _, err := os.Stat(filepath.Join(root, "mcp-runs", "task-"+job.ID.String())); !os.IsNotExist(err) {
		t.Errorf("a run dir was created for a task that never uses one: %v", err)
	}

	// A selected server that uses ${FLEET_WORKSPACE}: the run dir is created
	// before the sandbox starts and mounted read-only.
	withWS := &Runner{cfg: &config.Config{WorkspaceRoot: root, MCPServers: map[string]config.MCPServerConfig{
		"ledger": {Enabled: true, Command: "x", Env: map[string]string{"RUN_DIR": "${FLEET_WORKSPACE}"}},
	}}}
	scope, err = withWS.runSandboxScope(ctx, job, "")
	if err != nil {
		t.Fatalf("runSandboxScope with a workspace server: %v", err)
	}
	runDir := filepath.Join(root, "mcp-runs", "task-"+job.ID.String())
	if scope == nil || len(scope.ReadOnly) != 1 || scope.ReadOnly[0] != runDir {
		t.Fatalf("scope = %+v, want ReadOnly [%q]", scope, runDir)
	}
	if st, err := os.Stat(runDir); err != nil || !st.IsDir() {
		t.Fatalf("run dir must exist before the sandbox mounts it: %v", err)
	}

	// The whole-root mount stays for the cases that need the root.
	for name, tc := range map[string]struct {
		ctx    context.Context
		wtPath string
	}{
		"worktree run":          {ctx, filepath.Join(root, ".worktrees", "x")},
		"shared-workspace root": {tools.WithForcedWorkingDir(context.Background(), root), ""},
		"no forced working dir": {context.Background(), ""},
		"dir outside the root":  {tools.WithForcedWorkingDir(context.Background(), t.TempDir()), ""},
	} {
		if got, err := r.runSandboxScope(tc.ctx, job, tc.wtPath); err != nil || got != nil {
			t.Errorf("%s: scope = %+v, err = %v; want nil, nil (whole-root mount)", name, got, err)
		}
	}
}
