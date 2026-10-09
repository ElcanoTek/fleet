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
	dst := filepath.Join(t.TempDir(), "inputs")
	task := &models.Task{Files: []string{"domains_deadbeef.csv"}, FileNames: []string{"domains.csv"}}
	if err := r.stageTaskInputs(task, dst); err != nil {
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
	if err := r.stageTaskInputs(task, filepath.Join(t.TempDir(), "inputs")); err == nil {
		t.Fatal("expected alias mismatch error")
	}
}

// TestStageTaskInputsIdempotentOnReuse pins the retry path: re-staging into the
// same inputs dir overwrites with the current bytes, leaves no temp files and
// sweeps a crashed attempt's leftover.
func TestStageTaskInputsIdempotentOnReuse(t *testing.T) {
	dataDir := t.TempDir()
	uploads := filepath.Join(dataDir, "temp_uploads")
	if err := os.MkdirAll(uploads, 0o700); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(uploads, "d_1.csv")
	if err := os.WriteFile(src, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &Runner{cfg: &config.Config{DataDir: dataDir}}
	dst := filepath.Join(t.TempDir(), "inputs")
	task := &models.Task{Files: []string{"d_1.csv"}, FileNames: []string{"d.csv"}}
	if err := r.stageTaskInputs(task, dst); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, stagingTempPrefix+"crashed"), []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.stageTaskInputs(task, dst); err != nil {
		t.Fatalf("re-stage: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "d.csv"))
	if err != nil || string(got) != "v2" {
		t.Fatalf("staged = %q, %v; want v2", got, err)
	}
	if fi, err := os.Stat(filepath.Join(dst, "d.csv")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("staged mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
	entries, _ := os.ReadDir(dst)
	if len(entries) != 1 {
		t.Fatalf("inputs dir should hold only d.csv, got %d entries", len(entries))
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
