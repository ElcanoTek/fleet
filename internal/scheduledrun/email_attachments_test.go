package scheduledrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The runner binds a run's email attachment step to that run's working
// directory and stages the copies under <EMAIL_ATTACHMENT_DIR>/outbound.
func TestScheduledEmailAttachmentSnapshotter(t *testing.T) {
	wsRoot := t.TempDir()
	job := filepath.Join(wsRoot, "tasks", "job-a")
	other := filepath.Join(wsRoot, "tasks", "job-b")
	for _, d := range []string{job, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(job, "r.csv"), []byte("r"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "x.csv"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	attachDir := t.TempDir()
	prepare := scheduledEmailAttachmentSnapshotter(wsRoot, job, attachDir)

	if _, err := prepare(map[string]any{"attachments": []any{map[string]any{"path": filepath.Join(other, "x.csv")}}}); err == nil {
		t.Fatal("another job's file was accepted")
	}
	args := map[string]any{"attachments": []any{map[string]any{"path": "r.csv"}}}
	cleanup, err := prepare(args)
	if err != nil {
		t.Fatal(err)
	}
	got := args["attachments"].([]any)[0].(map[string]any)["path"].(string)
	if !strings.HasPrefix(got, filepath.Join(attachDir, "outbound")+string(filepath.Separator)) {
		t.Fatalf("copy at %q", got)
	}
	cleanup()
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Fatal("copy not removed")
	}
}
