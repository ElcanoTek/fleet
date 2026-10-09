package scheduledrun

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func TestStageTaskInputsUsesLogicalNames(t *testing.T) {
	dataDir := t.TempDir()
	uploads := filepath.Join(dataDir, "temp_uploads")
	if err := os.MkdirAll(uploads, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uploads, "domains_deadbeef.csv"), []byte("domain\nexample.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: &config.Config{DataDir: dataDir}}
	runDir, run := openRunRoot(t)
	dst := filepath.Join(runDir, "inputs")
	task := &models.Task{Files: []string{"domains_deadbeef.csv"}, FileNames: []string{"domains.csv"}}
	if err := r.stageTaskInputs(task, run); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "domains.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "domain\nexample.com\n" {
		t.Fatalf("staged bytes = %q", got)
	}
}

func TestStageTaskInputsRejectsAliasMismatch(t *testing.T) {
	r := &Runner{cfg: &config.Config{DataDir: t.TempDir()}}
	task := &models.Task{Files: []string{"a.csv"}, FileNames: []string{"a.csv", "b.csv"}}
	_, run := openRunRoot(t)
	if err := r.stageTaskInputs(task, run); err == nil {
		t.Fatal("expected alias mismatch error")
	}
}

// openRunRoot stands in for the run dir agentcore.OpenStableMCPWorkspace opens.
func openRunRoot(t *testing.T) (string, *os.Root) {
	t.Helper()
	dir := t.TempDir()
	run, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = run.Close() })
	return dir, run
}

// TestStageTaskInputsIdempotentOnReuse pins the retry path: re-staging into the
// same run dir replaces the snapshot with the current bytes, leaves no stage
// dirs and sweeps a crashed attempt's leftover stage.
func TestStageTaskInputsIdempotentOnReuse(t *testing.T) {
	dataDir := t.TempDir()
	writeUpload(t, dataDir, "d_1.csv", "v1")
	r := &Runner{cfg: &config.Config{DataDir: dataDir}}
	runDir, run := openRunRoot(t)
	dst := filepath.Join(runDir, "inputs")
	task := &models.Task{Files: []string{"d_1.csv"}, FileNames: []string{"d.csv"}}
	if err := r.stageTaskInputs(task, run); err != nil {
		t.Fatal(err)
	}
	crashed := filepath.Join(runDir, inputsStagePrefix+"crashed")
	if err := os.MkdirAll(crashed, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(crashed, "half"), []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeUpload(t, dataDir, "d_1.csv", "v2")
	if err := r.stageTaskInputs(task, run); err != nil {
		t.Fatalf("re-stage: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "d.csv"))
	if err != nil || string(got) != "v2" {
		t.Fatalf("staged = %q, %v; want v2", got, err)
	}
	if fi, err := os.Stat(filepath.Join(dst, "d.csv")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("staged mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
	if names := dirNames(t, dst); len(names) != 1 {
		t.Fatalf("inputs dir should hold only d.csv, got %v", names)
	}
	if names := dirNames(t, runDir); len(names) != 1 || names[0] != "inputs" {
		t.Fatalf("run dir should hold only inputs/ (stages swept), got %v", names)
	}
}

// TestStageTaskInputsFailureKeepsNoPartialSnapshot pins that a staging failure
// leaves neither a half-built inputs/ nor a stage dir behind.
func TestStageTaskInputsFailureKeepsNoPartialSnapshot(t *testing.T) {
	dataDir := t.TempDir()
	writeUpload(t, dataDir, "a_1.csv", "a")
	r := &Runner{cfg: &config.Config{DataDir: dataDir}}
	runDir, run := openRunRoot(t)
	task := &models.Task{Files: []string{"a_1.csv", "missing.csv"}, FileNames: []string{"a.csv", "m.csv"}}
	if err := r.stageTaskInputs(task, run); err == nil {
		t.Fatal("expected error for a missing upload")
	}
	if names := dirNames(t, runDir); len(names) != 0 {
		t.Fatalf("failed staging left %v behind", names)
	}
}

// TestStableTaskWorkdirPerOccurrence pins the retry/recurrence contract: two
// attempts of one task row share a dir and its ledger; another occurrence
// (recurring successor, re-run) gets its own; the dir is evidence and is not
// removed between or after attempts.
func TestStableTaskWorkdirPerOccurrence(t *testing.T) {
	t.Setenv("FLEET_WORKSPACE_ROOT", t.TempDir())
	r := &Runner{cfg: &config.Config{DataDir: t.TempDir()}}
	occ := &models.Task{ID: uuid.New()}
	next := &models.Task{ID: uuid.New(), PreviousOccurrenceID: &occ.ID}

	first, err := r.stableTaskWorkdir(occ)
	if err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(first, "creates.jsonl")
	if err := os.WriteFile(ledger, []byte("deal-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	occ.AttemptCount++ // user retry
	occ.InfraRetryCount++
	second, err := r.stableTaskWorkdir(occ)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("retry changed workdir: %q vs %q", first, second)
	}
	if b, err := os.ReadFile(ledger); err != nil || string(b) != "deal-1\n" {
		t.Fatalf("ledger lost across attempts: %q, %v", b, err)
	}
	other, err := r.stableTaskWorkdir(next)
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Fatal("next occurrence must not share the previous occurrence's workdir")
	}
	if _, err := os.Stat(filepath.Join(other, "creates.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("ledger leaked into next occurrence: %v", err)
	}
	// Terminal: nothing prunes the evidence dir.
	occ.Status = models.TaskStatusSuccess
	if _, err := os.Stat(ledger); err != nil {
		t.Fatalf("ledger removed after terminal: %v", err)
	}
}

// TestPrepareTaskMCPWorkspaceStableAcrossAttempts drives the real entry point
// (scope-opener path) twice for one task row.
func TestPrepareTaskMCPWorkspaceStableAcrossAttempts(t *testing.T) {
	t.Setenv("FLEET_WORKSPACE_ROOT", t.TempDir())
	r := &Runner{
		cfg: &config.Config{DataDir: t.TempDir()},
		openTaskMCPScope: func(context.Context, agentcore.MCPSelection, agent.MCPScopePolicy, string, string) (*agent.MCPScope, error) {
			return nil, nil
		},
		mcpServerInventory: func() map[string]TaskMCPServerInfo { return map[string]TaskMCPServerInfo{"ssp": {UsesWorkspace: true}} },
	}
	task := &models.Task{ID: uuid.New()}
	sel := agentcore.MCPSelection{{Server: "ssp"}}
	a, err := r.prepareTaskMCPWorkspace(task, sel)
	if err != nil || a == "" {
		t.Fatalf("attempt 1: %q, %v", a, err)
	}
	b, err := r.prepareTaskMCPWorkspace(task, sel)
	if err != nil || a != b {
		t.Fatalf("attempt 2: %q vs %q, %v", a, b, err)
	}
}

// writeUpload drops a server-owned upload object under DataDir/temp_uploads.
func writeUpload(t *testing.T, dataDir, stored, body string) {
	t.Helper()
	uploads := filepath.Join(dataDir, "temp_uploads")
	if err := os.MkdirAll(uploads, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(uploads, stored), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestStableTaskWorkdirRefusesSymlinkedInputs pins Codex P1 on #1709: the
// sandbox can write the workspace mount, so between attempts it can replace the
// stable run dir's inputs/ with a symlink to a host directory. The retry must
// neutralize it (never write through it) and keep the ledger.
func TestStableTaskWorkdirRefusesSymlinkedInputs(t *testing.T) {
	t.Setenv("FLEET_WORKSPACE_ROOT", t.TempDir())
	dataDir := t.TempDir()
	writeUpload(t, dataDir, "d_1.csv", "attachment")
	r := &Runner{cfg: &config.Config{DataDir: dataDir}}
	task := &models.Task{ID: uuid.New(), Files: []string{"d_1.csv"}, FileNames: []string{"d.csv"}}

	workdir, err := r.stableTaskWorkdir(task)
	if err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(workdir, "creates.jsonl")
	if err := os.WriteFile(ledger, []byte("deal-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The sandboxed attempt plants inputs -> victim between attempts.
	victim := t.TempDir()
	if err := os.WriteFile(filepath.Join(victim, "d.csv"), []byte("victim"), 0o600); err != nil {
		t.Fatal(err)
	}
	inputs := filepath.Join(workdir, "inputs")
	if err := os.RemoveAll(inputs); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, inputs); err != nil {
		t.Fatal(err)
	}

	again, err := r.stableTaskWorkdir(task)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if again != workdir {
		t.Fatalf("retry changed workdir: %q vs %q", again, workdir)
	}
	if b, err := os.ReadFile(filepath.Join(victim, "d.csv")); err != nil || string(b) != "victim" {
		t.Fatalf("staging wrote through the planted symlink: %q, %v", b, err)
	}
	if entries, _ := os.ReadDir(victim); len(entries) != 1 {
		t.Fatalf("victim dir gained entries: %d", len(entries))
	}
	fi, err := os.Lstat(inputs)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		t.Fatalf("inputs must be a real directory after the retry, got %v, %v", fi, err)
	}
	if b, err := os.ReadFile(filepath.Join(inputs, "d.csv")); err != nil || string(b) != "attachment" {
		t.Fatalf("staged = %q, %v", b, err)
	}
	if b, err := os.ReadFile(ledger); err != nil || string(b) != "deal-1\n" {
		t.Fatalf("ledger lost: %q, %v", b, err)
	}
}

// TestStableTaskWorkdirRefusesSymlinkedRunDir pins that a run dir (or the
// mcp-runs base) the sandbox swapped for a symlink fails the MCP setup closed
// instead of being mounted, and nothing is written at the symlink's target.
func TestStableTaskWorkdirRefusesSymlinkedRunDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
	dataDir := t.TempDir()
	writeUpload(t, dataDir, "d_1.csv", "attachment")
	r := &Runner{cfg: &config.Config{DataDir: dataDir}}
	task := &models.Task{ID: uuid.New(), Files: []string{"d_1.csv"}, FileNames: []string{"d.csv"}}

	workdir, err := r.stableTaskWorkdir(task)
	if err != nil {
		t.Fatal(err)
	}
	victim := t.TempDir()
	if err := os.RemoveAll(workdir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, workdir); err != nil {
		t.Fatal(err)
	}
	if _, err := r.stableTaskWorkdir(task); err == nil {
		t.Fatal("a symlinked run dir must be refused")
	}
	if entries, _ := os.ReadDir(victim); len(entries) != 0 {
		t.Fatalf("staging wrote through the symlinked run dir: %d entries", len(entries))
	}

	// Same for the mcp-runs base itself.
	base := filepath.Join(root, "mcp-runs")
	if err := os.RemoveAll(base); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, base); err != nil {
		t.Fatal(err)
	}
	if _, err := r.stableTaskWorkdir(task); err == nil {
		t.Fatal("a symlinked mcp-runs base must be refused")
	}
	if entries, _ := os.ReadDir(victim); len(entries) != 0 {
		t.Fatalf("run dir created through the symlinked base: %d entries", len(entries))
	}
}

// TestStableTaskWorkdirDropsRemovedAttachments pins Codex P2 on #1709: a
// requeued task is editable, so each attempt rebuilds inputs/ from the task's
// CURRENT attachments — renamed or removed files disappear — while the ledger
// at the run root survives.
func TestStableTaskWorkdirDropsRemovedAttachments(t *testing.T) {
	t.Setenv("FLEET_WORKSPACE_ROOT", t.TempDir())
	dataDir := t.TempDir()
	writeUpload(t, dataDir, "a_1.csv", "a")
	writeUpload(t, dataDir, "b_1.csv", "b")
	r := &Runner{cfg: &config.Config{DataDir: dataDir}}
	task := &models.Task{ID: uuid.New(), Files: []string{"a_1.csv", "b_1.csv"}, FileNames: []string{"a.csv", "b.csv"}}

	workdir, err := r.stableTaskWorkdir(task)
	if err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(workdir, "creates.jsonl")
	if err := os.WriteFile(ledger, []byte("deal-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inputs := filepath.Join(workdir, "inputs")

	// Operator edits the requeued task: drops b, renames a.
	task.Files, task.FileNames = []string{"a_1.csv"}, []string{"renamed.csv"}
	if _, err := r.stableTaskWorkdir(task); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if names := dirNames(t, inputs); len(names) != 1 || names[0] != "renamed.csv" {
		t.Fatalf("inputs after edit = %v; want [renamed.csv]", names)
	}
	if names := dirNames(t, workdir); len(names) != 2 {
		t.Fatalf("run dir should hold only the ledger and inputs/, got %v", names)
	}

	// Operator removes every attachment.
	task.Files, task.FileNames = nil, nil
	if _, err := r.stableTaskWorkdir(task); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if _, err := os.Lstat(inputs); !os.IsNotExist(err) {
		t.Fatalf("inputs must be gone once the task has no attachments: %v", err)
	}
	if b, err := os.ReadFile(ledger); err != nil || string(b) != "deal-1\n" {
		t.Fatalf("ledger lost: %q, %v", b, err)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
