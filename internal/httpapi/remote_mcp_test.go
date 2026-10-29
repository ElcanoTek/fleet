package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
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
		{"share", http.MethodPost, "/remote-mcp-servers/abc/shares", s.remoteMCPServerByID},
		{"unshare", http.MethodDelete, "/remote-mcp-servers/abc/shares/mate%40x.com", s.remoteMCPServerByID},
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

func TestOperatorOAuthClientFor(t *testing.T) {
	catalog := []clientconfig.RemoteMCPCatalogEntry{
		{
			Name:               "slack",
			URL:                "https://mcp.slack.com/mcp",
			Auth:               "oauth",
			ClientRegistration: "manual",
			ClientSecret:       "required",
			OAuthClientID:      "slack-cid",
			OAuthClientSecret:  "slack-csec",
			URLVariants: []clientconfig.RemoteMCPURLVariant{
				{ID: "gov", Label: "GovSlack", URL: "https://mcp.slack-gov.com/mcp"},
			},
		},
	}

	// Exact URL match -> injects credentials.
	if cid, csec, ok := operatorOAuthClientFor("slack", "https://mcp.slack.com/mcp", "oauth", catalog); !ok || cid != "slack-cid" || csec != "slack-csec" {
		t.Errorf("exact match = (%q, %q, %v), want (slack-cid, slack-csec, true)", cid, csec, ok)
	}
	// Canonical host/port spelling -> injects credentials.
	if cid, csec, ok := operatorOAuthClientFor("slack", "https://MCP.Slack.com:443/mcp", "", catalog); !ok || cid != "slack-cid" || csec != "slack-csec" {
		t.Errorf("canonical match = (%q, %q, %v), want (slack-cid, slack-csec, true)", cid, csec, ok)
	}
	// Declared URL variant -> injects credentials.
	if cid, csec, ok := operatorOAuthClientFor("slack", "https://mcp.slack-gov.com/mcp", "oauth", catalog); !ok || cid != "slack-cid" || csec != "slack-csec" {
		t.Errorf("variant match = (%q, %q, %v), want (slack-cid, slack-csec, true)", cid, csec, ok)
	}
	// Mismatched URL under the same name -> MUST NOT inject credentials.
	if cid, csec, ok := operatorOAuthClientFor("slack", "https://evil.example.com/mcp", "oauth", catalog); ok || cid != "" || csec != "" {
		t.Errorf("mismatched URL must not receive operator credentials, got (%q, %q, %v)", cid, csec, ok)
	}
	// Non-oauth auth mode -> MUST NOT inject credentials.
	if cid, csec, ok := operatorOAuthClientFor("slack", "https://mcp.slack.com/mcp", "api_key", catalog); ok || cid != "" || csec != "" {
		t.Errorf("api_key mode must not receive operator credentials, got (%q, %q, %v)", cid, csec, ok)
	}
}
