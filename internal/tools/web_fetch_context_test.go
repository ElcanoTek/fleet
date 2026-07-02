package tools

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFetchURLForContext_SSRFBlocksLoopback proves the @url context-handle
// fetcher (#517) enforces the SAME SSRF guard as the web_fetch tool: a loopback
// httptest server is refused by the guarded dialer, so a user-typed
// `@url:http://127.0.0.1/...` cannot exfiltrate internal services.
func TestFetchURLForContext_SSRFBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "sensitive internal data")
	}))
	defer srv.Close()

	_, err := FetchURLForContext(context.Background(), srv.URL)
	if err == nil {
		t.Fatal("expected SSRF block for loopback URL, got nil error")
	}
	if !strings.Contains(err.Error(), "access to private IP denied") {
		t.Errorf("expected SSRF denial, got: %v", err)
	}
}
