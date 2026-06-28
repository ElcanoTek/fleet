package handlers

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// fakeStream is a trivial TaskStream double: it replays a fixed set of preformatted
// SSE frames (honoring lastEventID) so the handler's live path is exercised without
// the worker pool. flushRecorder below captures the output.
type fakeStream struct {
	frames []string // each already an SSE "id: N\nevent: X\ndata: …\n\n" block, 1-indexed by position
}

func (f *fakeStream) Attach(_ context.Context, lastEventID uint64, w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for i, frame := range f.frames {
		if uint64(i+1) <= lastEventID {
			continue
		}
		if _, err := fmt.Fprint(w, frame); err != nil {
			return err
		}
	}
	return nil
}

type flushRecorder struct {
	mu   sync.Mutex
	hdr  http.Header
	code int
	buf  bytes.Buffer
}

func newFlushRecorder() *flushRecorder       { return &flushRecorder{hdr: http.Header{}} }
func (r *flushRecorder) Header() http.Header { return r.hdr }
func (r *flushRecorder) WriteHeader(c int)   { r.code = c }
func (r *flushRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}
func (r *flushRecorder) Flush() {}
func (r *flushRecorder) Body() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

func streamRouter(h *Handlers) *chi.Mux {
	r := chi.NewRouter()
	r.Get("/tasks/{task_id}/stream", h.StreamTaskLogs)
	return r
}

// TestStreamTaskLogs_LivePath: when the task is in flight the handler attaches to
// the injected live buffer and streams its frames.
func TestStreamTaskLogs_LivePath(t *testing.T) {
	h, _ := setupTest(t)
	taskID := uuid.New()
	stream := &fakeStream{frames: []string{
		"id: 1\nevent: status\ndata: {\"status\":\"running\"}\n\n",
		"id: 2\nevent: agent_message\ndata: {\"content\":\"hi\"}\n\n",
	}}
	h.SetTaskStreamProvider(func(id uuid.UUID) (TaskStream, bool) {
		if id == taskID {
			return stream, true
		}
		return nil, false
	})

	req := httptest.NewRequest("GET", "/tasks/"+taskID.String()+"/stream", nil)
	req.Header.Set("X-API-Key", "admin-key")
	rw := newFlushRecorder()
	streamRouter(h).ServeHTTP(rw, req)

	body := rw.Body()
	if !strings.Contains(body, "event: agent_message") || !strings.Contains(body, "event: status") {
		t.Errorf("live stream frames missing:\n%s", body)
	}
}

// TestStreamTaskLogs_LastEventIDReplay: the Last-Event-ID header is parsed and
// forwarded to Attach so a reconnect resumes past the events already seen.
func TestStreamTaskLogs_LastEventIDReplay(t *testing.T) {
	h, _ := setupTest(t)
	taskID := uuid.New()
	stream := &fakeStream{frames: []string{
		"id: 1\nevent: a\ndata: {}\n\n",
		"id: 2\nevent: b\ndata: {}\n\n",
		"id: 3\nevent: c\ndata: {}\n\n",
	}}
	h.SetTaskStreamProvider(func(uuid.UUID) (TaskStream, bool) { return stream, true })

	req := httptest.NewRequest("GET", "/tasks/"+taskID.String()+"/stream", nil)
	req.Header.Set("X-API-Key", "admin-key")
	req.Header.Set("Last-Event-ID", "2")
	rw := newFlushRecorder()
	streamRouter(h).ServeHTTP(rw, req)

	body := rw.Body()
	if strings.Contains(body, "event: a") || strings.Contains(body, "event: b") {
		t.Errorf("events <= Last-Event-ID should be skipped:\n%s", body)
	}
	if !strings.Contains(body, "event: c") {
		t.Errorf("event after Last-Event-ID should replay:\n%s", body)
	}
}

// TestStreamTaskLogs_FallbackToStoredLog: with no live buffer the handler replays
// the persisted log as SSE frames, ending with a terminal status. Requires the DB.
func TestStreamTaskLogs_FallbackToStoredLog(t *testing.T) {
	h, store := setupTest(t)
	taskID := uuid.New()
	if _, err := store.AddLog(taskID, &models.LogSession{
		ID:    "sess-1",
		Title: "t",
		Cost:  0.42,
		Messages: []models.LogMessage{
			{ID: "m1", Role: "assistant", Content: "doing the work", ToolCalls: []models.LogToolCall{{ID: "c1", Name: "bash", Arguments: "{}"}}},
			{ID: "m2", Role: "tool", Content: "ok", ToolCallID: strptr("c1")},
		},
	}); err != nil {
		t.Fatalf("AddLog: %v", err)
	}
	// No provider wired → always falls back to the stored log.

	req := httptest.NewRequest("GET", "/tasks/"+taskID.String()+"/stream", nil)
	req.Header.Set("X-API-Key", "admin-key")
	rw := newFlushRecorder()
	streamRouter(h).ServeHTTP(rw, req)

	body := rw.Body()
	for _, want := range []string{"event: tool_call", "event: agent_message", "event: tool_result", "event: status", `"status":"succeeded"`, `"cost_usd":0.42`} {
		if !strings.Contains(body, want) {
			t.Errorf("stored-log replay missing %q:\n%s", want, body)
		}
	}
}

// TestStreamTaskLogs_NotFound: an unknown task with no live buffer and no stored
// log returns 404.
func TestStreamTaskLogs_NotFound(t *testing.T) {
	h, _ := setupTest(t)
	req := httptest.NewRequest("GET", "/tasks/"+uuid.New().String()+"/stream", nil)
	req.Header.Set("X-API-Key", "admin-key")
	rw := httptest.NewRecorder()
	streamRouter(h).ServeHTTP(rw, req)
	if rw.Code != http.StatusNotFound {
		t.Errorf("expected 404 for unknown task, got %d: %s", rw.Code, rw.Body.String())
	}
}

// TestStreamTaskLogs_Forbidden: a request without view-logs permission is rejected
// before any stream lookup or DB access.
func TestStreamTaskLogs_Forbidden(t *testing.T) {
	h, _ := setupTest(t)
	called := false
	h.SetTaskStreamProvider(func(uuid.UUID) (TaskStream, bool) { called = true; return nil, false })

	req := httptest.NewRequest("GET", "/tasks/"+uuid.New().String()+"/stream", nil)
	// No X-API-Key and no user/key context → unauthenticated principal, no permission.
	rw := httptest.NewRecorder()
	streamRouter(h).ServeHTTP(rw, req)
	if rw.Code != http.StatusForbidden {
		t.Errorf("expected 403 without permission, got %d: %s", rw.Code, rw.Body.String())
	}
	if called {
		t.Error("stream lookup must not run for an unauthorized request")
	}
}

func strptr(s string) *string { return &s }
