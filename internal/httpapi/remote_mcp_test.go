package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// When the feature is unconfigured (s.remoteMCP == nil), every remote-MCP
// endpoint must fail closed with 503 and a machine-readable reason — never panic
// or touch the store. DB-independent: the disabled check short-circuits first.
func TestRemoteMCPEndpointsDisabledReturn503(t *testing.T) {
	s := &Server{} // remoteMCP nil → feature disabled

	cases := []struct {
		name, method, path string
		handler            http.HandlerFunc
	}{
		{"list", http.MethodGet, "/remote-mcp-servers", s.remoteMCPServers},
		{"add", http.MethodPost, "/remote-mcp-servers", s.remoteMCPServers},
		{"delete", http.MethodDelete, "/remote-mcp-servers/abc", s.remoteMCPServerByID},
		{"authorize", http.MethodPost, "/remote-mcp-servers/abc/authorize", s.remoteMCPServerByID},
		{"callback", http.MethodPost, "/oauth/mcp/callback", s.remoteMCPOAuthCallback},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyUser, "u@x.com"))
		w := httptest.NewRecorder()
		tc.handler(w, req)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status %d, want 503", tc.name, w.Code)
		}
		if !strings.Contains(w.Body.String(), "remote_mcp_disabled") {
			t.Errorf("%s: body %q missing remote_mcp_disabled", tc.name, w.Body.String())
		}
	}
}
