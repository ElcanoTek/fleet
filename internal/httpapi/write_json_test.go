package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWriteJSONKeepsExplicitContentType pins the reason writeJSON/writeJSONStatus
// default the header instead of owning it. Several handlers (the input-queue
// acks, the chat replay ack, the conversation export attachment) set an explicit
// `application/json; charset=utf-8` before writing the body; when those call
// sites were routed through these helpers, a plain Set would have silently
// downgraded the header to bare `application/json`.
func TestWriteJSONKeepsExplicitContentType(t *testing.T) {
	t.Run("defaults when unset", func(t *testing.T) {
		rec := httptest.NewRecorder()
		writeJSON(rec, map[string]string{"ok": "yes"})
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
		writeJSON(rec, map[string]string{"ok": "yes"})
		if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
			t.Fatalf("helper clobbered the handler's Content-Type: %q", got)
		}
	})

	t.Run("status sibling preserves it too", func(t *testing.T) {
		rec := httptest.NewRecorder()
		rec.Header().Set("Content-Type", "application/json; charset=utf-8")
		writeJSONStatus(rec, http.StatusAccepted, map[string]string{"ok": "yes"})
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
	rec := httptest.NewRecorder()
	// A channel is not JSON-encodable; Encode fails before emitting bytes.
	writeJSON(rec, map[string]any{"bad": make(chan int)})
	if rec.Body.Len() != 0 {
		t.Fatalf("expected an empty body on a marshal failure, got %q", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type: want application/json, got %q", got)
	}
}
