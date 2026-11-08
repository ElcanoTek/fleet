package httpapi

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/logging"
)

// TestWriteJSONKeepsExplicitContentType pins the reason writeJSON/writeJSONStatus
// default the header instead of owning it. Several handlers (the input-queue
// acks, the chat replay ack, the conversation export attachment) set an explicit
// `application/json; charset=utf-8` before writing the body; when those call
// sites were routed through these helpers, a plain Set would have silently
// downgraded the header to bare `application/json`.
func TestWriteJSONKeepsExplicitContentType(t *testing.T) {
	t.Parallel()

	t.Run("defaults when unset", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeJSON(rec, nil, map[string]string{"ok": "yes"})
		if got := rec.Header().Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type: want application/json, got %q", got)
		}
		if !strings.Contains(rec.Body.String(), `"ok"`) {
			t.Fatalf("body not encoded: %q", rec.Body.String())
		}
	})

	t.Run("preserves an explicit charset", func(t *testing.T) {
		rec := httptest.NewRecorder()
		rec.Header().Set("Content-Type", "application/json; charset=utf-8")
		writeJSON(rec, nil, map[string]string{"ok": "yes"})
		if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
			t.Fatalf("helper clobbered the handler's Content-Type: %q", got)
		}
	})

	t.Run("status sibling preserves it too", func(t *testing.T) {
		rec := httptest.NewRecorder()
		rec.Header().Set("Content-Type", "application/json; charset=utf-8")
		writeJSONStatus(rec, nil, http.StatusAccepted, map[string]string{"ok": "yes"})
		if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
			t.Fatalf("helper clobbered the handler's Content-Type: %q", got)
		}
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status: want 202, got %d", rec.Code)
		}
	})
}

// TestWriteJSONLogsEncodeFailure covers the point of routing the previously
// `_ =`-silenced call sites through these helpers: an unmarshalable value means
// Encode writes NOTHING (it marshals fully before writing), so without the log
// the client gets a committed status with an empty body and the server records
// nothing at all.
func TestWriteJSONLogsEncodeFailure(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	// A channel is not JSON-encodable; Encode fails before emitting bytes.
	writeJSON(rec, nil, map[string]any{"bad": make(chan int)})
	if rec.Body.Len() != 0 {
		t.Fatalf("expected an empty body on a marshal failure, got %q", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type: want application/json, got %q", got)
	}
}

// writeJSON takes the request it answers, so an encode failure is logged with
// that request's correlation (request_id, user_id) rather than as a
// context-free line (#2074 review). Not parallel: it swaps the default logger.
func TestWriteJSONLogsEncodeFailureWithTheRequestContext(t *testing.T) {
	var out lockedLogBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler("json", &out, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req = req.WithContext(logging.WithUser(logging.WithRequestID(req.Context(), "req-wj"), "a@b.co"))
	writeJSON(httptest.NewRecorder(), req, map[string]any{"bad": make(chan int)})
	writeJSONStatus(httptest.NewRecorder(), req, http.StatusOK, map[string]any{"bad": make(chan int)})
	recs := out.records(t)
	if len(recs) != 2 {
		t.Fatalf("want 2 encode-failure records, got %v", recs)
	}
	for _, m := range recs {
		if m["msg"] != "write json failed" || m["request_id"] != "req-wj" || m["user_id"] != "a@b.co" {
			t.Fatalf("record lacks the request's correlation: %v", m)
		}
	}
}
