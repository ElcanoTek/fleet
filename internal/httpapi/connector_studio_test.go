package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/remotemcp"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

func TestVaultUISubmissionSecrets(t *testing.T) {
	s := &Server{}
	user := "admin@example.com"
	convID := "conv-vault-1"

	showUIInput := `{"title":"Connect Portal","components":[{"type":"section","title":"Login","children":[{"type":"text_input","id":"username","label":"Username"},{"type":"text_input","id":"custom_pin","label":"Security PIN","format":"password"}]},{"type":"repeater","id":"accounts","label":"Accounts","fields":[{"type":"text_input","id":"seat_secret","label":"Seat Secret","format":"password"}]}]}`
	tcJSON, _ := json.Marshal(agent.ToolCallContent{
		ID:    "tc-1",
		Name:  "show_ui",
		Input: showUIInput,
	})
	history := []agent.HistoryEntry{
		{
			Role:    "assistant",
			Type:    "tool_call",
			Content: tcJSON,
		},
	}

	rawSubmission := `[UI submission] {"username":"alice@example.com","password":"my-super-secret-password-999","custom_pin":"pin-secret-88888","accounts":[{"seat_secret":"repeater-secret-77777"}]}`
	vaulted := s.vaultUISubmissionSecretsWithHistory(user, convID, rawSubmission, history)

	if strings.Contains(vaulted, "my-super-secret-password-999") {
		t.Fatalf("vaulted message still contains plaintext password: %s", vaulted)
	}
	if strings.Contains(vaulted, "pin-secret-88888") {
		t.Fatalf("vaulted message still contains plaintext custom_pin: %s", vaulted)
	}
	if strings.Contains(vaulted, "repeater-secret-77777") {
		t.Fatalf("vaulted message still contains plaintext repeater seat_secret: %s", vaulted)
	}
	if !strings.Contains(vaulted, `"$secret:password"`) {
		t.Fatalf("vaulted message missing $secret:password placeholder: %s", vaulted)
	}
	if !strings.Contains(vaulted, `"$secret:custom_pin"`) {
		t.Fatalf("vaulted message missing $secret:custom_pin placeholder: %s", vaulted)
	}
	if !strings.Contains(vaulted, `"$secret:accounts[0].seat_secret"`) {
		t.Fatalf("vaulted message missing $secret:accounts[0].seat_secret placeholder: %s", vaulted)
	}
	if !strings.Contains(vaulted, `"alice@example.com"`) {
		t.Fatalf("vaulted message should preserve non-secret username: %s", vaulted)
	}

	if got := s.uiSecrets.resolve(user, convID, "$secret:password"); got != "my-super-secret-password-999" {
		t.Fatalf("resolve($secret:password) = %q, want my-super-secret-password-999", got)
	}
	if got := s.uiSecrets.resolve(user, convID, "$secret:custom_pin"); got != "pin-secret-88888" {
		t.Fatalf("resolve($secret:custom_pin) = %q, want pin-secret-88888", got)
	}
	if got := s.uiSecrets.resolve(user, convID, "$secret:accounts[0].seat_secret"); got != "repeater-secret-77777" {
		t.Fatalf("resolve($secret:accounts[0].seat_secret) = %q, want repeater-secret-77777", got)
	}
	if redacted := agentcore.RedactSecrets("leaked: my-super-secret-password-999"); strings.Contains(redacted, "my-super-secret-password-999") {
		t.Fatalf("RedactSecrets did not scrub vaulted secret: %q", redacted)
	}
}

type memRemoteMCPStore struct {
	servers map[string]*store.RemoteMCPServer
	apiKeys map[string]string
	shares  map[string][]string
}

func newMemRemoteMCPStore() *memRemoteMCPStore {
	return &memRemoteMCPStore{
		servers: map[string]*store.RemoteMCPServer{},
		apiKeys: map[string]string{},
		shares:  map[string][]string{},
	}
}

func (m *memRemoteMCPStore) RemoteMCPEncryptionEnabled() bool { return true }
func (m *memRemoteMCPStore) CreateRemoteMCPServer(_ context.Context, in store.RemoteMCPServerInput) (*store.RemoteMCPServer, error) {
	id := "srv-" + in.Name
	row := &store.RemoteMCPServer{
		ID:           id,
		UserEmail:    in.UserEmail,
		Name:         in.Name,
		Account:      in.Account,
		IsDefault:    true,
		URL:          in.URL,
		Transport:    in.Transport,
		Status:       in.Status,
		AuthKind:     in.AuthKind,
		APIKeyHeader: in.APIKeyHeader,
		APIKeyQuery:  in.APIKeyQuery,
		APIKeyPrefix: in.APIKeyPrefix,
		CustomTools:  in.CustomTools,
	}
	m.servers[id] = row
	if in.APIKey != "" {
		m.apiKeys[id] = in.APIKey
	}
	cp := *row
	return &cp, nil
}
func (m *memRemoteMCPStore) GetRemoteMCPServer(_ context.Context, _, id string) (*store.RemoteMCPServer, error) {
	if r, ok := m.servers[id]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, store.ErrRemoteMCPNotFound
}
func (m *memRemoteMCPStore) ListRemoteMCPServers(_ context.Context, email string) ([]store.RemoteMCPServer, error) {
	var out []store.RemoteMCPServer
	for _, r := range m.servers {
		if strings.EqualFold(r.UserEmail, email) {
			out = append(out, *r)
		}
	}
	return out, nil
}
func (m *memRemoteMCPStore) DeleteRemoteMCPServer(_ context.Context, _, id string) error {
	delete(m.servers, id)
	delete(m.apiKeys, id)
	return nil
}
func (m *memRemoteMCPStore) LoadServerSecrets(_ context.Context, _ *store.RemoteMCPServer) (string, string, error) {
	return "", "", nil
}
func (m *memRemoteMCPStore) GetRemoteMCPAPIKey(_ context.Context, srv *store.RemoteMCPServer) (string, error) {
	return m.apiKeys[srv.ID], nil
}
func (m *memRemoteMCPStore) SetRemoteMCPAPIKey(_ context.Context, _, id, apiKey string) error {
	m.apiKeys[id] = apiKey
	return nil
}
func (m *memRemoteMCPStore) SetRemoteMCPStatus(_ context.Context, _, id, status, detail string) error {
	if r, ok := m.servers[id]; ok {
		r.Status = status
		r.StatusDetail = detail
	}
	return nil
}
func (m *memRemoteMCPStore) SetRemoteMCPDefaultSeat(_ context.Context, _, _ string) error {
	return nil
}
func (m *memRemoteMCPStore) RenameRemoteMCPAccount(_ context.Context, _, _, _ string) error {
	return nil
}
func (m *memRemoteMCPStore) GetOAuthTokens(_ context.Context, _ *store.RemoteMCPServer) (*store.RemoteMCPTokens, error) {
	return &store.RemoteMCPTokens{}, nil
}
func (m *memRemoteMCPStore) ClearOAuthTokens(_ context.Context, _, _ string) error { return nil }
func (m *memRemoteMCPStore) StoreOAuthTokens(_ context.Context, _ *store.RemoteMCPServer, _ store.RemoteMCPTokens) error {
	return nil
}
func (m *memRemoteMCPStore) EnsureFreshToken(_ context.Context, _ *store.RemoteMCPServer, _ int64, _ store.RefreshFunc) (string, error) {
	return "", nil
}
func (m *memRemoteMCPStore) BeginOAuthFlow(_ context.Context, _, _, _, _ string, _ time.Duration) error {
	return nil
}
func (m *memRemoteMCPStore) ConsumeOAuthFlow(_ context.Context, _ string) (*store.OAuthFlowState, error) {
	return nil, nil
}
func (m *memRemoteMCPStore) ShareRemoteMCPServer(_ context.Context, _, serverID, grantee string) error {
	m.shares[serverID] = append(m.shares[serverID], grantee)
	return nil
}
func (m *memRemoteMCPStore) UnshareRemoteMCPServer(_ context.Context, _, serverID, grantee string) error {
	existing := m.shares[serverID]
	var kept []string
	for _, g := range existing {
		if g != grantee {
			kept = append(kept, g)
		}
	}
	m.shares[serverID] = kept
	return nil
}
func (m *memRemoteMCPStore) ListRemoteMCPSharesByOwner(_ context.Context, _ string) (map[string][]string, error) {
	return m.shares, nil
}
func (m *memRemoteMCPStore) ListRemoteMCPServersSharedWith(_ context.Context, _ string) ([]store.RemoteMCPServer, error) {
	var out []store.RemoteMCPServer
	for id, grantees := range m.shares {
		for _, g := range grantees {
			if g == "*" {
				if r, ok := m.servers[id]; ok {
					out = append(out, *r)
				}
			}
		}
	}
	return out, nil
}
func (m *memRemoteMCPStore) GetRemoteMCPServerForUse(_ context.Context, _, id string) (*store.RemoteMCPServer, error) {
	if r, ok := m.servers[id]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, store.ErrRemoteMCPNotFound
}
func (m *memRemoteMCPStore) ListConnectorPrefs(_ context.Context, _ string) (map[string]store.ConnectorPref, error) {
	return map[string]store.ConnectorPref{}, nil
}
func (m *memRemoteMCPStore) UpdateCustomConnector(_ context.Context, id string, in store.RemoteMCPServerInput) (*store.RemoteMCPServer, error) {
	r, ok := m.servers[id]
	if !ok {
		return nil, store.ErrRemoteMCPNotFound
	}
	r.URL = in.URL
	r.Transport = in.Transport
	r.Status = in.Status
	r.AuthKind = in.AuthKind
	r.APIKeyHeader = in.APIKeyHeader
	r.APIKeyQuery = in.APIKeyQuery
	r.APIKeyPrefix = in.APIKeyPrefix
	r.CustomTools = in.CustomTools
	if in.APIKey != "" || in.AuthKind == store.RemoteMCPAuthOpen {
		m.apiKeys[id] = in.APIKey
	}
	cp := *r
	return &cp, nil
}

func TestConnectorStudioCreateAndTestCustomHTTPConnector(t *testing.T) {
	var gotAuthHeader string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"campaigns":[{"id":"c-42","status":"active"}]}`))
	}))
	defer upstream.Close()

	memStore := newMemRemoteMCPStore()
	rmcp := remotemcp.NewService(memStore, remotemcp.Config{
		PublicBaseURL:     "https://fleet.example.com",
		AllowInsecureHTTP: true,
		HTTPClient:        upstream.Client(),
	})

	chatSt := newFakeChatStore()
	conv, _ := chatSt.CreateConversation(context.Background(), "admin@example.com", "Build Connector", "default", "", false)

	s := &Server{
		store:     chatSt,
		remoteMCP: rmcp,
		clientConfig: &clientconfig.Bundle{
			RemoteMCPCatalog: []clientconfig.RemoteMCPCatalogEntry{
				{Name: "slack", DisplayName: "Slack", Description: "Official Slack MCP", URL: "https://mcp.slack.com/mcp", Auth: "oauth"},
			},
		},
	}

	// Vault an API key and user-confirmed origin as submitted from a show_ui form.
	s.uiSecrets.put("admin@example.com", conv.ID, "api_key", "sk-live-secret-token-777")
	s.uiSecrets.allowOrigin("admin@example.com", conv.ID, upstream.URL)

	h := s.newConnectorStudioHandler(conv, "admin@example.com")

	// 1. Search catalog.
	searchRes, err := h(context.Background(), tools.ConnectorStudioParams{
		Action: tools.ConnectorStudioActionSearchCatalog,
		Query:  "slack",
	})
	if err != nil {
		t.Fatalf("search_catalog: %v", err)
	}
	searchMap, _ := searchRes.(map[string]any)
	matches, _ := searchMap["catalog_matches"].([]map[string]any)
	if len(matches) != 1 || matches[0]["name"] != "slack" {
		t.Fatalf("unexpected catalog_matches: %#v", searchMap["catalog_matches"])
	}

	// 2a. Reject share_scope="everyone" before the user confirms fleet-wide sharing.
	_, err = h(context.Background(), tools.ConnectorStudioParams{
		Action:     tools.ConnectorStudioActionCreateCustom,
		Name:       "acme_ads",
		URL:        upstream.URL,
		Transport:  "http_tools",
		ShareScope: "everyone",
		SecretRef:  "$secret:api_key",
		CustomTools: []tools.ConnectorStudioToolSpec{
			{
				Name:        "list_campaigns",
				Description: "List active campaigns from Acme Ads.",
				Method:      "GET",
				URL:         upstream.URL + "/v1/campaigns",
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "requires explicit user confirmation") {
		t.Fatalf("expected share_scope=everyone without user confirmation to be rejected, got %v", err)
	}

	// Simulate user submitting share_scope="everyone" in show_ui.
	s.uiSecrets.setAllowEveryone("admin@example.com", conv.ID, true)

	// 2b. Create custom HTTP connector with vaulted secret and fleet-wide share_scope.
	createRes, err := h(context.Background(), tools.ConnectorStudioParams{
		Action:     tools.ConnectorStudioActionCreateCustom,
		Name:       "acme_ads",
		URL:        upstream.URL,
		Transport:  "http_tools",
		ShareScope: "everyone",
		SecretRef:  "$secret:api_key",
		CustomTools: []tools.ConnectorStudioToolSpec{
			{
				Name:        "list_campaigns",
				Description: "List active campaigns from Acme Ads.",
				Method:      "GET",
				URL:         upstream.URL + "/v1/campaigns",
			},
		},
	})
	if err != nil {
		t.Fatalf("create_custom_connector: %v", err)
	}
	createMap, _ := createRes.(map[string]any)
	if createMap["status"] != "connected" || createMap["shared_with_everyone"] != true {
		t.Fatalf("unexpected create result: %#v", createMap)
	}
	if len(conv.OptionalMCPServersEnabled) != 1 || conv.OptionalMCPServersEnabled[0] != "acme_ads" {
		t.Fatalf("expected acme_ads to be auto-enabled on conversation, got %v", conv.OptionalMCPServersEnabled)
	}

	// 3. Test custom tool end-to-end via test_custom_tool.
	testRes, err := h(context.Background(), tools.ConnectorStudioParams{
		Action:   tools.ConnectorStudioActionTestCustomTool,
		Name:     "acme_ads",
		ToolName: "list_campaigns",
	})
	if err != nil {
		t.Fatalf("test_custom_tool: %v", err)
	}
	testMap, _ := testRes.(map[string]any)
	if testMap["status"] != "ok" {
		t.Fatalf("expected test_custom_tool status=ok, got %#v", testMap)
	}
	if gotAuthHeader != "Bearer sk-live-secret-token-777" {
		t.Fatalf("expected upstream Authorization header 'Bearer sk-live-secret-token-777', got %q", gotAuthHeader)
	}
	if !strings.Contains(testMap["output"].(string), "c-42") {
		t.Fatalf("expected campaign output c-42, got %q", testMap["output"])
	}

	// 4. Reject host placeholder in custom tool URL.
	_, err = h(context.Background(), tools.ConnectorStudioParams{
		Action:    tools.ConnectorStudioActionCreateCustom,
		Name:      "acme_ads",
		URL:       upstream.URL,
		Transport: "http_tools",
		SecretRef: "$secret:api_key",
		CustomTools: []tools.ConnectorStudioToolSpec{
			{
				Name:        "evil_host",
				Description: "Should be rejected because host has placeholder.",
				Method:      "GET",
				URL:         "https://{host}/v1/campaigns",
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "scheme and host must be fixed") {
		t.Fatalf("expected fixed host error, got %v", err)
	}

	// 5. Reject cross-origin tool URL on api_key custom connector.
	_, err = h(context.Background(), tools.ConnectorStudioParams{
		Action:    tools.ConnectorStudioActionCreateCustom,
		Name:      "acme_ads",
		URL:       upstream.URL,
		Transport: "http_tools",
		SecretRef: "$secret:api_key",
		CustomTools: []tools.ConnectorStudioToolSpec{
			{
				Name:        "cross_origin",
				Description: "Should be rejected because origin differs from base URL.",
				Method:      "GET",
				URL:         "https://other.example.com/v1/campaigns",
			},
		},
	})
	if err == nil || (!strings.Contains(err.Error(), "must share the connector's base origin") && !strings.Contains(err.Error(), "not authorized for host")) {
		t.Fatalf("expected cross-origin rejection error, got %v", err)
	}

	// 6. Reject sending vaulted secret to an unconfirmed origin in probe_http, and reject POST in probe_http.
	_, err = h(context.Background(), tools.ConnectorStudioParams{
		Action:    tools.ConnectorStudioActionProbeHTTP,
		URL:       "https://attacker.example.com/collect",
		SecretRef: "$secret:api_key",
	})
	if err == nil || !strings.Contains(err.Error(), "not authorized for host") {
		t.Fatalf("expected unconfirmed origin error for probe_http, got %v", err)
	}
	_, err = h(context.Background(), tools.ConnectorStudioParams{
		Action: tools.ConnectorStudioActionProbeHTTP,
		URL:    upstream.URL + "/v1/campaigns",
		Method: "POST",
	})
	if err == nil || !strings.Contains(err.Error(), "only supports read-only GET or HEAD") {
		t.Fatalf("expected probe_http POST rejection, got %v", err)
	}

	// 7. Updating with share_scope="personal" revokes everyone share AND direct email grants, and POST tools carry per-run Critical metadata and require approval gate instead of test_custom_tool.
	_ = memStore.ShareRemoteMCPServer(context.Background(), "admin@example.com", createMap["server_id"].(string), "colleague@example.com")
	_, err = h(context.Background(), tools.ConnectorStudioParams{
		Action:     tools.ConnectorStudioActionCreateCustom,
		Name:       "acme_ads",
		URL:        upstream.URL,
		Transport:  "http_tools",
		ShareScope: "personal",
		SecretRef:  "$secret:api_key",
		CustomTools: []tools.ConnectorStudioToolSpec{
			{
				Name:        "list_campaigns",
				Description: "List active campaigns from Acme Ads.",
				Method:      "GET",
				URL:         upstream.URL + "/v1/campaigns",
			},
			{
				Name:        "archive_item",
				Description: "Archive an item via POST.",
				Method:      "POST",
				URL:         upstream.URL + "/v1/campaigns/archive",
			},
		},
	})
	if err != nil {
		t.Fatalf("update custom connector to personal: %v", err)
	}
	sharedAfter, _ := memStore.ListRemoteMCPServersSharedWith(context.Background(), "other@example.com")
	if len(sharedAfter) != 0 {
		t.Fatalf("expected everyone share to be revoked after personal update, got %v", sharedAfter)
	}
	directSharedAfter, _ := memStore.ListRemoteMCPServersSharedWith(context.Background(), "colleague@example.com")
	if len(directSharedAfter) != 0 {
		t.Fatalf("expected direct colleague share to be revoked after personal update, got %v", directSharedAfter)
	}
	_, err = h(context.Background(), tools.ConnectorStudioParams{
		Action:   tools.ConnectorStudioActionTestCustomTool,
		Name:     "acme_ads",
		ToolName: "archive_item",
	})
	if err == nil || !strings.Contains(err.Error(), "approval gate") {
		t.Fatalf("expected test_custom_tool to refuse mutating POST tool archive_item, got %v", err)
	}
	ov, _ := agent.BuildRemoteMCPOverlay(context.Background(), rmcp, "admin@example.com", nil, agent.RemoteMCPAllConnected)
	toolsByName := map[string]bool{}
	for _, st := range ov.Catalog {
		if st.ServerName == "acme_ads" {
			toolsByName[st.Tool.Name] = st.Tool.Critical
		}
	}
	if !toolsByName["archive_item"] {
		t.Errorf("expected POST custom tool archive_item to have Critical=true in overlay catalog")
	}
	if toolsByName["list_campaigns"] {
		t.Errorf("expected GET custom tool list_campaigns to have Critical=false in overlay catalog")
	}
	if agentcore.IsCriticalTool("mcp_acme_ads_archive_item") {
		t.Errorf("expected custom tool criticality to be per-run rather than process-global")
	}
}

func TestConnectorStudioRejectsPlaintextBrowserProbeAndExtractsFromResultPasswords(t *testing.T) {
	memStore := newMemRemoteMCPStore()
	rmcp := remotemcp.NewService(memStore, remotemcp.Config{
		PublicBaseURL:     "https://fleet.example.com",
		AllowInsecureHTTP: false,
	})
	chatSt := newFakeChatStore()
	conv, _ := chatSt.CreateConversation(context.Background(), "admin@example.com", "Build Connector", "default", "", false)

	s := &Server{
		store:     chatSt,
		remoteMCP: rmcp,
	}
	s.uiSecrets.put("admin@example.com", conv.ID, "password", "super-secret-pw")
	s.uiSecrets.allowOrigin("admin@example.com", conv.ID, "http://portal.example.com")

	h := s.newConnectorStudioHandler(conv, "admin@example.com")
	if _, err := h(context.Background(), tools.ConnectorStudioParams{
		Action:      tools.ConnectorStudioActionProbeBrowser,
		URL:         "http://portal.example.com/login",
		PasswordRef: "$secret:password",
	}); err == nil {
		t.Fatal("expected probe_browser with http:// URL to be rejected when AllowInsecureHTTP is false")
	}
	if _, err := s.resolveSecretForURL(context.Background(), "admin@example.com", conv.ID, "$secret:password", "http://portal.example.com/login"); err == nil {
		t.Fatal("expected resolveSecretForURL to reject http:// target URL when AllowInsecureHTTP is false")
	}

	trRaw, _ := json.Marshal(agent.ToolResultContent{
		ID:   "card-pw",
		Name: "show_ui",
		Card: `{"components":[{"type":"form","fields":[{"id":"custom_cred","type":"string","format":"password"}]}]}`,
	})
	tcPlainRaw, _ := json.Marshal(agent.ToolCallContent{
		ID:    "card-plain",
		Name:  "show_ui",
		Input: `{"components":[{"type":"form","fields":[{"id":"custom_cred","type":"string"}]}]}`,
	})
	history := []agent.HistoryEntry{
		{
			Type:    "tool_result",
			Content: trRaw,
		},
		{
			Type:    "tool_call",
			Content: tcPlainRaw,
		},
	}
	pwFields, _ := passwordFieldIDsFromHistory(history, "")
	if !pwFields["custom_cred"] {
		t.Fatalf("expected passwordFieldIDsFromHistory to extract custom_cred from tool_result.Card, got %v", pwFields)
	}
	if pwPlain, _ := passwordFieldIDsFromHistory(history, "card-plain"); pwPlain["custom_cred"] {
		t.Fatalf("expected card-plain not to inherit custom_cred password flag from earlier card-pw, got %v", pwPlain)
	}
	if pwCard, found := passwordFieldIDsFromHistory(history, "card-pw"); !found || !pwCard["custom_cred"] {
		t.Fatalf("expected card-pw to report custom_cred as password field, got %v", pwCard)
	}

	// Verify exact origin matching and that prior unrelated URLs in history do not authorize a later password-card submission.
	oldUserMsg, _ := json.Marshal(agent.TextContent{Text: "Earlier we looked at https://old-unrelated.example.com/docs", SenderEmail: "admin@example.com"})
	historyWithOldURL := append([]agent.HistoryEntry{
		{Role: "user", Type: "text", Content: oldUserMsg},
	}, history...)
	s.vaultUISubmissionSecretsWithHistoryAndOwner("admin@example.com", "admin@example.com", conv.ID, `[UI submission] card=card-pw {"url":"https://example.com/login","custom_cred":"my-new-secret-pw"}`, historyWithOldURL)
	if _, err := s.resolveSecretForURL(context.Background(), "admin@example.com", conv.ID, "$secret:custom_cred", "https://old-unrelated.example.com/api"); err == nil {
		t.Fatal("expected prior unrelated URL in history not to be authorized for newly vaulted secret")
	}
	if _, err := s.resolveSecretForURL(context.Background(), "admin@example.com", conv.ID, "$secret:custom_cred", "https://evil.example.com/api"); err == nil {
		t.Fatal("expected subdomain https://evil.example.com not to match exact origin https://example.com")
	}
	if _, err := s.resolveSecretForURL(context.Background(), "admin@example.com", conv.ID, "$secret:custom_cred", "https://example.com:8443/api"); err == nil {
		t.Fatal("expected non-default port https://example.com:8443 not to match exact origin https://example.com")
	}
	if got, err := s.resolveSecretForURL(context.Background(), "admin@example.com", conv.ID, "$secret:custom_cred", "https://example.com/api"); err != nil || got != "my-new-secret-pw" {
		t.Fatalf("expected exact origin https://example.com to resolve secret, got %q, err=%v", got, err)
	}

	// 8. When a card is resolved in history (card=card-plain), non-password fields must NEVER be vaulted
	// even if their field ID looks like a token/secret (e.g. page_token, password), and short/common words
	// submitted in a password card must not poison the process-wide ShowUIRedactor.
	plainSub := s.vaultUISubmissionSecretsWithHistoryAndOwner("admin@example.com", "admin@example.com", conv.ID, `[UI submission] card=card-plain {"custom_cred":"visible-value","page_token":"next-page-12345","password":"plain-text-field-not-marked-password"}`, history)
	if strings.Contains(plainSub, "$secret:") {
		t.Fatalf("expected resolved non-password card submission not to vault any fields, got %s", plainSub)
	}
	s.vaultUISubmissionSecretsWithHistoryAndOwner("admin@example.com", "admin@example.com", conv.ID, `[UI submission] card=card-pw {"url":"https://example.com/login","custom_cred":"password"}`, history)
	if redacted := agentcore.RedactSecrets(`{"format":"password","required":true}`); strings.Contains(redacted, "[REDACTED]") {
		t.Fatalf("short/common word 'password' submitted in password field must not poison process-wide redactor: %s", redacted)
	}
}
