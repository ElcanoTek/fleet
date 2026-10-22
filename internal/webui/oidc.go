// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

// OIDC / OAuth2 single sign-on with central Auth (#240).
//
// SSO lives in the browser tier, not chat-server: an OIDC login is "prove the
// email via the IdP, then mint the SAME elcano_session cookie the password
// form mints", so every downstream gate keeps working unchanged.
//
// Flow: Authorization Code + PKCE (S256) for a confidential client. The ID
// token is read directly from the token endpoint over server-to-server TLS, so
// per OIDC Core §3.1.3.7 its claims (iss/aud/exp/nonce) are validated without
// re-verifying the signature — the TLS channel is the integrity guarantee.
// State + nonce + PKCE defend the redirect against CSRF and code interception.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ElcanoTek/fleet/internal/websession"
)

// silentSSOResult is the /login query the callback sets when a silent
// (prompt=none) attempt found no central session.
const silentSSOResult = "sso=none"

type discoveryDoc struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	UserinfoEndpoint                  string   `json:"userinfo_endpoint,omitempty"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported,omitempty"`
}

// oidcClient memoizes each issuer's discovery document for the life of the
// process (the endpoints are static config). A failure is not cached, so a
// transient error does not poison every later attempt.
type oidcClient struct {
	http *http.Client
	mu   sync.Mutex
	docs map[string]*discoveryDoc
}

func newOIDCClient(c *http.Client) *oidcClient {
	return &oidcClient{http: c, docs: map[string]*discoveryDoc{}}
}

func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second}
}

func (o *oidcClient) discover(issuer string) (*discoveryDoc, error) {
	o.mu.Lock()
	if d := o.docs[issuer]; d != nil {
		o.mu.Unlock()
		return d, nil
	}
	o.mu.Unlock()

	req, err := http.NewRequest(http.MethodGet, strings.TrimRight(issuer, "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	res, err := o.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, errors.New("OIDC discovery failed: " + res.Status)
	}
	var doc discoveryDoc
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&doc); err != nil {
		return nil, err
	}
	if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" || doc.Issuer == "" {
		return nil, errors.New("OIDC discovery document missing required endpoints")
	}
	// OIDC Discovery 4.3: the document's issuer MUST equal the issuer it was
	// fetched from. fleet also keys external-session epochs by doc.issuer at
	// login and by the configured issuer at back-channel logout; a mismatch
	// would make every logout rotate a row no session points at.
	if strings.TrimRight(doc.Issuer, "/") != strings.TrimRight(issuer, "/") {
		return nil, errors.New("OIDC discovery issuer does not match the configured issuer")
	}
	o.mu.Lock()
	o.docs[issuer] = &doc
	o.mu.Unlock()
	return &doc, nil
}

// randomURLSafe returns n CSPRNG bytes, base64url-encoded (state, nonce,
// PKCE verifier).
func randomURLSafe(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("webui: crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// pkceChallenge derives the S256 code_challenge from a verifier.
func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// decodeJWTClaims decodes a JWT's payload WITHOUT verifying its signature —
// safe only for a token read directly from the token endpoint over TLS.
func decodeJWTClaims(jwt string) map[string]any {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return nil
	}
	return claims
}

// emailDomainAllowed enforces the optional domain allowlist; empty admits
// any domain (the chat-server user list stays the membership gate).
func emailDomainAllowed(email string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	at := strings.LastIndexByte(email, '@')
	if at < 0 {
		return false
	}
	domain := strings.ToLower(email[at+1:])
	for _, d := range allowed {
		if d == domain {
			return true
		}
	}
	return false
}

// validateIDToken checks the security-relevant claims of an ID token from
// the token endpoint and extracts the verified email and subject. The reason
// is for tests and logs only; the browser sees a coarse error code.
func validateIDToken(claims map[string]any, cfg *OIDCConfig, doc *discoveryDoc, expectedNonce string, now time.Time) (email, subject, reason string) {
	if claims == nil {
		return "", "", "unparseable id_token"
	}
	if iss, _ := claims["iss"].(string); iss != doc.Issuer {
		return "", "", "issuer mismatch"
	}
	audOK := false
	switch aud := claims["aud"].(type) {
	case string:
		audOK = aud == cfg.ClientID
	case []any:
		for _, a := range aud {
			if s, _ := a.(string); s == cfg.ClientID {
				audOK = true
			}
		}
	}
	if !audOK {
		return "", "", "audience mismatch"
	}
	// azp (authorized party) must be our client when present — the
	// multi-audience case.
	if azp, isString := claims["azp"].(string); isString && azp != cfg.ClientID {
		return "", "", "azp mismatch"
	}
	exp, isNum := claims["exp"].(float64)
	if !isNum || exp <= float64(now.Unix()) {
		return "", "", "expired"
	}
	if nonce, _ := claims["nonce"].(string); nonce != expectedNonce || expectedNonce == "" {
		return "", "", "nonce mismatch"
	}
	e, _ := claims["email"].(string)
	email = strings.ToLower(strings.TrimSpace(e))
	if email == "" {
		return "", "", "no email claim"
	}
	sub, _ := claims["sub"].(string)
	subject = strings.TrimSpace(sub)
	if subject == "" || len(subject) > 255 {
		return "", "", "no subject claim"
	}
	// An explicit email_verified:false is honored; absent counts as verified
	// (not every IdP emits it).
	if v, present := claims["email_verified"].(bool); present && !v {
		return "", "", "email not verified"
	}
	return email, subject, ""
}

// redirectURI is the callback the IdP returns to: a pinned
// FLEET_OIDC_REDIRECT_URI, else the canonical origin's, else (local dev only)
// the forwarded host's — never the forwarded host on a deployment that knows
// its origin, or the IdP would be handed an attacker-picked callback.
func (g *Gateway) redirectURI(r *http.Request) string {
	if g.cfg.OIDC.RedirectURI != "" {
		return g.cfg.OIDC.RedirectURI
	}
	return g.redirectURL(r, "/api/auth/oidc/callback")
}

// shouldAutoStartLogin decides whether an anonymous /login visit starts a
// silent SSO attempt instead of rendering the card. Never with auto-start
// off, on a login error (?e=), after a silent attempt came back empty
// (?sso=none), or on ?manual=1 — so the password form is always one URL away
// and a failed attempt cannot loop.
func shouldAutoStartLogin(q url.Values, cfg *OIDCConfig) bool {
	if cfg == nil || !cfg.AutoStart {
		return false
	}
	if q.Has("e") || q.Has("manual") {
		return false
	}
	return q.Get("sso") != "none"
}

// handleOIDCStart is GET /api/auth/oidc/start: mint state + nonce + PKCE
// verifier into short-lived cookies and 303 to the IdP. ?silent=1 adds
// prompt=none (FLEET_OIDC_AUTO_START).
func (g *Gateway) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	cfg := g.cfg.OIDC
	if cfg == nil {
		seeOther(w, g.redirectURL(r, "/login?e=oidc_unavailable"))
		return
	}
	doc, err := g.oidc.discover(cfg.Issuer)
	if err != nil {
		seeOther(w, g.redirectURL(r, "/login?e=oidc_error"))
		return
	}
	authURL, err := url.Parse(doc.AuthorizationEndpoint)
	if err != nil {
		seeOther(w, g.redirectURL(r, "/login?e=oidc_error"))
		return
	}
	state, nonce, verifier := randomURLSafe(32), randomURLSafe(32), randomURLSafe(64)
	q := authURL.Query()
	q.Set("response_type", "code")
	q.Set("client_id", cfg.ClientID)
	q.Set("redirect_uri", g.redirectURI(r))
	q.Set("scope", cfg.Scopes)
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", pkceChallenge(verifier))
	q.Set("code_challenge_method", "S256")
	if r.URL.Query().Get("silent") == "1" {
		q.Set("prompt", "none")
	}
	authURL.RawQuery = q.Encode()
	secure := g.secureRequest(r)
	setCookie(w, oidcStateCookie, state, oidcTempCookieMaxAge, secure, "")
	setCookie(w, oidcNonceCookie, nonce, oidcTempCookieMaxAge, secure, "")
	setCookie(w, oidcVerifierCookie, verifier, oidcTempCookieMaxAge, secure, "")
	seeOther(w, authURL.String())
}

func clearOIDCTempCookies(w http.ResponseWriter, secure bool) {
	for _, c := range []string{oidcStateCookie, oidcNonceCookie, oidcVerifierCookie} {
		setCookie(w, c, "", 0, secure, "")
	}
}

// handleOIDCCallback is GET /api/auth/oidc/callback: check state, exchange the
// code (+ PKCE verifier + client secret), validate the ID token, enforce the
// domain allowlist, read the central-identity epoch, mint elcano_session and
// 303 home. Membership is still chat-server's user list: an SSO login for an
// unprovisioned email lands on /no-access. Any failure clears the temp
// cookies and bounces to /login with a coarse, non-enumerating code.
func (g *Gateway) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	cfg := g.cfg.OIDC
	if cfg == nil {
		seeOther(w, g.redirectURL(r, "/login?e=oidc_unavailable"))
		return
	}
	secure := g.secureRequest(r)
	fail := func(code string) {
		clearOIDCTempCookies(w, secure)
		seeOther(w, g.redirectURL(r, "/login?e="+code))
	}
	q := r.URL.Query()
	switch idpErr := q.Get("error"); {
	case idpErr == "login_required" || idpErr == "interaction_required":
		// A silent attempt found no usable central session: show the login
		// page with both options and no banner, and do not auto-start again.
		clearOIDCTempCookies(w, secure)
		seeOther(w, g.redirectURL(r, "/login?"+silentSSOResult))
		return
	case idpErr != "":
		fail("oidc_denied")
		return
	}
	code, state := q.Get("code"), q.Get("state")
	cookieState := cookieValue(r, oidcStateCookie)
	nonce := cookieValue(r, oidcNonceCookie)
	verifier := cookieValue(r, oidcVerifierCookie)
	if code == "" || state == "" || cookieState == "" || nonce == "" || verifier == "" || state != cookieState {
		fail("oidc_error")
		return
	}
	doc, err := g.oidc.discover(cfg.Issuer)
	if err != nil {
		fail("oidc_error")
		return
	}
	idToken, err := g.exchangeCode(cfg, doc, code, verifier, g.redirectURI(r))
	if err != nil {
		fail("oidc_error")
		return
	}
	email, subject, reason := validateIDToken(decodeJWTClaims(idToken), cfg, doc, nonce, time.Now())
	if reason != "" {
		fail("oidc_error")
		return
	}
	if !emailDomainAllowed(email, cfg.AllowedDomains) {
		fail("oidc_domain")
		return
	}
	// Central identities use their own generation, separate from the password
	// hash; Auth's back-channel logout rotates it (and the password session
	// salt), so one central sign-out ends every fleet session of the account.
	epoch := g.fetchExternalSessionEpoch(email, doc.Issuer, subject)
	if epoch == "" {
		fail("oidc_error")
		return
	}
	tok, err := g.sessions.HMAC.MintOIDC(email, epoch, doc.Issuer, subject)
	if err != nil {
		fail("oidc_error")
		return
	}
	setCookie(w, websession.CookieName, tok, websession.AbsoluteSeconds, secure, "")
	clearOIDCTempCookies(w, secure)
	seeOther(w, g.redirectURL(r, "/"))
}

// exchangeCode POSTs the authorization code to the token endpoint. The client
// authenticates with HTTP Basic when the provider advertises
// client_secret_basic, else with client_secret in the form (the historical
// default, and what providers advertising nothing accept).
func (g *Gateway) exchangeCode(cfg *OIDCConfig, doc *discoveryDoc, code, verifier, redirectURI string) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("client_id", cfg.ClientID)
	form.Set("code_verifier", verifier)
	basic := false
	for _, m := range doc.TokenEndpointAuthMethodsSupported {
		if m == "client_secret_basic" {
			basic = true
		}
	}
	if !basic {
		form.Set("client_secret", cfg.ClientSecret)
	}
	req, err := http.NewRequest(http.MethodPost, doc.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if basic {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(cfg.ClientID+":"+cfg.ClientSecret)))
	}
	res, err := g.oidc.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return "", errors.New("token endpoint: " + res.Status)
	}
	var tokens struct {
		IDToken string `json:"id_token"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&tokens); err != nil {
		return "", err
	}
	if tokens.IDToken == "" {
		return "", errors.New("token endpoint returned no id_token")
	}
	return tokens.IDToken, nil
}
