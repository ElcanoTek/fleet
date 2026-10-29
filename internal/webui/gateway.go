// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

// Package webui is fleet's browser tier: the one public HTTP origin, served by
// the fleet binary itself (ADR-0091). It serves the embedded single-page app
// (web/, built by Vite) and every /api/* route the browser calls, and it owns
// the browser-facing security boundary:
//
//   - the request gate (which paths need a session, the two session cookies,
//     the moc bearer pass-through, the team-link cookie, idle-limit refresh);
//   - CSRF Origin checks on every mutating route;
//   - the security headers and Content-Security-Policy on every response, and
//     the X-App-Version build stamp the client uses to notice a deploy;
//   - login, logout, central-Auth SSO (OIDC) and its back-channel logout.
//
// It replaced a Next.js server whose route handlers verified the session and
// proxied to the chat-server and orchestrator listeners over loopback; the
// gateway calls those same handlers in-process (backend.go), so their
// contracts — shared-secret channel, X-User-Email scoping, session epochs —
// are unchanged.
package webui

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/ElcanoTek/fleet/internal/websession"
)

// BuildIDHeader is stamped on every response; the client compares it with the
// build id baked into its bundle to notice a deploy (shared/deployDrift.ts).
const BuildIDHeader = "X-App-Version"

// Team links (`/chat?team=<id>`, docs/TEAM-SHARING.md) survive sign-in through
// a short-lived cookie: no sign-in path carries a return URL.
const (
	teamLinkCookie        = "fleet_team_link"
	teamLinkMaxAgeSeconds = 1800
)

var teamLinkID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// publicAPIPaths are reachable without a session: the login legs, SSO, the
// back-channel, moc's bearer login, and the deployment-wide brand assets the
// pre-auth login page and anonymous unfurl scrapers need.
var publicAPIPaths = map[string]bool{
	"/api/auth/login":                     true,
	"/api/auth/logout":                    true,
	"/api/auth/elcano-login":              true,
	"/api/auth/oidc/start":                true,
	"/api/auth/oidc/callback":             true,
	"/api/auth/backchannel-logout":        true,
	"/api/orchestrator/auth/login":        true,
	"/api/orchestrator/auth/logout":       true,
	"/api/orchestrator/auth/elcano-login": true,
	"/api/theme":                          true,
	"/api/brand/logo":                     true,
	"/api/brand/share-image":              true,
}

// Gateway is the browser tier's http.Handler.
type Gateway struct {
	cfg      Config
	chat     http.Handler
	orch     http.Handler
	sessions *websession.Resolver
	api      *http.ServeMux
	spa      *spa
	branding *brandingCache
	oidc     *oidcClient
	jwks     *jwksCache
	models   *modelCatalogs
}

// Options are New's collaborators.
type Options struct {
	// Chat and Orchestrator are the two backend handlers (chat-server's
	// Routes() and the orchestrator router). Nil answers 502 on every call
	// that needs it — the browser-test harness boots the gateway alone.
	Chat         http.Handler
	Orchestrator http.Handler
	// HTTPClient reaches external services (OIDC provider, Auth's JWKS,
	// OpenRouter and catwalk catalogs). Nil uses a client with timeouts.
	HTTPClient *http.Client
	// Bundle overrides the embedded SPA (tests).
	Bundle BundleFS
}

// New builds the gateway.
func New(cfg Config, opts Options) (*Gateway, error) {
	g := &Gateway{
		cfg:  cfg,
		chat: opts.Chat,
		orch: opts.Orchestrator,
		sessions: &websession.Resolver{
			HMAC:             websession.NewHMAC(cfg.SessionSecret),
			ElcanoPubKey:     cfg.elcanoPubKey(),
			ElcanoCookieName: cfg.elcanoCookieName(),
			RefusePassword:   cfg.PasswordLoginDisabled,
		},
	}
	client := opts.HTTPClient
	if client == nil {
		client = defaultHTTPClient()
	}
	g.oidc = newOIDCClient(client)
	g.jwks = newJWKSCache(client)
	g.models = newModelCatalogs(client, cfg.CatwalkURL)
	g.branding = newBrandingCache(g)
	bundle := opts.Bundle
	if bundle == nil {
		bundle = EmbeddedBundle()
	}
	s, err := newSPA(bundle, cfg.DevServer)
	if err != nil {
		return nil, err
	}
	g.spa = s
	g.api = http.NewServeMux()
	g.registerAPI(g.api)
	return g, nil
}

// BuildID is the embedded bundle's build id ("dev" without one).
func (g *Gateway) BuildID() string { return g.spa.buildID }

// BundlePresent reports whether a real SPA bundle is embedded.
func (g *Gateway) BundlePresent() bool { return g.spa.present }

// session resolves the request's cookies.
func (g *Gateway) session(r *http.Request) *websession.Session {
	return g.sessions.Resolve(cookieValue(r, websession.CookieName), cookieValue(r, g.sessions.ElcanoCookieName))
}

// bearerToken is the moc username/password Bearer token, when presented. It
// is opaque to the gateway: the orchestrator owns and validates it.
func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if len(auth) < 7 || !strings.EqualFold(auth[:6], "bearer") {
		return ""
	}
	rest := auth[6:]
	trimmed := strings.TrimLeft(rest, " \t")
	if len(trimmed) == len(rest) {
		return "" // "Bearer" must be followed by whitespace
	}
	return strings.TrimSpace(trimmed)
}

// ServeHTTP is the request gate (the port of the Next middleware, proxy.ts).
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Built static assets are public and content-addressed (or deliberately
	// public: icons, the service worker, the theme-init script). They carry
	// the security headers but no session work.
	if !strings.HasPrefix(path, "/api/") && g.spa.serveStatic(w, r, g) {
		return
	}
	if path == "/manifest.webmanifest" {
		g.decorate(w, path)
		g.serveManifest(w, r)
		return
	}

	// A page URL with a trailing slash (/chat/, /help/chat/, /shared/<t>/)
	// redirects to the slash-less form, keeping the query — what Next did
	// (308, trailingSlash: false). The target is rebuilt from the ESCAPED path
	// with every leading slash collapsed to one, so it is always a same-origin
	// path: "//evil.example/" becomes "/evil.example", and a backslash stays
	// percent-encoded ("/%5Cevil"), never a scheme-relative URL.
	if !strings.HasPrefix(path, "/api/") && path != "/" && strings.HasSuffix(path, "/") &&
		(r.Method == http.MethodGet || r.Method == http.MethodHead) {
		target := "/" + strings.Trim(r.URL.EscapedPath(), "/")
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		g.decorate(w, path)
		w.Header().Set("Location", target)
		w.WriteHeader(http.StatusPermanentRedirect)
		return
	}

	// Public read-only shared conversations: viewable by anyone with the link,
	// logged in or not, and NOT bounced to /chat when signed in.
	if strings.HasPrefix(path, "/shared/") {
		g.decorate(w, path)
		g.servePage(w, r, nil)
		return
	}

	sess := g.session(r)

	if path == "/login" {
		g.decorate(w, path)
		if sess != nil {
			if parked := parkedTeamLink(r); parked != "" {
				g.redirectToParkedTeamLink(w, r, parked)
				return
			}
			temporaryRedirect(w, g.redirectURL(r, "/chat"))
			return
		}
		g.servePage(w, r, nil)
		return
	}

	if publicAPIPaths[path] {
		g.decorate(w, path)
		g.api.ServeHTTP(w, r)
		return
	}

	if sess == nil && bearerToken(r) == "" {
		g.decorate(w, path)
		if strings.HasPrefix(path, "/api/") {
			unauthorized(w)
			return
		}
		if path == "/chat" {
			if id := r.URL.Query().Get("team"); teamLinkID.MatchString(id) {
				setCookie(w, teamLinkCookie, id, teamLinkMaxAgeSeconds, g.secureRequest(r), "")
				temporaryRedirect(w, g.redirectURL(r, "/login?team_link=1"))
				return
			}
		}
		temporaryRedirect(w, g.redirectURL(r, "/login"))
		return
	}

	// Signed in and landing home with a team link parked: every sign-in path
	// ends at / (or /chat), so this is where the visitor is sent on to the chat
	// they opened. A request naming its own team link wins and just clears the
	// parked one; so does a parked value that is not a valid id.
	landing := path == "/" || path == "/chat"
	_, cookieErr := r.Cookie(teamLinkCookie)
	hasParked := cookieErr == nil
	g.decorate(w, path)
	if sess != nil && landing && hasParked {
		if parked := parkedTeamLink(r); parked != "" && !r.URL.Query().Has("team") {
			g.redirectToParkedTeamLink(w, r, parked)
			return
		}
		clearTeamLink(w, g.secureRequest(r))
	}
	// Activity keeps an HMAC session alive (ADR-0064). Bearer-only and
	// elcano_auth requests have nothing fleet can refresh.
	if sess != nil {
		if tok, maxAge, ok := g.sessions.HMAC.Refresh(sess); ok {
			setCookie(w, websession.CookieName, tok, maxAge, g.secureRequest(r), "")
		}
	}

	if strings.HasPrefix(path, "/api/") {
		g.api.ServeHTTP(w, r)
		return
	}
	g.servePage(w, r, sess)
}

func parkedTeamLink(r *http.Request) string {
	v := cookieValue(r, teamLinkCookie)
	if teamLinkID.MatchString(v) {
		return v
	}
	return ""
}

func clearTeamLink(w http.ResponseWriter, _ bool) {
	// The Next tier cleared it without Secure; a deletion needs only name+path.
	setCookie(w, teamLinkCookie, "", 0, false, "")
}

func (g *Gateway) redirectToParkedTeamLink(w http.ResponseWriter, r *http.Request, id string) {
	clearTeamLink(w, false)
	temporaryRedirect(w, g.redirectURL(r, "/chat?team="+url.QueryEscape(id)))
}

// decorate stamps the headers every gated response carries. A route handler
// may override Cache-Control (an upstream download's validators, the brand
// assets' short max-age) and set a stricter CSP of its own.
func (g *Gateway) decorate(w http.ResponseWriter, path string) {
	h := w.Header()
	h.Set(BuildIDHeader, g.spa.buildID)
	h.Set("Cache-Control", "no-store, must-revalidate")
	h.Set("Content-Security-Policy", g.contentSecurityPolicy(path))
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
}

// contentSecurityPolicy builds the page policy (#590).
//
// Scripts are 'self' only: the SPA ships no inline script (the theme
// bootstrap is /theme-init.js), which is the strict policy the Next tier could
// not have — its hydration payload was inline. Styles keep 'unsafe-inline'
// for React style attributes. Workspace previews use data:/blob: URLs and
// assistant markdown may show public https images.
//
// /shared/* (the public share view) drops external https images: assistant
// HTML renders there in a sandbox="" iframe, which blocks scripts but not
// sub-resource loads, so <img src=//attacker/…> would beacon every anonymous
// viewer. A srcdoc iframe inherits this policy.
//
// form-action admits the OIDC issuer's origin: sign-out is a form POST whose
// 303 lands on central Auth's RP-initiated logout, and Chromium checks the
// redirect against form-action — without it the hop is silently cancelled and
// auto-start signs the user straight back in.
func (g *Gateway) contentSecurityPolicy(path string) string {
	dev := g.cfg.DevServer != ""
	shared := strings.HasPrefix(path, "/shared/")
	script := "script-src 'self'"
	if dev {
		// Vite's dev client and the React refresh preamble.
		script += " 'unsafe-inline' 'unsafe-eval'"
	}
	img := "img-src 'self' data: blob:"
	if !shared {
		img += " https:"
	}
	connect := "connect-src 'self'"
	if dev {
		connect += " ws: wss:"
	}
	return strings.Join([]string{
		"default-src 'self'",
		script,
		"style-src 'self' 'unsafe-inline'",
		img,
		"font-src 'self' data:",
		connect,
		"media-src 'self' data: blob:",
		"frame-src 'self'",
		"object-src 'none'",
		"base-uri 'self'",
		"form-action " + g.formAction(),
		"frame-ancestors 'none'",
		"worker-src 'self' blob:",
	}, "; ")
}

func (g *Gateway) formAction() string {
	if g.cfg.OIDC == nil {
		return "'self'"
	}
	u, err := url.Parse(g.cfg.OIDC.Issuer)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "'self'"
	}
	return "'self' " + u.Scheme + "://" + u.Host
}
