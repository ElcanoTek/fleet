package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/config"

	"github.com/ElcanoTek/fleet/internal/tools"
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

// TestValidateAttachments_ConfinesToUploadsRoot pins that validateAttachments
// only accepts regular files that live under <EmailAttachmentDir>/uploads: an
// absolute path or a ".." escape pointing outside the uploads root is dropped, so
// a hostile client can't smuggle a host file (e.g. a secret) into a turn's
// attachment set. This is the confinement the filepath.Rel + filepath.IsLocal
// gate enforces.
func TestValidateAttachments_ConfinesToUploadsRoot(t *testing.T) {
	base := t.TempDir()
	uploads := filepath.Join(base, "uploads", "tok")
	if err := os.MkdirAll(uploads, 0o755); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(uploads, "chart.png")
	if err := os.WriteFile(good, []byte("img"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A secret OUTSIDE the uploads root that a traversal / absolute path targets.
	secret := filepath.Join(base, "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Server{cfg: &config.Config{EmailAttachmentDir: base}}

	got := s.validateAttachments([]chatAttachment{
		{Name: "chart.png", Path: good},                                            // legit, under uploads root
		{Name: "escape", Path: secret},                                             // absolute, outside uploads root
		{Name: "traverse", Path: filepath.Join(uploads, "..", "..", "secret.txt")}, // ".." escape
	})

	if len(got) != 1 {
		t.Fatalf("want exactly 1 accepted attachment (the confined file), got %d: %#v", len(got), got)
	}
	if filepath.Clean(got[0].Path) != filepath.Clean(good) {
		t.Errorf("accepted path = %q; want the confined file %q", got[0].Path, good)
	}
}

func TestAppendAttachmentsBlock_ImagesAndOthers(t *testing.T) {
	images := []chatAttachment{{Name: "shot.png", Size: 100}}
	others := []chatAttachment{{Name: "data.csv", Path: "/uploads/abc/data.csv", Size: 1024}}
	got := appendAttachmentsBlock("hi", images, others, false)
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
	got := appendAttachmentsBlock("hello", nil, nil, false)
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

// ── postAttachments HTTP-level tests ─────────────────────────────────────
//
// The handler only reads s.cfg, so a bare Server with a temp attachment
// dir exercises the full multipart → size-check → save path without
// Postgres.

func attachmentServerFixture(t *testing.T, maxBytes int64) *Server {
	t.Helper()
	return &Server{
		cfg: &config.Config{
			EmailAttachmentDir: t.TempDir(),
			UploadMaxBytes:     maxBytes,
		},
	}
}

func multipartBody(t *testing.T, files map[string][]byte) (*bytes.Buffer, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, content := range files {
		part, err := mw.CreateFormFile("files", name)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := part.Write(content); err != nil {
			t.Fatalf("write part: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

func countUploadedFiles(t *testing.T, s *Server) int {
	t.Helper()
	return duTree(context.Background(), filepath.Join(s.cfg.EmailAttachmentDir, "uploads")).Files
}

func TestPostAttachments_SavesFilesUnderLimit(t *testing.T) {
	s := attachmentServerFixture(t, 1024)
	body, ct := multipartBody(t, map[string][]byte{
		"report.csv": []byte("a,b\n1,2\n"),
		"notes.txt":  []byte("hello"),
	})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/attachments", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()

	s.postAttachments(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Attachments []uploadedAttachment `json:"attachments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Attachments) != 2 {
		t.Fatalf("attachments = %d, want 2", len(resp.Attachments))
	}
	for _, a := range resp.Attachments {
		if _, err := os.Stat(filepath.FromSlash(a.Path)); err != nil {
			t.Errorf("saved file missing: %s: %v", a.Path, err)
		}
	}
}

func TestPostAttachments_OversizeIs413WithReadableMessage(t *testing.T) {
	// Cap 1 KiB → request cap 2 KiB. A 1.5 KiB file clears the request
	// cap (with multipart overhead) but trips the per-file check.
	s := attachmentServerFixture(t, 1024)
	body, ct := multipartBody(t, map[string][]byte{
		"big.bin": bytes.Repeat([]byte("x"), 1500),
	})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/attachments", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()

	s.postAttachments(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body: %s", rec.Code, rec.Body.String())
	}
	msg := rec.Body.String()
	if !strings.Contains(msg, `"big.bin"`) || !strings.Contains(msg, "upload limit") {
		t.Errorf("413 body should name the file and the limit, got: %s", msg)
	}
}

func TestPostAttachments_OversizeBatchSavesNothing(t *testing.T) {
	// An oversize file anywhere in the batch must reject the whole
	// request BEFORE any file is written, so no orphans wait on the
	// TTL sweep.
	s := attachmentServerFixture(t, 1024)
	body, ct := multipartBody(t, map[string][]byte{
		"ok.txt":  []byte("tiny"),
		"big.bin": bytes.Repeat([]byte("x"), 1500),
	})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/attachments", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()

	s.postAttachments(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body: %s", rec.Code, rec.Body.String())
	}
	if got := countUploadedFiles(t, s); got != 0 {
		t.Errorf("files on disk after rejected batch = %d, want 0", got)
	}
}

func TestPostAttachments_RequestCapIs413NotOpaque400(t *testing.T) {
	// A batch whose combined size trips http.MaxBytesReader used to
	// surface as `400 parse multipart: ... request body too large`.
	// It must come back as a 413 with actionable copy.
	s := attachmentServerFixture(t, 1024)
	body, ct := multipartBody(t, map[string][]byte{
		"a.bin": bytes.Repeat([]byte("x"), 1000),
		"b.bin": bytes.Repeat([]byte("x"), 1000),
		"c.bin": bytes.Repeat([]byte("x"), 1000),
	})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/attachments", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()

	s.postAttachments(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "combined request limit") {
		t.Errorf("413 body should explain the combined limit, got: %s", rec.Body.String())
	}
	if got := countUploadedFiles(t, s); got != 0 {
		t.Errorf("files on disk after rejected batch = %d, want 0", got)
	}
}

func TestPostAttachments_ZeroConfigFallsBackToDefault(t *testing.T) {
	// A zero-value config (old deployments, tests) must fall back to
	// defaultMaxUploadBytes rather than rejecting everything.
	s := attachmentServerFixture(t, 0)
	body, ct := multipartBody(t, map[string][]byte{"a.txt": []byte("hi")})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/attachments", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()

	s.postAttachments(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
}

// ── kubernetes attachment staging (docs/DEPLOYMENT-KUBERNETES.md) ──
//
// Load-bearing assertions: staging copies a validated attachment into the
// conversation workspace's attachments/ dir and rewrites Path to the staged
// copy; re-staging the same metadata (the queue-drain echo) reuses the copy
// instead of duplicating; a same-name different-size attachment gets a
// numbered variant rather than clobbering; a per-entry failure keeps that
// entry's uploads path instead of failing the turn; and the staged-variant
// prompt trailer replaces the uploads-TTL wording.

func TestStageAttachmentsIntoWorkspace(t *testing.T) {
	t.Setenv("FLEET_WORKSPACE_ROOT", t.TempDir())
	uploads := t.TempDir()
	src := filepath.Join(uploads, "tok1", "data.csv")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("a,b\n1,2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	atts := []chatAttachment{{Name: "data.csv", Path: src, Size: 8}}

	staged := stageAttachmentsIntoWorkspace(uploads, "conv-stage", atts)
	if len(staged) != 1 {
		t.Fatalf("staged = %v", staged)
	}
	want := filepath.Join(tools.WorkspaceDirForConversation("conv-stage"), "attachments", "data.csv")
	if filepath.Clean(staged[0].Path) != filepath.Clean(want) {
		t.Fatalf("staged path = %q, want %q", staged[0].Path, want)
	}
	if got, err := os.ReadFile(staged[0].Path); err != nil || string(got) != "a,b\n1,2\n" {
		t.Fatalf("staged content = (%q, %v)", got, err)
	}

	// Queue-drain echo: same name + size reuses the staged copy.
	again := stageAttachmentsIntoWorkspace(uploads, "conv-stage", atts)
	if again[0].Path != staged[0].Path {
		t.Fatalf("re-stage path = %q, want the reused %q", again[0].Path, staged[0].Path)
	}
	entries, _ := os.ReadDir(filepath.Dir(want))
	if len(entries) != 1 {
		t.Fatalf("re-stage duplicated the file: %v", entries)
	}

	// A NEW attachment reusing the filename (different size) gets a variant,
	// never a clobber of the copy the agent may already have read.
	src2 := filepath.Join(uploads, "tok2", "data.csv")
	if err := os.MkdirAll(filepath.Dir(src2), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src2, []byte("different bytes!"), 0o600); err != nil {
		t.Fatal(err)
	}
	variant := stageAttachmentsIntoWorkspace(uploads, "conv-stage", []chatAttachment{{Name: "data.csv", Path: src2, Size: 16}})
	if got := filepath.Base(variant[0].Path); got != "data-2.csv" {
		t.Fatalf("variant name = %q, want data-2.csv", got)
	}
	if got, err := os.ReadFile(filepath.Clean(variant[0].Path)); err != nil || string(got) != "different bytes!" {
		t.Fatalf("variant content = (%q, %v)", got, err)
	}
	if got, err := os.ReadFile(staged[0].Path); err != nil || string(got) != "a,b\n1,2\n" {
		t.Fatalf("original staged copy was clobbered: (%q, %v)", got, err)
	}
}

func TestStageAttachmentsIntoWorkspace_FailureKeepsUploadsPath(t *testing.T) {
	t.Setenv("FLEET_WORKSPACE_ROOT", t.TempDir())
	uploads := t.TempDir()
	atts := []chatAttachment{{Name: "gone.bin", Path: filepath.Join(uploads, "missing"), Size: 4}}
	out := stageAttachmentsIntoWorkspace(uploads, "conv-stage-fail", atts)
	if len(out) != 1 || out[0].Path != atts[0].Path {
		t.Fatalf("failed staging should keep the uploads path, got %v", out)
	}
}

func TestStageAttachmentsIntoWorkspace_RefusesEscapedSource(t *testing.T) {
	// The stager must not trust its caller: a Path outside the uploads root —
	// a flow that skipped validateAttachments — is kept unstaged, never opened
	// relative to anything.
	t.Setenv("FLEET_WORKSPACE_ROOT", t.TempDir())
	uploads := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := stageAttachmentsIntoWorkspace(uploads, "conv-escape", []chatAttachment{{Name: "secret.txt", Path: outside, Size: 6}})
	if len(out) != 1 || out[0].Path != outside {
		t.Fatalf("escaped source should stay unstaged, got %v", out)
	}
	convAtt := filepath.Join(tools.WorkspaceDirForConversation("conv-escape"), "attachments")
	if entries, _ := os.ReadDir(convAtt); len(entries) != 0 {
		t.Fatalf("escaped source was staged: %v", entries)
	}
}

func TestAppendAttachmentsBlock_StagedTrailer(t *testing.T) {
	others := []chatAttachment{{Name: "d.csv", Path: "/ws/conv/attachments/d.csv", Size: 8}}
	got := appendAttachmentsBlock("hi", nil, others, true)
	if !strings.Contains(got, "conversation's workspace") {
		t.Errorf("staged trailer missing in:\n%s", got)
	}
	if strings.Contains(got, "temporary uploads area") {
		t.Errorf("uploads-TTL trailer should not appear for staged attachments:\n%s", got)
	}
}
