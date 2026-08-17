package httpapi

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDeleteConversations_MalformedJSONDoesNotWipe is the #1110 regression:
// a client that meant to send a targeted bulk delete but produced truncated
// or invalid JSON used to fall through to DeleteAllUnpinned (200 + wipe).
// Non-EOF decode errors must now 400 and touch nothing.
func TestDeleteConversations_MalformedJSONDoesNotWipe(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{name: "truncated conversation_ids array", body: `{"conversation_ids": [`},
		{name: "not JSON", body: `this is not json`},
		{name: "wrong type for conversation_ids", body: `{"conversation_ids": "oops"}`},
		{name: "truncated object", body: `{`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newFakeChatStore()
			srv := newDefaultChatServer(t, &fakeEngine{}, st)
			req := httptest.NewRequest(http.MethodDelete, "/conversations", strings.NewReader(tc.body))
			req.Header.Set("X-Chat-Server-Token", "tok")
			req.Header.Set("X-User-Email", "u@x.com")
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			srv.Routes().ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %q)", w.Code, w.Body.String())
			}
			st.mu.Lock()
			n := st.deleteAllUnpinned
			st.mu.Unlock()
			if n != 0 {
				t.Fatalf("DeleteAllUnpinned called %d times, want 0", n)
			}
		})
	}
}

// TestDeleteConversations_EmptyBodyKeepsLegacyWipe preserves the bare
// DELETE /conversations affordance: io.EOF (empty body) still means
// "delete all unpinned".
func TestDeleteConversations_EmptyBodyKeepsLegacyWipe(t *testing.T) {
	st := newFakeChatStore()
	srv := newDefaultChatServer(t, &fakeEngine{}, st)
	w := do(t, srv.Routes(), http.MethodDelete, "/conversations", nil, "u@x.com")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", w.Code, w.Body.String())
	}
	st.mu.Lock()
	n := st.deleteAllUnpinned
	st.mu.Unlock()
	if n != 1 {
		t.Fatalf("DeleteAllUnpinned called %d times, want 1", n)
	}
}

// TestBodyLimitMiddleware_CapsDELETE proves DELETE is subject to the same
// 1 MiB JSON-body cap as POST/PUT/PATCH (#1110). An oversized body must
// not be fully readable by the handler.
func TestBodyLimitMiddleware_CapsDELETE(t *testing.T) {
	var read int64
	var readErr error
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		read, readErr = io.Copy(io.Discard, r.Body)
		if readErr != nil {
			http.Error(w, readErr.Error(), http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	h := bodyLimitMiddleware(inner)
	body := strings.Repeat("x", maxJSONBodyBytes+8)
	req := httptest.NewRequest(http.MethodDelete, "/conversations", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (body %q)", w.Code, w.Body.String())
	}
	if read > maxJSONBodyBytes {
		t.Fatalf("handler read %d bytes, want ≤ %d", read, maxJSONBodyBytes)
	}
	if readErr == nil {
		t.Fatal("expected MaxBytesReader error, got nil")
	}
}
