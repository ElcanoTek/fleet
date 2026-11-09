package ms365mcp

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	pkceStoreTTL        = time.Hour
	pkceStoreMaxEntries = 1000
)

var defaultLoopbackRedirectPatterns = []string{
	"http://localhost:*",
	"http://127.0.0.1:*",
	"http://[::1]:*",
	"https://localhost:*",
	"https://127.0.0.1:*",
	"https://[::1]:*",
}

type pkceEntry struct {
	state               string
	clientCodeChallenge string
	serverCodeVerifier  string
	createdAt           time.Time
}

type pkceStore struct {
	mu      sync.Mutex
	entries []pkceEntry
}

func (p *pkceStore) sweepLocked(now time.Time) {
	kept := p.entries[:0]
	for _, e := range p.entries {
		if now.Sub(e.createdAt) <= pkceStoreTTL {
			kept = append(kept, e)
		}
	}
	p.entries = kept
}

func (p *pkceStore) put(state, clientCodeChallenge, serverCodeVerifier string) {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()

	p.sweepLocked(now)
	for i := range p.entries {
		if p.entries[i].state == state {
			p.entries = append(p.entries[:i], p.entries[i+1:]...)
			break
		}
	}
	for len(p.entries) >= pkceStoreMaxEntries {
		p.entries = p.entries[1:]
	}
	p.entries = append(p.entries, pkceEntry{
		state:               state,
		clientCodeChallenge: clientCodeChallenge,
		serverCodeVerifier:  serverCodeVerifier,
		createdAt:           now,
	})
}

func (p *pkceStore) consumeMatchingVerifier(clientCodeVerifier string) string {
	clientChallenge := computeS256Challenge(clientCodeVerifier)
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()

	p.sweepLocked(now)
	for i, e := range p.entries {
		if e.clientCodeChallenge == clientChallenge {
			p.entries = append(p.entries[:i], p.entries[i+1:]...)
			return e.serverCodeVerifier
		}
	}
	return ""
}

func generatePKCEVerifier() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func computeS256Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (h *Handler) handleOAuthProtectedResource(w http.ResponseWriter, r *http.Request) {
	baseURL := h.cfg.baseURLForRequest(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 baseURL + "/mcp",
		"authorization_servers":    []string{baseURL},
		"scopes_supported":         DefaultAllowedScopes,
		"bearer_methods_supported": []string{"header"},
	})
}

func (h *Handler) handleOAuthAuthorizationServer(w http.ResponseWriter, r *http.Request) {
	baseURL := h.cfg.baseURLForRequest(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                baseURL,
		"authorization_endpoint":                baseURL + "/authorize",
		"token_endpoint":                        baseURL + "/token",
		"registration_endpoint":                 baseURL + "/register",
		"response_types_supported":              []string{"code"},
		"response_modes_supported":              []string{"query"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"code_challenge_methods_supported":      []string{"S256"},
		"scopes_supported":                      DefaultAllowedScopes,
	})
}

func (h *Handler) handleDynamicClientRegistration(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ClientName    string   `json:"client_name"`
		RedirectURIs  []string `json:"redirect_uris"`
		GrantTypes    []string `json:"grant_types"`
		ResponseTypes []string `json:"response_types"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body)
	}
	if body.ClientName == "" {
		body.ClientName = "MCP Client"
	}
	if len(body.RedirectURIs) == 0 {
		body.RedirectURIs = []string{}
	}
	if len(body.GrantTypes) == 0 {
		body.GrantTypes = []string{"authorization_code", "refresh_token"}
	}
	if len(body.ResponseTypes) == 0 {
		body.ResponseTypes = []string{"code"}
	}
	now := time.Now()
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  fmt.Sprintf("mcp-client-%d", now.UnixMilli()),
		"client_name":                body.ClientName,
		"redirect_uris":              body.RedirectURIs,
		"grant_types":                body.GrantTypes,
		"response_types":             body.ResponseTypes,
		"token_endpoint_auth_method": "none",
		"client_id_issued_at":        now.Unix(),
	})
}

func (h *Handler) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	responseType := q.Get("response_type")
	redirectURI := q.Get("redirect_uri")
	scope := q.Get("scope")
	state := q.Get("state")
	codeChallenge := q.Get("code_challenge")
	codeChallengeMethod := q.Get("code_challenge_method")

	if redirectURI == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":             "invalid_request",
			"error_description": "Missing required parameter: redirect_uri",
		})
		return
	}
	patterns := append([]string{}, defaultLoopbackRedirectPatterns...)
	patterns = append(patterns, h.cfg.AllowedRedirectURIs...)
	if !isRedirectURIAllowed(redirectURI, patterns) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":             "invalid_request",
			"error_description": fmt.Sprintf("redirect_uri %q is not in the allowed list", redirectURI),
		})
		return
	}
	if responseType != "" && responseType != "code" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":             "unsupported_response_type",
			"error_description": fmt.Sprintf("Only response_type=code is supported, got %q", responseType),
		})
		return
	}
	if codeChallenge != "" && codeChallengeMethod != "" && codeChallengeMethod != "S256" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":             "invalid_request",
			"error_description": fmt.Sprintf("Only code_challenge_method=S256 is supported, got %q", codeChallengeMethod),
		})
		return
	}

	resolvedScope := filterAllowedScopes(scope)

	authBase := strings.TrimRight(h.cfg.AuthBaseURL, "/")
	if authBase == "" {
		authBase = defaultAuthBaseURL
	}
	tenantID := h.cfg.TenantID
	if tenantID == "" {
		tenantID = defaultTenantID
	}
	msAuthURL, err := url.Parse(fmt.Sprintf("%s/%s/oauth2/v2.0/authorize", authBase, url.PathEscape(tenantID)))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":             "server_error",
			"error_description": err.Error(),
		})
		return
	}
	params := url.Values{}
	params.Set("client_id", h.cfg.ClientID)
	params.Set("response_type", "code")
	params.Set("redirect_uri", redirectURI)
	params.Set("scope", resolvedScope)
	if state != "" {
		params.Set("state", state)
	}
	if codeChallenge != "" && state != "" {
		if serverVerifier, err := generatePKCEVerifier(); err == nil {
			h.pkce.put(state, codeChallenge, serverVerifier)
			params.Set("code_challenge", computeS256Challenge(serverVerifier))
			params.Set("code_challenge_method", "S256")
		} else {
			params.Set("code_challenge", codeChallenge)
			params.Set("code_challenge_method", "S256")
		}
	} else if codeChallenge != "" {
		params.Set("code_challenge", codeChallenge)
		if codeChallengeMethod != "" {
			params.Set("code_challenge_method", codeChallengeMethod)
		} else {
			params.Set("code_challenge_method", "S256")
		}
	}
	msAuthURL.RawQuery = params.Encode()
	http.Redirect(w, r, msAuthURL.String(), http.StatusFound)
}

func (h *Handler) handleToken(w http.ResponseWriter, r *http.Request) {
	vals, err := parseTokenRequestParams(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":             "invalid_request",
			"error_description": err.Error(),
		})
		return
	}

	grantType := vals.Get("grant_type")
	code := vals.Get("code")
	redirectURI := vals.Get("redirect_uri")
	codeVerifier := vals.Get("code_verifier")
	refreshToken := vals.Get("refresh_token")

	authBase := strings.TrimRight(h.cfg.AuthBaseURL, "/")
	if authBase == "" {
		authBase = defaultAuthBaseURL
	}
	tenantID := h.cfg.TenantID
	if tenantID == "" {
		tenantID = defaultTenantID
	}
	tokenURL := fmt.Sprintf("%s/%s/oauth2/v2.0/token", authBase, url.PathEscape(tenantID))

	upParams := url.Values{}
	upParams.Set("client_id", h.cfg.ClientID)
	upParams.Set("scope", strings.Join(DefaultAllowedScopes, " "))

	switch grantType {
	case "authorization_code":
		if code == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":             "invalid_request",
				"error_description": "Missing required parameter: code",
			})
			return
		}
		upParams.Set("grant_type", "authorization_code")
		upParams.Set("code", code)
		if redirectURI != "" {
			upParams.Set("redirect_uri", redirectURI)
		}
		if codeVerifier != "" {
			if serverVerifier := h.pkce.consumeMatchingVerifier(codeVerifier); serverVerifier != "" {
				upParams.Set("code_verifier", serverVerifier)
			} else {
				upParams.Set("code_verifier", codeVerifier)
			}
		}
	case "refresh_token":
		if refreshToken == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":             "invalid_request",
				"error_description": "Missing required parameter: refresh_token",
			})
			return
		}
		upParams.Set("grant_type", "refresh_token")
		upParams.Set("refresh_token", refreshToken)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":             "unsupported_grant_type",
			"error_description": fmt.Sprintf("Grant type %q is not supported", grantType),
		})
		return
	}

	origin := strings.TrimSpace(r.Header.Get("Origin"))
	status, payload, err := h.postTokenToMicrosoft(r.Context(), tokenURL, upParams, origin)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":             "server_error",
			"error_description": err.Error(),
		})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, payload)
}

func (h *Handler) postTokenToMicrosoft(ctx interface {
	Deadline() (time.Time, bool)
	Done() <-chan struct{}
	Err() error
	Value(any) any
}, tokenURL string, baseParams url.Values, origin string) (int, map[string]any, error) {
	send := func(includeSecret bool) (int, map[string]any, string, error) {
		params := url.Values{}
		for k, v := range baseParams {
			params[k] = append([]string(nil), v...)
		}
		if includeSecret && h.cfg.ClientSecret != "" {
			params.Set("client_secret", h.cfg.ClientSecret)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(params.Encode()))
		if err != nil {
			return 0, nil, "", err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" && h.cfg.ClientSecret == "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := h.httpClient().Do(req)
		if err != nil {
			return 0, nil, "", err
		}
		defer resp.Body.Close()
		rawBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var data map[string]any
		if err := json.Unmarshal(rawBytes, &data); err != nil || data == nil {
			data = map[string]any{
				"error":             "upstream_error",
				"error_description": strings.TrimSpace(string(rawBytes)),
			}
		}
		return resp.StatusCode, data, string(rawBytes), nil
	}

	status, data, rawText, err := send(h.cfg.ClientSecret != "")
	if err != nil {
		return 0, nil, err
	}
	if status >= 400 && h.cfg.ClientSecret != "" && isPublicClientError(data, rawText) {
		status, data, _, err = send(false)
		if err != nil {
			return 0, nil, err
		}
	}
	return status, data, nil
}

func isPublicClientError(data map[string]any, rawText string) bool {
	if strings.Contains(rawText, "AADSTS700025") {
		return true
	}
	if codes, ok := data["error_codes"].([]any); ok {
		for _, c := range codes {
			if num, ok := c.(float64); ok && int(num) == 700025 {
				return true
			}
		}
	}
	if desc, ok := data["error_description"].(string); ok && strings.Contains(desc, "AADSTS700025") {
		return true
	}
	return false
}

func parseTokenRequestParams(r *http.Request) (url.Values, error) {
	ct := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	if ct == "application/json" {
		var body map[string]any
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
			return nil, fmt.Errorf("decode JSON token request: %w", err)
		}
		vals := url.Values{}
		for k, v := range body {
			if s, ok := v.(string); ok {
				vals.Set(k, s)
			}
		}
		return vals, nil
	}
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read token request body: %w", err)
	}
	vals, err := url.ParseQuery(string(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("parse form token request: %w", err)
	}
	return vals, nil
}

func filterAllowedScopes(raw string) string {
	allowedSet := make(map[string]string, len(DefaultAllowedScopes))
	for _, s := range DefaultAllowedScopes {
		allowedSet[strings.ToLower(s)] = s
	}
	var filtered []string
	for _, token := range strings.Fields(raw) {
		if canonical, ok := allowedSet[strings.ToLower(token)]; ok {
			filtered = appendUniqueString(filtered, canonical)
		}
	}
	if len(filtered) == 0 {
		return strings.Join(DefaultAllowedScopes, " ")
	}
	return strings.Join(filtered, " ")
}

func isRedirectURIAllowed(redirectURI string, patterns []string) bool {
	parsed, err := url.Parse(redirectURI)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return false
	}
	for _, rawPattern := range patterns {
		pattern := strings.TrimSpace(rawPattern)
		if pattern == "" {
			continue
		}
		if strings.Contains(pattern, ":*") {
			withDummyPort := strings.Replace(pattern, ":*", ":80", 1)
			pp, err := url.Parse(withDummyPort)
			if err != nil {
				continue
			}
			if !strings.EqualFold(parsed.Scheme, pp.Scheme) || !strings.EqualFold(parsed.Hostname(), pp.Hostname()) {
				continue
			}
			afterPort := ""
			if idx := strings.Index(pattern, ":*"); idx >= 0 {
				afterPort = pattern[idx+2:]
			}
			if afterPort == "" || afterPort == "/" || parsed.Path == pp.Path {
				return true
			}
			continue
		}
		pp, err := url.Parse(pattern)
		if err != nil {
			continue
		}
		if strings.EqualFold(parsed.Scheme, pp.Scheme) &&
			strings.EqualFold(parsed.Hostname(), pp.Hostname()) &&
			effectivePort(parsed) == effectivePort(pp) &&
			parsed.Path == pp.Path {
			return true
		}
	}
	return false
}

func effectivePort(u *url.URL) string {
	if u == nil {
		return ""
	}
	if p := u.Port(); p != "" {
		if (strings.EqualFold(u.Scheme, "https") && p == "443") || (strings.EqualFold(u.Scheme, "http") && p == "80") {
			return ""
		}
		return p
	}
	if _, p, err := net.SplitHostPort(u.Host); err == nil {
		if (strings.EqualFold(u.Scheme, "https") && p == "443") || (strings.EqualFold(u.Scheme, "http") && p == "80") {
			return ""
		}
		return p
	}
	return ""
}
