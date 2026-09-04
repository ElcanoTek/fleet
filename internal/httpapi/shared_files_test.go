package httpapi

// Load-bearing assertions for the cross-chat shared file library
// (docs/SHARED-FILES.md): members can list and download but not mutate
// (403 before any side effect), viewers are read-only via the mutate gate,
// admins can upload/rename/delete, every mutation keeps the staged tree in
// step with the manifest, the library-total cap rejects with 413 BEFORE any
// byte lands, and the per-turn prompt block advertises the shared/ paths.

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

	"github.com/ElcanoTek/fleet/internal/store"
)

// sharedFilesFixture is serverFixture plus the on-disk trees and an admin +
// member + viewer trio.
func sharedFilesFixture(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	srv := serverFixture(t)
	srv.cfg.DataDir = t.TempDir()
	srv.cfg.WorkspaceRoot = t.TempDir()
	// The admin gate reads the role membershipMiddleware enriches the request
	// with, and the allow-all isMember seam skips that enrichment — so this
	// suite seeds real users and takes the real membership path.
	srv.isMember = nil
	seedUser(t, srv.concreteStore(t), "admin@x")
	seedUser(t, srv.concreteStore(t), "member@x")
	seedUser(t, srv.concreteStore(t), "viewer@x")
	setRole(t, srv, "admin@x", store.RoleAdmin, "")
	setRole(t, srv, "viewer@x", store.RoleViewer, "")
	return srv, srv.Routes()
}

// uploadShared POSTs files as multipart form-data the way the web tier does.
func uploadShared(t *testing.T, h http.Handler, user, folder, description string, files map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if folder != "" {
		_ = mw.WriteField("folder", folder)
	}
	if description != "" {
		_ = mw.WriteField("description", description)
	}
	for name, content := range files {
		fw, err := mw.CreateFormFile("files", name)
		if err != nil {
			t.Fatalf("form file: %v", err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatalf("write form file: %v", err)
		}
	}
	_ = mw.Close()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/shared-files", &buf)
	req.Header.Set("X-Chat-Server-Token", "tok")
	req.Header.Set("X-User-Email", user)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestSharedFilesLifecycle(t *testing.T) {
	srv, h := sharedFilesFixture(t)
	stagedRoot := srv.sharedFilesLibrary().StagedRoot

	// Upload two files into a folder, as the admin.
	w := uploadShared(t, h, "admin@x", "History", "2019 dataset", map[string]string{
		"sales 2019.csv": "a,b\n1,2\n",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("upload: status %d body %s", w.Code, w.Body.String())
	}
	var created struct {
		Files []store.SharedFile `json:"files"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || len(created.Files) != 1 {
		t.Fatalf("upload resp: %v (%s)", err, w.Body.String())
	}
	f := created.Files[0]
	if f.Folder != "History" || f.Name != "sales 2019.csv" || f.SizeBytes != 8 || f.SHA256 == "" || f.UploadedBy != "admin@x" {
		t.Fatalf("created row = %+v", f)
	}
	// Staged copy landed at the manifest path.
	if got, err := os.ReadFile(filepath.Join(stagedRoot, "History", "sales 2019.csv")); err != nil || string(got) != "a,b\n1,2\n" {
		t.Fatalf("staged copy = (%q, %v)", got, err)
	}

	// Duplicate name in the same folder → 409.
	if w := uploadShared(t, h, "admin@x", "History", "", map[string]string{"sales 2019.csv": "x"}); w.Code != http.StatusConflict {
		t.Fatalf("duplicate upload: status %d body %s", w.Code, w.Body.String())
	}

	// Members can list; the payload carries totals for the usage meter.
	w = do(t, h, http.MethodGet, "/shared-files", nil, "member@x")
	if w.Code != http.StatusOK {
		t.Fatalf("member list: status %d body %s", w.Code, w.Body.String())
	}
	var listed sharedFilesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil || len(listed.Files) != 1 || listed.TotalBytes != 8 {
		t.Fatalf("member list resp: %v (%s)", err, w.Body.String())
	}

	// Members can download the canonical bytes.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/shared-files/"+f.ID+"/download", nil)
	req.Header.Set("X-Chat-Server-Token", "tok")
	req.Header.Set("X-User-Email", "member@x")
	dl := httptest.NewRecorder()
	h.ServeHTTP(dl, req)
	if dl.Code != http.StatusOK || dl.Body.String() != "a,b\n1,2\n" {
		t.Fatalf("download: status %d body %q", dl.Code, dl.Body.String())
	}

	// PATCH renames + moves to the root; the staged tree follows.
	w = do(t, h, http.MethodPatch, "/shared-files/"+f.ID, map[string]any{"name": "renamed.csv", "folder": "", "description": "moved"}, "admin@x")
	if w.Code != http.StatusOK {
		t.Fatalf("patch: status %d body %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(stagedRoot, "History")); !os.IsNotExist(err) {
		t.Fatalf("old staged folder survived rename: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(stagedRoot, "renamed.csv")); err != nil || string(got) != "a,b\n1,2\n" {
		t.Fatalf("restaged copy = (%q, %v)", got, err)
	}

	// The prompt block advertises the shared/ path and the description.
	block := srv.appendSharedFilesBlock(context.Background(), "hello")
	if !strings.Contains(block, "`shared/renamed.csv`") || !strings.Contains(block, "moved") {
		t.Fatalf("prompt block = %q", block)
	}

	// DELETE removes the row, the staged copy, and the canonical bytes.
	w = do(t, h, http.MethodDelete, "/shared-files/"+f.ID, nil, "admin@x")
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete: status %d body %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(stagedRoot, "renamed.csv")); !os.IsNotExist(err) {
		t.Fatalf("staged copy survived delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(srv.sharedFilesLibrary().CanonicalDir, f.ID)); !os.IsNotExist(err) {
		t.Fatalf("canonical bytes survived delete: %v", err)
	}
	// An empty library appends no block.
	if got := srv.appendSharedFilesBlock(context.Background(), "hello"); got != "hello" {
		t.Fatalf("empty-library prompt block = %q", got)
	}
}

func TestSharedFilesAuthorization(t *testing.T) {
	srv, h := sharedFilesFixture(t)

	// A member cannot mutate — and the 403 happens before any side effect.
	if w := uploadShared(t, h, "member@x", "", "", map[string]string{"x.txt": "x"}); w.Code != http.StatusForbidden {
		t.Fatalf("member upload: status %d", w.Code)
	}
	if entries, _ := os.ReadDir(srv.sharedFilesLibrary().CanonicalDir); len(entries) != 0 {
		t.Fatalf("member upload left canonical files: %v", entries)
	}
	if w := do(t, h, http.MethodPatch, "/shared-files/whatever", map[string]any{"name": "y"}, "member@x"); w.Code != http.StatusForbidden {
		t.Fatalf("member patch: status %d", w.Code)
	}
	if w := do(t, h, http.MethodDelete, "/shared-files/whatever", nil, "member@x"); w.Code != http.StatusForbidden {
		t.Fatalf("member delete: status %d", w.Code)
	}

	// A viewer is blocked one layer earlier, by the read-only-role gate.
	if w := uploadShared(t, h, "viewer@x", "", "", map[string]string{"x.txt": "x"}); w.Code != http.StatusForbidden {
		t.Fatalf("viewer upload: status %d", w.Code)
	}
	// …but reads work for everyone.
	if w := do(t, h, http.MethodGet, "/shared-files", nil, "viewer@x"); w.Code != http.StatusOK {
		t.Fatalf("viewer list: status %d", w.Code)
	}

	// Unknown id surfaces as 404, occupied rename as 409.
	if w := do(t, h, http.MethodPatch, "/shared-files/missing", map[string]any{"name": "y"}, "admin@x"); w.Code != http.StatusNotFound {
		t.Fatalf("patch missing: status %d body %s", w.Code, w.Body.String())
	}
}

func TestSharedFilesQuota(t *testing.T) {
	srv, h := sharedFilesFixture(t)
	srv.cfg.SharedFilesMaxTotalMB = 1 // 1 MiB library cap

	if w := uploadShared(t, h, "admin@x", "", "", map[string]string{"small.txt": "fits"}); w.Code != http.StatusOK {
		t.Fatalf("small upload: status %d body %s", w.Code, w.Body.String())
	}
	big := strings.Repeat("x", 1<<20) // pushes the total past 1 MiB
	w := uploadShared(t, h, "admin@x", "", "", map[string]string{"big.bin": big})
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-quota upload: status %d body %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "shared_files_max_total_mb") {
		t.Fatalf("over-quota message should name the knob: %s", w.Body.String())
	}
	// Refused before any byte landed: no canonical file, no staged file, no row.
	if entries, _ := os.ReadDir(srv.sharedFilesLibrary().CanonicalDir); len(entries) != 1 {
		t.Fatalf("over-quota upload left canonical files: %v", entries)
	}
	files, err := srv.store.ListSharedFiles(context.Background())
	if err != nil || len(files) != 1 {
		t.Fatalf("library after refused upload: %v, %v", files, err)
	}

	// 0 = unlimited.
	srv.cfg.SharedFilesMaxTotalMB = 0
	if w := uploadShared(t, h, "admin@x", "", "", map[string]string{"big.bin": big}); w.Code != http.StatusOK {
		t.Fatalf("unlimited upload: status %d body %s", w.Code, w.Body.String())
	}
}

func TestSharedFilesSyncSelfHeals(t *testing.T) {
	srv, h := sharedFilesFixture(t)
	if w := uploadShared(t, h, "admin@x", "", "", map[string]string{"heal.txt": "content"}); w.Code != http.StatusOK {
		t.Fatalf("upload: status %d", w.Code)
	}
	staged := filepath.Join(srv.sharedFilesLibrary().StagedRoot, "heal.txt")
	if err := os.Remove(staged); err != nil {
		t.Fatalf("simulate drift: %v", err)
	}
	if err := srv.SyncSharedFiles(context.Background()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if got, err := os.ReadFile(staged); err != nil || string(got) != "content" {
		t.Fatalf("staged copy not healed: (%q, %v)", got, err)
	}
}

// TestSharedFilesPathNamespaceConflict409 walks the file-vs-folder collision
// end to end: it must come back 409 (a conflict the admin can act on), not 500,
// and — because the upload streams bytes before the row is written — the
// refused upload must leave neither canonical bytes nor a staged file behind.
func TestSharedFilesPathNamespaceConflict409(t *testing.T) {
	srv, h := sharedFilesFixture(t)
	lib := srv.sharedFilesLibrary()

	// A file inside folder "q3" claims the path "shared/q3" as a directory.
	if w := uploadShared(t, h, "admin@x", "q3", "", map[string]string{"x.csv": "a,b\n"}); w.Code != http.StatusOK {
		t.Fatalf("seed upload: status %d body %s", w.Code, w.Body.String())
	}
	canonicalBefore, _ := os.ReadDir(lib.CanonicalDir)

	// A root-level file named "q3" would need the same path as a plain file.
	w := uploadShared(t, h, "admin@x", "", "", map[string]string{"q3": "collides"})
	if w.Code != http.StatusConflict {
		t.Fatalf("colliding upload: status %d (want 409) body %s", w.Code, w.Body.String())
	}

	// No orphaned canonical bytes: the handler removes what it streamed.
	canonicalAfter, _ := os.ReadDir(lib.CanonicalDir)
	if len(canonicalAfter) != len(canonicalBefore) {
		t.Fatalf("refused upload leaked canonical bytes: %d -> %d", len(canonicalBefore), len(canonicalAfter))
	}
	// And "shared/q3" is still the directory the first upload made.
	st, err := os.Stat(filepath.Join(lib.StagedRoot, "q3"))
	if err != nil || !st.IsDir() {
		t.Fatalf("staged q3 = (%v, isDir=%v); want the seeded directory", err, err == nil && st.IsDir())
	}

	// The PATCH path is gated too: renaming a root file onto a folder name.
	if w := uploadShared(t, h, "admin@x", "", "", map[string]string{"other.csv": "x"}); w.Code != http.StatusOK {
		t.Fatalf("second seed upload: status %d body %s", w.Code, w.Body.String())
	}
	var listed sharedFilesResponse
	lw := do(t, h, http.MethodGet, "/shared-files", nil, "admin@x")
	if err := json.NewDecoder(lw.Body).Decode(&listed); err != nil {
		t.Fatalf("decode listing: %v", err)
	}
	var rootID string
	for _, f := range listed.Files {
		if f.Name == "other.csv" {
			rootID = f.ID
		}
	}
	if rootID == "" {
		t.Fatalf("seeded root file missing from listing: %+v", listed.Files)
	}
	pw := do(t, h, http.MethodPatch, "/shared-files/"+rootID, map[string]any{"name": "q3"}, "admin@x")
	if pw.Code != http.StatusConflict {
		t.Fatalf("colliding rename: status %d (want 409) body %s", pw.Code, pw.Body.String())
	}
}
