package agentcore

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/logging"
)

// The warning for a planted entry the run dir setup removed carries the
// run's correlation (run_id, task_id): the sandbox planted it during THAT run
// (#2074 review).
func TestPlantedEntryWarningCarriesTheRunContext(t *testing.T) {
	t.Setenv("FLEET_WORKSPACE_ROOT", t.TempDir())
	dir, run, err := OpenStableMCPWorkspace(t.Context(), "task-ctx")
	if err != nil {
		t.Fatal(err)
	}
	_ = run.Close()
	if err := os.Symlink(os.DevNull, filepath.Join(dir, "creates.jsonl")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler("json", &buf, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx := logging.WithTask(context.Background(), "task-7", "run-42")
	_, run, err = OpenStableMCPWorkspace(ctx, "task-ctx")
	if err != nil {
		t.Fatal(err)
	}
	_ = run.Close()
	out := buf.String()
	if !strings.Contains(out, "removed planted entry") || !strings.Contains(out, `"run_id":"run-42"`) || !strings.Contains(out, `"task_id":"task-7"`) {
		t.Fatalf("planted-entry warning without the run's correlation: %s", out)
	}
}
