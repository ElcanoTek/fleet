package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBrowserSessionViewOfAnUnavailableSessionIsAPage: the live-view link a
// person opens from chat answers with a readable page once the session is
// gone (or is not theirs), while the endpoints the view's script calls keep
// returning JSON errors.
func TestBrowserSessionViewOfAnUnavailableSessionIsAPage(t *testing.T) {
	t.Parallel()
	s := &Server{}
	call := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(asUser(context.Background(), "alice@example.com"), method, path, nil)
		rec := httptest.NewRecorder()
		s.handleBrowserSessions(rec, req)
		return rec
	}

	view := call(http.MethodGet, "/browser-sessions/0123456789abcdef01234567/view")
	if view.Code != http.StatusNotFound {
		t.Errorf("view status = %d, want 404", view.Code)
	}
	if ct := view.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("view Content-Type = %q, want text/html", ct)
	}
	body := view.Body.String()
	for _, want := range []string{"This live browser session is no longer available", `href="/chat"`} {
		if !strings.Contains(body, want) {
			t.Errorf("view page is missing %q:\n%s", want, body)
		}
	}
	// One page for every reason, so it does not say which one applied.
	for _, leak := range []string{"not found", "not authorized", `"error"`} {
		if strings.Contains(body, leak) {
			t.Errorf("view page contains %q:\n%s", leak, body)
		}
	}
	if head := call(http.MethodHead, "/browser-sessions/0123456789abcdef01234567/view"); head.Code != http.StatusNotFound || !strings.HasPrefix(head.Header().Get("Content-Type"), "text/html") {
		t.Errorf("HEAD view = %d %q, want 404 text/html", head.Code, head.Header().Get("Content-Type"))
	}

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/browser-sessions/0123456789abcdef01234567/copilot.js"},
		{http.MethodGet, "/browser-sessions/0123456789abcdef01234567/stream"},
		{http.MethodPost, "/browser-sessions/0123456789abcdef01234567/input"},
		{http.MethodPost, "/browser-sessions/0123456789abcdef01234567/close"},
	} {
		rec := call(tc.method, tc.path)
		if rec.Code != http.StatusNotFound || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			t.Errorf("%s %s = %d %q, want a 404 JSON error", tc.method, tc.path, rec.Code, rec.Header().Get("Content-Type"))
		}
		if body := rec.Body.String(); !strings.Contains(body, "this live browser session is no longer available") {
			t.Errorf("%s %s body = %q, want the one answer every reason gets", tc.method, tc.path, body)
		}
	}
}
