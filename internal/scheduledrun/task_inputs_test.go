package scheduledrun

import (
	"os"
	"path/filepath"
	"testing"

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
