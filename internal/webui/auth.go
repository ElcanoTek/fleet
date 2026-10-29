// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/ElcanoTek/fleet/internal/websession"
)

// OIDC per-attempt cookies (state, nonce, PKCE verifier), carried from
// /start to /callback. Short-lived, httpOnly, SameSite=Lax so they survive
// the IdP's top-level redirect back.
const (
	oidcStateCookie        = "fleet_oidc_state"
	oidcNonceCookie        = "fleet_oidc_nonce"
	oidcVerifierCookie     = "fleet_oidc_verifier"
	oidcTempCookieMaxAge   = 600
	maxBackendEnvelopeSize = 1 << 20
	// maxLoginBody caps the password form's body (either encoding).
	maxLoginBody = 64 << 10
	// maxBackendDocumentSize caps the two documents the gateway reads whole
	// for its own use — a shared-conversation snapshot (embedded into the
	// /shared page) and a scheduled run's task + log (digested by Discuss).
	// Those are user content and routinely exceed a JSON envelope; the Next
	// tier read them uncapped. A truncated document would be invalid JSON and
	// read as "not found", so the cap is set far above any real transcript and
	// only bounds a runaway.
	maxBackendDocumentSize = 256 << 20
)

// callCapture runs a backend call whose JSON answer the gateway itself
// consumes (login's /auth/verify, the epoch lookups, the membership probe),
// buffering at most maxBackendEnvelopeSize of it. ok is false when there is no
// backend.
func (g *Gateway) callCapture(h http.Handler, c call) (status int, header http.Header, body []byte, ok bool) {
	return g.callCaptureLimit(h, c, maxBackendEnvelopeSize)
}

// callCaptureLimit is callCapture with an explicit buffer cap (the brand
// images may exceed a JSON envelope's).
func (g *Gateway) callCaptureLimit(h http.Handler, c call, limit int) (status int, header http.Header, body []byte, ok bool) {
	if h == nil {
		return 0, nil, nil, false
	}
	b := c.body
	if b == nil {
		b = http.NoBody
	}
	req, err := http.NewRequestWithContext(context.Background(), c.method, "http://127.0.0.1"+c.target, b)
	if err != nil {
		return 0, nil, nil, false
	}
	req.RequestURI = c.target
	req.RemoteAddr = backendRemoteAddr
	if c.header != nil {
		req.Header = c.header
	}
	if c.body == nil {
		req.ContentLength = 0
	} else {
		req.ContentLength = c.contentLength
	}
	rec := &captureWriter{header: http.Header{}, limit: limit}
	h.ServeHTTP(rec, req)
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	return rec.status, rec.header, rec.body.Bytes(), true
}

// captureWriter is a minimal in-memory ResponseWriter for callCapture.
type captureWriter struct {
	header http.Header
	status int
	limit  int
	body   bytes.Buffer
}

func (c *captureWriter) Header() http.Header { return c.header }

func (c *captureWriter) WriteHeader(code int) {
	if c.status == 0 && code >= 200 {
		c.status = code
	}
}

func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if room := c.limit - c.body.Len(); room > 0 {
		if len(b) > room {
			c.body.Write(b[:room])
		} else {
			c.body.Write(b)
		}
	}
	return len(b), nil
}

func jsonReader(v any) (*bytes.Reader, int64) {
	raw, _ := json.Marshal(v)
	return bytes.NewReader(raw), int64(len(raw))
}

// fetchSessionEpoch reads the account's current session epoch so a cookie
// about to be minted carries it. "" means chat-server could not answer, and
// the caller must refuse the login: an epoch-less cookie is refused on the
// very next request, so minting one would strand the user in a login loop.
func (g *Gateway) fetchSessionEpoch(email string) string {
	status, _, body, ok := g.callCapture(g.chat, call{method: http.MethodGet, target: "/auth/session-epoch", header: g.chatHeaders(identity{Email: email})})
	if !ok || status < 200 || status > 299 {
		return ""
	}
	var resp struct {
		SessionEpoch string `json:"session_epoch"`
	}
	if json.Unmarshal(body, &resp) != nil {
		return ""
	}
	return resp.SessionEpoch
}

// fetchExternalSessionEpoch is the central-identity counterpart: created or
// read at login mint, keyed by issuer+subject.
func (g *Gateway) fetchExternalSessionEpoch(email, issuer, subject string) string {
	h := g.chatHeaders(identity{Email: email})
	h.Set("Content-Type", "application/json")
	body, n := jsonReader(map[string]string{"issuer": issuer, "subject": subject})
	status, _, resp, ok := g.callCapture(g.chat, call{method: http.MethodPost, target: "/auth/external-session-epoch", header: h, body: body, contentLength: n})
	if !ok || status < 200 || status > 299 {
		return ""
	}
	var out struct {
		SessionEpoch string `json:"session_epoch"`
	}
	if json.Unmarshal(resp, &out) != nil {
		return ""
	}
	return out.SessionEpoch
}

func (g *Gateway) registerAuth(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/auth/login", g.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", g.handleLogout)
	mux.HandleFunc("GET /api/auth/elcano-login", func(w http.ResponseWriter, r *http.Request) {
		g.elcanoLoginRedirect(w, r, "/")
	})
	mux.HandleFunc("GET /api/auth/oidc/start", g.handleOIDCStart)
	mux.HandleFunc("GET /api/auth/oidc/callback", g.handleOIDCCallback)
	mux.HandleFunc("POST /api/auth/backchannel-logout", g.handleBackchannel)

	mux.HandleFunc("GET /api/session", func(w http.ResponseWriter, r *http.Request) {
		s := g.session(r)
		if s == nil {
			unauthorized(w)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"email": s.Email})
	})
	// GET /api/version — the deploy-drift probe: the build id in the body and
	// in X-App-Version (decorate sets it on every response).
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]string{"buildId": g.spa.buildID})
	})
	mux.HandleFunc("GET /api/oauth/mcp/callback", g.handleMCPOAuthCallback)
}

// handleLogin is POST /api/auth/login, the password form:
//  1. chat-server POST /auth/verify (bcrypt compare against its users table);
//  2. on success, the account's session epoch;
//  3. mint elcano_session and 303 home.
//
// Errors redirect back to /login?e=<code> with a non-enumerating code; on a
// Fleet that signs in through central Auth only the code is password_off.
func (g *Gateway) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !g.verifyOrigin(w, r) {
		return
	}
	back := func(code string) { seeOther(w, g.redirectURL(r, "/login?e="+code)) }
	// Central Auth only: refused before the body is read, so no password is
	// ever checked (chat-server's /auth/verify refuses too).
	if g.cfg.PasswordLoginDisabled {
		back("password_off")
		return
	}

	// An email and a password are a few hundred bytes. Cap the whole body
	// before parsing so neither encoding can make the gateway buffer much —
	// and, for multipart, so nothing ever spills to a temp file on disk.
	r.Body = http.MaxBytesReader(w, r.Body, maxLoginBody)
	var perr error
	if isMultipart(r) {
		perr = r.ParseMultipartForm(maxLoginBody)
	} else {
		perr = r.ParseForm()
	}
	var tooLarge *http.MaxBytesError
	if errors.As(perr, &tooLarge) {
		back("missing")
		return
	}
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("email")))
	password := r.PostFormValue("password")
	if email == "" || password == "" {
		back("missing")
		return
	}

	h := g.chatHeaders(identity{Email: email})
	h.Set("Content-Type", "application/json")
	body, n := jsonReader(map[string]string{"email": email, "password": password})
	status, _, resp, ok := g.callCapture(g.chat, call{method: http.MethodPost, target: "/auth/verify", header: h, body: body, contentLength: n})
	if !ok {
		log.Printf("webui: login: no chat backend")
		back("server")
		return
	}
	if status == http.StatusTooManyRequests {
		// /auth/verify rate-limits pre-login attempts; say so honestly.
		back("throttled")
		return
	}
	if status < 200 || status > 299 {
		back("server")
		return
	}
	var verify struct {
		OK bool `json:"ok"`
	}
	if json.Unmarshal(resp, &verify) != nil {
		back("server")
		return
	}
	if !verify.OK {
		back("invalid")
		return
	}
	epoch := g.fetchSessionEpoch(email)
	if epoch == "" {
		back("server")
		return
	}
	tok, err := g.sessions.HMAC.Mint(email, epoch)
	if err != nil {
		log.Printf("webui: login: %v", err)
		back("server")
		return
	}
	setCookie(w, websession.CookieName, tok, websession.AbsoluteSeconds, g.secureRequest(r), "")
	seeOther(w, g.redirectURL(r, "/"))
}

// handleLogout is POST /api/auth/logout, the one sign-out for every surface.
//
// It clears elcano_session, the shared elcano_auth cookie (in BOTH the
// configured-domain and host-only shapes: AUTH_COOKIE_DOMAIN can drift from
// how the auth service actually minted it, and a deletion that misses its
// shape silently no-ops), and the in-flight OIDC transaction cookies so a
// callback still in flight cannot mint a session after the user left.
//
// With central Auth configured the browser then goes to the provider's
// RP-initiated logout (<issuer>/logout?client_id=…) whatever session it held:
// fleet cannot see Auth's own cookie, and a person who signed in with the
// break-glass password AND at Auth expects one sign-out to end both. Without
// it (or with a malformed issuer) it lands on /login?manual=1.
func (g *Gateway) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !g.verifyOrigin(w, r) {
		return
	}
	landing := g.redirectURL(r, "/login?manual=1")
	if o := g.cfg.OIDC; o != nil {
		if u, err := url.Parse(o.Issuer); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" {
			end := u.ResolveReference(&url.URL{Path: "/logout"})
			q := url.Values{}
			q.Set("client_id", o.ClientID)
			end.RawQuery = q.Encode()
			landing = end.String()
		}
	}
	secure := g.secureRequest(r)
	w.Header().Set("Cache-Control", "no-store")
	setCookie(w, websession.CookieName, "", 0, secure, "")
	name := g.cfg.elcanoCookieName()
	if d := g.cfg.ElcanoCookieDomain; d != "" {
		setCookie(w, name, "", 0, secure, d)
	}
	setCookie(w, name, "", 0, secure, "")
	for _, c := range []string{oidcStateCookie, oidcNonceCookie, oidcVerifierCookie} {
		setCookie(w, c, "", 0, secure, "")
	}
	seeOther(w, landing)
}

// elcanoLoginRedirect is the "Use Elcano email" handoff: the auth service's
// magic-link form, signed back to returnToPath. Without a configured public
// key fleet could never verify the cookie auth would mint, so sending the
// user there would loop (auth sets it, fleet cannot read it, back to /login):
// bounce to the password login with a message instead. Keep that guard.
func (g *Gateway) elcanoLoginRedirect(w http.ResponseWriter, r *http.Request, returnToPath string) {
	if g.cfg.ElcanoPubKeyB64 == "" {
		seeOther(w, g.redirectURL(r, "/login?e=elcano_unavailable"))
		return
	}
	returnTo := g.redirectURL(r, returnToPath)
	seeOther(w, g.cfg.authLoginURL()+"/?return_to="+encodeURIComponent(returnTo))
}

// handleMCPOAuthCallback completes a per-user remote MCP OAuth flow (#443):
// the provider redirects the browser here, the code+state are POSTed to
// chat-server, and the user lands back on Settings → Connections with the
// outcome in the query.
func (g *Gateway) handleMCPOAuthCallback(w http.ResponseWriter, r *http.Request) {
	settings := func(q string) { temporaryRedirect(w, g.redirectURL(r, "/settings/connections"+q)) }
	s := g.session(r)
	if s == nil {
		temporaryRedirect(w, g.redirectURL(r, "/login"))
		return
	}
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		settings("?error=" + encodeURIComponent(e))
		return
	}
	code, state := q.Get("code"), q.Get("state")
	if code == "" || state == "" {
		settings("?error=missing_code_or_state")
		return
	}
	h := g.chatHeaders(identityOf(s))
	h.Set("Content-Type", "application/json")
	body, n := jsonReader(struct {
		Code  string `json:"code"`
		State string `json:"state"`
	}{code, state})
	status, hdr, resp, ok := g.callCapture(g.chat, call{method: http.MethodPost, target: "/oauth/mcp/callback", header: h, body: body, contentLength: n})
	if !ok {
		settings("?error=backend_unreachable")
		return
	}
	if status == http.StatusUnauthorized && hdr.Get("X-Session-Revoked") == "1" {
		g.clearSessionCookie(w, r)
	}
	if status >= 200 && status < 300 {
		var data struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(resp, &data)
		settings("?connected=" + encodeURIComponent(firstNonEmpty(data.Name, "1")))
		return
	}
	detail := "authorization_failed"
	var data struct {
		Detail string `json:"detail"`
		Error  string `json:"error"`
	}
	if json.Unmarshal(resp, &data) == nil {
		detail = firstNonEmpty(data.Detail, data.Error, detail)
	}
	settings("?error=" + encodeURIComponent(detail))
}
