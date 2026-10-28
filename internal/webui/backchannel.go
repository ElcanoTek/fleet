// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

// Central Auth → fleet signed events, POSTed server-to-server to
// /api/auth/backchannel-logout:
//
//   - logout_token (OIDC Back-Channel Logout 1.0, typ logout+jwt): end every
//     fleet session of the account (chat-server rotates the external epoch
//     and the password session salt);
//   - access_token (typ access+jwt, event urn:elcanotek:event:application-access):
//     grant or revoke the account's membership, versioned so a stale event
//     cannot undo a newer one.
//
// Both are EdDSA JWTs signed by Auth. Keys: AUTH_SIGNING_PUBKEY and
// AUTH_SIGNING_PREVIOUS_PUBKEYS (bootstrap and offline fallback) plus Auth's
// published <issuer>/jwks.json, cached ten minutes, so a key rotation is a
// one-sided change on Auth. A token whose key is static or in a fresh cache
// never waits on the network; refreshes are rate-limited to one a minute so
// tokens with unknown kids cannot amplify requests to Auth.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

const (
	logoutEvent            = "http://schemas.openid.net/event/backchannel-logout"
	applicationAccessEvent = "urn:elcanotek:event:application-access"
	teamRenameEvent        = "urn:elcanotek:event:team-rename"

	jwksCacheTTL        = 10 * time.Minute
	jwksMinRefresh      = time.Minute
	jwksTimeout         = 5 * time.Second
	maxJWKSBytes        = 64 * 1024
	maxBackchannelBody  = 20_000
	maxBackchannelToken = 16_384
)

var base64URLChars = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

type jwksEntry struct {
	keys        []string // standard-base64 raw Ed25519 keys
	fetchedAt   time.Time
	lastAttempt time.Time
}

// jwksCache holds each issuer's published keys.
type jwksCache struct {
	http *http.Client
	mu   sync.Mutex
	m    map[string]jwksEntry
}

func newJWKSCache(c *http.Client) *jwksCache {
	// Never follow a redirect: the keys must come from the issuer itself.
	client := *c
	client.Timeout = jwksTimeout
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }
	return &jwksCache{http: &client, m: map[string]jwksEntry{}}
}

func (j *jwksCache) get(issuer string) (jwksEntry, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	e, ok := j.m[issuer]
	return e, ok
}

func (j *jwksCache) fetch(issuer string) ([]string, bool) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, strings.TrimRight(issuer, "/")+"/jwks.json", nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("Accept", "application/json")
	res, err := j.http.Do(req)
	if err != nil {
		return nil, false
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, false
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxJWKSBytes+1))
	if err != nil || len(raw) > maxJWKSBytes {
		return nil, false
	}
	var doc struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if json.Unmarshal(raw, &doc) != nil || doc.Keys == nil {
		return nil, false
	}
	keys := []string{}
	for _, entry := range doc.Keys {
		var jwk struct {
			Kty string `json:"kty"`
			Crv string `json:"crv"`
			X   any    `json:"x"`
		}
		if json.Unmarshal(entry, &jwk) != nil || jwk.Kty != "OKP" || jwk.Crv != "Ed25519" {
			continue
		}
		x, isString := jwk.X.(string)
		if !isString {
			continue
		}
		pub, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(x, "="))
		if err != nil || len(pub) != ed25519.PublicKeySize {
			continue
		}
		keys = append(keys, base64.StdEncoding.EncodeToString(pub))
	}
	return keys, true
}

func (j *jwksCache) refresh(issuer string, now time.Time) {
	j.mu.Lock()
	prev, ok := j.m[issuer]
	if ok && now.Sub(prev.lastAttempt) < jwksMinRefresh {
		j.mu.Unlock()
		return
	}
	prev.lastAttempt = now
	j.m[issuer] = prev
	j.mu.Unlock()

	keys, fetched := j.fetch(issuer)
	if !fetched {
		return // a failed fetch keeps whatever was cached
	}
	j.mu.Lock()
	j.m[issuer] = jwksEntry{keys: keys, fetchedAt: now, lastAttempt: now}
	j.mu.Unlock()
}

// kidFor is Auth's key id: base64url of the first 16 bytes of SHA-256(key).
func kidFor(stdB64 string) string {
	raw, err := base64.StdEncoding.DecodeString(stdB64)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return ""
	}
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(sum[:16])
}

func (g *Gateway) staticSigningKeys() []string {
	var out []string
	if k := strings.TrimSpace(g.cfg.ElcanoPubKeyB64); k != "" {
		out = append(out, k)
	}
	return append(out, g.cfg.PreviousPubKeys...)
}

// resolveSigningKeys returns the keys to try for one token: the static keys
// plus Auth's JWKS, refreshing once when the token names a kid none match.
// When a configured key (or a key from a FRESH cache) already matches the
// kid, receipt of a signed event does not depend on Auth being reachable at
// that instant; a key known only from a STALE cache is re-read first, so a key
// Auth retired stops being trusted within the cache window.
func (g *Gateway) resolveSigningKeys(issuer, kid string, now time.Time) []string {
	merge := func() []string {
		seen := map[string]bool{}
		var out []string
		e, _ := g.jwks.get(issuer)
		for _, k := range append(g.staticSigningKeys(), e.keys...) {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
		return out
	}
	e, ok := g.jwks.get(issuer)
	fresh := ok && !e.fetchedAt.IsZero() && now.Sub(e.fetchedAt) <= jwksCacheTTL
	if kid != "" {
		trusted := g.staticSigningKeys()
		if fresh {
			trusted = merge()
		}
		for _, k := range trusted {
			if kidFor(k) == kid {
				return merge()
			}
		}
	}
	if !fresh {
		g.jwks.refresh(issuer, now)
	}
	keys := merge()
	if kid != "" {
		known := false
		for _, k := range keys {
			if kidFor(k) == kid {
				known = true
				break
			}
		}
		if !known {
			g.jwks.refresh(issuer, now)
			keys = merge()
		}
	}
	return keys
}

var errInvalidToken = errors.New("invalid token")

func jsLen(s string) int { return len(utf16.Encode([]rune(s))) }

func isInteger(v any) (float64, bool) {
	f, ok := v.(float64)
	if !ok || math.IsInf(f, 0) || f != math.Trunc(f) {
		return 0, false
	}
	return f, true
}

// verifySignedEvent checks a signed event about one account (logout,
// access): the envelope (verifySignedEnvelope) plus the subject and email it
// must name, returning the claims and the normalized identity.
func (g *Gateway) verifySignedEvent(raw, typ, issuer, audience string, now time.Time) (claims map[string]any, ev backchannelIdentity, err error) {
	claims, ev, err = g.verifySignedEnvelope(raw, typ, issuer, audience, now)
	if err != nil {
		return nil, ev, err
	}
	sub, _ := claims["sub"].(string)
	email, _ := claims["email"].(string)
	ev.subject = strings.TrimSpace(sub)
	ev.email = strings.ToLower(strings.TrimSpace(email))
	at := strings.LastIndexByte(ev.email, '@')
	if ev.subject == "" || jsLen(ev.subject) > 255 ||
		ev.email == "" || jsLen(ev.email) > 254 || at <= 0 || at == len(ev.email)-1 {
		return nil, ev, errInvalidToken
	}
	return claims, ev, nil
}

// verifySignedEnvelope checks the JWS shape, header and signature, and the
// claims every signed event carries (issuer, audience, event id, iat, exp, no
// nonce), returning the claims and the event id, issuer and iat.
func (g *Gateway) verifySignedEnvelope(raw, typ, issuer, audience string, now time.Time) (claims map[string]any, ev backchannelIdentity, err error) {
	if len(raw) > maxBackchannelToken {
		return nil, ev, errInvalidToken
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, ev, errInvalidToken
	}
	decode := func(s string) ([]byte, error) {
		if !base64URLChars.MatchString(s) {
			return nil, errInvalidToken
		}
		return base64.RawURLEncoding.DecodeString(s)
	}
	hraw, err1 := decode(parts[0])
	craw, err2 := decode(parts[1])
	if err1 != nil || err2 != nil {
		return nil, ev, errInvalidToken
	}
	var header map[string]any
	if json.Unmarshal(hraw, &header) != nil || json.Unmarshal(craw, &claims) != nil || claims == nil {
		return nil, ev, errInvalidToken
	}
	kid, kidOK := header["kid"].(string)
	if header["typ"] != typ || header["alg"] != "EdDSA" || !kidOK {
		return nil, ev, errInvalidToken
	}
	sig, err := decode(parts[2])
	if err != nil {
		return nil, ev, errInvalidToken
	}
	signed := []byte(parts[0] + "." + parts[1])
	verified := false
	for _, k := range g.resolveSigningKeys(issuer, kid, now) {
		if kidFor(k) != kid {
			continue
		}
		pub, err := base64.StdEncoding.DecodeString(k)
		if err != nil || len(pub) != ed25519.PublicKeySize {
			continue
		}
		if ed25519.Verify(ed25519.PublicKey(pub), signed, sig) {
			verified = true
			break
		}
	}
	if !verified {
		return nil, ev, errInvalidToken
	}

	normIssuer := strings.TrimRight(issuer, "/")
	iss, _ := claims["iss"].(string)
	jti, _ := claims["jti"].(string)
	ev = backchannelIdentity{
		eventID: strings.TrimSpace(jti),
		issuer:  normIssuer,
	}
	iat, iatOK := isInteger(claims["iat"])
	exp, expOK := isInteger(claims["exp"])
	nowSec := float64(now.Unix())
	_, hasNonce := claims["nonce"]
	if strings.TrimRight(iss, "/") != normIssuer ||
		claims["aud"] != audience ||
		ev.eventID == "" || jsLen(ev.eventID) > 255 ||
		!iatOK || iat <= 0 || iat > nowSec+60 ||
		// Back-Channel Logout 1.0 requires exp; Auth signs a fresh iat/exp per
		// delivery attempt, so a retry hours later still passes.
		!expOK || exp+60 <= nowSec ||
		hasNonce {
		return nil, ev, errInvalidToken
	}
	ev.issuedAt = int64(iat)
	return claims, ev, nil
}

type backchannelIdentity struct {
	eventID, subject, email, issuer string
	issuedAt                        int64
}

func (g *Gateway) verifyLogoutToken(raw, issuer, audience string, now time.Time) (backchannelIdentity, error) {
	claims, ev, err := g.verifySignedEvent(raw, "logout+jwt", issuer, audience, now)
	if err != nil {
		return ev, err
	}
	events, _ := claims["events"].(map[string]any)
	if v, present := events[logoutEvent]; !present || v == nil {
		return ev, errInvalidToken
	}
	switch events[logoutEvent].(type) {
	case map[string]any, []any:
	default:
		return ev, errInvalidToken
	}
	return ev, nil
}

type applicationAccess struct {
	backchannelIdentity
	action   string
	version  int64
	settings map[string]string // nil when absent
}

func (g *Gateway) verifyAccessToken(raw, issuer, audience string, now time.Time) (applicationAccess, error) {
	var a applicationAccess
	claims, ev, err := g.verifySignedEvent(raw, "access+jwt", issuer, audience, now)
	if err != nil {
		return a, err
	}
	a.backchannelIdentity = ev
	events, _ := claims["events"].(map[string]any)
	access, _ := events[applicationAccessEvent].(map[string]any)
	if rawSettings, present := access["settings"]; present {
		m, isMap := rawSettings.(map[string]any)
		if !isMap {
			return a, errInvalidToken
		}
		a.settings = map[string]string{}
		for k, v := range m {
			s, isString := v.(string)
			if !isString || jsLen(k) > 64 || jsLen(s) > 64 {
				return a, errInvalidToken
			}
			a.settings[k] = s
		}
	}
	a.action, _ = access["action"].(string)
	version, versionOK := isInteger(access["version"])
	if (a.action != "grant" && a.action != "revoke") || !versionOK || version <= 0 {
		return a, errInvalidToken
	}
	a.version = int64(version)
	return a, nil
}

// teamRename is a verified central-Auth team rename: app-wide, so it names
// no account.
type teamRename struct {
	eventID, issuer string
	version         int64
	from, to        string
}

// verifyTeamRenameToken checks a team+jwt: the envelope, no account claims
// (an app-wide command is never mistaken for, or made from, an account
// event), and exactly {from, to, version} under the team-rename event.
func (g *Gateway) verifyTeamRenameToken(raw, issuer, audience string, now time.Time) (teamRename, error) {
	var t teamRename
	claims, ev, err := g.verifySignedEnvelope(raw, "team+jwt", issuer, audience, now)
	if err != nil {
		return t, err
	}
	_, hasSub := claims["sub"]
	_, hasEmail := claims["email"]
	events, _ := claims["events"].(map[string]any)
	rename, isMap := events[teamRenameEvent].(map[string]any)
	if hasSub || hasEmail || len(events) != 1 || !isMap || len(rename) != 3 {
		return t, errInvalidToken
	}
	t.eventID, t.issuer = ev.eventID, ev.issuer
	t.from, _ = rename["from"].(string)
	t.to, _ = rename["to"].(string)
	version, versionOK := isInteger(rename["version"])
	// The same bounds as an access event's settings values; chat-server
	// applies the full team-label rule (store.ValidExternalTeam).
	if !versionOK || version <= 0 || t.from == "" || t.to == "" || jsLen(t.from) > 64 || jsLen(t.to) > 64 {
		return t, errInvalidToken
	}
	t.version = int64(version)
	return t, nil
}

// handleBackchannel is POST /api/auth/backchannel-logout. Exactly one of
// logout_token / access_token / team_token. 204 when chat-server recorded it, 503 when it
// could not (Auth retries), 400 for anything malformed or unverifiable.
func (g *Gateway) handleBackchannel(w http.ResponseWriter, r *http.Request) {
	cfg := g.cfg.OIDC
	if cfg == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.ContentLength < 0 {
		w.WriteHeader(http.StatusLengthRequired)
		return
	}
	if r.ContentLength > maxBackchannelBody {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBackchannelBody)
	var err error
	if isMultipart(r) {
		err = r.ParseMultipartForm(maxBackchannelBody)
	} else {
		err = r.ParseForm()
	}
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	_, hasLogout := r.PostForm["logout_token"]
	_, hasAccess := r.PostForm["access_token"]
	_, hasTeam := r.PostForm["team_token"]
	if n := boolCount(hasLogout, hasAccess, hasTeam); n != 1 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	now := time.Now()
	if hasTeam {
		t, err := g.verifyTeamRenameToken(r.PostForm.Get("team_token"), cfg.Issuer, cfg.ClientID, now)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		g.backchannelCall(w, "", "/auth/external-team-rename", struct {
			EventID string `json:"event_id"`
			Issuer  string `json:"issuer"`
			Version int64  `json:"version"`
			From    string `json:"from"`
			To      string `json:"to"`
		}{t.eventID, t.issuer, t.version, t.from, t.to})
		return
	}
	if hasAccess {
		a, err := g.verifyAccessToken(r.PostForm.Get("access_token"), cfg.Issuer, cfg.ClientID, now)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		payload := struct {
			EventID  string            `json:"event_id"`
			Issuer   string            `json:"issuer"`
			Subject  string            `json:"subject"`
			Action   string            `json:"action"`
			Version  int64             `json:"version"`
			IssuedAt int64             `json:"issued_at"`
			Settings map[string]string `json:"settings,omitempty"`
		}{a.eventID, a.issuer, a.subject, a.action, a.version, a.issuedAt, a.settings}
		if a.settings != nil && len(a.settings) == 0 {
			// An explicit empty settings object is forwarded as {} (the TS
			// tier spread it whenever present).
			g.backchannelCall(w, a.email, "/auth/external-access", map[string]any{
				"event_id": a.eventID, "issuer": a.issuer, "subject": a.subject, "action": a.action,
				"version": a.version, "issued_at": a.issuedAt, "settings": map[string]string{},
			})
			return
		}
		g.backchannelCall(w, a.email, "/auth/external-access", payload)
		return
	}
	ev, err := g.verifyLogoutToken(r.PostForm.Get("logout_token"), cfg.Issuer, cfg.ClientID, now)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	g.backchannelCall(w, ev.email, "/auth/external-session-revoke", struct {
		EventID string `json:"event_id"`
		Issuer  string `json:"issuer"`
		Subject string `json:"subject"`
	}{ev.eventID, ev.issuer, ev.subject})
}

func (g *Gateway) backchannelCall(w http.ResponseWriter, email, target string, payload any) {
	raw, _ := json.Marshal(payload)
	h := g.chatHeaders(identity{Email: email})
	h.Set("Content-Type", "application/json")
	status, _, _, ok := g.callCapture(g.chat, call{method: http.MethodPost, target: target, header: h, body: bytes.NewReader(raw), contentLength: int64(len(raw))})
	if ok && status >= 200 && status < 300 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusServiceUnavailable)
}

func boolCount(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}
