// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"crypto/ed25519"
	"net/url"
	"os"
	"strings"

	"github.com/ElcanoTek/fleet/internal/websession"
)

// Config is everything the browser tier needs that is not a backend handler.
// It used to be the Next.js tier's environment (/etc/fleet/fleet-web.env); the
// keys keep their names so an upgrade moves them into fleet.env verbatim
// (scripts/update.sh does this — docs/adr/0091).
type Config struct {
	// SessionSecret signs the elcano_session cookie (APP_SESSION_SECRET).
	// Empty disables password and SSO sessions (fail closed).
	SessionSecret string

	// PublicOrigin is the deployment's canonical public origin
	// (FLEET_PUBLIC_BASE_URL, falling back to the Next-era
	// NEXT_PUBLIC_PUBLIC_ORIGIN). Redirect targets, the Secure-cookie decision,
	// the OIDC redirect_uri and the CSRF expected host prefer it over
	// X-Forwarded-*, which a client can set when it reaches the listener
	// directly. Nil means "derive from the request" (local dev).
	PublicOrigin *url.URL

	// ElcanoPubKeyB64 is AUTH_SIGNING_PUBKEY (standard base64); empty disables
	// the "Use Elcano email" path. PreviousPubKeys is
	// AUTH_SIGNING_PREVIOUS_PUBKEYS, accepted for back-channel tokens only.
	ElcanoPubKeyB64    string
	PreviousPubKeys    []string
	ElcanoCookieName   string // AUTH_COOKIE_NAME (default elcano_auth)
	ElcanoCookieDomain string // AUTH_COOKIE_DOMAIN ("" = host-only)
	AuthLoginURL       string // AUTH_LOGIN_URL (default https://auth.elcanotek.com)

	// OIDC is nil unless FLEET_OIDC_ISSUER, _CLIENT_ID and _CLIENT_SECRET are
	// all set.
	OIDC *OIDCConfig

	// PasswordLoginDisabled is config.Config.PasswordLoginDisabled, set by the
	// caller (cmd/fleet), never read from the environment here: the backends
	// must make the same decision, so it is made once. The password form is
	// refused and hidden, and a session a password login minted (before the
	// switch, or on another replica) no longer authenticates.
	PasswordLoginDisabled bool

	// AccountsManagedByAuth is config.Config.AccountsManagedByAuth, set by
	// the caller like PasswordLoginDisabled: the pages that manage accounts,
	// roles and teams become view-only and point at Auth's console.
	AccountsManagedByAuth bool

	// CatwalkURL is the catwalk model database base (CATWALK_URL).
	CatwalkURL string

	// ChatToken authenticates in-process calls to chat-server
	// (FLEET_SERVER_TOKEN); OrchestratorToken the orchestrator's header-trust
	// channel (the same secret unless ORCHESTRATOR_SERVER_TOKEN overrides it).
	ChatToken         string
	OrchestratorToken string

	// DevServer, when set (FLEET_WEB_DEV_SERVER, e.g. http://127.0.0.1:5173),
	// makes the gateway fetch index.html and proxy module/asset requests from a
	// running Vite dev server instead of the embedded bundle (`make dev`).
	DevServer string
}

// OIDCConfig is the central-Auth SSO configuration (#240).
type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	Scopes       string
	// AllowedDomains is a lowercased email-domain allowlist (no leading "@");
	// empty admits any domain the IdP authenticates.
	AllowedDomains []string
	ButtonLabel    string
	// RedirectURI pins the callback URL when set (must match the IdP
	// registration); otherwise it is derived from the public origin.
	RedirectURI string
	// AutoStart: an anonymous /login first tries SSO silently (prompt=none).
	AutoStart bool
}

// ConfigFromEnv reads the browser tier's configuration from the process
// environment.
func ConfigFromEnv() Config {
	get := func(k string) string { return strings.TrimSpace(os.Getenv(k)) }
	c := Config{
		SessionSecret:      os.Getenv("APP_SESSION_SECRET"),
		ElcanoPubKeyB64:    get("AUTH_SIGNING_PUBKEY"),
		ElcanoCookieName:   get("AUTH_COOKIE_NAME"),
		ElcanoCookieDomain: get("AUTH_COOKIE_DOMAIN"),
		AuthLoginURL:       get("AUTH_LOGIN_URL"),
		CatwalkURL:         get("CATWALK_URL"),
		ChatToken:          os.Getenv("FLEET_SERVER_TOKEN"),
		DevServer:          get("FLEET_WEB_DEV_SERVER"),
	}
	c.OrchestratorToken = firstNonEmpty(os.Getenv("ORCHESTRATOR_SERVER_TOKEN"), c.ChatToken)
	for _, k := range strings.Split(os.Getenv("AUTH_SIGNING_PREVIOUS_PUBKEYS"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			c.PreviousPubKeys = append(c.PreviousPubKeys, k)
		}
	}
	c.PublicOrigin = parseOrigin(firstNonEmpty(get("FLEET_PUBLIC_BASE_URL"), get("NEXT_PUBLIC_PUBLIC_ORIGIN")))
	c.OIDC = oidcFromEnv(get)
	return c
}

func oidcFromEnv(get func(string) string) *OIDCConfig {
	issuer := strings.TrimRight(get("FLEET_OIDC_ISSUER"), "/")
	clientID := get("FLEET_OIDC_CLIENT_ID")
	secret := get("FLEET_OIDC_CLIENT_SECRET")
	if issuer == "" || clientID == "" || secret == "" {
		return nil
	}
	o := &OIDCConfig{
		Issuer:       issuer,
		ClientID:     clientID,
		ClientSecret: secret,
		Scopes:       firstNonEmpty(get("FLEET_OIDC_SCOPES"), "openid email"),
		ButtonLabel:  firstNonEmpty(get("FLEET_OIDC_BUTTON_LABEL"), "Sign in with SSO"),
		RedirectURI:  get("FLEET_OIDC_REDIRECT_URI"),
	}
	for _, d := range strings.Split(get("FLEET_OIDC_ALLOWED_DOMAINS"), ",") {
		d = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(d)), "@")
		if d != "" {
			o.AllowedDomains = append(o.AllowedDomains, d)
		}
	}
	switch strings.ToLower(get("FLEET_OIDC_AUTO_START")) {
	case "1", "true", "yes", "on":
		o.AutoStart = true
	}
	return o
}

// parseOrigin parses a configured origin; an empty or unparseable value is
// treated as unset.
func parseOrigin(raw string) *url.URL {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil
	}
	return u
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func (c *Config) elcanoCookieName() string {
	return firstNonEmpty(c.ElcanoCookieName, websession.DefaultElcanoCookieName)
}

// authAdminURL is central Auth's admin console, <issuer>/admin, or "" when
// the issuer is not an http(s) URL.
func (c *Config) authAdminURL() string {
	if c.OIDC == nil {
		return ""
	}
	u, err := url.Parse(c.OIDC.Issuer)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return ""
	}
	return u.ResolveReference(&url.URL{Path: strings.TrimRight(u.Path, "/") + "/admin"}).String()
}

func (c *Config) authLoginURL() string {
	return strings.TrimRight(firstNonEmpty(c.AuthLoginURL, "https://auth.elcanotek.com"), "/")
}

func (c *Config) elcanoPubKey() ed25519.PublicKey {
	return websession.ParseElcanoPubKey(c.ElcanoPubKeyB64)
}
