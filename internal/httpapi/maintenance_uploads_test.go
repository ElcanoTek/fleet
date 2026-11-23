package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/testdb/fleetdb"
)

// maintenanceUploadsFixture is a Server on a store of its own with a private
// attachment directory, plus a raw handle on the same database so a test can
// age a conversation the way weeks of inactivity would.
func maintenanceUploadsFixture(t *testing.T) (*Server, *store.Store, *sql.DB, string) {
	t.Helper()
	db := fleetdb.New(t)
	st, err := store.Open(db.ChatDSN(), fleetdb.ChatPool())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// What the first server of a release does at boot: until it has run, no
	// upload is removed.
	if _, err := st.BackfillMessageUploads(context.Background()); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	raw, err := sql.Open("pgx", db.ChatDSN())
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	attachDir := t.TempDir()
	srv := &Server{
		cfg: &config.Config{
			PersonaDefault:     "victoria",
			ConversationTTL:    14,
			EmailAttachmentDir: attachDir,
		},
		store:    st,
		dir:      st.Identity(),
		inflight: make(map[string]inflightEntry),
		isMember: allowAllMembers,
	}
	return srv, st, raw, attachDir
}

// writeAgedUpload writes an upload where POST /attachments puts one and ages
// it past the 14-day window.
func writeAgedUpload(t *testing.T, attachDir, user, token, name string, age time.Duration) string {
	t.Helper()
	dir := filepath.Join(userUploadsRoot(attachDir, user), token)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("\x89PNG fake"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-age)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	return p
}

// sendImage records a user message that attached the image at path, the way a
// turn persists it (TextContent.Images, replayed on every later turn).
func sendImage(t *testing.T, st *store.Store, convID, path string) {
	t.Helper()
	content, err := json.Marshal(agent.TextContent{
		Text:   "what is in this picture?",
		Images: []agent.ImageRefMeta{{Path: path, MediaType: "image/png", Name: filepath.Base(path)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendHistory(context.Background(), convID, []agent.HistoryEntry{{Role: "user", Type: "text", Content: content}}); err != nil {
		t.Fatalf("append history: %v", err)
	}
}

func exists(t *testing.T, p string) bool {
	t.Helper()
	_, err := os.Stat(p)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", p, err)
	}
	return err == nil
}

// TestRunMaintenanceKeepsUploadsOfKeptConversations: the hourly cleanup removes
// an upload only once no conversation that is still kept refers to it. An image
// sent in a pinned chat, or in a temporary chat someone is still using, stays
// however old the file is, and so does a Multiplayer participant's stashed
// attachment; an upload never sent, and the upload of a chat that expired, are
// removed.
func TestRunMaintenanceKeepsUploadsOfKeptConversations(t *testing.T) {
	// FLEET_WORKSPACE_ROOT is process-wide: RunMaintenance also sweeps the
	// workspace root, which must be this test's own.
	t.Setenv("FLEET_WORKSPACE_ROOT", t.TempDir())
	srv, st, raw, attachDir := maintenanceUploadsFixture(t)
	ctx := context.Background()
	const user = "keeper@example.com"
	seedUser(t, st, user)
	const old = 30 * 24 * time.Hour

	pinned, err := st.CreateConversation(ctx, user, "pinned", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	pinnedImg := writeAgedUpload(t, attachDir, user, "pinnedtoken", "chart.png", old)
	sendImage(t, st, pinned.ID, pinnedImg)
	if err := st.SetPinned(ctx, user, pinned.ID, true); err != nil {
		t.Fatal(err)
	}
	// Weeks without activity: only the pin keeps this chat.
	if _, err := raw.ExecContext(ctx, `UPDATE conversations SET updated_at = $1 WHERE id = $2`, time.Now().Add(-old).Unix(), pinned.ID); err != nil {
		t.Fatal(err)
	}

	// A temporary chat still in use: the image is old, the chat is not.
	active, err := st.CreateConversation(ctx, user, "active", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	activeImg := writeAgedUpload(t, attachDir, user, "activetoken", "photo.png", old)
	sendImage(t, st, active.ID, activeImg)

	// A temporary chat past the window: it expires on this pass, and its
	// upload goes with it.
	expiring, err := st.CreateConversation(ctx, user, "expiring", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	expiringImg := writeAgedUpload(t, attachDir, user, "expiringtoken", "scan.png", old)
	sendImage(t, st, expiring.ID, expiringImg)
	if _, err := raw.ExecContext(ctx, `UPDATE conversations SET updated_at = $1 WHERE id = $2`, time.Now().Add(-old).Unix(), expiring.ID); err != nil {
		t.Fatal(err)
	}

	// A Multiplayer participant's attachment, stashed between their turns.
	stashed := filepath.Join(attachDir, store.AttachmentPrivateStashDir, active.ID, "participants", "p1", "attachments", "data.csv")
	if err := os.MkdirAll(filepath.Dir(stashed), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stashed, []byte("a,b"), 0o600); err != nil {
		t.Fatal(err)
	}
	if past := time.Now().Add(-old); os.Chtimes(stashed, past, past) != nil {
		t.Fatal("chtimes stash")
	}

	// Uploaded and never sent.
	neverSent := writeAgedUpload(t, attachDir, user, "unsenttoken", "draft.png", old)
	// Uploaded a moment ago and not sent yet (a message being composed).
	fresh := writeAgedUpload(t, attachDir, user, "freshtoken", "new.png", 0)

	srv.RunMaintenance(ctx)

	if !exists(t, pinnedImg) {
		t.Error("an image sent in a pinned chat was removed by the hourly cleanup")
	}
	if !exists(t, activeImg) {
		t.Error("an image sent in a temporary chat that is still in use was removed by the hourly cleanup")
	}
	if !exists(t, stashed) {
		t.Error("a Multiplayer participant's stashed attachment was removed by the hourly cleanup")
	}
	if exists(t, expiringImg) {
		t.Error("the image of a chat that expired on this pass is still on disk")
	}
	if exists(t, neverSent) {
		t.Error("an upload never sent in any message outlived the window")
	}
	if !exists(t, fresh) {
		t.Error("a fresh upload was removed")
	}

	// Once the pinned chat is deleted, nothing keeps its image.
	if err := st.Delete(ctx, user, pinned.ID); err != nil {
		t.Fatal(err)
	}
	srv.RunMaintenance(ctx)
	if exists(t, pinnedImg) {
		t.Error("the image of a deleted chat is still on disk after the next cleanup")
	}
}

// TestAdminStorageCleanupKeepsFilesStillInUse: "Clean up now" with a one-day
// cutoff removes the uploads and task files nothing uses (a task file with
// its .checksums and .uploaders sidecars, counted once), and leaves an image
// sent in a pinned chat and a file a task still names, sidecars included.
func TestAdminStorageCleanupKeepsFilesStillInUse(t *testing.T) {
	t.Setenv("FLEET_WORKSPACE_ROOT", t.TempDir())
	srv, st, _, attachDir := maintenanceUploadsFixture(t)
	srv.cfg.DataDir = t.TempDir()
	ctx := context.Background()
	const user = "admin-cleanup@example.com"
	seedUser(t, st, user)
	const old = 3 * 24 * time.Hour

	pinned, err := st.CreateConversation(ctx, user, "pinned", "victoria", "", false)
	if err != nil {
		t.Fatal(err)
	}
	pinnedImg := writeAgedUpload(t, attachDir, user, "pinnedtoken", "chart.png", old)
	sendImage(t, st, pinned.ID, pinnedImg)
	if err := st.SetPinned(ctx, user, pinned.ID, true); err != nil {
		t.Fatal(err)
	}
	neverSent := writeAgedUpload(t, attachDir, user, "unsenttoken", "draft.png", old)

	tempDir := filepath.Join(srv.cfg.DataDir, "temp_uploads")
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		t.Fatal(err)
	}
	taskFile, unusedTemp := filepath.Join(tempDir, "brief_1a2b3c4d.txt"), filepath.Join(tempDir, "draft_5e6f7a8b.txt")
	sidecars := func(name string) []string {
		return []string{filepath.Join(tempDir, ".checksums", name+".sha256"), filepath.Join(tempDir, ".uploaders", name)}
	}
	for _, p := range append(append([]string{taskFile, unusedTemp}, sidecars("brief_1a2b3c4d.txt")...), sidecars("draft_5e6f7a8b.txt")...) {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		past := time.Now().Add(-old)
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatal(err)
		}
	}
	// The scheduler's sweep, with a keep rule that names the task file.
	srv.tempUploadSweeper = func(_ context.Context, dir string, window time.Duration) int {
		return storage.SweepTempUploadsWith(dir, window, func(path string) (bool, error) { return path == taskFile, nil }, nil).Removed
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/admin/storage/cleanup",
		strings.NewReader(`{"older_than_days":1,"delete_conversations":true,"sweep_files":true}`))
	srv.handleAdminStorageCleanup(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cleanup: %d %s", rec.Code, rec.Body.String())
	}
	if !exists(t, pinnedImg) {
		t.Error("the admin cleanup removed an image sent in a pinned chat")
	}
	if !exists(t, taskFile) {
		t.Error("the admin cleanup removed a file a task still names")
	}
	if exists(t, neverSent) {
		t.Error("the admin cleanup kept an upload never sent")
	}
	if exists(t, unusedTemp) {
		t.Error("the admin cleanup kept a temp upload no task names")
	}
	for _, p := range sidecars("brief_1a2b3c4d.txt") {
		if !exists(t, p) {
			t.Errorf("the admin cleanup removed %s, a sidecar of a file a task still names", p)
		}
	}
	for _, p := range sidecars("draft_5e6f7a8b.txt") {
		if exists(t, p) {
			t.Errorf("the admin cleanup left %s behind after removing its upload", p)
		}
	}
	var got storageCleanupResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.RemovedTempFiles != 1 {
		t.Errorf("removed_temp_files = %d, want 1 (sidecars are not counted)", got.RemovedTempFiles)
	}
}
