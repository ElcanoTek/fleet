package httpapi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSplitAttachmentsByKind(t *testing.T) {
	atts := []chatAttachment{
		{Name: "a.png", Path: "/uploads/a.png", Size: 100, MIME: "image/png"},
		{Name: "b.jpg", Path: "/uploads/b.jpg", Size: 100, MIME: ""}, // ext fallback
		{Name: "c.csv", Path: "/uploads/c.csv", Size: 50, MIME: "text/csv"},
		{Name: "d.svg", Path: "/uploads/d.svg", Size: 50, MIME: "image/svg+xml"}, // SVG excluded
	}
	images, others := splitAttachmentsByKind(atts)
	if len(images) != 2 {
		t.Fatalf("expected 2 images, got %d (%+v)", len(images), images)
	}
	if images[0].Name != "a.png" || images[1].Name != "b.jpg" {
		t.Errorf("unexpected image order: %+v", images)
	}
	if images[1].MIME != "image/jpeg" {
		t.Errorf("ext-fallback MIME not applied: %+v", images[1])
	}
	if len(others) != 2 {
		t.Errorf("expected 2 others (csv + svg), got %d", len(others))
	}
}

func TestToAgentImageAttachments(t *testing.T) {
	in := []chatAttachment{
		{Name: "x.png", Path: "/p/x.png", Size: 1, MIME: "image/png"},
		{Name: "y.JPG", Path: "/p/y.JPG", Size: 1, MIME: ""},
	}
	out := toAgentImageAttachments(in)
	if len(out) != 2 {
		t.Fatalf("got %d", len(out))
	}
	if out[0].MediaType != "image/png" {
		t.Errorf("[0] media = %q", out[0].MediaType)
	}
	if out[1].MediaType != "image/jpeg" {
		t.Errorf("[1] media = %q (extension fallback)", out[1].MediaType)
	}
}

func TestAppendAttachmentsBlock_ImagesAndOthers(t *testing.T) {
	images := []chatAttachment{{Name: "shot.png", Size: 100}}
	others := []chatAttachment{{Name: "data.csv", Path: "/uploads/abc/data.csv", Size: 1024}}
	got := appendAttachmentsBlock("hi", images, others)
	if !strings.Contains(got, "User attached images") {
		t.Errorf("missing image header in:\n%s", got)
	}
	if !strings.Contains(got, "vision input") {
		t.Errorf("missing vision-input note in:\n%s", got)
	}
	if !strings.Contains(got, "do NOT call view_file") {
		t.Errorf("missing view_file warning in:\n%s", got)
	}
	if !strings.Contains(got, "User attached files") {
		t.Errorf("missing files header for non-image attachments in:\n%s", got)
	}
	if !strings.Contains(got, "/uploads/abc/data.csv") {
		t.Errorf("non-image attachment path missing in:\n%s", got)
	}
}

func TestAppendAttachmentsBlock_NoAttachmentsIsNoOp(t *testing.T) {
	got := appendAttachmentsBlock("hello", nil, nil)
	if got != "hello" {
		t.Errorf("expected unchanged, got %q", got)
	}
}

func TestAppendWorkspaceInventory_EmptyDirIsNoOp(t *testing.T) {
	dir := t.TempDir()
	got := appendWorkspaceInventoryBlock("hi", dir)
	if got != "hi" {
		t.Errorf("expected unchanged for empty dir, got:\n%s", got)
	}
}

func TestAppendWorkspaceInventory_MissingDirIsNoOp(t *testing.T) {
	// First turn of a new conv: the workspace dir doesn't exist yet.
	got := appendWorkspaceInventoryBlock("hi", filepath.Join(t.TempDir(), "does-not-exist"))
	if got != "hi" {
		t.Errorf("expected unchanged when dir is missing, got:\n%s", got)
	}
}

func TestAppendWorkspaceInventory_BlankWorkspaceDirIsNoOp(t *testing.T) {
	got := appendWorkspaceInventoryBlock("hi", "")
	if got != "hi" {
		t.Errorf("expected unchanged when workspaceDir is blank, got:\n%s", got)
	}
}

func TestAppendWorkspaceInventory_ListsRegularFilesNewestFirst(t *testing.T) {
	dir := t.TempDir()
	// Older file first, then newer — write order shouldn't matter.
	if err := os.WriteFile(filepath.Join(dir, "older.csv"), []byte("a,b\n1,2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	older := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "older.csv"), older, older); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "newer.xlsx"), []byte("xlsx-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := appendWorkspaceInventoryBlock("question?", dir)
	if !strings.Contains(got, "Workspace files persisted from earlier turns") {
		t.Fatalf("missing inventory header in:\n%s", got)
	}
	newerIdx := strings.Index(got, "newer.xlsx")
	olderIdx := strings.Index(got, "older.csv")
	if newerIdx < 0 || olderIdx < 0 {
		t.Fatalf("missing entries in:\n%s", got)
	}
	if newerIdx > olderIdx {
		t.Errorf("expected newer file listed before older; got:\n%s", got)
	}
}

func TestAppendWorkspaceInventory_SkipsSymlinksAndDotfilesAndEmptyFiles(t *testing.T) {
	dir := t.TempDir()
	// Symlink targeting an external dir — the protocols/personas/system_prompts
	// symlinks EnsureWorkspaceDir installs should never be surfaced as state.
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(dir, "protocols")); err != nil {
		t.Skipf("symlink not supported on this platform: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".hidden"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "empty.csv"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "real.csv"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := appendWorkspaceInventoryBlock("hi", dir)
	if strings.Contains(got, "protocols") {
		t.Errorf("inventory must skip symlinks, got:\n%s", got)
	}
	if strings.Contains(got, ".hidden") {
		t.Errorf("inventory must skip dotfiles, got:\n%s", got)
	}
	if strings.Contains(got, "empty.csv") {
		t.Errorf("inventory must skip zero-byte files, got:\n%s", got)
	}
	if !strings.Contains(got, "real.csv") {
		t.Errorf("missing real.csv in:\n%s", got)
	}
}

func TestAppendWorkspaceInventory_CapsLongListings(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < maxWorkspaceInventoryEntries+5; i++ {
		name := filepath.Join(dir, "file-"+strings.Repeat("x", i+1)+".csv")
		if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := appendWorkspaceInventoryBlock("hi", dir)
	if !strings.Contains(got, "and 5 more") {
		t.Errorf("expected overflow marker for 5 truncated entries, got:\n%s", got)
	}
}
