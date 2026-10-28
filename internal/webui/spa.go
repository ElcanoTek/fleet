// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/websession"
)

// BundleFS is a built SPA: index.html, build-id.txt, content-hashed assets
// under assets/, and the public files (icons, sw.js, theme-init.js, …).
type BundleFS = fs.FS

// Markers the Vite index.html carries; the gateway renders per-request
// content into them (web/index.html).
const (
	headMarker      = "<!--fleet:head-->"
	bootstrapMarker = "<!--fleet:bootstrap-->"
)

// spa serves the bundle: hashed assets with immutable caching, the other
// public files revalidated, and index.html — rendered per request with the
// page's metadata and bootstrap JSON — never cached.
type spa struct {
	fsys    fs.FS
	present bool
	buildID string
	// index is the bundle's index.html, split at the two markers.
	indexHead, indexMid, indexTail string
	etags                          map[string]string
	dev                            *devServer
}

func newSPA(fsys fs.FS, devURL string) (*spa, error) {
	s := &spa{fsys: fsys, buildID: "dev", etags: map[string]string{}}
	if devURL != "" {
		u, err := url.Parse(devURL)
		if err != nil || u.Host == "" {
			return nil, errors.New("webui: FLEET_WEB_DEV_SERVER is not a URL: " + devURL)
		}
		s.dev = newDevServer(u)
		s.present = true
		return s, nil
	}
	index, err := fs.ReadFile(fsys, "index.html")
	if errors.Is(err, fs.ErrNotExist) {
		// No bundle embedded (a `go build` that skipped `make web`): the API
		// still works; pages answer a 503 that says how to build one.
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	s.present = true
	if err := s.setIndex(string(index)); err != nil {
		return nil, err
	}
	if id, err := fs.ReadFile(fsys, "build-id.txt"); err == nil && strings.TrimSpace(string(id)) != "" {
		s.buildID = strings.TrimSpace(string(id))
	} else {
		sum := sha256.Sum256(index)
		s.buildID = hex.EncodeToString(sum[:6])
	}
	return s, nil
}

func (s *spa) setIndex(index string) error {
	h := strings.Index(index, headMarker)
	b := strings.Index(index, bootstrapMarker)
	if h < 0 || b < 0 || b < h {
		return errors.New("webui: index.html is missing the " + headMarker + " / " + bootstrapMarker + " markers")
	}
	s.indexHead = index[:h]
	s.indexMid = index[h+len(headMarker) : b]
	s.indexTail = index[b+len(bootstrapMarker):]
	return nil
}

// hiddenFiles are bundle files that are not served.
var hiddenFiles = map[string]bool{"index.html": true, "build-id.txt": true}

// serveStatic serves a bundle file when path names one. Hashed assets are
// immutable; everything else (icons, the service worker, theme-init.js) is
// revalidated by ETag so a deploy is picked up. A missing /assets/ file is a
// plain 404 — never index.html, which a stale tab would try to run as a
// module.
func (s *spa) serveStatic(w http.ResponseWriter, r *http.Request, g *Gateway) bool {
	p := r.URL.Path
	if s.dev != nil {
		if s.dev.owns(p) {
			g.decorate(w, p)
			w.Header().Del("Cache-Control")
			s.dev.proxy.ServeHTTP(w, r)
			return true
		}
		return false
	}
	if p == "/" || !s.present {
		return false
	}
	// Rooted before cleaning, so no ".." can climb above the bundle root, and
	// then checked with fs.ValidPath — the bundle is an fs.FS (go:embed), which
	// cannot reach the host filesystem anyway, but a name it would reject is
	// never looked up. r.URL.Path is already percent-decoded, so %2e%2e and
	// %2f arrive here as "." and "/" and are handled the same way.
	name := strings.TrimPrefix(path.Clean("/"+strings.Trim(p, "/")), "/")
	if name == "" || !fs.ValidPath(name) || hiddenFiles[name] || strings.HasPrefix(name, ".") {
		return false
	}
	f, err := s.fsys.Open(name)
	if err != nil {
		if strings.HasPrefix(name, "assets/") {
			g.decorate(w, p)
			http.NotFound(w, r)
			return true
		}
		return false
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		return false
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return false
	}
	g.decorate(w, p)
	h := w.Header()
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		h.Set("Content-Type", ct)
	}
	// The method check comes first: an error response on an asset URL must
	// never carry the immutable header (a cache would pin the error).
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		h.Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	if strings.HasPrefix(name, "assets/") {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
		h.Set("ETag", s.etag(name, data))
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
	return true
}

func (s *spa) etag(name string, data []byte) string {
	if e, ok := s.etags[name]; ok {
		return e
	}
	sum := sha256.Sum256(data)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

// ── pages ──────────────────────────────────────────────────────────────────

// helpGuides are the /help/{slug} pages (help/guideContent.ts).
var helpGuides = map[string]string{"chat": "Chat", "operations-center": "Operations Center"}

// pagePaths are the client routes the SPA renders. Anything else gets the
// SPA's not-found page with a 404 status.
var pagePaths = map[string]bool{
	"/chat": true, "/orchestrator": true, "/login": true, "/no-access": true,
	"/settings": true, "/settings/admin": true, "/settings/admin/doctor": true,
	"/settings/admin/features": true, "/settings/admin/notifications": true,
	"/settings/admin/providers": true, "/settings/admin/server": true,
	"/settings/admin/users": true, "/settings/connections": true,
	"/settings/shared-files": true, "/settings/skills": true, "/settings/team": true,
	"/settings/personas": true, "/help": true,
}

// Bootstrap is the JSON the SPA reads at boot from #fleet-bootstrap: the
// per-request state the Next tier's server components computed.
type Bootstrap struct {
	BuildID  string       `json:"buildId"`
	Branding Branding     `json:"branding"`
	Session  *bootSession `json:"session"`
	Login    bootLogin    `json:"login"`
	// Shared is the /shared/{token} snapshot, verbatim from chat-server.
	Shared   json.RawMessage `json:"shared,omitempty"`
	NotFound bool            `json:"notFound,omitempty"`
	// FullLoadRoutes are the paths a client-side navigation must not take
	// on its own: entering or leaving one is a full page load, so the decision
	// servePage makes for it runs (see fullLoadRoutes).
	FullLoadRoutes []string `json:"fullLoadRoutes"`
	// CentralAuth is set when Auth manages accounts (see bootCentralAuth).
	CentralAuth *bootCentralAuth `json:"centralAuth,omitempty"`
}

// fullLoadRoutes are the client routes whose page depends on a decision only
// the server makes in servePage, so the SPA's router hands any navigation into
// (or out of) them to the browser. An entry ending in "/" is a prefix.
//
//   - /login: the silent-SSO auto start and the team-link state.
//   - /shared/: the snapshot is embedded into the page by the server.
//   - /no-access: the outcome of /chat's membership gate. Leaving it (its
//     "Try again" link to /chat) must re-run that gate, which a client-side
//     hop would skip, rendering a shell whose every call 403s.
//
// The /chat gate itself is not listed: an in-app hop to /chat comes from a
// session that already passed it, and a reload there would drop the chat
// state the router keeps across /settings round trips.
var fullLoadRoutes = []string{"/login", "/no-access", "/shared/"}

// bootCentralAuth is present when central Auth manages this Fleet's
// accounts, roles and teams: the account pages become view-only and link to
// Auth's console.
type bootCentralAuth struct {
	AdminURL string `json:"adminUrl"`
}

type bootSession struct {
	Email string `json:"email"`
}

type bootLogin struct {
	// PasswordLoginEnabled is false on a Fleet that signs in through central
	// Auth only: the login card drops its password form, and Settings → Admin
	// → Users its password controls.
	PasswordLoginEnabled  bool   `json:"passwordLoginEnabled"`
	MagicLinkLoginEnabled bool   `json:"magicLinkLoginEnabled"`
	OIDCEnabled           bool   `json:"oidcEnabled"`
	OIDCLabel             string `json:"oidcLabel"`
	// TeamLink: the visitor is signing in to open a team link (B24).
	TeamLink bool `json:"teamLink"`
}

// servePage renders a client route, doing first what the Next server
// components did before rendering: redirects, the membership entry gate, the
// silent-SSO auto start, the shared snapshot.
func (g *Gateway) servePage(w http.ResponseWriter, r *http.Request, sess *websession.Session) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p := r.URL.Path
	boot := Bootstrap{BuildID: g.spa.buildID, FullLoadRoutes: fullLoadRoutes}
	status := http.StatusOK
	title := ""

	switch {
	case p == "/":
		temporaryRedirect(w, g.redirectURL(r, "/chat"))
		return
	case p == "/admin":
		// /admin moved under the settings area; the alias keeps bookmarks working.
		w.Header().Set("Location", g.redirectURL(r, "/settings/admin"))
		w.WriteHeader(http.StatusPermanentRedirect)
		return
	case strings.HasPrefix(p, "/orchestrator/tasks/") && strings.Count(p, "/") == 3 && len(p) > len("/orchestrator/tasks/"):
		// The link a task notification carries (push, email, webhook log_url).
		id := r.PathValue("taskId")
		if id == "" {
			id, _ = url.PathUnescape(strings.TrimPrefix(p, "/orchestrator/tasks/"))
		}
		temporaryRedirect(w, g.redirectURL(r, "/orchestrator?task="+encodeURIComponent(id)))
		return
	case p == "/chat":
		// Membership entry gate for identities minted elsewhere (#1522): an
		// account Auth admits but fleet's user list does not gets one clear
		// no-access page, not a shell whose every call 403s. chat-server's
		// membership middleware remains the real boundary.
		if sess != nil && (sess.Source == websession.SourceElcano || sess.Source == websession.SourceOIDC) && g.notAMember(identityOf(sess)) {
			temporaryRedirect(w, g.redirectURL(r, "/no-access"))
			return
		}
	case p == "/login":
		if shouldAutoStartLogin(r.URL.Query(), g.cfg.OIDC) {
			temporaryRedirect(w, g.redirectURL(r, "/api/auth/oidc/start?silent=1"))
			return
		}
		boot.Login.TeamLink = r.URL.Query().Get("team_link") == "1" || parkedTeamLink(r) != ""
	case strings.HasPrefix(p, "/shared/"):
		token, err := url.PathUnescape(strings.TrimPrefix(p, "/shared/"))
		if err != nil || token == "" || strings.Contains(token, "/") {
			status, boot.NotFound = http.StatusNotFound, true
			break
		}
		h := http.Header{}
		h.Set("X-Chat-Server-Token", g.cfg.ChatToken)
		st, _, body, ok := g.callCaptureLimit(g.chat, call{method: http.MethodGet, target: "/shared/" + encodeURIComponent(token), header: h}, maxBackendDocumentSize)
		if !ok || st < 200 || st > 299 || !json.Valid(body) {
			status, boot.NotFound = http.StatusNotFound, true
			break
		}
		boot.Shared = body
	case p == "/help":
		title = "Guides"
	case strings.HasPrefix(p, "/help/"):
		if name, ok := helpGuides[strings.TrimPrefix(p, "/help/")]; ok {
			title = name + " guide"
		} else {
			status, boot.NotFound = http.StatusNotFound, true
		}
	case !pagePaths[p]:
		status, boot.NotFound = http.StatusNotFound, true
	}

	boot.Branding = g.branding.get()
	if sess != nil {
		boot.Session = &bootSession{Email: sess.Email}
	}
	boot.Login.PasswordLoginEnabled = !g.cfg.PasswordLoginDisabled
	if g.cfg.AccountsManagedByAuth {
		boot.CentralAuth = &bootCentralAuth{AdminURL: g.cfg.authAdminURL()}
	}
	boot.Login.MagicLinkLoginEnabled = g.cfg.ElcanoPubKeyB64 != ""
	if g.cfg.OIDC != nil {
		boot.Login.OIDCEnabled = true
		boot.Login.OIDCLabel = g.cfg.OIDC.ButtonLabel
	} else {
		boot.Login.OIDCLabel = "Sign in with SSO"
	}
	g.renderIndex(w, r, status, title, boot)
}

// notAMember asks chat-server whether a session minted elsewhere belongs to
// a provisioned user. Only an explicit 403 counts: an unreachable or failing
// backend must not trap the user on the no-access page.
func (g *Gateway) notAMember(id identity) bool {
	status, _, _, ok := g.callCapture(g.chat, call{method: http.MethodGet, target: "/auth/membership", header: g.chatHeaders(id)})
	return ok && status == http.StatusForbidden
}

// renderIndex writes index.html with the page's head metadata and the
// bootstrap JSON. Without an embedded bundle it answers 503 with how to build
// one (`go build` alone does not run Vite; the release binary always embeds
// it — see `make web` and docs/adr/0091).
func (g *Gateway) renderIndex(w http.ResponseWriter, r *http.Request, status int, title string, boot Bootstrap) {
	if !g.spa.present {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "<!doctype html><meta charset=utf-8><title>fleet</title><p>This fleet binary was built without its web bundle. Build it with <code>make build</code> (which runs <code>make web</code> first).</p>")
		return
	}
	head, mid, tail := g.spa.indexHead, g.spa.indexMid, g.spa.indexTail
	if g.spa.dev != nil {
		index, err := g.spa.dev.index()
		if err != nil {
			http.Error(w, "vite dev server: "+err.Error(), http.StatusBadGateway)
			return
		}
		tmp := &spa{}
		if err := tmp.setIndex(index); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		head, mid, tail = tmp.indexHead, tmp.indexMid, tmp.indexTail
	}
	b := boot.Branding
	pageTitle := b.AppName
	if title != "" {
		pageTitle = title
		if strings.TrimSpace(b.AppName) != "" {
			pageTitle = title + " — " + b.AppName
		}
	}
	bootJSON, err := json.Marshal(boot) // escapes <, >, & — safe inside <script>
	if err != nil {
		http.Error(w, "bootstrap: "+err.Error(), http.StatusInternalServerError)
		return
	}
	var out strings.Builder
	out.WriteString(head)
	out.WriteString(g.headTags(pageTitle, b))
	out.WriteString(mid)
	out.WriteString(`<script type="application/json" id="fleet-bootstrap">`)
	out.Write(bootJSON)
	out.WriteString(`</script>`)
	out.WriteString(tail)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.WriteString(w, out.String())
}

// publicOriginForMeta is metadataBase: the origin unfurl scrapers resolve
// og:image against. The env-configured public origin, else a neutral
// placeholder (one bundle may serve several origins, so this is a property of
// the host, not the bundle).
func (g *Gateway) publicOriginForMeta() *url.URL {
	if g.cfg.PublicOrigin != nil {
		return &url.URL{Scheme: g.cfg.PublicOrigin.Scheme, Host: g.cfg.PublicOrigin.Host}
	}
	return &url.URL{Scheme: "https", Host: "chat.example.com"}
}

// headTags renders the per-request <head> metadata (title, share tags,
// browser-chrome tint) from the bundle's branding.
func (g *Gateway) headTags(title string, b Branding) string {
	esc := html.EscapeString
	base := g.publicOriginForMeta()
	abs := func(p string) string {
		ref, err := url.Parse(p)
		if err != nil {
			return p
		}
		return base.ResolveReference(ref).String()
	}
	shareTitle := firstNonEmpty(b.ShareTitle, b.AppName)
	var s strings.Builder
	meta := func(attr, key, val string) {
		s.WriteString(`<meta ` + attr + `="` + esc(key) + `" content="` + esc(val) + `">`)
	}
	s.WriteString("<title>" + esc(title) + "</title>")
	meta("name", "description", b.ShareDescription)
	meta("name", "application-name", b.AppName)
	meta("name", "author", b.AppName)
	meta("name", "robots", "noindex, nofollow")
	meta("name", "mobile-web-app-capable", "yes")
	meta("name", "apple-mobile-web-app-title", b.AppName)
	meta("name", "apple-mobile-web-app-status-bar-style", "black-translucent")
	s.WriteString(`<meta name="theme-color" media="(prefers-color-scheme: light)" content="` + esc(firstNonEmpty(b.BackgroundLight, defaultBackgroundLight)) + `">`)
	s.WriteString(`<meta name="theme-color" media="(prefers-color-scheme: dark)" content="` + esc(firstNonEmpty(b.BackgroundDark, defaultBackgroundDark)) + `">`)
	meta("property", "og:type", "website")
	meta("property", "og:site_name", b.AppName)
	meta("property", "og:title", shareTitle)
	meta("property", "og:description", b.ShareDescription)
	meta("property", "og:url", base.String())
	image := firstNonEmpty(b.ShareImageURL, fleetShareCard)
	meta("property", "og:image", abs(image))
	// Dimensions only for fleet's own card: fleet does not decode a bundle's
	// image, and a wrong declared size distorts the preview.
	if b.ShareImageURL == "" {
		meta("property", "og:image:width", "1280")
		meta("property", "og:image:height", "640")
	}
	meta("property", "og:image:alt", shareTitle)
	meta("name", "twitter:card", "summary_large_image")
	meta("name", "twitter:title", shareTitle)
	meta("name", "twitter:description", b.ShareDescription)
	meta("name", "twitter:image", abs(image))
	return s.String()
}

// ── vite dev server ────────────────────────────────────────────────────────

// devServer fronts a running `vite` for `make dev`: index.html and every
// module/asset request come from Vite (HMR websocket included); pages and
// /api stay the gateway's, so the dev loop exercises the real gate.
type devServer struct {
	base   *url.URL
	proxy  *httputil.ReverseProxy
	client *http.Client
}

func newDevServer(u *url.URL) *devServer {
	return &devServer{base: u, proxy: httputil.NewSingleHostReverseProxy(u), client: &http.Client{Timeout: 10 * time.Second}}
}

// owns: Vite's own endpoints, source modules, and any path with a file
// extension (public files and imported assets).
func (d *devServer) owns(p string) bool {
	for _, prefix := range []string{"/@vite/", "/@react-refresh", "/@id/", "/@fs/", "/src/", "/node_modules/", "/__vite"} {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return !strings.HasPrefix(p, "/api/") && p != "/manifest.webmanifest" && path.Ext(p) != ""
}

func (d *devServer) index() (string, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, d.base.ResolveReference(&url.URL{Path: "/index.html"}).String(), nil)
	if err != nil {
		return "", err
	}
	res, err := d.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return "", errors.New(res.Status)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	return string(b), err
}
