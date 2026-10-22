// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Branding is the deployment's white-label identity, for the surfaces that
// cannot use the member-gated /client-config: the login card (pre-auth), the
// page <title> and og/twitter tags (anonymous unfurl scrapers), the
// theme-color meta and the PWA manifest. It comes from chat-server's
// token-gated, identity-less /brand/meta; every field is public by
// construction. Failure is always cosmetic: any error yields the defaults,
// which mirror clientconfig.applyBrandingDefaults.
type Branding struct {
	AppName          string `json:"appName"`
	LoginTitle       string `json:"loginTitle"`
	LoginTagline     string `json:"loginTagline"`
	ShareTitle       string `json:"shareTitle"`
	ShareDescription string `json:"shareDescription"`
	// BackgroundLight/Dark are the bundle's background token per mode, or ""
	// for fleet's own.
	BackgroundLight string `json:"backgroundLight,omitempty"`
	BackgroundDark  string `json:"backgroundDark,omitempty"`
	// ShareImageURL is a same-origin path to the bundle's og:image, or "".
	ShareImageURL string `json:"shareImageUrl,omitempty"`
}

// DefaultBranding mirrors clientconfig's defaults for a no-bundle deployment.
var DefaultBranding = Branding{
	AppName:          "Fleet",
	LoginTitle:       "Welcome aboard.",
	LoginTagline:     "Sign in to your workspace and pick up where you left off.",
	ShareDescription: "An AI workspace with real tool use.",
}

// fleet's own --color-bg per theme (globals.css), used when the bundle
// declares no background for a mode.
const (
	defaultBackgroundDark  = "#1a0b1e"
	defaultBackgroundLight = "#f4f6fb"
	// fleetShareCard is fleet's neutral og:image (1280×640).
	fleetShareCard = "/share.png"
)

// brandingCache memoizes /brand/meta for a minute: the bundle only changes
// across a restart, and this is on every page render's critical path.
type brandingCache struct {
	g     *Gateway
	mu    sync.Mutex
	at    time.Time
	value Branding
}

func newBrandingCache(g *Gateway) *brandingCache { return &brandingCache{g: g} }

const brandingTTL = time.Minute

func (b *brandingCache) get() Branding {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.at.IsZero() && time.Since(b.at) < brandingTTL {
		return b.value
	}
	b.value = b.g.fetchBranding()
	b.at = time.Now()
	return b.value
}

func (g *Gateway) tokenOnlyHeaders() http.Header {
	h := http.Header{}
	h.Set("X-Chat-Server-Token", g.cfg.ChatToken)
	return h
}

func (g *Gateway) fetchBranding() Branding {
	v := DefaultBranding
	status, _, body, ok := g.callCapture(g.chat, call{method: http.MethodGet, target: "/brand/meta", header: g.tokenOnlyHeaders()})
	if !ok || status != http.StatusOK {
		return v
	}
	var d map[string]any
	if json.Unmarshal(body, &d) != nil || d == nil {
		return v
	}
	str := func(key, fallback string) string {
		if s, isString := d[key].(string); isString && strings.TrimSpace(s) != "" {
			return s
		}
		return fallback
	}
	color := func(key string) string {
		s, _ := d[key].(string)
		return strings.TrimSpace(s)
	}
	v.AppName = str("app_name", v.AppName)
	v.LoginTitle = str("login_title", v.LoginTitle)
	v.LoginTagline = str("login_tagline", v.LoginTagline)
	// The backend already defaults share_title to app_name; mirror it.
	v.ShareTitle = str("share_title", v.AppName)
	v.ShareDescription = str("share_description", v.ShareDescription)
	v.BackgroundLight = color("background_light")
	v.BackgroundDark = color("background_dark")
	// Only ever a fleet-authored path, but constrained anyway: it lands in an
	// og:image tag, and an absolute URL would point every unfurl at another host.
	if s, _ := d["share_image_url"].(string); strings.HasPrefix(strings.TrimSpace(s), "/") && !strings.HasPrefix(strings.TrimSpace(s), "//") {
		v.ShareImageURL = strings.TrimSpace(s)
	}
	return v
}

// maxThemeCSSBytes bounds the buffered theme stylesheet (a palette is a few
// KiB; the bound only stops a pathological bundle from pinning memory).
const maxThemeCSSBytes = 4 << 20

var (
	logoTypes       = map[string]bool{"image/svg+xml": true, "image/png": true, "image/webp": true, "image/jpeg": true, "image/x-icon": true}
	shareImageTypes = map[string]bool{"image/png": true, "image/webp": true, "image/jpeg": true}
)

func (g *Gateway) registerBrand(mux *http.ServeMux) {
	// GET /api/theme — PUBLIC: the bundle palette as a stylesheet, linked
	// render-blocking by every page (login included). It ALWAYS answers 200
	// valid CSS — empty on any backend trouble — so it can never break first
	// paint.
	mux.HandleFunc("GET /api/theme", func(w http.ResponseWriter, r *http.Request) {
		css := []byte{}
		if status, _, body, ok := g.callCaptureLimit(g.chat, call{method: http.MethodGet, target: "/theme.css", header: g.tokenOnlyHeaders()}, maxThemeCSSBytes); ok && status >= 200 && status < 300 {
			css = body
		}
		// A stable URL whose content follows the bundle: revalidated on every
		// use (no-cache + ETag), never cached blind — see AGENTS.md "Caching".
		sum := sha256.Sum256(css)
		etag := `"` + hex.EncodeToString(sum[:8]) + `"`
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(css)
	})
	// GET /api/brand/logo — PUBLIC: the bundle's mark (the favicon points
	// here), or a redirect to fleet's own mark so the URL is never dead.
	mux.HandleFunc("GET /api/brand/logo", func(w http.ResponseWriter, r *http.Request) {
		g.brandAsset(w, r, "/brand/logo", logoTypes, "/logos/fleet-mark.svg",
			"default-src 'none'; style-src 'unsafe-inline'; sandbox")
	})
	// GET /api/brand/share-image — PUBLIC (unfurl scrapers are anonymous): the
	// bundle's og:image, or fleet's neutral card. SVG is not accepted: no
	// scraper renders an SVG og:image.
	mux.HandleFunc("GET /api/brand/share-image", func(w http.ResponseWriter, r *http.Request) {
		g.brandAsset(w, r, "/brand/share-image", shareImageTypes, fleetShareCard, "default-src 'none'; sandbox")
	})
}

// brandAsset proxies one brand image, re-checking the content type against
// the allowlist so a future upstream change cannot turn it into a markup
// passthrough, and falling back (307) to fleet's own asset on anything else.
// The image streams through; nothing is buffered.
func (g *Gateway) brandAsset(w http.ResponseWriter, r *http.Request, target string, allowed map[string]bool, fallback, csp string) {
	fallbackURL := requestOriginURL(r, fallback)
	if g.chat == nil {
		temporaryRedirect(w, fallbackURL)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://127.0.0.1"+target, http.NoBody)
	if err != nil {
		temporaryRedirect(w, fallbackURL)
		return
	}
	req.RequestURI = target
	req.RemoteAddr = backendRemoteAddr
	req.Header = g.tokenOnlyHeaders()
	bw := &brandWriter{w: w, hdr: http.Header{}, allowed: allowed, fallback: fallbackURL, csp: csp}
	g.chat.ServeHTTP(bw, req)
	if !bw.wrote {
		bw.WriteHeader(http.StatusOK)
	}
}

type brandWriter struct {
	w        http.ResponseWriter
	hdr      http.Header
	allowed  map[string]bool
	fallback string
	csp      string
	wrote    bool
	discard  bool
}

func (b *brandWriter) Header() http.Header { return b.hdr }

func (b *brandWriter) WriteHeader(code int) {
	if b.wrote || code < 200 {
		return
	}
	b.wrote = true
	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(b.hdr.Get("Content-Type"), ";", 2)[0]))
	if code > 299 || !b.allowed[ct] {
		b.discard = true
		temporaryRedirect(b.w, b.fallback)
		return
	}
	h := b.w.Header()
	h.Set("Content-Type", ct)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", b.csp)
	// Stable URL, bundle-driven content: revalidate on every use (AGENTS.md
	// "Caching"); the upstream's own validators, if any, are not forwarded.
	h.Set("Cache-Control", "no-cache")
	b.w.WriteHeader(http.StatusOK)
}

func (b *brandWriter) Write(p []byte) (int, error) {
	if !b.wrote {
		b.WriteHeader(http.StatusOK)
	}
	if b.discard {
		return len(p), nil
	}
	return b.w.Write(p)
}

// requestOriginURL is `new URL(path, request.url)`: the fallback asset on the
// origin the request reached.
func requestOriginURL(r *http.Request, path string) string {
	return requestProto(r) + "://" + requestHost(r) + path
}

// serveManifest is the PWA manifest, resolved per request from the bundle so
// an installed app wears the deployment's name and colors (#895). One color
// pair: the dark background, the mode fleet defaults to.
func (g *Gateway) serveManifest(w http.ResponseWriter, _ *http.Request) {
	b := g.branding.get()
	splash := firstNonEmpty(b.BackgroundDark, defaultBackgroundDark)
	manifest := map[string]any{
		"name":             b.AppName,
		"short_name":       b.AppName,
		"start_url":        "/",
		"display":          "standalone",
		"background_color": splash,
		"theme_color":      splash,
		"icons": []map[string]string{
			// The bundle's mark; "any" because fleet does not decode it to claim
			// a size. The route falls back to fleet's mark, so never dead.
			{"src": "/api/brand/logo", "sizes": "any", "purpose": "any"},
			// Maskable stays fleet's correctly padded asset: an arbitrary
			// full-bleed bundle mark would be cropped by Android.
			{"src": "/app-icons/maskable-icon-512.png", "sizes": "512x512", "type": "image/png", "purpose": "maskable"},
		},
	}
	w.Header().Set("Content-Type", "application/manifest+json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(manifest)
}
