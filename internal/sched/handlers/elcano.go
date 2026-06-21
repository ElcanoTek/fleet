// Copyright (c) 2025 ElcanoTek
// All rights reserved. This is a private repository.

// Elcano unified-auth integration (scoped tier, mirrors chat).
//
// The server accepts the shared `elcano_auth` cookie minted by the auth service
// (auth.elcanotek.com) as a second browser login alongside the existing
// username/password path. The cookie is an Ed25519-signed token; the server holds
// only the PUBLIC key (AUTH_SIGNING_PUBKEY) and verifies it natively
// (Pattern B) — enough to verify, never to mint, so the value is safe to
// distribute. Verification mirrors auth/internal/token/token.go byte-for-byte.
//
// "Scoped tier": a valid cookie only proves WHO the user is. After it
// verifies, AdminOrUserAuthMiddleware looks the email up in the local users
// table (membership). A validly-signed-in email that isn't provisioned gets
// 403 {"error":"not_a_member"} — not a redirect loop.

package handlers

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// elcanoSession is the verified identity extracted from an elcano_auth cookie.
type elcanoSession struct {
	Email  string
	Tenant string
	Exp    int64
}

type elcanoClaims struct {
	Email  string `json:"email"`
	Tenant string `json:"tenant"`
	Exp    int64  `json:"exp"`
}

// ParseElcanoPubKey decodes a standard-base64 Ed25519 public key (the
// AUTH_SIGNING_PUBKEY value, matching `auth-admin keygen` output). Returns nil
// for an empty or malformed value so the cookie path fails closed — the "Use
// Elcano email" button still renders, but every cookie is rejected.
func ParseElcanoPubKey(b64 string) ed25519.PublicKey {
	b64 = strings.TrimSpace(b64)
	if b64 == "" {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil
	}
	return ed25519.PublicKey(raw)
}

// verifyElcanoToken verifies a base64url(payloadJSON).base64url(ed25519Sig)
// token against pub. The signature covers the base64url-encoded body STRING
// (not the raw JSON), mirroring auth/internal/token. Returns nil on any
// failure (bad key, encoding, signature, expiry, or missing email).
func verifyElcanoToken(pub ed25519.PublicKey, token string) *elcanoSession {
	// ed25519.Verify panics on a wrong-sized key; guard so a misconfigured
	// public key fails closed as "invalid" rather than crashing the server.
	if len(pub) != ed25519.PublicKeySize || token == "" {
		return nil
	}
	dot := strings.IndexByte(token, '.')
	if dot < 1 || dot == len(token)-1 {
		return nil
	}
	body, sigPart := token[:dot], token[dot+1:]

	sig, err := base64.RawURLEncoding.DecodeString(sigPart)
	if err != nil {
		return nil
	}
	if !ed25519.Verify(pub, []byte(body), sig) {
		return nil
	}

	rawJSON, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil
	}
	var c elcanoClaims
	if err := json.Unmarshal(rawJSON, &c); err != nil {
		return nil
	}
	if c.Email == "" || c.Exp <= time.Now().Unix() {
		return nil
	}
	return &elcanoSession{Email: strings.ToLower(c.Email), Tenant: c.Tenant, Exp: c.Exp}
}

// elcanoSessionFromRequest reads and verifies the elcano_auth cookie. Returns
// nil when the cookie is absent/invalid or the public key isn't configured.
func (h *Handlers) elcanoSessionFromRequest(r *http.Request) *elcanoSession {
	if h.config.ElcanoPubKey == nil {
		return nil
	}
	c, err := r.Cookie(h.config.ElcanoCookieName)
	if err != nil {
		return nil
	}
	return verifyElcanoToken(h.config.ElcanoPubKey, c.Value)
}

// lookupMember resolves an email to a user for the scoped-tier membership
// gate. In production it hits the users table (username == lowercase email);
// tests inject memberLookup to avoid a live database.
func (h *Handlers) lookupMember(ctx context.Context, email string) (*models.User, error) {
	if h.memberLookup != nil {
		return h.memberLookup(ctx, email)
	}
	return h.storage.GetUserByUsernameWithContext(ctx, strings.ToLower(email))
}

// ElcanoLogin handles GET /auth/elcano-login — the "Use Elcano email" button.
// It bounces the (unauthenticated) browser to the auth service's magic-link
// login, signed back here. After the user clicks the emailed link, auth sets
// the shared elcano_auth cookie and redirects here; the server then verifies it
// natively and gates on the user-list.
//
// If no public key is configured the server can never verify auth's cookie, so
// sending the user there would trap them in a redirect loop — bounce back to
// the dashboard with a flag instead.
func (h *Handlers) ElcanoLogin(w http.ResponseWriter, r *http.Request) {
	if h.config.ElcanoPubKey == nil {
		http.Redirect(w, r, "/?e=elcano_unavailable", http.StatusSeeOther)
		return
	}
	dest := h.config.AuthLoginURL + "/?return_to=" + url.QueryEscape(h.mocPublicURL(r))
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// ElcanoLogout handles GET /auth/logout. It deletes the shared elcano_auth
// cookie itself (rather than bouncing to auth) and returns the user to the
// server's own login, mirroring chat's logout. Because the cookie lives on the
// shared parent domain, deleting it signs the user out of EVERY Elcano service
// — the expected meaning of "log out". The deletion only takes effect when
// name + domain + path match how auth set the cookie, so we mirror them here.
func (h *Handlers) ElcanoLogout(w http.ResponseWriter, r *http.Request) {
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{
		Name:     h.config.ElcanoCookieName,
		Value:    "",
		Path:     "/",
		Domain:   h.config.ElcanoCookieDomain, // "" → host-only (Go omits the attr)
		MaxAge:   -1,                          // delete now
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// mocPublicURL is where auth should send the browser back after login. We use
// the configured ORCHESTRATOR_URL (the externally-visible URL) when set,
// falling back to the request's host for local dev.
func (h *Handlers) mocPublicURL(r *http.Request) string {
	if h.config.OrchestratorURL != "" {
		return strings.TrimRight(h.config.OrchestratorURL, "/") + "/"
	}
	scheme := "http"
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	} else if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + "/"
}
