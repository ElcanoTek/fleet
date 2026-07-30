package httpapi

// theme.go serves the deployment's brand palette as a render-blocking
// stylesheet so the web shell — including the pre-auth login page — paints in
// the client's colors with no flash. The values come from the client-config
// bundle manifest (branding.colors); an absent/sparse block emits nothing and
// the hardcoded globals.css defaults stand. Colors are non-secret,
// deployment-wide, and not user-scoped, so the route is token-gated but
// identity-less (see tokenOnlyMiddleware).

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
)

// themeTokenOrder maps each themable manifest color key to the CSS custom
// property it overrides in globals.css, in a STABLE order so the emitted
// stylesheet is deterministic (and testable). Keys outside this set are ignored
// — the manifest can list extra tokens without affecting output.
var themeTokenOrder = []struct{ key, cssVar string }{
	{"primary", "--color-primary"},
	{"primary_hover", "--color-primary-hover"},
	// on_primary is the readable foreground ON a primary fill. It cannot be
	// derived from the palette — a dark primary needs light text and a light
	// primary (yellow, lime, cyan) needs dark text — so a bundle whose primary
	// is light MUST set it or its buttons render white-on-light.
	{"on_primary", "--color-on-primary"},
	{"secondary", "--color-secondary"},
	{"accent", "--color-accent"},
	{"background", "--color-bg"},
	{"surface_1", "--color-surface-1"},
	{"surface_2", "--color-surface-2"},
	{"text_primary", "--color-text-primary"},
	{"text_secondary", "--color-text-secondary"},
	{"text_muted", "--color-text-muted"},
	{"text_disabled", "--color-text-disabled"},
	{"border", "--color-border"},
	// The structural neighbours of the tokens above. globals.css hand-tints
	// these from fleet's own primary hue rather than deriving them, so a bundle
	// that set only `border` still showed fleet-purple emphasis borders, muted
	// scrims, and rail rows next to its own palette. --rail-hover/--rail-active
	// close the follow-up globals.css records inline ("the later theming PR
	// repaints these from the per-deployment manifest").
	{"border_strong", "--color-border-strong"},
	{"border_subtle", "--color-border-subtle"},
	{"overlay_soft", "--color-overlay-soft"},
	{"overlay_strong", "--color-overlay-strong"},
	{"rail_hover", "--rail-hover"},
	{"rail_active", "--rail-active"},
}

// Semantic status colors (--color-success/-danger/-warning and their borders)
// are deliberately NOT themable. They encode meaning rather than brand — a
// failed tool call must read as failure in every deployment — and several are
// derived with color-mix() from the base hue, so a partial override would
// desynchronize a swatch from its own border.

// colorValueRe whitelists CSS color syntaxes the manifest may use: hex
// (#rgb..#rrggbbaa) and the rgb()/rgba()/hsl()/hsla() functional forms over a
// safe character class. Anything else (notably `;`, `{`, `}`, `<`) is dropped
// at render time, so a malformed or hostile value can neither break the
// stylesheet nor inject markup, and the affected token falls back to its
// default. Manifest content is operator-controlled (bundle push == host code
// exec per SECURITY.md), so this is defense-in-depth, not a trust boundary.
var colorValueRe = regexp.MustCompile(`^#[0-9a-fA-F]{3,8}$|^(?:rgb|rgba|hsl|hsla)\([0-9a-zA-Z.,%/\s]+\)$`)

// writeThemeBlock appends a single `selector{...}` rule for the non-empty,
// valid tokens in colors. It writes nothing (not even an empty rule) when no
// token survives validation.
func writeThemeBlock(b *strings.Builder, selector string, colors map[string]string) {
	if len(colors) == 0 {
		return
	}
	var decls strings.Builder
	for _, t := range themeTokenOrder {
		v := strings.TrimSpace(colors[t.key])
		if v == "" || !colorValueRe.MatchString(v) {
			continue
		}
		fmt.Fprintf(&decls, "%s:%s;", t.cssVar, v)
	}
	if decls.Len() == 0 {
		return
	}
	fmt.Fprintf(b, "%s{%s}", selector, decls.String())
}

// renderThemeCSS builds the brand stylesheet for a palette. The selectors are
// `html:root[data-theme="..."]` so the rules out-specify globals.css's
// `:root` / `:root[data-theme="..."]` blocks and win regardless of stylesheet
// load order — no @import ordering or !important needed.
func renderThemeCSS(colors clientconfig.BrandColors) string {
	var b strings.Builder
	b.WriteString("/* fleet brand theme (client-config bundle) */")
	writeThemeBlock(&b, `html:root[data-theme="light"]`, colors.Light)
	writeThemeBlock(&b, `html:root[data-theme="dark"]`, colors.Dark)
	return b.String()
}

// themeCSS serves the brand palette as text/css. Always 200 with valid CSS
// (just the comment header when no bundle / no colors) so it can never block
// paint of the shell that links it.
func (s *Server) themeCSS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	// Deployment-wide, non-secret branding; cacheable. Short max-age so a bundle
	// re-theme (operator restart) propagates promptly without a hard reload.
	w.Header().Set("Cache-Control", "public, max-age=300")
	css := "/* fleet brand theme (client-config bundle) */"
	if s.clientConfig != nil {
		css = renderThemeCSS(s.clientConfig.Branding.Colors)
	}
	_, _ = w.Write([]byte(css))
}

// brandLogoMaxBytes caps what /brand/logo will stream. A mark is a few KB; the
// cap exists so a bundle that points the field at a huge file degrades to
// fleet's own mark instead of shipping megabytes on every page load.
const brandLogoMaxBytes = 2 << 20 // 2 MiB

// brandShareImageMaxBytes caps /brand/share-image. Larger than the logo cap
// because the asset genuinely is: a 1280x640 unfurl card is hundreds of KB
// (fleet's own is ~700 KB) where a rail mark is a few. It is still capped —
// unfurl scrapers fetch it and some give up on slow responses, so an oversized
// card would break the share preview it exists to provide.
const brandShareImageMaxBytes = 5 << 20 // 5 MiB

// brandLogo serves the bundle's branding.logo file, or 404 when the bundle
// declares none — the web then falls back to fleet's own mark. Same trust class
// as /theme.css: a deployment-wide, non-secret brand asset, so the route is
// token-gated but identity-less (the pre-auth login shell may show it).
//
// clientconfig resolved and containment-checked the path at load
// (resolveBrandLogo), so this handler does no path arithmetic on request input —
// there is no request input. It re-stats only to fail soft if the file vanished
// under a running process.
func (s *Server) brandLogo(w http.ResponseWriter, r *http.Request) {
	path := ""
	if s.clientConfig != nil {
		path = s.clientConfig.BrandLogoPath
	}
	s.serveBrandImage(w, r, path, "branding.logo", brandLogoMaxBytes)
}

// brandShareImage serves the bundle's branding.share_image — the og:image /
// twitter:image link-unfurl scrapers render for this deployment — or 404 when
// the bundle declares none, in which case the web falls back to fleet's own
// neutral card. Same trust class and same hardening as /brand/logo; unfurl
// scrapers are anonymous, so this must be reachable without a session.
func (s *Server) brandShareImage(w http.ResponseWriter, r *http.Request) {
	path := ""
	if s.clientConfig != nil {
		path = s.clientConfig.BrandShareImagePath
	}
	s.serveBrandImage(w, r, path, "branding.share_image", brandShareImageMaxBytes)
}

// serveBrandImage streams a load-validated bundle image. field names the manifest
// key for log messages. An empty path means the bundle declared none — a 404, and
// the web falls back to fleet's own asset.
func (s *Server) serveBrandImage(w http.ResponseWriter, r *http.Request, path, field string, maxBytes int64) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if path == "" {
		http.NotFound(w, r)
		return
	}
	ctype := clientconfig.BrandLogoContentType(path)
	if ctype == "" {
		// Unreachable: load-time validation rejects an unknown extension. Fail
		// soft rather than guessing a type the browser would sniff.
		http.NotFound(w, r)
		return
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	if info.Size() > maxBytes {
		log.Printf("httpapi: %s %s is %d bytes (cap %d); serving fleet's default asset instead", field, path, info.Size(), maxBytes)
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ctype)
	// An SVG mark is a document the browser will parse, and this route is
	// directly reachable. Bundle content is operator-authored (a bundle push is
	// already host code execution per SECURITY.md), so this is defense in depth,
	// not a trust boundary: nosniff pins the declared type, and the CSP means an
	// SVG carrying <script> still executes nothing if someone opens the URL
	// directly instead of via the <img> the shell renders.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	// Same short max-age as /theme.css: a re-theme (operator restart) propagates
	// promptly without a hard reload.
	w.Header().Set("Cache-Control", "public, max-age=300")
	// G304: path is not request-derived — there is no request input on either
	// route. It is Bundle.BrandLogoPath or Bundle.BrandShareImagePath, both of
	// which clientconfig.resolveBrandImage already containment-checked against
	// the bundle root (lexically via filepath.IsLocal, then again after
	// EvalSymlinks) at load time.
	f, err := os.Open(path) //nolint:gosec // see comment above
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), f)
}
