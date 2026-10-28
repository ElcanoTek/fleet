// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

// Package websession owns the two browser session cookies fleet accepts, in
// one place for every Go component that reads them:
//
//   - elcano_session: fleet's own HMAC-SHA256 cookie, minted by the password
//     form and the OIDC callback (internal/webui). Stateless: the payload
//     carries the email, an absolute deadline (exp), an idle deadline (idle),
//     the chat-server session epoch it was minted against, and — for a session
//     from central Auth — the OIDC issuer and subject. Format:
//     base64url(payloadJSON) + "." + base64url(HMAC-SHA256(secret, body)),
//     the MAC covering the base64url body STRING.
//   - elcano_auth: the Ed25519 cookie minted by the legacy auth service for
//     the "Use Elcano email" magic-link path. fleet holds only the PUBLIC key
//     (AUTH_SIGNING_PUBKEY) — enough to verify, never to mint.
//
// The web tier used to implement both in TypeScript (web/src/app/lib/auth.ts)
// while the orchestrator carried its own Ed25519 verifier; ADR-0091 moved the
// browser tier into the Go binary and this package is now the only
// implementation. Cookie wire formats are unchanged, so sessions minted by the
// Next.js tier before an upgrade stay valid across it (same APP_SESSION_SECRET).
//
// Lifetimes are ADR-0064's: one day absolute, twelve hours idle, re-minted on
// activity at most once a minute.
package websession

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// CookieName is the HMAC session cookie fleet mints.
const CookieName = "elcano_session"

// DefaultElcanoCookieName is the auth service's cookie name when
// AUTH_COOKIE_NAME is unset.
const DefaultElcanoCookieName = "elcano_auth"

// Session lifetimes (ADR-0064). The fleet session is an APPLICATION session in
// the Elcano two-layer model: while the user's central Auth session is live an
// expired fleet session costs them only a redirect through the OIDC handoff,
// so these bound a stolen cookie rather than decide how often people log in.
const (
	AbsoluteSeconds = 60 * 60 * 24
	IdleSeconds     = 60 * 60 * 12
	// TouchSeconds: a request re-mints the cookie only when the previous mint is
	// more than this old, so a page's burst of requests costs one Set-Cookie.
	TouchSeconds = 60
)

// Source says which credential authenticated a request.
type Source string

const (
	SourcePassword Source = "password"
	SourceOIDC     Source = "oidc"
	SourceElcano   Source = "elcano"
)

// Session is a verified browser session.
type Session struct {
	Email string
	// Exp is the absolute deadline (unix seconds).
	Exp float64
	// Idle is the HMAC cookie's idle deadline; zero for elcano sessions, which
	// fleet cannot re-mint.
	Idle   float64
	Source Source
	// Epoch is the chat-server session epoch the cookie was minted against;
	// empty for elcano sessions (the auth service mints that cookie).
	Epoch   string
	Issuer  string
	Subject string
	Tenant  string
}

// payload is the HMAC cookie body. Field order is the order the TypeScript
// tier serialized, so a cookie re-minted here is byte-identical in shape.
type payload struct {
	Email   string  `json:"email"`
	Exp     float64 `json:"exp"`
	Idle    float64 `json:"idle"`
	Epoch   string  `json:"epoch"`
	Source  string  `json:"source,omitempty"`
	Issuer  string  `json:"issuer,omitempty"`
	Subject string  `json:"subject,omitempty"`
}

// HMAC is the elcano_session signer/verifier for one secret.
type HMAC struct {
	secret []byte
	now    func() time.Time
}

// NewHMAC returns a signer for secret (APP_SESSION_SECRET). An empty secret
// yields a signer that refuses every token and every mint (Configured false):
// the deployment then has no password/OIDC sessions, which is fail-closed.
func NewHMAC(secret string) *HMAC {
	return &HMAC{secret: []byte(secret), now: time.Now}
}

// WithClock returns a copy that reads time from now (tests).
func (h *HMAC) WithClock(now func() time.Time) *HMAC {
	c := *h
	c.now = now
	return &c
}

// Configured reports whether a secret is set.
func (h *HMAC) Configured() bool { return h != nil && len(h.secret) > 0 }

func (h *HMAC) sign(body string) string {
	m := hmac.New(sha256.New, h.secret)
	m.Write([]byte(body))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (h *HMAC) encode(p payload) (string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	return body + "." + h.sign(body), nil
}

// freshDeadlines returns exp/idle for a session minted at now; idle never
// passes exp.
func freshDeadlines(now int64) (exp, idle float64) {
	e := now + AbsoluteSeconds
	i := now + IdleSeconds
	if i > e {
		i = e
	}
	return float64(e), float64(i)
}

// Mint mints a password session. epoch is mandatory: a token without one is
// refused by Verify, so no mint path may skip it.
func (h *HMAC) Mint(email, epoch string) (string, error) {
	if !h.Configured() {
		return "", errNoSecret
	}
	exp, idle := freshDeadlines(h.now().Unix())
	return h.encode(payload{Email: strings.ToLower(email), Exp: exp, Idle: idle, Epoch: epoch})
}

// MintOIDC mints a session for an identity proven by central Auth.
func (h *HMAC) MintOIDC(email, epoch, issuer, subject string) (string, error) {
	if !h.Configured() {
		return "", errNoSecret
	}
	exp, idle := freshDeadlines(h.now().Unix())
	return h.encode(payload{
		Email: strings.ToLower(email), Exp: exp, Idle: idle, Epoch: epoch,
		Source: string(SourceOIDC), Issuer: issuer, Subject: subject,
	})
}

// Refresh implements the idle limit: it re-mints s with a later idle deadline
// when that deadline would move by at least TouchSeconds, copying the absolute
// deadline (never extending it) and every identity claim. It returns the new
// token and its remaining lifetime in seconds, or ok=false when nothing should
// be written (an elcano session, or a mint under a minute old).
func (h *HMAC) Refresh(s *Session) (token string, maxAge int64, ok bool) {
	if !h.Configured() || s == nil || s.Source == SourceElcano || s.Idle == 0 || s.Epoch == "" {
		return "", 0, false
	}
	now := h.now().Unix()
	idle := float64(now + IdleSeconds)
	if idle > s.Exp {
		idle = s.Exp
	}
	if idle-s.Idle < TouchSeconds {
		return "", 0, false
	}
	p := payload{Email: s.Email, Exp: s.Exp, Idle: idle, Epoch: s.Epoch}
	if s.Source == SourceOIDC {
		p.Source, p.Issuer, p.Subject = string(SourceOIDC), s.Issuer, s.Subject
	}
	tok, err := h.encode(p)
	if err != nil {
		return "", 0, false
	}
	return tok, int64(s.Exp) - now, true
}

type sentinelError string

func (e sentinelError) Error() string { return string(e) }

const errNoSecret = sentinelError("websession: APP_SESSION_SECRET is not configured")

// Verify checks an elcano_session token and returns its session, or nil.
//
// Refusals beyond the MAC, each load-bearing:
//   - no idle claim: the cookie predates the idle limit (ADR-0064) and was
//     signed for fourteen days flat; it is not grandfathered;
//   - no epoch claim: the cookie predates per-user revocation and nothing
//     downstream could check it (chat-server admits a request with no claim);
//   - an oidc source without issuer and subject, or an unknown source.
func (h *HMAC) Verify(token string) *Session {
	if !h.Configured() || token == "" {
		return nil
	}
	// The TypeScript verifier destructured the first two dot-separated parts;
	// keep that shape so the two agree on every input.
	parts := strings.SplitN(token, ".", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return nil
	}
	body, sigPart := parts[0], parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(sigPart)
	if err != nil {
		return nil
	}
	m := hmac.New(sha256.New, h.secret)
	m.Write([]byte(body))
	if !hmac.Equal(sig, m.Sum(nil)) {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil
	}
	var claims map[string]any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil
	}
	now := float64(h.now().UnixMilli())
	email, _ := claims["email"].(string)
	exp, expOK := claims["exp"].(float64)
	if email == "" || !expOK || exp*1000 < now {
		return nil
	}
	idle, idleOK := claims["idle"].(float64)
	if !idleOK || idle*1000 < now {
		return nil
	}
	epoch, _ := claims["epoch"].(string)
	if epoch == "" {
		return nil
	}
	source := SourcePassword
	issuer, _ := claims["issuer"].(string)
	subject, _ := claims["subject"].(string)
	if rawSource, present := claims["source"]; present && rawSource != nil {
		src, isString := rawSource.(string)
		switch {
		case !isString:
			return nil
		case src == "" || src == string(SourcePassword):
			// "" is falsy in the TS check: treated as a password session.
		case src == string(SourceOIDC):
			if issuer == "" || subject == "" {
				return nil
			}
			source = SourceOIDC
		default:
			return nil
		}
	}
	s := &Session{Email: email, Exp: exp, Idle: idle, Source: source, Epoch: epoch}
	if source == SourceOIDC {
		s.Issuer, s.Subject = issuer, subject
	}
	return s
}

// ── elcano_auth (Ed25519) ───────────────────────────────────────────────────

// ParseElcanoPubKey decodes a standard-base64 Ed25519 public key (the
// AUTH_SIGNING_PUBKEY value, matching `auth-admin keygen` output). It returns
// nil for an empty or malformed value so the cookie path fails closed.
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

// ElcanoClaims is the verified identity in an elcano_auth cookie.
type ElcanoClaims struct {
	Email  string
	Tenant string
	Exp    int64
}

type elcanoPayload struct {
	Email  string `json:"email"`
	Tenant string `json:"tenant"`
	Exp    int64  `json:"exp"`
}

// VerifyElcanoToken verifies a base64url(payloadJSON).base64url(ed25519Sig)
// token against pub. The signature covers the base64url-encoded body STRING
// (not the raw JSON), mirroring the auth service byte-for-byte. Returns nil on
// any failure (bad key, encoding, signature, expiry, or missing email). The
// email is lowercased: every downstream scope keys off the lowercase form.
func VerifyElcanoToken(pub ed25519.PublicKey, token string) *ElcanoClaims {
	return verifyElcanoAt(pub, token, time.Now())
}

func verifyElcanoAt(pub ed25519.PublicKey, token string, now time.Time) *ElcanoClaims {
	// ed25519.Verify panics on a wrong-sized key; fail closed instead.
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
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return nil
	}
	var c elcanoPayload
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil
	}
	if c.Email == "" || c.Exp <= now.Unix() {
		return nil
	}
	return &ElcanoClaims{Email: strings.ToLower(c.Email), Tenant: c.Tenant, Exp: c.Exp}
}

// Resolver turns a request's cookies into a session: the HMAC cookie wins when
// both are present (it is the more specific, fleet-owned session); otherwise
// the shared elcano_auth cookie.
type Resolver struct {
	HMAC             *HMAC
	ElcanoPubKey     ed25519.PublicKey
	ElcanoCookieName string
	// RefusePassword ignores an HMAC cookie a password login minted (a
	// verified SourcePassword session, which is also what a cookie without a
	// source claim is): the deployment signs people in through central Auth
	// only. The elcano_auth cookie is still considered, so a browser holding
	// both is not stranded by the one it may no longer use.
	RefusePassword bool
}

// Resolve verifies the two cookie values.
func (r *Resolver) Resolve(hmacToken, elcanoToken string) *Session {
	if s := r.HMAC.Verify(hmacToken); s != nil && (!r.RefusePassword || s.Source != SourcePassword) {
		return s
	}
	if c := VerifyElcanoToken(r.ElcanoPubKey, elcanoToken); c != nil {
		return &Session{Email: c.Email, Exp: float64(c.Exp), Tenant: c.Tenant, Source: SourceElcano}
	}
	return nil
}
