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
	"net/http"
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
	{"secondary", "--color-secondary"},
	{"accent", "--color-accent"},
	{"background", "--color-bg"},
	{"surface_1", "--color-surface-1"},
	{"surface_2", "--color-surface-2"},
	{"text_primary", "--color-text-primary"},
	{"text_secondary", "--color-text-secondary"},
	{"text_muted", "--color-text-muted"},
	{"border", "--color-border"},
}

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
