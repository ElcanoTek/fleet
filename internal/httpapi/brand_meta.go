package httpapi

// brand_meta.go serves the deployment's white-label identity — its name, login
// copy, share strings, and the two background colors — as JSON for the web's
// SERVER-SIDE metadata generation.
//
// It exists because the surfaces that need these strings cannot use
// /client-config:
//
//   - The login card renders before a session exists, and /client-config is
//     member-gated. So `branding.login_title` / `login_tagline` were parsed,
//     defaulted, API-served, typed in the web client — and then hardcoded in
//     login-card.tsx, because that component structurally could not fetch them
//     (#892).
//   - The root layout's <title>, og:*, twitter:* and <meta name="theme-color">
//     are resolved by Next in a server context with no user, and unfurl
//     scrapers (Slack, iMessage, Discord) are anonymous by definition. So
//     `share_title` / `share_description` were read by zero components and the
//     tab title fell back to a build-time env var (#894, #895).
//
// Same trust class as /theme.css and /brand/logo: token-gated (only the trusted
// Next layer holds the shared secret, so a browser still cannot reach
// chat-server directly) but IDENTITY-less. That is sound rather than convenient
// — every field here is public by construction. The app name is in the browser
// tab, the login copy is printed on the pre-auth login page, the share strings
// go into OG tags that anonymous scrapers read, and the backgrounds are already
// served to the same audience by /theme.css.
//
// Deliberately NOT the whole of /client-config: the empty-state catalog it also
// carries is workspace content (prompt cards, protocol pills) rather than public
// identity, and has no business being readable without a session.

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
)

// brandMetaResponse is the white-label identity the web resolves server-side.
// Field names mirror the manifest's branding keys so the two read the same.
type brandMetaResponse struct {
	AppName          string `json:"app_name"`
	LoginTitle       string `json:"login_title"`
	LoginTagline     string `json:"login_tagline"`
	ShareTitle       string `json:"share_title"`
	ShareDescription string `json:"share_description"`
	// BackgroundLight / BackgroundDark are the bundle's `background` color token
	// per mode, echoed here so the web can set <meta name="theme-color"> and the
	// PWA manifest's background/theme color — neither of which can read CSS
	// custom properties, and both of which were hardcoded fleet purple.
	//
	// Omitted when the bundle sets no background for that mode, or sets one the
	// theme renderer would reject, so the web keeps its own default rather than
	// being handed an unusable value. Validated with the SAME colorValueRe as
	// /theme.css: one definition of "a color fleet will emit".
	BackgroundLight string `json:"background_light,omitempty"`
	BackgroundDark  string `json:"background_dark,omitempty"`
}

// validBackground returns the mode's background token when it is present and a
// color shape fleet emits, else "".
func validBackground(colors map[string]string) string {
	v := strings.TrimSpace(colors["background"])
	if v == "" || !colorValueRe.MatchString(v) {
		return ""
	}
	return v
}

// brandMeta serves the deployment's white-label identity. Always 200 with a
// complete, coherent object: with no bundle it returns the same generic defaults
// clientconfig.applyBrandingDefaults gives a sparse manifest, which is the same
// source of truth /client-config uses — so the no-bundle and sparse-bundle
// identities cannot drift from each other or from the member-gated route.
func (s *Server) brandMeta(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	d := clientconfig.DefaultBranding()
	resp := brandMetaResponse{
		AppName:          d.AppName,
		LoginTitle:       d.LoginTitle,
		LoginTagline:     d.LoginTagline,
		ShareTitle:       d.ShareTitle,
		ShareDescription: d.ShareDescription,
	}
	if s.clientConfig != nil {
		b := s.clientConfig.Branding
		resp = brandMetaResponse{
			AppName:          b.AppName,
			LoginTitle:       b.LoginTitle,
			LoginTagline:     b.LoginTagline,
			ShareTitle:       b.ShareTitle,
			ShareDescription: b.ShareDescription,
			BackgroundLight:  validBackground(b.Colors.Light),
			BackgroundDark:   validBackground(b.Colors.Dark),
		}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Same short max-age as /theme.css and /brand/logo: a re-theme (which needs
	// an operator restart anyway) propagates promptly. The web memoizes this in
	// process on top, so a page render does not pay a round trip per request.
	w.Header().Set("Cache-Control", "public, max-age=300")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("httpapi: encode /brand/meta: %v", err)
	}
}
