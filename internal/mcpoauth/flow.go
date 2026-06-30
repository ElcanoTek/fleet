package mcpoauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Token is the relevant subset of an RFC 6749 token response. Expiry is computed
// from expires_in at parse time so callers can compare against time.Now()
// without re-deriving it.
type Token struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	Scope        string
	Expiry       time.Time
}

// GeneratePKCE returns a fresh PKCE (RFC 7636) verifier and its S256 challenge.
// The verifier is 32 CSPRNG bytes base64url-encoded (43 chars, within the
// 43–128 range); the challenge is base64url(SHA256(verifier)).
func GeneratePKCE() (verifier, challenge string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("generate pkce verifier: %w", err)
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// GenerateState returns a CSPRNG, URL-safe state value (≥128 bits) for CSRF
// protection and as the server-side lookup key for the flow's stored PKCE
// verifier.
func GenerateState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// FlowConfig captures everything needed to drive the authorization-code flow
// against one authorization server for one MCP resource.
type FlowConfig struct {
	AuthorizationEndpoint string
	TokenEndpoint         string
	ClientID              string
	ClientSecret          string // empty for a public (PKCE-only) client
	RedirectURI           string
	Scopes                []string
	// Resource is the canonical MCP server URI sent as the RFC 8707 resource
	// indicator so the issued token's audience is bound to this server.
	Resource string
	// AuthMethods is token_endpoint_auth_methods_supported from AS metadata;
	// it selects client_secret_basic vs client_secret_post when a secret exists.
	AuthMethods []string
}

// AuthCodeURL builds the authorization request URL including PKCE S256 and the
// RFC 8707 resource indicator. state and codeChallenge are caller-supplied.
func (f FlowConfig) AuthCodeURL(state, codeChallenge string) string {
	v := url.Values{}
	v.Set("response_type", "code")
	v.Set("client_id", f.ClientID)
	v.Set("redirect_uri", f.RedirectURI)
	if len(f.Scopes) > 0 {
		v.Set("scope", strings.Join(f.Scopes, " "))
	}
	v.Set("state", state)
	v.Set("code_challenge", codeChallenge)
	v.Set("code_challenge_method", "S256")
	if f.Resource != "" {
		v.Set("resource", f.Resource)
	}
	sep := "?"
	if strings.Contains(f.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	return f.AuthorizationEndpoint + sep + v.Encode()
}

// Exchange swaps an authorization code for tokens, sending the PKCE verifier and
// the RFC 8707 resource indicator. If the AS rejects the resource parameter
// (invalid_target), it retries once without it and relies on scope-based
// audience.
func (f FlowConfig) Exchange(ctx context.Context, httpClient *http.Client, code, codeVerifier string) (*Token, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", f.RedirectURI)
	form.Set("code_verifier", codeVerifier)
	return f.tokenRequestWithResourceFallback(ctx, httpClient, form)
}

// Refresh exchanges a refresh token for a new access token (and possibly a
// rotated refresh token), carrying the resource indicator so the new token keeps
// the same audience binding. x/oauth2's reusable TokenSource cannot pass the
// resource param, which is exactly why this is a hand-rolled POST.
func (f FlowConfig) Refresh(ctx context.Context, httpClient *http.Client, refreshToken string) (*Token, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	if len(f.Scopes) > 0 {
		form.Set("scope", strings.Join(f.Scopes, " "))
	}
	tok, err := f.tokenRequestWithResourceFallback(ctx, httpClient, form)
	if err != nil {
		return nil, err
	}
	// Per OAuth 2.1, an AS MAY omit a new refresh token on refresh (no rotation).
	// Preserve the existing one so the caller doesn't lose its ability to refresh.
	if tok.RefreshToken == "" {
		tok.RefreshToken = refreshToken
	}
	return tok, nil
}

func (f FlowConfig) tokenRequestWithResourceFallback(ctx context.Context, httpClient *http.Client, form url.Values) (*Token, error) {
	tok, err := f.tokenRequest(ctx, httpClient, form, true)
	if err != nil && IsInvalidTarget(err) && f.Resource != "" {
		// The AS doesn't honor RFC 8707 resource — retry without it.
		return f.tokenRequest(ctx, httpClient, form, false)
	}
	return tok, err
}

// tokenRequest POSTs a form to the token endpoint, applying client
// authentication, optionally the resource indicator, and parsing the response
// (or RFC 6749 §5.2 error) into a Token / OAuthError.
func (f FlowConfig) tokenRequest(ctx context.Context, httpClient *http.Client, form url.Values, includeResource bool) (*Token, error) {
	// Copy so retries with/without resource don't mutate the caller's form.
	body := url.Values{}
	for k, vs := range form {
		for _, v := range vs {
			body.Add(k, v)
		}
	}
	if includeResource && f.Resource != "" {
		body.Set("resource", f.Resource)
	}

	useBasic := f.ClientSecret != "" && f.allowsBasicAuth()
	if !useBasic {
		// Public client, or client_secret_post: client_id (and secret) in body.
		body.Set("client_id", f.ClientID)
		if f.ClientSecret != "" {
			body.Set("client_secret", f.ClientSecret)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.TokenEndpoint, strings.NewReader(body.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if useBasic {
		// client_secret_basic: HTTP Basic with form-urlencoded id:secret.
		req.SetBasicAuth(url.QueryEscape(f.ClientID), url.QueryEscape(f.ClientSecret))
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token endpoint request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBytes))
	if err != nil {
		return nil, fmt.Errorf("read token response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, parseTokenError(raw, resp.StatusCode)
	}

	var tr struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(raw, &tr); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("token response contained no access_token")
	}
	tok := &Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		TokenType:    tr.TokenType,
		Scope:        tr.Scope,
	}
	if tr.ExpiresIn > 0 {
		tok.Expiry = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	return tok, nil
}

func (f FlowConfig) allowsBasicAuth() bool {
	if len(f.AuthMethods) == 0 {
		return true // default to Basic when the AS doesn't specify
	}
	for _, m := range f.AuthMethods {
		if strings.EqualFold(m, "client_secret_basic") {
			return true
		}
	}
	return false
}

// parseTokenError decodes an RFC 6749 §5.2 error body into an OAuthError. A
// non-JSON or fieldless body still yields a usable OAuthError carrying the HTTP
// status so callers can distinguish transient (5xx) from terminal failures.
func parseTokenError(raw []byte, status int) error {
	var er struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &er)
	code := er.Error
	if code == "" {
		code = fmt.Sprintf("http_%d", status)
	}
	return &OAuthError{Code: code, Description: er.ErrorDescription, HTTPStatus: status}
}
