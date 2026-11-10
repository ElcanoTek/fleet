package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
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
	"github.com/ElcanoTek/fleet/internal/redact"
	"github.com/ElcanoTek/fleet/internal/remotemcp"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

const uiSecretTTL = 2 * time.Hour

type uiSecretBucket struct {
	bucketKey          string
	convID             string
	values             map[string]string
	fieldIDs           map[string]bool
	fieldOrigins       map[string]map[string]bool
	allowedHosts       map[string]bool
	shareScopeSet      bool
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
	scopeKey := b.bucketKey
	if scopeKey == "" {
		scopeKey = b.convID
	}
	for fieldID := range b.fieldIDs {
		agentcore.RegisterSecretLiterals("ui_secret:"+scopeKey+":"+fieldID, true)
	}
	agentcore.RegisterSecretLiterals("probe_http:"+scopeKey, true)
	agentcore.RegisterSecretLiterals("connect_mcp:"+scopeKey, true)
	agentcore.RegisterSecretLiterals("pw_password:"+scopeKey, true)
	agentcore.RegisterSecretLiterals("pw_secret:"+scopeKey, true)
}

func normalizeOriginHost(rawURLOrHost string) string {
	s := strings.TrimSpace(rawURLOrHost)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		if !strings.ContainsAny(s, " \t\r\n/\\@") && strings.Contains(s, ".") {
			s = "https://" + s
		} else {
			return ""
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	scheme := strings.ToLower(strings.TrimSpace(u.Scheme))
	if scheme != "https" && scheme != "http" {
		return ""
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		return scheme + "://" + net.JoinHostPort(host, port)
	}
	return scheme + "://" + host
}

func hostMatchesAllowedOrigin(targetHost, allowedHost string) bool {
	targetHost = strings.ToLower(strings.TrimSpace(targetHost))
	allowedHost = strings.ToLower(strings.TrimSpace(allowedHost))
	if targetHost == "" || allowedHost == "" {
		return false
	}
	return targetHost == allowedHost
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
			bucketKey:    key,
			convID:       strings.TrimSpace(convID),
			values:       make(map[string]string),
			fieldIDs:     make(map[string]bool),
			fieldOrigins: make(map[string]map[string]bool),
			allowedHosts: make(map[string]bool),
		}
		st.buckets[key] = b
	}
	if b.fieldOrigins == nil {
		b.fieldOrigins = make(map[string]map[string]bool)
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

func (st *uiSecretStash) bindSubmissionOrigins(userEmail, convID string, fieldIDs []string, rawURLs []string) {
	now := time.Now()
	st.mu.Lock()
	defer st.mu.Unlock()
	b := st.ensureBucketLocked(userEmail, convID, now)
	originSet := make(map[string]bool, len(rawURLs))
	for _, raw := range rawURLs {
		if o := normalizeOriginHost(raw); o != "" {
			originSet[o] = true
		}
	}
	for _, fid := range fieldIDs {
		fid = strings.TrimSpace(fid)
		if fid == "" {
			continue
		}
		cp := make(map[string]bool, len(originSet))
		for o := range originSet {
			cp[o] = true
		}
		b.fieldOrigins[fid] = cp
		b.fieldOrigins[strings.ToLower(fid)] = cp
	}
	b.allowedHosts = make(map[string]bool, len(originSet))
	for o := range originSet {
		b.allowedHosts[o] = true
	}
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
	for fid, origins := range b.fieldOrigins {
		if len(origins) == 0 {
			if origins == nil {
				origins = make(map[string]bool, 1)
				b.fieldOrigins[fid] = origins
			}
			origins[host] = true
		}
	}
	b.updatedAt = now
}

func (st *uiSecretStash) setAllowEveryone(userEmail, convID string, allow bool) {
	now := time.Now()
	st.mu.Lock()
	defer st.mu.Unlock()
	b := st.ensureBucketLocked(userEmail, convID, now)
	b.shareScopeSet = true
	b.allowEveryoneShare = allow
	b.updatedAt = now
}

func (st *uiSecretStash) consumeEveryoneShare(userEmail, convID string) (allow bool, explicit bool) {
	now := time.Now()
	key := uiSecretKey(userEmail, convID)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.buckets == nil {
		return false, false
	}
	b, ok := st.buckets[key]
	if !ok || now.Sub(b.updatedAt) > uiSecretTTL {
		return false, false
	}
	if !b.shareScopeSet {
		return false, false
	}
	allow = b.allowEveryoneShare
	// Consume a positive confirmation so it cannot be reused on a subsequent connector.
	if b.allowEveryoneShare {
		b.allowEveryoneShare = false
	}
	return allow, true
}

func (st *uiSecretStash) isFieldOriginAllowed(userEmail, convID, fieldID, targetURLOrHost string) (allowed bool, hasExplicitBinding bool) {
	targetHost := normalizeOriginHost(targetURLOrHost)
	if targetHost == "" {
		return false, false
	}
	now := time.Now()
	key := uiSecretKey(userEmail, convID)
	fieldID = strings.TrimSpace(fieldID)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.buckets == nil {
		return false, false
	}
	b, ok := st.buckets[key]
	if !ok || now.Sub(b.updatedAt) > uiSecretTTL {
		return false, false
	}
	if fieldID != "" && b.fieldOrigins != nil {
		if origins, has := b.fieldOrigins[fieldID]; has {
			for allowedOrigin := range origins {
				if hostMatchesAllowedOrigin(targetHost, allowedOrigin) {
					return true, true
				}
			}
			return false, len(origins) > 0
		}
		if origins, has := b.fieldOrigins[strings.ToLower(fieldID)]; has {
			for allowedOrigin := range origins {
				if hostMatchesAllowedOrigin(targetHost, allowedOrigin) {
					return true, true
				}
			}
			return false, len(origins) > 0
		}
	}
	for allowedOrigin := range b.allowedHosts {
		if hostMatchesAllowedOrigin(targetHost, allowedOrigin) {
			return true, false
		}
	}
	return false, false
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

// isLikelySecretFieldID reports whether a show_ui form field ID represents an
// explicit credential by name, acting as a fallback only when the submitted
// show_ui card specification could not be resolved in conversation history.
// Never match bare "token" or generic "_token" suffixes (e.g. page_token,
// continuation_token, deal_token) so non-secret pagination or identifier inputs
// are never vaulted by heuristic.
func isLikelySecretFieldID(fieldID string) bool {
	k := strings.ToLower(strings.TrimSpace(fieldID))
	if k == "" {
		return false
	}
	switch k {
	case "password", "passwd", "passphrase", "api_key", "apikey", "secret",
		"client_secret", "secret_key", "access_token", "auth_token",
		"bearer_token", "refresh_token", "session_token", "api_token",
		"secret_token", "private_key":
		return true
	}
	for _, suffix := range []string{
		"_password", "_passwd", "_secret", "_api_key", "_apikey",
		"_access_token", "_auth_token", "_bearer_token", "_refresh_token",
		"_session_token", "_api_token", "_secret_token", "_private_key",
	} {
		if strings.HasSuffix(k, suffix) {
			return true
		}
	}
	return false
}

// isSafeProcessWideSecretLiteral guards the process-wide literal redactor
// (agentcore.RegisterSecretLiterals) against over-redaction when a user submits
// a short word, number, URL, path, or GenUI keyword in a password field. The
// per-conversation uiSecretStash and local connector_studio / playwrightrunner
// scrubbers still vault and mask the value for the conversation regardless.
func isSafeProcessWideSecretLiteral(val string) bool {
	v := strings.TrimSpace(val)
	if len(v) < 12 {
		return false
	}
	if strings.ContainsAny(v, " \t\r\n") {
		return false
	}
	lower := strings.ToLower(v)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "$secret:") {
		return false
	}
	if redact.IsPathListValue(v) {
		return false
	}
	hasAlpha := false
	hasDigitOrSym := false
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			hasAlpha = true
		case c == '_':
			// Underscore alone with letters (e.g. "include_exclude") can collide with GenUI/JSON identifiers.
		default:
			hasDigitOrSym = true
		}
	}
	return hasAlpha && hasDigitOrSym
}

func registerUISecretLiteral(scope, val string) {
	if !isSafeProcessWideSecretLiteral(val) {
		return
	}
	agentcore.RegisterSecretLiterals(scope, true, val)
}

// passwordFieldIDsFromHistory extracts the field IDs declared with
// `format: "password"` in `show_ui` cards in the conversation history, walking
// nested layout components (`section`, `columns`, `tabs`, `repeater`).
//
// With a cardID it reads only the latest card with that ID and reports
// whether one was found; it never merges fields from unrelated earlier cards.
// With cardID == "" it collects password fields from every card in history.
func passwordFieldIDsFromHistory(history []agent.HistoryEntry, cardID string) (map[string]bool, bool) {
	out := make(map[string]bool)
	cardID = strings.TrimSpace(cardID)
	if cardID != "" {
		// Scan in reverse so a reused tool-call ID resolves to the latest card with that ID.
		for i := len(history) - 1; i >= 0; i-- {
			h := history[i]
			if len(h.Content) == 0 {
				continue
			}
			switch h.Type {
			case "tool_result":
				var tr agent.ToolResultContent
				if err := json.Unmarshal(h.Content, &tr); err != nil {
					continue
				}
				if tr.ID != cardID || strings.TrimSpace(tr.Card) == "" {
					continue
				}
				var payload map[string]any
				if err := json.Unmarshal([]byte(tr.Card), &payload); err != nil {
					continue
				}
				collectPasswordComponentIDs(payload["components"], out)
				return out, true
			case "tool_call":
				var tc agent.ToolCallContent
				if err := json.Unmarshal(h.Content, &tc); err != nil {
					continue
				}
				if tc.ID != cardID || tc.Name != "show_ui" || strings.TrimSpace(tc.Input) == "" {
					continue
				}
				var payload map[string]any
				if err := json.Unmarshal([]byte(tc.Input), &payload); err != nil {
					continue
				}
				if _, ok := payload["components"]; !ok {
					continue
				}
				collectPasswordComponentIDs(payload["components"], out)
				return out, true
			}
		}
		// An explicit card=<id> was provided on the submission, so do not merge
		// password field IDs from unrelated earlier cards in the conversation.
		return out, false
	}
	for _, h := range history {
		if len(h.Content) == 0 {
			continue
		}
		switch h.Type {
		case "tool_call":
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
		case "tool_result":
			var tr agent.ToolResultContent
			if err := json.Unmarshal(h.Content, &tr); err != nil {
				continue
			}
			if strings.TrimSpace(tr.Card) == "" {
				continue
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(tr.Card), &payload); err != nil {
				continue
			}
			collectPasswordComponentIDs(payload["components"], out)
		}
	}
	return out, false
}

func submissionCardID(message string) string {
	header := message
	if nl := strings.IndexByte(header, '\n'); nl >= 0 {
		header = header[:nl]
	}
	if brace := strings.IndexByte(header, '{'); brace >= 0 {
		header = header[:brace]
	}
	for _, field := range strings.Fields(header) {
		if strings.HasPrefix(field, "card=") {
			return strings.Trim(strings.TrimPrefix(field, "card="), " []")
		}
	}
	return ""
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
func (s *Server) conversationOwnerEmail(ctx context.Context, userEmail, convID string) string {
	if s.store == nil || strings.TrimSpace(convID) == "" {
		return ""
	}
	if conv, err := s.getOwnedOrMultiplayerConversation(ctx, userEmail, convID); err == nil && conv != nil {
		return strings.TrimSpace(conv.UserEmail)
	}
	return ""
}

func historyEntryAuthoredByCaller(tc agent.TextContent, userEmail, convOwnerEmail string) bool {
	if strings.TrimSpace(tc.Kind) != "" {
		return false
	}
	caller := strings.TrimSpace(userEmail)
	if caller == "" {
		return false
	}
	if sender := strings.TrimSpace(tc.SenderEmail); sender != "" {
		return strings.EqualFold(sender, caller)
	}
	if owner := strings.TrimSpace(convOwnerEmail); owner != "" {
		return strings.EqualFold(owner, caller)
	}
	return true
}

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
	ownerEmail := s.conversationOwnerEmail(ctx, userEmail, convID)
	return s.vaultUISubmissionSecretsWithHistoryAndOwner(userEmail, ownerEmail, convID, message, history)
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

func postSubmissionConfirmedOriginsFromHistory(history []agent.HistoryEntry, userEmail, convOwnerEmail string) []string {
	lastSubmissionIdx := -1
	for i := len(history) - 1; i >= 0; i-- {
		h := history[i]
		if h.Role != "user" || (h.Type != "" && h.Type != "text") || len(h.Content) == 0 {
			continue
		}
		var tc agent.TextContent
		if err := json.Unmarshal(h.Content, &tc); err != nil {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(tc.Text), "[UI submission]") {
			lastSubmissionIdx = i
			break
		}
	}
	if lastSubmissionIdx < 0 {
		return nil
	}
	var out []string
	for i := len(history) - 1; i > lastSubmissionIdx; i-- {
		h := history[i]
		if h.Role != "user" || (h.Type != "" && h.Type != "text") || len(h.Content) == 0 {
			continue
		}
		var tc agent.TextContent
		if err := json.Unmarshal(h.Content, &tc); err != nil {
			continue
		}
		if !historyEntryAuthoredByCaller(tc, userEmail, convOwnerEmail) {
			continue
		}
		tokens := extractURLTokensFromText(tc.Text)
		if len(tokens) > 0 {
			out = append(out, tokens...)
			break
		}
	}
	return out
}

func (s *Server) vaultUISubmissionSecretsWithHistory(userEmail, convID, message string, history []agent.HistoryEntry) string {
	return s.vaultUISubmissionSecretsWithHistoryAndOwner(userEmail, "", convID, message, history)
}

func (s *Server) vaultUISubmissionSecretsWithHistoryAndOwner(userEmail, convOwnerEmail, convID, message string, history []agent.HistoryEntry) string {
	_ = convOwnerEmail
	trimmed := strings.TrimSpace(message)
	if !strings.HasPrefix(trimmed, "[UI submission]") {
		return message
	}
	openIdx := strings.IndexByte(message, '{')
	closeIdx := strings.LastIndexByte(message, '}')
	if openIdx < 0 || closeIdx <= openIdx {
		return message
	}
	jsonPart := message[openIdx : closeIdx+1]
	var data map[string]any
	if err := json.Unmarshal([]byte(jsonPart), &data); err != nil || len(data) == 0 {
		return message
	}

	pwFields, cardFound := passwordFieldIDsFromHistory(history, submissionCardID(message))
	mutated, vaultedKeys, cardOrigins := s.vaultSubmissionMap(userEmail, convID, "", data, pwFields, cardFound)
	if mutated || len(cardOrigins) > 0 {
		s.uiSecrets.bindSubmissionOrigins(userEmail, convID, vaultedKeys, cardOrigins)
	}
	if !mutated {
		return message
	}
	redactedBytes, err := json.Marshal(data)
	if err != nil {
		return message
	}
	return message[:openIdx] + string(redactedBytes) + message[closeIdx+1:]
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
	if allow, explicit := s.uiSecrets.consumeEveryoneShare(userEmail, convID); explicit {
		return allow
	}
	if s.store == nil || strings.TrimSpace(convID) == "" {
		return false
	}
	history, err := s.store.LoadHistory(ctx, convID)
	if err != nil {
		return false
	}
	ownerEmail := s.conversationOwnerEmail(ctx, userEmail, convID)
	for i := len(history) - 1; i >= 0; i-- {
		h := history[i]
		if h.Role != "user" || (h.Type != "" && h.Type != "text") || len(h.Content) == 0 {
			continue
		}
		var tc agent.TextContent
		if err := json.Unmarshal(h.Content, &tc); err != nil {
			continue
		}
		if !historyEntryAuthoredByCaller(tc, userEmail, ownerEmail) {
			continue
		}
		lower := strings.ToLower(tc.Text)
		confirmed := strings.Contains(lower, "everyone") ||
			strings.Contains(lower, "fleet-wide") ||
			strings.Contains(lower, "fleet wide") ||
			strings.Contains(lower, "all users") ||
			strings.Contains(lower, "whole team")
		if confirmed {
			// Mark consumed so a single turn's confirmation cannot authorize a later connector.
			s.uiSecrets.setAllowEveryone(userEmail, convID, false)
		}
		return confirmed
	}
	return false
}

func (s *Server) vaultSubmissionMap(userEmail, convID, prefix string, data map[string]any, pwFields map[string]bool, cardFound bool) (mutated bool, vaultedKeys []string, cardOrigins []string) {
	scopeKey := uiSecretKey(userEmail, convID)
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
			// When the submitted card was resolved in conversation history (cardFound == true),
			// its component tree is authoritative: ONLY fields explicitly declared with
			// `format: "password"` on that card are vaulted. Heuristic field-name matching
			// applies only as a fallback when no matching card spec was found in history.
			if pwFields[k] || pwFields[strings.ToLower(k)] || (!cardFound && isLikelySecretFieldID(k)) {
				s.uiSecrets.put(userEmail, convID, stashKey, val)
				registerUISecretLiteral("ui_secret:"+scopeKey+":"+stashKey, val)
				vaultedKeys = append(vaultedKeys, stashKey)
				if stashKey != k {
					s.uiSecrets.put(userEmail, convID, k, val)
					registerUISecretLiteral("ui_secret:"+scopeKey+":"+k, val)
					vaultedKeys = append(vaultedKeys, k)
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
				cardOrigins = append(cardOrigins, val)
			}
		case map[string]any:
			subMut, subKeys, subOrigins := s.vaultSubmissionMap(userEmail, convID, stashKey, val, pwFields, cardFound)
			if subMut {
				mutated = true
			}
			vaultedKeys = append(vaultedKeys, subKeys...)
			cardOrigins = append(cardOrigins, subOrigins...)
		case []any:
			for idx, item := range val {
				if itemMap, ok := item.(map[string]any); ok {
					itemPrefix := fmt.Sprintf("%s[%d]", stashKey, idx)
					subMut, subKeys, subOrigins := s.vaultSubmissionMap(userEmail, convID, itemPrefix, itemMap, pwFields, cardFound)
					if subMut {
						mutated = true
					}
					vaultedKeys = append(vaultedKeys, subKeys...)
					cardOrigins = append(cardOrigins, subOrigins...)
				}
			}
		}
	}
	return mutated, vaultedKeys, cardOrigins
}

// isCatalogApprovedHost reports whether targetHost matches the operator-curated
// RemoteMCPCatalog entry URL for the named connector. An empty connectorName
// never matches so catalog approval is strictly bound to the selected connector.
func (s *Server) isCatalogApprovedHost(connectorName, targetURLOrHost string) bool {
	connectorName = strings.TrimSpace(connectorName)
	if s.clientConfig == nil || connectorName == "" {
		return false
	}
	targetHost := normalizeOriginHost(targetURLOrHost)
	if targetHost == "" {
		return false
	}
	for _, entry := range s.clientConfig.RemoteMCPCatalog {
		if !strings.EqualFold(strings.TrimSpace(entry.Name), connectorName) {
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
// origin from the conversation's show_ui submission or user chat messages.
func (s *Server) resolveSecretForURL(ctx context.Context, userEmail, convID, refOrValue, targetURL string) (string, error) {
	return s.resolveSecretForConnectorURL(ctx, userEmail, convID, "", refOrValue, targetURL)
}

func secretFieldID(refOrValue string) string {
	trimmed := strings.TrimSpace(refOrValue)
	if strings.HasPrefix(trimmed, "$secret:") {
		return strings.TrimSpace(strings.TrimPrefix(trimmed, "$secret:"))
	}
	return trimmed
}

func (s *Server) resolveSecretForConnectorURL(ctx context.Context, userEmail, convID, connectorName, refOrValue, targetURL string) (string, error) {
	val, isVaulted := s.uiSecrets.lookupSecret(userEmail, convID, refOrValue)
	if !isVaulted {
		return val, nil
	}
	if val == "" {
		return "", fmt.Errorf("vaulted secret %q was not found or expired; ask the user to submit the credential again via show_ui", strings.TrimSpace(refOrValue))
	}
	if _, err := mcpoauth.ValidateServerURL(targetURL, s.allowInsecureHTTP()); err != nil {
		return "", fmt.Errorf("vaulted secret %q requires a valid HTTPS target URL: %w", strings.TrimSpace(refOrValue), err)
	}
	targetHost := normalizeOriginHost(targetURL)
	if targetHost == "" {
		return "", fmt.Errorf("vaulted secret %q requires a valid target URL", strings.TrimSpace(refOrValue))
	}
	fieldID := secretFieldID(refOrValue)
	allowed, hasExplicitBinding := s.uiSecrets.isFieldOriginAllowed(userEmail, convID, fieldID, targetHost)
	if !allowed && !hasExplicitBinding && !s.isCatalogApprovedHost(connectorName, targetHost) {
		if s.store != nil && convID != "" {
			if history, err := s.store.LoadHistory(ctx, convID); err == nil {
				ownerEmail := s.conversationOwnerEmail(ctx, userEmail, convID)
				for _, u := range postSubmissionConfirmedOriginsFromHistory(history, userEmail, ownerEmail) {
					s.uiSecrets.allowOrigin(userEmail, convID, u)
				}
			}
		}
		allowed, _ = s.uiSecrets.isFieldOriginAllowed(userEmail, convID, fieldID, targetHost)
	}
	if !allowed && !s.isCatalogApprovedHost(connectorName, targetHost) {
		return "", fmt.Errorf("vaulted secret %q is not authorized for host %q; ask the user to confirm the target URL in a show_ui form alongside the credential or in their chat message", strings.TrimSpace(refOrValue), targetHost)
	}
	if fieldID != "" {
		registerUISecretLiteral("ui_secret:"+uiSecretKey(userEmail, convID)+":"+fieldID, val)
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
		// Normalized here as well as in the tool: an approved call reaches
		// this handler with the action exactly as the model wrote it.
		switch tools.NormalizeConnectorStudioAction(params.Action) {
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

func connectorReauthHint(name string) string {
	return fmt.Sprintf("The user's '%s' connection requires re-authentication (status: login_required). Stop immediately and ask the user to click 'Sign in' next to '%s' in Settings → Connections (/settings/connections) — do NOT fall back to shared report tools or other users' data as a substitute.", name, name)
}

func sharedConnectorReauthHint(name, owner string) string {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		owner = "the connection owner"
	}
	return fmt.Sprintf("The shared '%s' connection (owned by %s) requires re-authentication (status: login_required). Stop immediately and tell the user to ask %s to re-sign in to '%s' in Settings → Connections (/settings/connections), or to connect their own '%s' account — do NOT fall back to unrelated shared report tools or other users' data as a substitute.", name, owner, owner, name, name)
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
	var reauthHints []string
	if s.remoteMCP != nil && s.remoteMCP.Enabled() && user != "" {
		if own, err := s.remoteMCP.ListServers(ctx, user); err == nil {
			for _, srv := range own {
				if matchesQuery(srv.Name, srv.Account, srv.URL, srv.Transport) {
					entry := map[string]any{
						"id":                srv.ID,
						"name":              srv.Name,
						"account":           srv.Account,
						"url":               srv.URL,
						"auth_kind":         srv.AuthKind,
						"transport":         srv.Transport,
						"status":            srv.Status,
						"custom_tool_count": len(srv.CustomTools),
						"scope":             "personal",
					}
					if strings.EqualFold(strings.TrimSpace(srv.Status), "login_required") {
						hint := connectorReauthHint(srv.Name)
						entry["hint"] = hint
						reauthHints = append(reauthHints, hint)
					}
					existing = append(existing, entry)
				}
			}
		}
		if shared, err := s.remoteMCP.SharedWithMe(ctx, user); err == nil {
			for _, srv := range shared {
				if matchesQuery(srv.Name, srv.Account, srv.URL, srv.Transport) {
					entry := map[string]any{
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
					}
					if strings.EqualFold(strings.TrimSpace(srv.Status), "login_required") {
						hint := sharedConnectorReauthHint(srv.Name, srv.UserEmail)
						entry["hint"] = hint
						reauthHints = append(reauthHints, hint)
					}
					existing = append(existing, entry)
				}
			}
		}
	}

	out := map[string]any{
		"query":                query,
		"catalog_matches":      catalogMatches,
		"bundled_matches":      bundledMatches,
		"existing_connections": existing,
		"tier_guidance":        "Tier 1 (Preferred): If an official MCP server exists in catalog_matches, connect it with action='connect_mcp'. Tier 2 (API Wrapper): If the service has a REST/GraphQL/JSON API (or you discover one via action='probe_http' or action='probe_browser' with capture_network=true), build a custom 'http_tools' connector with action='create_custom_connector'. Tier 3 (Browser Automation Fallback): If the site has no usable API, use action='probe_browser' (or action='start_copilot_session' if login hits CAPTCHA/2FA) and build a custom 'playwright' connector with action='create_custom_connector'.",
	}
	if len(reauthHints) > 0 {
		out["reauth_required_hint"] = strings.Join(reauthHints, " ")
	}
	return out, nil
}

func (s *Server) allowInsecureHTTP() bool {
	return s.remoteMCP != nil && s.remoteMCP.AllowInsecureHTTP()
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
	targetURL, err := mcpoauth.ValidateServerURL(strings.TrimSpace(params.URL), s.allowInsecureHTTP())
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}

	secret, err := s.resolveSecretForURL(ctx, user, convID, params.SecretRef, targetURL)
	if err != nil {
		return nil, err
	}
	if secret != "" {
		registerUISecretLiteral("probe_http:"+uiSecretKey(user, convID), secret)
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
	req, err := http.NewRequestWithContext(probeCtx, method, reqURL, bodyReader) // #nosec G704 — reqURL is validated via mcpoauth.ValidateServerURL and dialed via safeProbeHTTPClient
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

	resp, err := s.safeProbeHTTPClient().Do(req) // #nosec G704 — reqURL is validated via mcpoauth.ValidateServerURL and dialed via safeProbeHTTPClient
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
	scopeKey := uiSecretKey(user, convID)
	if password != "" {
		registerUISecretLiteral("pw_password:"+scopeKey, password)
	}
	if secret != "" {
		registerUISecretLiteral("pw_secret:"+scopeKey, secret)
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
	sessionID := strings.TrimSpace(params.SessionID)
	var sess *playwrightrunner.LiveSession
	if sessionID != "" {
		var serr error
		sess, serr = playwrightrunner.DefaultSessionManager().GetSession(sessionID, user, "")
		if serr != nil {
			return nil, serr
		}
	}
	cleanURL := strings.TrimSpace(params.URL)
	if cleanURL != "" || sess == nil {
		var uerr error
		cleanURL, uerr = playwrightrunner.ValidateTargetURL(params.URL, s.allowInsecureHTTP())
		if uerr != nil {
			return nil, uerr
		}
	}
	credTargetURL := cleanURL
	if credTargetURL == "" && sess != nil {
		credTargetURL = sess.BaseURL()
	}
	creds, err := s.resolvePlaywrightCredentials(ctx, user, convID, credTargetURL, params)
	if err != nil {
		return nil, err
	}
	inspectDOM := params.InspectDOM
	captureNet := params.CaptureNetwork
	if !inspectDOM && !captureNet {
		inspectDOM = true
		captureNet = true
	}

	if sess != nil {
		if creds.Username != "" || creds.Password != "" || creds.Secret != "" {
			sess.UpdateCredentials(creds)
		}
		steps := substituteStepCreds(params.Steps, creds)
		if cleanURL != "" && (len(steps) == 0 || steps[0].Action != "goto") {
			steps = append([]playwrightrunner.Step{{Action: "goto", Value: cleanURL}}, steps...)
		}
		if len(steps) > 0 {
			return sess.RunSteps(ctx, steps, inspectDOM, false)
		}
		return sess.Snapshot(ctx, inspectDOM, false)
	}

	return playwrightrunner.Run(ctx, playwrightrunner.RunRequest{
		URL:            cleanURL,
		Steps:          params.Steps,
		Credentials:    creds,
		InspectDOM:     inspectDOM,
		CaptureNetwork: captureNet,
	})
}

func (s *Server) connectorStudioStartCopilotSession(ctx context.Context, user, convID string, params tools.ConnectorStudioParams) (any, error) {
	cleanURL, err := playwrightrunner.ValidateTargetURL(params.URL, s.allowInsecureHTTP())
	if err != nil {
		return nil, err
	}
	creds, err := s.resolvePlaywrightCredentials(ctx, user, convID, cleanURL, params)
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

func (s *Server) enableConnectorOnConversation(ctx context.Context, user string, conv *store.Conversation, connectorName, account string) {
	if s.store == nil || conv == nil || strings.TrimSpace(connectorName) == "" {
		return
	}
	norm := strings.ToLower(strings.TrimSpace(connectorName))
	cleanAcct := strings.TrimSpace(account)
	unlock := s.connectorLocks.lock(conv.ID)
	defer unlock()
	fresh, err := s.getOwnedOrMultiplayerConversation(ctx, user, conv.ID)
	if err != nil || fresh == nil {
		fresh = conv
	}
	ownerEmail := convOwnerEmail(fresh, user)
	merged := append([]string(nil), fresh.OptionalMCPServersEnabled...)
	found := false
	for _, existing := range merged {
		if strings.EqualFold(strings.TrimSpace(existing), norm) {
			found = true
			break
		}
	}
	if !found {
		merged = append(merged, norm)
		sort.Strings(merged)
		if err := s.store.SetOptionalMCPServers(ctx, ownerEmail, fresh.ID, merged); err == nil {
			conv.OptionalMCPServersEnabled = merged
		}
	}
	if cleanAcct != "" {
		accounts := make(map[string]string, len(fresh.MCPAccounts)+1)
		for k, v := range fresh.MCPAccounts {
			accounts[k] = v
		}
		accounts[norm] = cleanAcct
		if err := s.store.SetConversationMCPAccounts(ctx, ownerEmail, fresh.ID, accounts); err == nil {
			conv.MCPAccounts = accounts
		}
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
	apiKey, err := s.resolveSecretForConnectorURL(ctx, user, convID, params.Name, params.SecretRef, params.URL)
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		registerUISecretLiteral("connect_mcp:"+uiSecretKey(user, convID), apiKey)
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

	s.enableConnectorOnConversation(ctx, user, conv, server.Name, server.Account)

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

	s.enableConnectorOnConversation(ctx, user, conv, server.Name, server.Account)

	regName := agentcore.RegisteredMCPName(server.Name, server.Account)
	toolNames := make([]string, 0, len(server.CustomTools))
	for _, ct := range server.CustomTools {
		toolNames = append(toolNames, fmt.Sprintf("mcp_%s_%s", regName, ct.Name))
	}

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

func resolveMountedCustomTool(ov *agent.RemoteMCPOverlay, wantName, wantAcct, wantTool string) (regName, resolvedAcct, resolvedTool, fullToolName string) {
	regName = agentcore.RegisteredMCPName(wantName, wantAcct)
	resolvedAcct = wantAcct
	for rn, seat := range ov.Seats {
		if strings.EqualFold(seat.Server, wantName) && (wantAcct == "" || strings.EqualFold(seat.Account, wantAcct)) {
			regName = rn
			resolvedAcct = seat.Account
			break
		}
	}
	if !ov.Servers[regName] {
		for _, st := range ov.Catalog {
			if st.ServerName != "" {
				regName = st.ServerName
				break
			}
		}
	}
	resolvedTool = strings.TrimPrefix(wantTool, "mcp_"+regName+"_")
	resolvedTool = strings.TrimPrefix(resolvedTool, "mcp_"+wantName+"_")
	fullToolName = fmt.Sprintf("mcp_%s_%s", regName, resolvedTool)
	return regName, resolvedAcct, resolvedTool, fullToolName
}

func (s *Server) checkCustomToolTestReadOnly(ctx context.Context, ov *agent.RemoteMCPOverlay, user, wantName, wantAcct, regName, wantTool, fullToolName string) error {
	if agentcore.IsCriticalTool(fullToolName) {
		return fmt.Errorf("test_custom_tool only executes read-only tools directly; %q is a mutating/critical tool — invoke %s directly so it routes through the approval gate", wantTool, fullToolName)
	}
	for _, st := range ov.Catalog {
		if st.ServerName == regName && st.Tool.Name == wantTool && st.Tool.Critical {
			return fmt.Errorf("test_custom_tool only executes read-only tools directly; %q is a mutating/critical tool — invoke %s directly so it routes through the approval gate", wantTool, fullToolName)
		}
	}
	if conns, cerr := s.remoteMCP.ConnectedServersForUser(ctx, user); cerr == nil {
		for _, c := range conns {
			if !strings.EqualFold(c.Name, wantName) || (wantAcct != "" && c.Account != wantAcct) {
				continue
			}
			for _, ct := range c.CustomTools {
				if ct.Name == wantTool && c.Transport == "http_tools" && ct.Method != "" && !strings.EqualFold(ct.Method, http.MethodGet) {
					return fmt.Errorf("test_custom_tool only executes read-only GET tools directly; %q uses HTTP %s — invoke %s directly so it routes through the approval gate", wantTool, ct.Method, fullToolName)
				}
			}
		}
	}
	return nil
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

	regName, wantAcct, wantTool, fullToolName := resolveMountedCustomTool(ov, wantName, wantAcct, wantTool)
	if err := s.checkCustomToolTestReadOnly(ctx, ov, user, wantName, wantAcct, regName, wantTool, fullToolName); err != nil {
		return nil, err
	}
	args := params.ToolArgs
	if args == nil {
		args = map[string]any{}
	}
	res, err := ov.Client.CallToolOn(ctx, regName, wantTool, args)
	if err != nil {
		return map[string]any{
			"status": "error",
			"tool":   fullToolName,
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
		"tool":     fullToolName,
		"output":   strings.Join(texts, "\n"),
	}, nil
}
