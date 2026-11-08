package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/testdb/fleetdb"
)

type syncLogBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLogBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// userIDsOf returns the user_id of every record with message msg.
func (b *syncLogBuf) userIDsOf(msg string) []any {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []any
	for _, line := range bytes.Split(bytes.TrimSpace(b.buf.Bytes()), []byte("\n")) {
		var m map[string]any
		if json.Unmarshal(line, &m) == nil && m["msg"] == msg {
			out = append(out, m[logging.KeyUserID])
		}
	}
	return out
}

// POST /tasks authenticates inline (authorizeTaskCreator), outside the auth
// middleware, so it must put the principal on the context the way the
// middleware does: the "Task created" line carries user_id for a web-tier
// cookie user and for an API key (#2074 review). Not parallel: it swaps the
// default logger.
func TestCreateTaskLogLineCarriesTheCaller(t *testing.T) {
	var out syncLogBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler("json", &out, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	tmpDir := t.TempDir()
	store := storage.New()
	if err := store.Initialize(fleetdb.New(t).SchedDSN(), fleetdb.SchedPool()); err != nil {
		t.Fatalf("storage init: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	keyMgr, err := apikeys.NewManager(filepath.Join(tmpDir, "api_keys.json"), filepath.Join(tmpDir, "audit_log.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	h := New(Config{DefaultTaskModel: "test/model", AdminAPIKey: "test-admin-key", Version: "0.1.0"}, store, keyMgr)
	member := seedOpsUser(t, store, uuid.New(), "alice@elcanotek.com", "client", "")
	r := chi.NewRouter()
	r.Post("/tasks", h.CreateTask)

	key, raw, err := keyMgr.CreateKey(context.Background(), "ci", []models.Permission{models.PermissionCreateTask}, nil, 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	post := func(set func(*http.Request)) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/tasks", bytes.NewBufferString(`{"prompt":"a task whose creator is logged"}`))
		req.Header.Set("Content-Type", "application/json")
		set(req)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("create: %d %s", w.Code, w.Body.String())
		}
	}
	post(func(req *http.Request) { req.Header.Set("X-API-Key", raw) })
	post(func(req *http.Request) { *req = *asSession(req, member.Username) })
	post(func(req *http.Request) { req.Header.Set("X-API-Key", "test-admin-key") })

	got := out.userIDsOf("Task created")
	want := []any{"apikey/" + key.KeyID, member.Username, "admin"}
	if len(got) != len(want) {
		t.Fatalf("Task created user_ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Task created user_ids = %v, want %v", got, want)
		}
	}
}

// a2aCreateError logs an infrastructure failure through the request's
// context logger, so the line carries the request's correlation ids
// (#2074 review). Not parallel: it swaps the default logger.
func TestA2ACreateErrorLogsWithTheRequestContext(t *testing.T) {
	var out syncLogBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler("json", &out, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ctx := logging.WithUser(logging.WithRequestID(context.Background(), "req-a2a"), "apikey/key_x")
	_ = a2aCreateError(ctx, json.RawMessage(`1`), errors.New("database unavailable"))
	if got := out.userIDsOf("A2A: task create failed (infrastructure)"); len(got) != 1 || got[0] != "apikey/key_x" {
		t.Fatalf("user_id on the A2A failure line = %v", got)
	}
	out.mu.Lock()
	defer out.mu.Unlock()
	if !bytes.Contains(out.buf.Bytes(), []byte(`"request_id":"req-a2a"`)) {
		t.Fatalf("request_id missing: %s", out.buf.String())
	}
}
