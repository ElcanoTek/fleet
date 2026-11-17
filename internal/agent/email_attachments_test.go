package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The chat turn's email attachment step is bound to its conversation: a path
// outside that conversation's workspace is refused, one inside it becomes a
// private copy under <EMAIL_ATTACHMENT_DIR>/outbound.
func TestEmailAttachmentSnapshotter_BoundToConversation(t *testing.T) {
	wsRoot := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", wsRoot)
	for _, dir := range []string{"conv-a", "conv-b"} {
		if err := os.MkdirAll(filepath.Join(wsRoot, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wsRoot, dir, "f.txt"), []byte(dir), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	attachDir := t.TempDir()
	prepare := emailAttachmentSnapshotter("conv-a", attachDir)

	refused := map[string]any{"attachments": []any{map[string]any{"path": filepath.Join(wsRoot, "conv-b", "f.txt")}}}
	if _, err := prepare(refused); err == nil {
		t.Fatal("an attachment from another conversation was accepted")
	}

	ok := map[string]any{"attachments": []any{map[string]any{"path": "f.txt"}}}
	cleanup, err := prepare(ok)
	if err != nil {
		t.Fatalf("own workspace attachment: %v", err)
	}
	defer cleanup()
	got := ok["attachments"].([]any)[0].(map[string]any)["path"].(string)
	if !strings.HasPrefix(got, filepath.Join(attachDir, "outbound")+string(filepath.Separator)) {
		t.Fatalf("copy at %q, want under %s/outbound", got, attachDir)
	}
	if data, err := os.ReadFile(got); err != nil || string(data) != "conv-a" {
		t.Fatalf("copy content %q err %v", data, err)
	}
}
