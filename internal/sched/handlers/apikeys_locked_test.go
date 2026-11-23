package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/sys/unix"

	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
)

// A key write that meets the key store locked by another process (a stopped
// CLI, say) answers 503 with Retry-After and a fixed message — not 404 or a
// bare 500, and never the lock file's path.
func TestKeyWritesAnswer503WhenTheStoreIsLocked(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "api_keys.json")
	mgr, err := apikeys.NewManager(path, filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := mgr.CreateTypedKey(context.Background(), "k", apikeys.KeyTypeTask, nil, 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	mgr.SetFileLockWait(100 * time.Millisecond)
	holder, err := os.Open(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	h := &Handlers{apiKeys: mgr}
	r := chi.NewRouter()
	r.Post("/keys", h.CreateAPIKey)
	r.Post("/keys/{key_id}/rotate", h.RotateAPIKey)
	r.Post("/keys/{key_id}/revoke", h.RevokeAPIKey)
	r.Delete("/keys/{key_id}", h.DeleteAPIKey)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodPost, "/keys", `{"name":"n","type":"task"}`},
		{http.MethodPost, "/keys/" + key.KeyID + "/rotate", ""},
		{http.MethodPost, "/keys/" + key.KeyID + "/revoke", ""},
		{http.MethodDelete, "/keys/" + key.KeyID, ""},
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(c.method, c.path, bytes.NewBufferString(c.body)))
		if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" {
			t.Errorf("%s %s: status %d Retry-After %q, want 503 with Retry-After", c.method, c.path, w.Code, w.Header().Get("Retry-After"))
		}
		if body := w.Body.String(); !strings.Contains(body, errKeyStoreBusy) || strings.Contains(body, dir) {
			t.Errorf("%s %s: body %q, want the fixed message and no path", c.method, c.path, body)
		}
	}
}
