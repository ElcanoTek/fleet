package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
	convID             string
	values             map[string]string
	fieldIDs           map[string]bool
	allowedHosts       map[string]bool
	allowEveryoneShare bool
	updatedAt          time.Time
}

// uiSecretStash holds host-side vaulted secrets submitted through show_ui
// password inputs during a conversation so they never enter conversation
// history, LLM prompts, or sandbox files. Each vaulted secret is bound to the
// user-confirmed origin(s) submitted by the user in their show_ui form or
// chat messages so prompt-injected model output cannot exfiltrate credentials
// to an arbitrary third-party host. Zero value is ready to use.
type uiSecretStash struct {
	mu      sync.Mutex
	buckets map[string]*uiSecretBucket
}

func uiSecretKey(userEmail, convID string) string {
	return strings.ToLower(strings.TrimSpace(userEmail)) + "\x00" + strings.TrimSpace(convID)
}

func retireExpiredSecretBucket(b *uiSecretBucket) {
	if b == nil {
		return
	}
	for fieldID := range b.fieldIDs {
		agentcore.RegisterSecretLiterals("ui_secret:"+b.convID+":"+fieldID, true)
	}
}

func normalizeOriginHost(rawURLOrHost string) string {
	s := strings.TrimSpace(rawURLOrHost)
	if s == "" {
		return ""
	}
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || u.Hostname() == "" {
			return ""
		}
		return strings.ToLower(strings.TrimSpace(u.Hostname()))
	}
	if !strings.ContainsAny(s, " \t\r\n/\\:@") && strings.Contains(s, ".") {
		u, err := url.Parse("https://" + s)
		if err == nil && u.Hostname() != "" {
			return strings.ToLower(strings.TrimSpace(u.Hostname()))
		}
	}
	return ""
}

func hostMatchesAllowedOrigin(targetHost, allowedHost string) bool {
	targetHost = strings.ToLower(strings.TrimSpace(targetHost))
	allowedHost = strings.ToLower(strings.TrimSpace(allowedHost))
	if targetHost == "" || allowedHost == "" {
		return false
	}
	if targetHost == allowedHost {
		return true
	}
	base := strings.TrimPrefix(allowedHost, "www.")
	if base != "" && strings.Contains(base, ".") && (targetHost == base || strings.HasSuffix(targetHost, "."+base)) {
		return true
	}
	return false
}

func (st *uiSecretStash) ensureBucketLocked(userEmail, convID string, now time.Time) *uiSecretBucket {
	if st.buckets == nil {
		st.buckets = make(map[string]*uiSecretBucket)
	}
	for k, b := range st.buckets {
		if now.Sub(b.updatedAt) > uiSecretTTL {
			retireExpiredSecretBucket(b)
			delete(st.buckets, k)
		}
	}
	key := uiSecretKey(userEmail, convID)
	b, ok := st.buckets[key]
	if !ok {
		b = &uiSecretBucket{
			convID:       strings.TrimSpace(convID),
			values:       make(map[string]string),
			fieldIDs:     make(map[string]bool),
			allowedHosts: make(map[string]bool),
		}
		st.buckets[key] = b
	}
	if b.allowedHosts == nil {
		b.allowedHosts = make(map[string]bool)
	}
	return b
}

func (st *uiSecretStash) put(userEmail, convID, fieldID, secret string) {
	fieldID = strings.TrimSpace(fieldID)
	convID = strings.TrimSpace(convID)
	if fieldID == "" || secret == "" {
		return
	}
	now := time.Now()
	st.mu.Lock()
	defer st.mu.Unlock()
	b := st.ensureBucketLocked(userEmail, convID, now)
	b.values[fieldID] = secret
	b.values[strings.ToLower(fieldID)] = secret
	b.fieldIDs[fieldID] = true
	b.updatedAt = now
}

func (st *uiSecretStash) allowOrigin(userEmail, convID, rawURLOrHost string) {
	host := normalizeOriginHost(rawURLOrHost)
	if host == "" {
		return
	}
	now := time.Now()
	st.mu.Lock()
	defer st.mu.Unlock()
	b := st.ensureBucketLocked(userEmail, convID, now)
	b.allowedHosts[host] = true
	b.updatedAt = now
}

func (st *uiSecretStash) setAllowEveryone(userEmail, convID string, allow bool) {
	now := time.Now()
	st.mu.Lock()
	defer st.mu.Unlock()
	b := st.ensureBucketLocked(userEmail, convID, now)
	b.allowEveryoneShare = allow
	b.updatedAt = now
}

func (st *uiSecretStash) isEveryoneShareAllowed(userEmail, convID string) bool {
	now := time.Now()
	key := uiSecretKey(userEmail, convID)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.buckets == nil {
		return false
	}
	b, ok := st.buckets[key]
	if !ok || now.Sub(b.updatedAt) > uiSecretTTL {
		return false
	}
	return b.allowEveryoneShare
}

func (st *uiSecretStash) isHostAllowed(userEmail, convID, targetURLOrHost string) bool {
	targetHost := normalizeOriginHost(targetURLOrHost)
	if targetHost == "" {
		return false
	}
	now := time.Now()
	key := uiSecretKey(userEmail, convID)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.buckets == nil {
		return false
	}
	b, ok := st.buckets[key]
	if !ok || now.Sub(b.updatedAt) > uiSecretTTL {
		return false
	}
	for allowed := range b.allowedHosts {
		if hostMatchesAllowedOrigin(targetHost, allowed) {
			return true
		}
	}
	return false
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
			retireExpiredSecretBucket(b)
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
	v, _ := st.lookupSecret(userEmail, convID, refOrValue)
	return v
}

// lookupSecret returns (resolvedValue, isVaultedSecret). When isVaultedSecret
// is true, the value came from the conversation's secret vault and MUST only be
// transmitted to a user-confirmed origin.
func (st *uiSecretStash) lookupSecret(userEmail, convID, refOrValue string) (string, bool) {
	trimmed := strings.TrimSpace(refOrValue)
	if trimmed == "" {
		return "", false
	}
	if strings.HasPrefix(trimmed, "$secret:") {
		fieldID := strings.TrimSpace(strings.TrimPrefix(trimmed, "$secret:"))
		if v, ok := st.get(userEmail, convID, fieldID); ok {
			return v, true
		}
		return "", true
	}
	if v, ok := st.get(userEmail, convID, trimmed); ok {
		return v, true
	}
	return refOrValue, false
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
// `format: "password"` in any `show_ui` tool call in the conversation history,
// walking nested layout components (`section`, `columns`, `tabs`, `repeater`).
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
		var payload map[string]any
		if err := json.Unmarshal([]byte(tc.Input), &payload); err != nil {
			continue
		}
		collectPasswordComponentIDs(payload["components"], out)
	}
	return out
}

func collectPasswordComponentIDs(node any, out map[string]bool) {
	switch v := node.(type) {
	case []any:
		for _, item := range v {
			collectPasswordComponentIDs(item, out)
		}
	case map[string]any:
		format, _ := v["format"].(string)
		id, _ := v["id"].(string)
		if strings.EqualFold(strings.TrimSpace(format), "password") && strings.TrimSpace(id) != "" {
			cleanID := strings.TrimSpace(id)
			out[cleanID] = true
			out[strings.ToLower(cleanID)] = true
		}
		for _, childKey := range []string{"children", "fields", "tabs", "columns", "left", "right"} {
			if child, ok := v[childKey]; ok {
				collectPasswordComponentIDs(child, out)
			}
		}
	}
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

func extractURLTokensFromText(text string) []string {
	var out []string
	for _, field := range strings.Fields(text) {
		tok := strings.Trim(field, " \t\r\n\"'`()[]{}<>,;!")
		if strings.HasPrefix(tok, "https://") || strings.HasPrefix(tok, "http://") {
			out = append(out, tok)
		}
	}
	return out
}

func userConfirmedOriginsFromHistory(history []agent.HistoryEntry) []string {
	var out []string
	for _, h := range history {
		if h.Role != "user" || (h.Type != "" && h.Type != "text") || len(h.Content) == 0 {
			continue
		}
		var tc agent.TextContent
		if err := json.Unmarshal(h.Content, &tc); err != nil {
			continue
		}
		out = append(out, extractURLTokensFromText(tc.Text)...)
	}
	return out
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
	mutated := s.vaultSubmissionMap(userEmail, convID, "", data, pwFields)
	for _, u := range userConfirmedOriginsFromHistory(history) {
		s.uiSecrets.allowOrigin(userEmail, convID, u)
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

func isLikelyURLFieldID(fieldID string) bool {
	k := strings.ToLower(strings.TrimSpace(fieldID))
	switch k {
	case "url", "base_url", "api_url", "portal_url", "server_url", "endpoint", "host", "domain", "site", "origin", "website":
		return true
	}
	return strings.HasSuffix(k, "_url") || strings.HasSuffix(k, "_endpoint") || strings.HasSuffix(k, "_host") || strings.HasSuffix(k, "_domain")
}

func isShareScopeFieldID(fieldID string) bool {
	k := strings.ToLower(strings.TrimSpace(fieldID))
	switch k {
	case "share_scope", "scope", "sharing":
		return true
	}
	return strings.HasSuffix(k, "_scope")
}

func (s *Server) userConfirmedEveryoneShare(ctx context.Context, userEmail, convID string) bool {
	if s.uiSecrets.isEveryoneShareAllowed(userEmail, convID) {
		return true
	}
	if s.store == nil || strings.TrimSpace(convID) == "" {
		return false
	}
	history, err := s.store.LoadHistory(ctx, convID)
	if err != nil {
		return false
	}
	for _, h := range history {
		if h.Role != "user" || (h.Type != "" && h.Type != "text") || len(h.Content) == 0 {
			continue
		}
		var tc agent.TextContent
		if err := json.Unmarshal(h.Content, &tc); err != nil {
			continue
		}
		lower := strings.ToLower(tc.Text)
		if strings.Contains(lower, "everyone") ||
			strings.Contains(lower, "fleet-wide") ||
			strings.Contains(lower, "fleet wide") ||
			strings.Contains(lower, "all users") ||
			strings.Contains(lower, "whole team") {
			return true
		}
	}
	return false
}

func (s *Server) vaultSubmissionMap(userEmail, convID, prefix string, data map[string]any, pwFields map[string]bool) bool {
	mutated := false
	for k, rawVal := range data {
		stashKey := k
		if prefix != "" {
			stashKey = prefix + "." + k
		}
		switch val := rawVal.(type) {
		case string:
			if strings.TrimSpace(val) == "" || strings.HasPrefix(strings.TrimSpace(val), "$secret:") {
				continue
			}
			if pwFields[k] || pwFields[strings.ToLower(k)] || isLikelySecretFieldID(k) {
				s.uiSecrets.put(userEmail, convID, stashKey, val)
				agentcore.RegisterSecretLiterals("ui_secret:"+convID+":"+stashKey, true, val)
				if stashKey != k {
					s.uiSecrets.put(userEmail, convID, k, val)
					agentcore.RegisterSecretLiterals("ui_secret:"+convID+":"+k, true, val)
				}
				data[k] = "$secret:" + stashKey
				mutated = true
				continue
			}
			if isShareScopeFieldID(k) {
				vLower := strings.ToLower(strings.TrimSpace(val))
				s.uiSecrets.setAllowEveryone(userEmail, convID, vLower == "everyone" || vLower == "fleet" || vLower == "workspace" || vLower == "*")
			}
			if isLikelyURLFieldID(k) || strings.HasPrefix(strings.TrimSpace(val), "https://") || strings.HasPrefix(strings.TrimSpace(val), "http://") {
				s.uiSecrets.allowOrigin(userEmail, convID, val)
			}
		case map[string]any:
			if s.vaultSubmissionMap(userEmail, convID, stashKey, val, pwFields) {
				mutated = true
			}
		case []any:
			for idx, item := range val {
				if itemMap, ok := item.(map[string]any); ok {
					itemPrefix := fmt.Sprintf("%s[%d]", stashKey, idx)
					if s.vaultSubmissionMap(userEmail, convID, itemPrefix, itemMap, pwFields) {
						mutated = true
					}
				}
			}
		}
	}
	return mutated
}

// isCatalogApprovedHost reports whether targetHost matches an operator-curated
// RemoteMCPCatalog entry URL.
func (s *Server) isCatalogApprovedHost(connectorName, targetURLOrHost string) bool {
	if s.clientConfig == nil {
		return false
	}
	targetHost := normalizeOriginHost(targetURLOrHost)
	if targetHost == "" {
		return false
	}
	for _, entry := range s.clientConfig.RemoteMCPCatalog {
		if connectorName != "" && !strings.EqualFold(strings.TrimSpace(entry.Name), strings.TrimSpace(connectorName)) {
			continue
		}
		if hostMatchesAllowedOrigin(targetHost, normalizeOriginHost(entry.URL)) {
			return true
		}
	}
	return false
}

// resolveSecretForURL resolves a "$secret:<field_id>" reference (or stashed
// secret) only after verifying that targetURL's host matches a user-confirmed
// origin from the conversation's show_ui submission, user chat messages, or
// operator-curated MCP catalog. This prevents prompt-injected model output from
// sending vaulted user credentials to an attacker-controlled endpoint.
func (s *Server) resolveSecretForURL(ctx context.Context, userEmail, convID, refOrValue, targetURL string) (string, error) {
	val, isVaulted := s.uiSecrets.lookupSecret(userEmail, convID, refOrValue)
	if !isVaulted {
		return val, nil
	}
	if val == "" {
		return "", fmt.Errorf("vaulted secret %q was not found or expired; ask the user to submit the credential again via show_ui", strings.TrimSpace(refOrValue))
	}
	targetHost := normalizeOriginHost(targetURL)
	if targetHost == "" {
		return "", fmt.Errorf("vaulted secret %q requires a valid target URL", strings.TrimSpace(refOrValue))
	}
	if !s.uiSecrets.isHostAllowed(userEmail, convID, targetHost) && !s.isCatalogApprovedHost("", targetHost) {
		if s.store != nil && convID != "" {
			if history, err := s.store.LoadHistory(ctx, convID); err == nil {
				for _, u := range userConfirmedOriginsFromHistory(history) {
					s.uiSecrets.allowOrigin(userEmail, convID, u)
				}
			}
		}
	}
	if !s.uiSecrets.isHostAllowed(userEmail, convID, targetHost) && !s.isCatalogApprovedHost("", targetHost) {
		return "", fmt.Errorf("vaulted secret %q is not authorized for host %q; ask the user to confirm the target URL in a show_ui form alongside the credential or in their chat message", strings.TrimSpace(refOrValue), targetHost)
	}
	if strings.HasPrefix(strings.TrimSpace(refOrValue), "$secret:") {
		fieldID := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(refOrValue), "$secret:"))
		agentcore.RegisterSecretLiterals("ui_secret:"+convID+":"+fieldID, true, val)
	}
	return val, nil
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
		ResolveSecret: func(ctx context.Context, ref, targetURL string) (string, error) {
			return s.resolveSecretForURL(ctx, user, convID, ref, targetURL)
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
	method := strings.ToUpper(strings.TrimSpace(params.Method))
	if method == "" {
		method = http.MethodGet
	}
	if method != http.MethodGet && method != http.MethodHead {
		return nil, fmt.Errorf("probe_http only supports read-only GET or HEAD requests (got %q); save mutating methods as custom connector tools so they route through the approval gate", method)
	}
	targetURL, err := mcpoauth.ValidateServerURL(strings.TrimSpace(params.URL), false)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}

	secret, err := s.resolveSecretForURL(ctx, user, convID, params.SecretRef, targetURL)
	if err != nil {
		return nil, err
	}
	if secret != "" {
		agentcore.RegisterSecretLiterals("probe_http:"+convID, true, secret)
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
	//nolint:gosec // G704: URL is validated via mcpoauth.ValidateServerURL, bound to user-confirmed origins for vaulted secrets, and dialed through SafeHTTPClient's netguard SSRF classifier.
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

	//nolint:gosec // G704: URL is validated via mcpoauth.ValidateServerURL, bound to user-confirmed origins for vaulted secrets, and dialed through SafeHTTPClient's netguard SSRF classifier.
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
	encoded := url.QueryEscape(key) + "=" + url.QueryEscape(val)
	if strings.Contains(rawURL, "?") {
		return rawURL + "&" + encoded
	}
	return rawURL + "?" + encoded
}

func (s *Server) resolvePlaywrightCredentials(ctx context.Context, user, convID, targetURL string, params tools.ConnectorStudioParams) (playwrightrunner.Credentials, error) {
	checkURLs := []string{strings.TrimSpace(targetURL)}
	for _, st := range params.Steps {
		if strings.EqualFold(strings.TrimSpace(st.Action), "goto") && strings.TrimSpace(st.Value) != "" {
			checkURLs = append(checkURLs, strings.TrimSpace(st.Value))
		}
	}
	for _, spec := range params.CustomTools {
		for _, st := range spec.Steps {
			if strings.EqualFold(strings.TrimSpace(st.Action), "goto") && strings.TrimSpace(st.Value) != "" {
				checkURLs = append(checkURLs, strings.TrimSpace(st.Value))
			}
		}
	}
	resolveAll := func(ref string) (string, error) {
		var resolved string
		for _, u := range checkURLs {
			if u == "" {
				continue
			}
			v, err := s.resolveSecretForURL(ctx, user, convID, ref, u)
			if err != nil {
				return "", err
			}
			resolved = v
		}
		if resolved == "" && strings.TrimSpace(ref) != "" {
			return s.resolveSecretForURL(ctx, user, convID, ref, targetURL)
		}
		return resolved, nil
	}
	username, err := resolveAll(params.UsernameRef)
	if err != nil {
		return playwrightrunner.Credentials{}, err
	}
	password, err := resolveAll(params.PasswordRef)
	if err != nil {
		return playwrightrunner.Credentials{}, err
	}
	secret, err := resolveAll(params.SecretRef)
	if err != nil {
		return playwrightrunner.Credentials{}, err
	}
	if password != "" {
		agentcore.RegisterSecretLiterals("pw_password:"+convID, true, password)
	}
	if secret != "" {
		agentcore.RegisterSecretLiterals("pw_secret:"+convID, true, secret)
	}
	return playwrightrunner.Credentials{
		Username: username,
		Password: password,
		Secret:   secret,
	}, nil
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
		out[i] = st
	}
	return out
}

func (s *Server) connectorStudioProbeBrowser(ctx context.Context, user, convID string, params tools.ConnectorStudioParams) (any, error) {
	creds, err := s.resolvePlaywrightCredentials(ctx, user, convID, params.URL, params)
	if err != nil {
		return nil, err
	}
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
	creds, err := s.resolvePlaywrightCredentials(ctx, user, convID, params.URL, params)
	if err != nil {
		return nil, err
	}
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
	liveViewURL := fmt.Sprintf("/api/browser-sessions/%s/view", info.ID)
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
	if strings.EqualFold(strings.TrimSpace(params.ShareScope), "everyone") && !s.userConfirmedEveryoneShare(ctx, user, convID) {
		return nil, fmt.Errorf("share_scope=\"everyone\" requires explicit user confirmation in the show_ui form (share_scope=\"everyone\") or chat conversation; use share_scope=\"personal\" or ask the user to confirm fleet-wide sharing first")
	}
	apiKey, err := s.resolveSecretForURL(ctx, user, convID, params.SecretRef, params.URL)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		agentcore.RegisterSecretLiterals("connect_mcp:"+convID, true, apiKey)
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
	if strings.EqualFold(strings.TrimSpace(params.ShareScope), "everyone") && !s.userConfirmedEveryoneShare(ctx, user, convID) {
		return nil, fmt.Errorf("share_scope=\"everyone\" requires explicit user confirmation in the show_ui form (share_scope=\"everyone\") or chat conversation; use share_scope=\"personal\" or ask the user to confirm fleet-wide sharing first")
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
				Action:    st.Action,
				Selector:  st.Selector,
				Value:     st.Value,
				Mode:      st.Mode,
				Attribute: st.Attribute,
				TimeoutMs: st.TimeoutMs,
				Optional:  st.Optional,
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

	creds, err := s.resolvePlaywrightCredentials(ctx, user, convID, params.URL, params)
	if err != nil {
		return nil, err
	}
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
	if authMode == "" && apiKeyPayload != "" {
		authMode = store.RemoteMCPAuthAPIKey
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
	fullToolName := fmt.Sprintf("mcp_%s_%s", regName, wantTool)
	if agentcore.IsCriticalTool(fullToolName) {
		return nil, fmt.Errorf("test_custom_tool only executes read-only tools directly; %q is a mutating/critical tool — invoke %s directly so it routes through the approval gate", wantTool, fullToolName)
	}
	if conns, cerr := s.remoteMCP.ConnectedServersForUser(ctx, user); cerr == nil {
		for _, c := range conns {
			if strings.EqualFold(c.Name, wantName) && (wantAcct == "" || c.Account == wantAcct) {
				for _, ct := range c.CustomTools {
					if ct.Name == wantTool && c.Transport == "http_tools" && ct.Method != "" && !strings.EqualFold(ct.Method, http.MethodGet) {
						return nil, fmt.Errorf("test_custom_tool only executes read-only GET tools directly; %q uses HTTP %s — invoke %s directly so it routes through the approval gate", wantTool, ct.Method, fullToolName)
					}
				}
			}
		}
	}
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
