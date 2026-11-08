package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A relative EmailAttachmentDir (the "./data/attachments" default under a
// service cwd of /var/lib/fleet) must still yield ABSOLUTE rewritten paths:
// the email connector reads them from its own cwd (mcp-runs/task-<id>), where
// "data/attachments/outbound/..." does not exist. Regression for the
// scheduled send that failed nine times with "Inline attachment file not
// found".
func TestSnapshotEmailAttachmentsRelativeStagingYieldsAbsolutePaths(t *testing.T) {
	wsRoot := t.TempDir()
	job := filepath.Join(wsRoot, "tasks", "job-a")
	if err := os.MkdirAll(job, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(job, "logo.png"), []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	serviceCwd := t.TempDir()
	t.Chdir(serviceCwd)

	staging := OutboundAttachmentStagingDir("./data/attachments")
	if !filepath.IsAbs(staging) || !strings.HasSuffix(staging, filepath.Join("data", "attachments", "outbound")) {
		t.Fatalf("OutboundAttachmentStagingDir(relative) = %q, want an absolute .../data/attachments/outbound", staging)
	}
	if OutboundAttachmentStagingDir("  ") != "" {
		t.Fatal("an unset attachment dir must keep meaning the OS temp dir")
	}

	// Both the derived dir and a caller passing a relative parent directly.
	for _, parent := range []string{staging, filepath.Join("data", "attachments", "outbound")} {
		args := map[string]any{"inline_attachments": []any{map[string]any{"path": "logo.png", "cid": "logo"}}}
		cleanup, err := SnapshotEmailAttachmentsInDir(wsRoot, job, args, parent)
		if err != nil {
			t.Fatalf("parent %q: %v", parent, err)
		}
		got := args["inline_attachments"].([]any)[0].(map[string]any)["path"].(string)
		if !filepath.IsAbs(got) {
			t.Fatalf("parent %q: rewritten path %q is relative", parent, got)
		}
		// Read it from a different cwd, as the connector subprocess does.
		t.Chdir(t.TempDir())
		if b, err := os.ReadFile(got); err != nil || string(b) != "png" {
			t.Fatalf("parent %q: copy %q unreadable from another cwd: %q, %v", parent, got, b, err)
		}
		t.Chdir(serviceCwd)
		cleanup()
	}
}

// Bare-string attachment items ("attachments": ["report.xlsx"]) are checked,
// snapshotted and rewritten like {"path": ...} items, each keeping its shape.
func TestSnapshotEmailAttachmentsRewritesBareStrings(t *testing.T) {
	wsRoot := t.TempDir()
	job := filepath.Join(wsRoot, "tasks", "job-a")
	if err := os.MkdirAll(job, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"report.xlsx": "xlsx", "logo.png": "png"} {
		if err := os.WriteFile(filepath.Join(job, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	staging := filepath.Join(t.TempDir(), "outbound")
	args := map[string]any{
		"attachments":        []any{filepath.Join(job, "report.xlsx"), map[string]any{"path": "logo.png", "filename": "l.png"}, "", 42.0},
		"inline_attachments": []any{"logo.png"},
	}
	cleanup, err := SnapshotEmailAttachmentsInDir(wsRoot, job, args, staging)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	defer cleanup()

	attach := args["attachments"].([]any)
	bare, ok := attach[0].(string)
	if !ok {
		t.Fatalf("bare string item changed shape: %#v", attach[0])
	}
	obj, ok := attach[1].(map[string]any)
	if !ok || obj["filename"] != "l.png" {
		t.Fatalf("object item changed shape or lost fields: %#v", attach[1])
	}
	inline, ok := args["inline_attachments"].([]any)[0].(string)
	if !ok {
		t.Fatalf("bare inline item changed shape: %#v", args["inline_attachments"])
	}
	for path, want := range map[string]string{bare: "xlsx", obj["path"].(string): "png", inline: "png"} {
		if !strings.HasPrefix(path, staging+string(filepath.Separator)) {
			t.Errorf("%q is not a copy under the staging dir", path)
		}
		if b, err := os.ReadFile(path); err != nil || string(b) != want {
			t.Errorf("copy %q = %q, %v", path, b, err)
		}
	}
	if attach[2] != "" || attach[3] != 42.0 {
		t.Errorf("empty and non-path items must pass through, got %#v", attach[2:])
	}

	// A bare string outside the run's workspace is refused, not passed through.
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("s"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SnapshotEmailAttachmentsInDir(wsRoot, job, map[string]any{"attachments": []any{outside}}, staging); err == nil {
		t.Fatal("bare-string attachment outside the workspace was accepted")
	}
}

// The staging-time rewrite handles bare strings too: a relative name becomes
// the absolute workspace path, and the item stays a string.
func TestMaterializeAttachmentPathsRewritesBareStrings(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
	convID := "conv-bare"
	out, err := MaterializeAttachmentPaths(convID, `{"attachments":["report.csv"],"inline_attachments":["chart.png"]}`)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	var got map[string][]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if s, _ := got["attachments"][0].(string); s != filepath.Join(root, convID, "report.csv") {
		t.Errorf("attachments[0] = %#v", got["attachments"][0])
	}
	if s, _ := got["inline_attachments"][0].(string); s != filepath.Join(root, convID, "chart.png") {
		t.Errorf("inline_attachments[0] = %#v", got["inline_attachments"][0])
	}
	if _, err := MaterializeAttachmentPaths(convID, `{"attachments":["../escape.txt"]}`); err == nil {
		t.Error("a bare-string escape must be refused")
	}
}
