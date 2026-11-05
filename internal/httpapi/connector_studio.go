package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/mcpoauth"
	"github.com/ElcanoTek/fleet/internal/playwrightrunner"
	"github.com/ElcanoTek/fleet/internal/remotemcp"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

const uiSecretTTL = 2 * time.Hour

type uiSecretBucket struct {
	values    map[string]string
	updatedAt time.Time
}

// uiSecretStash holds host-side vaulted secrets submitted through show_ui
// password inputs during a conversation so they never enter conversation
// history, LLM prompts, or sandbox files. Zero value is ready to use.
type uiSecretStash struct {
	mu      sync.Mutex
	buckets map[string]*uiSecretBucket
}

func uiSecretKey(userEmail, convID string) string {
	return strings.ToLower(strings.TrimSpace(userEmail)) + "\x00" + strings.TrimSpace(convID)
}

func (st *uiSecretStash) put(userEmail, convID, fieldID, secret string) {
	fieldID = strings.TrimSpace(fieldID)
	if fieldID == "" || secret == "" {
		return
	}
	now := time.Now()
	key := uiSecretKey(userEmail, convID)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.buckets == nil {
		st.buckets = make(map[string]*uiSecretBucket)
	}
	for k, b := range st.buckets {
		if now.Sub(b.updatedAt) > uiSecretTTL {
			delete(st.buckets, k)
		}
	}
	b, ok := st.buckets[key]
	if !ok {
		b = &uiSecretBucket{values: make(map[string]string)}
		st.buckets[key] = b
	}
	b.values[fieldID] = secret
	b.values[strings.ToLower(fieldID)] = secret
	b.updatedAt = now
}

func (st *uiSecretStash) get(userEmail, convID, fieldID string) (string, bool) {
	fieldID = strings.TrimSpace(fieldID)
	if fieldID == "" {
		return "", false
	}
	now := time.Now()
	key := uiSecretKey(userEmail, convID)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.buckets == nil {
		return "", false
	}
	b, ok := st.buckets[key]
	if !ok || now.Sub(b.updatedAt) > uiSecretTTL {
		if ok {
			delete(st.buckets, key)
		}
		return "", false
	}
	if v, ok := b.values[fieldID]; ok {
		return v, true
	}
	if v, ok := b.values[strings.ToLower(fieldID)]; ok {
		return v, true
	}
	return "", false
}

// resolve turns a "$secret:<field_id>" reference (or bare field_id if present
// in the conversation's secret stash) into the raw secret value.
func (st *uiSecretStash) resolve(userEmail, convID, refOrValue string) string {
	trimmed := strings.TrimSpace(refOrValue)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "$secret:") {
		fieldID := strings.TrimSpace(strings.TrimPrefix(trimmed, "$secret:"))
		if v, ok := st.get(userEmail, convID, fieldID); ok {
			return v
		}
		return ""
	}
	if v, ok := st.get(userEmail, convID, trimmed); ok {
		return v
	}
	return refOrValue
}

// isLikelySecretFieldID reports whether a show_ui form field ID represents a
// credential by name, acting as a defense-in-depth safety net alongside
// explicit `format: "password"` declarations in show_ui specs.
func isLikelySecretFieldID(fieldID string) bool {
	k := strings.ToLower(strings.TrimSpace(fieldID))
	if k == "" {
		return false
	}
	switch k {
	case "password", "passwd", "passphrase", "api_key", "apikey", "secret",
		"client_secret", "secret_key", "access_token", "auth_token",
		"bearer_token", "refresh_token", "private_key", "token":
		return true
	}
	for _, suffix := range []string{"_password", "_passwd", "_secret", "_api_key", "_apikey", "_token", "_private_key"} {
		if strings.HasSuffix(k, suffix) {
			return true
		}
	}
	return false
}

// passwordFieldIDsFromHistory extracts all field IDs declared with
// `format: "password"` in any `show_ui` tool call in the conversation history.
func passwordFieldIDsFromHistory(history []agent.HistoryEntry) map[string]bool {
	out := make(map[string]bool)
	for _, h := range history {
		if h.Type != "tool_call" || len(h.Content) == 0 {
			continue
		}
		var tc agent.ToolCallContent
		if err := json.Unmarshal(h.Content, &tc); err != nil {
			continue
		}
		if tc.Name != "show_ui" || strings.TrimSpace(tc.Input) == "" {
			continue
		}
		var payload struct {
			Components []struct {
				Type   string `json:"type"`
				ID     string `json:"id"`
				Format string `json:"format"`
			} `json:"components"`
		}
		if err := json.Unmarshal([]byte(tc.Input), &payload); err != nil {
			continue
		}
		for _, c := range payload.Components {
			if strings.EqualFold(strings.TrimSpace(c.Format), "password") && strings.TrimSpace(c.ID) != "" {
				id := strings.TrimSpace(c.ID)
				out[id] = true
				out[strings.ToLower(id)] = true
			}
		}
	}
	return out
}

// vaultUISubmissionSecrets intercepts `[UI submission] {...}` messages before
// they are queued, persisted, or sent to the LLM. Any field declared with
// `format: "password"` in a prior `show_ui` card (or carrying a secret field ID
// like `password` or `api_key`) is stored in the host-side `uiSecrets` stash,
// registered with the process-wide secret redactor, and replaced in the message
// JSON with `"$secret:<field_id>"`.
func (s *Server) vaultUISubmissionSecrets(ctx context.Context, userEmail, convID, message string) string {
	trimmed := strings.TrimSpace(message)
	if !strings.HasPrefix(trimmed, "[UI submission]") {
		return message
	}
	var history []agent.HistoryEntry
	if s.store != nil && convID != "" {
		if h, err := s.store.LoadHistory(ctx, convID); err == nil {
			history = h
		}
	}
	return s.vaultUISubmissionSecretsWithHistory(userEmail, convID, message, history)
}

func (s *Server) vaultUISubmissionSecretsWithHistory(userEmail, convID, message string, history []agent.HistoryEntry) string {
	trimmed := strings.TrimSpace(message)
	if !strings.HasPrefix(trimmed, "[UI submission]") {
		return message
	}
	rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "[UI submission]"))
	openIdx := strings.IndexByte(rest, '{')
	closeIdx := strings.LastIndexByte(rest, '}')
	if openIdx < 0 || closeIdx <= openIdx {
		return message
	}
	jsonPart := rest[openIdx : closeIdx+1]
	var data map[string]any
	if err := json.Unmarshal([]byte(jsonPart), &data); err != nil || len(data) == 0 {
		return message
	}

	pwFields := passwordFieldIDsFromHistory(history)
	mutated := false
	for k, rawVal := range data {
		strVal, ok := rawVal.(string)
		if !ok {
			continue
		}
		if strings.TrimSpace(strVal) == "" || strings.HasPrefix(strings.TrimSpace(strVal), "$secret:") {
			continue
		}
		if pwFields[k] || pwFields[strings.ToLower(k)] || isLikelySecretFieldID(k) {
			s.uiSecrets.put(userEmail, convID, k, strVal)
			agentcore.RegisterSecretLiterals("ui_secret:"+convID+":"+k, false, strVal)
			data[k] = "$secret:" + k
			mutated = true
		}
	}
	if !mutated {
		return message
	}
	redactedBytes, err := json.Marshal(data)
	if err != nil {
		return message
	}
	suffix := rest[closeIdx+1:]
	return "[UI submission] " + string(redactedBytes) + suffix
}

// newPlaywrightBrowserContext builds the per-turn PlaywrightBrowserContext that
// resolves vaulted secrets and wires live Co-Pilot browser sessions.
func (s *Server) newPlaywrightBrowserContext(conv *store.Conversation, user string) tools.PlaywrightBrowserContext {
	convID := ""
	if conv != nil {
		convID = conv.ID
	}
	pubURL := ""
	if s.cfg != nil {
		pubURL = s.cfg.PublicBaseURL
	}
	return tools.PlaywrightBrowserContext{
		UserEmail:     user,
		PublicBaseURL: pubURL,
		ResolveSecret: func(ref string) string {
			val := s.uiSecrets.resolve(user, convID, ref)
			if val != "" && strings.HasPrefix(strings.TrimSpace(ref), "$secret:") {
				agentcore.RegisterSecretLiterals("ui_secret:"+convID, false, val)
			}
			return val
		},
	}
}

// newConnectorStudioHandler binds connector_studio to the turn's user and conversation.
func (s *Server) newConnectorStudioHandler(conv *store.Conversation, user string) tools.ConnectorStudioHandler {
	convID := ""
	if conv != nil {
		convID = conv.ID
	}
	return func(ctx context.Context, params tools.ConnectorStudioParams) (any, error) {
		switch strings.ToLower(strings.TrimSpace(params.Action)) {
		case tools.ConnectorStudioActionSearchCatalog:
			return s.connectorStudioSearchCatalog(ctx, user, params.Query)
		case tools.ConnectorStudioActionProbeHTTP:
			return s.connectorStudioProbeHTTP(ctx, user, convID, params)
		case tools.ConnectorStudioActionProbeBrowser:
			return s.connectorStudioProbeBrowser(ctx, user, convID, params)
		case tools.ConnectorStudioActionStartCopilotSession:
			return s.connectorStudioStartCopilotSession(ctx, user, convID, params)
		case tools.ConnectorStudioActionConnectMCP:
			return s.connectorStudioConnectMCP(ctx, user, conv, params)
		case tools.ConnectorStudioActionCreateCustom:
			return s.connectorStudioCreateCustom(ctx, user, conv, params)
		case tools.ConnectorStudioActionTestCustomTool:
			return s.connectorStudioTestCustomTool(ctx, user, params)
		default:
			return nil, fmt.Errorf("unsupported action %q", params.Action)
		}
	}
}

func (s *Server) connectorStudioSearchCatalog(ctx context.Context, user, query string) (any, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	tokens := strings.Fields(q)
	matchesQuery := func(fields ...string) bool {
		if len(tokens) == 0 {
			return true
		}
		combined := strings.ToLower(strings.Join(fields, " "))
		for _, tok := range tokens {
			if strings.Contains(combined, tok) {
				return true
			}
		}
		return false
	}

	var catalogMatches []map[string]any
	if s.clientConfig != nil {
		for _, e := range s.clientConfig.RemoteMCPCatalog {
			if matchesQuery(e.Name, e.DisplayName, e.Description, e.URL, e.Category) {
				catalogMatches = append(catalogMatches, map[string]any{
					"tier":           "official_mcp_catalog",
					"name":           e.Name,
					"display_name":   e.DisplayName,
					"description":    e.Description,
					"url":            e.URL,
					"auth":           e.Auth,
					"api_key_header": e.APIKeyHeader,
					"api_key_prefix": e.APIKeyPrefix,
					"api_key_query":  e.APIKeyQuery,
					"category":       clientconfig.ConnectorCategoryOr(e.Category),
					"provenance":     e.Provenance,
				})
				if len(catalogMatches) >= 20 {
					break
				}
			}
		}
	}

	var bundledMatches []map[string]any
	for _, m := range s.connectorMeta() {
		if matchesQuery(m.Name, m.DisplayName, m.Description, m.Category) {
			bundledMatches = append(bundledMatches, map[string]any{
				"tier":         "bundled_mcp",
				"name":         m.Name,
				"display_name": m.DisplayName,
				"description":  m.Description,
				"configured":   m.Configured,
				"optional":     m.Optional,
				"category":     m.Category,
				"tool_count":   len(m.Tools),
			})
		}
	}

	var existing []map[string]any
	if s.remoteMCP != nil && s.remoteMCP.Enabled() && user != "" {
		if own, err := s.remoteMCP.ListServers(ctx, user); err == nil {
			for _, srv := range own {
				if matchesQuery(srv.Name, srv.Account, srv.URL, srv.Transport) {
					existing = append(existing, map[string]any{
						"id":                srv.ID,
						"name":              srv.Name,
						"account":           srv.Account,
						"url":               srv.URL,
						"auth_kind":         srv.AuthKind,
						"transport":         srv.Transport,
						"status":            srv.Status,
						"custom_tool_count": len(srv.CustomTools),
						"scope":             "personal",
					})
				}
			}
		}
		if shared, err := s.remoteMCP.SharedWithMe(ctx, user); err == nil {
			for _, srv := range shared {
				if matchesQuery(srv.Name, srv.Account, srv.URL, srv.Transport) {
					existing = append(existing, map[string]any{
						"id":                srv.ID,
						"name":              srv.Name,
						"account":           srv.Account,
						"url":               srv.URL,
						"auth_kind":         srv.AuthKind,
						"transport":         srv.Transport,
						"status":            srv.Status,
						"custom_tool_count": len(srv.CustomTools),
						"scope":             "shared",
						"owner":             srv.UserEmail,
					})
				}
			}
		}
	}

	return map[string]any{
		"query":                query,
		"catalog_matches":      catalogMatches,
		"bundled_matches":      bundledMatches,
		"existing_connections": existing,
		"tier_guidance":        "Tier 1 (Preferred): If an official MCP server exists in catalog_matches, connect it with action='connect_mcp'. Tier 2 (API Wrapper): If the service has a REST/GraphQL/JSON API (or you discover one via action='probe_http' or action='probe_browser' with capture_network=true), build a custom 'http_tools' connector with action='create_custom_connector'. Tier 3 (Browser Automation Fallback): If the site has no usable API, use action='probe_browser' (or action='start_copilot_session' if login hits CAPTCHA/2FA) and build a custom 'playwright' connector with action='create_custom_connector'.",
	}, nil
}

func (s *Server) safeProbeHTTPClient() *http.Client {
	if s.remoteMCP != nil && s.remoteMCP.SafeHTTPClient() != nil {
		return s.remoteMCP.SafeHTTPClient()
	}
	return mcpoauth.SafeHTTPClient(25 * time.Second)
}

func (s *Server) connectorStudioProbeHTTP(ctx context.Context, user, convID string, params tools.ConnectorStudioParams) (any, error) {
	targetURL, err := mcpoauth.ValidateServerURL(strings.TrimSpace(params.URL), false)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	method := strings.ToUpper(strings.TrimSpace(params.Method))
	if method == "" {
		method = http.MethodGet
	}

	secret := s.uiSecrets.resolve(user, convID, params.SecretRef)
	if secret != "" {
		agentcore.RegisterSecretLiterals("probe_http:"+convID, false, secret)
	}

	reqURL := targetURL
	if secret != "" && strings.TrimSpace(params.APIKeyQuery) != "" {
		reqURL = appendQueryParam(reqURL, strings.TrimSpace(params.APIKeyQuery), secret)
	}

	var bodyReader io.Reader
	if params.Body != "" && method != http.MethodGet && method != http.MethodHead {
		bodyReader = strings.NewReader(params.Body)
	}

	probeCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, method, reqURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	for k, v := range params.Headers {
		if strings.TrimSpace(k) != "" {
			req.Header.Set(k, v)
		}
	}
	if params.Body != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if secret != "" && strings.TrimSpace(params.APIKeyQuery) == "" {
		hdr := strings.TrimSpace(params.APIKeyHeader)
		pfx := params.APIKeyPrefix
		switch {
		case hdr == "":
			req.Header.Set("Authorization", "Bearer "+secret)
		case pfx != "":
			req.Header.Set(hdr, pfx+secret)
		default:
			req.Header.Set(hdr, secret)
		}
	}

	resp, err := s.safeProbeHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("probe request failed: %s", agentcore.RedactSecrets(err.Error()))
	}
	defer func() { _ = resp.Body.Close() }()

	rawBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	preview := string(rawBody)
	if len(preview) > 8192 {
		preview = preview[:8192] + "…(truncated)"
	}
	preview = agentcore.RedactSecrets(preview)
	if secret != "" {
		preview = strings.ReplaceAll(preview, secret, "[REDACTED]")
	}

	safeHeaders := map[string]string{}
	for _, h := range []string{"Content-Type", "Www-Authenticate", "X-Ratelimit-Remaining", "X-Request-Id"} {
		if v := resp.Header.Get(h); v != "" {
			safeHeaders[strings.ToLower(h)] = agentcore.RedactSecrets(v)
		}
	}

	return map[string]any{
		"url":          targetURL,
		"method":       method,
		"status_code":  resp.StatusCode,
		"status":       resp.Status,
		"content_type": resp.Header.Get("Content-Type"),
		"headers":      safeHeaders,
		"body_preview": preview,
	}, nil
}

func appendQueryParam(rawURL, key, val string) string {
	if strings.Contains(rawURL, "?") {
		return rawURL + "&" + key + "=" + val
	}
	return rawURL + "?" + key + "=" + val
}

func (s *Server) resolvePlaywrightCredentials(user, convID string, params tools.ConnectorStudioParams) playwrightrunner.Credentials {
	username := s.uiSecrets.resolve(user, convID, params.UsernameRef)
	password := s.uiSecrets.resolve(user, convID, params.PasswordRef)
	secret := s.uiSecrets.resolve(user, convID, params.SecretRef)
	if password != "" {
		agentcore.RegisterSecretLiterals("pw_password:"+convID, false, password)
	}
	if secret != "" {
		agentcore.RegisterSecretLiterals("pw_secret:"+convID, false, secret)
	}
	return playwrightrunner.Credentials{
		Username: username,
		Password: password,
		Secret:   secret,
	}
}

func substituteStepCreds(steps []playwrightrunner.Step, creds playwrightrunner.Credentials) []playwrightrunner.Step {
	if len(steps) == 0 {
		return nil
	}
	out := make([]playwrightrunner.Step, len(steps))
	repl := func(v string) string {
		if creds.Username != "" {
			v = strings.ReplaceAll(v, "{{USERNAME}}", creds.Username)
		}
		if creds.Password != "" {
			v = strings.ReplaceAll(v, "{{PASSWORD}}", creds.Password)
		}
		if creds.Secret != "" {
			v = strings.ReplaceAll(v, "{{SECRET}}", creds.Secret)
		}
		return v
	}
	for i, st := range steps {
		st.Value = repl(st.Value)
		st.Selector = repl(st.Selector)
		st.Expression = repl(st.Expression)
		out[i] = st
	}
	return out
}

func (s *Server) connectorStudioProbeBrowser(ctx context.Context, user, convID string, params tools.ConnectorStudioParams) (any, error) {
	creds := s.resolvePlaywrightCredentials(user, convID, params)
	inspectDOM := params.InspectDOM
	captureNet := params.CaptureNetwork
	if !inspectDOM && !captureNet {
		inspectDOM = true
		captureNet = true
	}

	if sessionID := strings.TrimSpace(params.SessionID); sessionID != "" {
		sess, err := playwrightrunner.DefaultSessionManager().GetSession(sessionID, user, "")
		if err != nil {
			return nil, err
		}
		steps := substituteStepCreds(params.Steps, creds)
		if strings.TrimSpace(params.URL) != "" && (len(steps) == 0 || steps[0].Action != "goto") {
			steps = append([]playwrightrunner.Step{{Action: "goto", Value: strings.TrimSpace(params.URL)}}, steps...)
		}
		if len(steps) > 0 {
			return sess.RunSteps(ctx, steps, inspectDOM, false)
		}
		return sess.Snapshot(ctx, inspectDOM, false)
	}

	return playwrightrunner.Run(ctx, playwrightrunner.RunRequest{
		URL:            strings.TrimSpace(params.URL),
		Steps:          params.Steps,
		Credentials:    creds,
		InspectDOM:     inspectDOM,
		CaptureNetwork: captureNet,
	})
}

func (s *Server) connectorStudioStartCopilotSession(ctx context.Context, user, convID string, params tools.ConnectorStudioParams) (any, error) {
	creds := s.resolvePlaywrightCredentials(user, convID, params)
	cleanURL, err := playwrightrunner.ValidateTargetURL(params.URL, false)
	if err != nil {
		return nil, err
	}
	info, err := playwrightrunner.DefaultSessionManager().StartSession(ctx, playwrightrunner.SessionStartRequest{
		OwnerEmail:  user,
		URL:         cleanURL,
		Steps:       params.Steps,
		Credentials: creds,
		InspectDOM:  true,
	})
	if err != nil {
		return nil, err
	}
	liveViewURL := fmt.Sprintf("/api/browser-sessions/%s/view?token=%s", info.ID, info.ViewToken)
	if s.cfg != nil && strings.TrimSpace(s.cfg.PublicBaseURL) != "" {
		liveViewURL = strings.TrimRight(strings.TrimSpace(s.cfg.PublicBaseURL), "/") + liveViewURL
	}
	return map[string]any{
		"status":        "copilot_session_started",
		"session_id":    info.ID,
		"live_view_url": liveViewURL,
		"expires_at":    info.ExpiresAt.UTC().Format(time.RFC3339),
		"initial_state": info.Result,
		"instructions":  fmt.Sprintf("Share this Co-Pilot Live Browser link with the user so they can complete login, CAPTCHA, or 2FA: [Open Live Browser View](%s). End your turn and wait for the user to confirm they have finished, then call connector_studio with session_id=%q to inspect the authenticated page or seal its session state into create_custom_connector.", liveViewURL, info.ID),
	}, nil
}

func (s *Server) enableConnectorOnConversation(ctx context.Context, user string, conv *store.Conversation, connectorName string) {
	if s.store == nil || conv == nil || strings.TrimSpace(connectorName) == "" {
		return
	}
	norm := strings.ToLower(strings.TrimSpace(connectorName))
	unlock := s.connectorLocks.lock(conv.ID)
	defer unlock()
	fresh, err := s.getOwnedOrMultiplayerConversation(ctx, user, conv.ID)
	if err != nil || fresh == nil {
		fresh = conv
	}
	merged := append([]string(nil), fresh.OptionalMCPServersEnabled...)
	for _, existing := range merged {
		if strings.EqualFold(strings.TrimSpace(existing), norm) {
			return
		}
	}
	merged = append(merged, norm)
	sort.Strings(merged)
	ownerEmail := convOwnerEmail(fresh, user)
	if err := s.store.SetOptionalMCPServers(ctx, ownerEmail, fresh.ID, merged); err == nil {
		conv.OptionalMCPServersEnabled = merged
	}
}

func (s *Server) connectorStudioConnectMCP(ctx context.Context, user string, conv *store.Conversation, params tools.ConnectorStudioParams) (any, error) {
	if s.remoteMCP == nil || !s.remoteMCP.Enabled() {
		return nil, fmt.Errorf("remote MCP connections are not configured on this server")
	}
	convID := ""
	if conv != nil {
		convID = conv.ID
	}
	apiKey := s.uiSecrets.resolve(user, convID, params.SecretRef)
	if apiKey != "" {
		agentcore.RegisterSecretLiterals("connect_mcp:"+convID, false, apiKey)
	}

	var clientID, clientSecret string
	if s.clientConfig != nil {
		if cid, csec, ok := operatorOAuthClientFor(params.Name, params.URL, params.AuthMode, s.clientConfig.RemoteMCPCatalog); ok {
			clientID = cid
			clientSecret = csec
		}
	}

	server, report, err := s.remoteMCP.AddServer(ctx, remotemcp.AddServerInput{
		Email:        user,
		Name:         params.Name,
		Account:      params.Account,
		URL:          params.URL,
		AuthMode:     params.AuthMode,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		APIKey:       apiKey,
		APIKeyHeader: params.APIKeyHeader,
		APIKeyQuery:  params.APIKeyQuery,
		APIKeyPrefix: params.APIKeyPrefix,
	})
	if err != nil {
		return nil, fmt.Errorf("connect MCP server: %s", agentcore.RedactSecrets(err.Error()))
	}

	sharedEveryone := strings.EqualFold(strings.TrimSpace(params.ShareScope), "everyone")
	if sharedEveryone {
		if serr := s.remoteMCP.ShareServer(ctx, user, server.ID, "*"); serr != nil {
			return nil, fmt.Errorf("server connected, but sharing with everyone failed: %w", serr)
		}
	}

	s.enableConnectorOnConversation(ctx, user, conv, server.Name)

	if server.Status == store.RemoteMCPStatusLoginRequired {
		authURL, aerr := s.remoteMCP.Authorize(ctx, user, server.ID)
		if aerr != nil {
			return nil, fmt.Errorf("initiate OAuth flow: %s", agentcore.RedactSecrets(aerr.Error()))
		}
		return map[string]any{
			"status":               "pending_oauth",
			"server_id":            server.ID,
			"name":                 server.Name,
			"url":                  server.URL,
			"authorize_url":        authURL,
			"shared_with_everyone": sharedEveryone,
			"instructions":         fmt.Sprintf("Share this authorization link with the user so they can sign in and complete OAuth: [Authorize %s](%s) (or visit `/settings/connections`).", server.Name, authURL),
		}, nil
	}

	return map[string]any{
		"status":               "connected",
		"server_id":            server.ID,
		"name":                 server.Name,
		"account":              server.Account,
		"url":                  server.URL,
		"auth_kind":            server.AuthKind,
		"tool_count":           report.ToolCount,
		"key_verified":         report.KeyVerified,
		"shared_with_everyone": sharedEveryone,
		"settings_url":         "/settings/connections",
	}, nil
}

func (s *Server) connectorStudioCreateCustom(ctx context.Context, user string, conv *store.Conversation, params tools.ConnectorStudioParams) (any, error) {
	if s.remoteMCP == nil || !s.remoteMCP.Enabled() {
		return nil, fmt.Errorf("custom connectors require remote MCP storage to be configured on this server")
	}
	if len(params.CustomTools) == 0 {
		return nil, fmt.Errorf("custom_tools is required and must define at least one tool")
	}
	convID := ""
	if conv != nil {
		convID = conv.ID
	}

	transport := strings.ToLower(strings.TrimSpace(params.Transport))
	if transport == "" {
		transport = store.RemoteMCPTransportHTTPTools
		for _, t := range params.CustomTools {
			if len(t.Steps) > 0 {
				transport = store.RemoteMCPTransportPlaywright
				break
			}
		}
	}
	if transport != store.RemoteMCPTransportHTTPTools && transport != store.RemoteMCPTransportPlaywright {
		return nil, fmt.Errorf("transport must be %q or %q", store.RemoteMCPTransportHTTPTools, store.RemoteMCPTransportPlaywright)
	}

	storeTools := make([]store.CustomConnectorTool, 0, len(params.CustomTools))
	toolNames := make([]string, 0, len(params.CustomTools))
	for _, spec := range params.CustomTools {
		var rawSchema json.RawMessage
		if len(spec.InputSchema) > 0 {
			if b, err := json.Marshal(spec.InputSchema); err == nil {
				rawSchema = b
			}
		}
		var steps []store.CustomConnectorStep
		for _, st := range spec.Steps {
			steps = append(steps, store.CustomConnectorStep{
				Action:     st.Action,
				Selector:   st.Selector,
				Value:      st.Value,
				Mode:       st.Mode,
				Attribute:  st.Attribute,
				Expression: st.Expression,
				TimeoutMs:  st.TimeoutMs,
				Optional:   st.Optional,
			})
		}
		storeTools = append(storeTools, store.CustomConnectorTool{
			Name:        strings.TrimSpace(spec.Name),
			Description: strings.TrimSpace(spec.Description),
			Method:      strings.TrimSpace(spec.Method),
			URL:         strings.TrimSpace(spec.URL),
			Headers:     spec.Headers,
			Body:        spec.Body,
			InputSchema: rawSchema,
			Steps:       steps,
		})
		toolNames = append(toolNames, fmt.Sprintf("mcp_%s_%s", strings.TrimSpace(params.Name), strings.TrimSpace(spec.Name)))
	}

	creds := s.resolvePlaywrightCredentials(user, convID, params)
	hasStorageState := false
	if sessionID := strings.TrimSpace(params.SessionID); sessionID != "" {
		sess, err := playwrightrunner.DefaultSessionManager().GetSession(sessionID, user, "")
		if err != nil {
			return nil, fmt.Errorf("load session_id %s: %w", sessionID, err)
		}
		snap, err := sess.Snapshot(ctx, false, true)
		if err != nil {
			return nil, fmt.Errorf("export browser session storage state: %w", err)
		}
		if len(snap.StorageState) > 0 {
			creds.StorageState = snap.StorageState
			hasStorageState = true
		}
	}

	var apiKeyPayload string
	switch transport {
	case store.RemoteMCPTransportHTTPTools:
		apiKeyPayload = creds.Secret
		if apiKeyPayload == "" && creds.Password != "" {
			apiKeyPayload = creds.Password
		}
	case store.RemoteMCPTransportPlaywright:
		if creds.Username != "" || creds.Password != "" || creds.Secret != "" || len(creds.StorageState) > 0 {
			b, err := json.Marshal(creds) //nolint:gosec // Intentionally marshaling credentials for AES-GCM sealing in remote_mcp_servers.api_key_enc.
			if err != nil {
				return nil, fmt.Errorf("encode playwright credentials: %w", err)
			}
			apiKeyPayload = string(b)
		}
	}

	authMode := strings.ToLower(strings.TrimSpace(params.AuthMode))
	if authMode == "" {
		if apiKeyPayload != "" {
			authMode = store.RemoteMCPAuthAPIKey
		} else {
			authMode = store.RemoteMCPAuthOpen
		}
	}

	server, err := s.remoteMCP.AddCustomConnector(ctx, remotemcp.AddCustomConnectorInput{
		Email:        user,
		Name:         params.Name,
		Account:      params.Account,
		BaseURL:      params.URL,
		Transport:    transport,
		AuthMode:     authMode,
		APIKey:       apiKeyPayload,
		APIKeyHeader: params.APIKeyHeader,
		APIKeyQuery:  params.APIKeyQuery,
		APIKeyPrefix: params.APIKeyPrefix,
		CustomTools:  storeTools,
		ShareScope:   params.ShareScope,
	})
	if err != nil {
		return nil, fmt.Errorf("create custom connector: %s", agentcore.RedactSecrets(err.Error()))
	}

	s.enableConnectorOnConversation(ctx, user, conv, server.Name)

	return map[string]any{
		"status":               "connected",
		"server_id":            server.ID,
		"name":                 server.Name,
		"account":              server.Account,
		"url":                  server.URL,
		"transport":            server.Transport,
		"auth_kind":            server.AuthKind,
		"tool_count":           len(server.CustomTools),
		"registered_tools":     toolNames,
		"has_storage_state":    hasStorageState,
		"shared_with_everyone": strings.EqualFold(strings.TrimSpace(params.ShareScope), "everyone"),
		"settings_url":         "/settings/connections",
		"message":              fmt.Sprintf("Custom connector %q created and enabled for this conversation (%d tools: %s). It is also visible and manageable in Settings → Connections.", server.Name, len(server.CustomTools), strings.Join(toolNames, ", ")),
	}, nil
}

func (s *Server) connectorStudioTestCustomTool(ctx context.Context, user string, params tools.ConnectorStudioParams) (any, error) {
	if s.remoteMCP == nil || !s.remoteMCP.Enabled() {
		return nil, fmt.Errorf("remote MCP connections are not configured on this server")
	}
	wantName := strings.TrimSpace(params.Name)
	wantTool := strings.TrimSpace(params.ToolName)
	if wantName == "" || wantTool == "" {
		return nil, fmt.Errorf("both 'name' (connector name) and 'tool_name' are required")
	}
	wantTool = strings.TrimPrefix(wantTool, "mcp_"+wantName+"_")

	var accounts map[string]string
	wantAcct := strings.TrimSpace(params.Account)
	if wantAcct != "" {
		accounts = map[string]string{wantName: wantAcct}
	}

	ov, err := agent.BuildRemoteMCPOverlay(ctx, s.remoteMCP, user, nil, agent.RemoteMCPEnabledOnly([]string{wantName}, accounts))
	if err != nil {
		return nil, fmt.Errorf("mount custom connector %q: %s", wantName, agentcore.RedactSecrets(err.Error()))
	}
	if ov == nil || !ov.Active() || ov.Client == nil {
		return nil, fmt.Errorf("custom connector %q could not be mounted (verify that it exists and is connected)", wantName)
	}
	defer ov.Close()

	regName := agentcore.RegisteredMCPName(wantName, wantAcct)
	args := params.ToolArgs
	if args == nil {
		args = map[string]any{}
	}
	res, err := ov.Client.CallToolOn(ctx, regName, wantTool, args)
	if err != nil {
		return map[string]any{
			"status": "error",
			"tool":   fmt.Sprintf("mcp_%s_%s", regName, wantTool),
			"error":  agentcore.RedactSecrets(err.Error()),
		}, nil
	}
	var texts []string
	for _, b := range res.Content {
		if strings.TrimSpace(b.Text) != "" {
			texts = append(texts, agentcore.RedactSecrets(b.Text))
		}
	}
	status := "ok"
	if res.IsError {
		status = "error"
	}
	return map[string]any{
		"status":   status,
		"is_error": res.IsError,
		"tool":     fmt.Sprintf("mcp_%s_%s", regName, wantTool),
		"output":   strings.Join(texts, "\n"),
	}, nil
}
