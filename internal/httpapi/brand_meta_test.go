package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
)

// getBrandMeta issues GET /brand/meta against s and decodes the response.
func getBrandMeta(t *testing.T, s *Server) (*httptest.ResponseRecorder, brandMetaResponse) {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/brand/meta", nil)
	w := httptest.NewRecorder()
	s.brandMeta(w, req)
	var got brandMetaResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal /brand/meta: %v (body %q)", err, w.Body.String())
		}
	}
	return w, got
}

// A bundle's identity strings must actually come out of the endpoint — this is
// the plumbing that #892/#894 showed was missing at the render layer.
func TestBrandMeta_ServesBundleIdentity(t *testing.T) {
	s := &Server{
		clientConfig: &clientconfig.Bundle{
			Branding: clientconfig.Branding{
				AppName:          "Reklaim",
				LoginTitle:       "Reklaim what's yours.",
				LoginTagline:     "Sign in and pick up where you left off.",
				ShareTitle:       "Reklaim — AI workspace",
				ShareDescription: "Persistent conversations with real tool use.",
				Colors: clientconfig.BrandColors{
					Light: map[string]string{"background": "#FAFAF9", "primary": "#FFDF03"},
					Dark:  map[string]string{"background": "#0A0908", "primary": "#FFDF03"},
				},
			},
		},
	}
	w, got := getBrandMeta(t, s)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got.AppName != "Reklaim" {
		t.Errorf("app_name = %q, want %q", got.AppName, "Reklaim")
	}
	if got.LoginTitle != "Reklaim what's yours." {
		t.Errorf("login_title = %q", got.LoginTitle)
	}
	if got.LoginTagline != "Sign in and pick up where you left off." {
		t.Errorf("login_tagline = %q", got.LoginTagline)
	}
	if got.ShareTitle != "Reklaim — AI workspace" {
		t.Errorf("share_title = %q", got.ShareTitle)
	}
	if got.ShareDescription != "Persistent conversations with real tool use." {
		t.Errorf("share_description = %q", got.ShareDescription)
	}
	// Only the background token is echoed — the rest of the palette is /theme.css's job.
	if got.BackgroundLight != "#FAFAF9" || got.BackgroundDark != "#0A0908" {
		t.Errorf("backgrounds = %q / %q, want #FAFAF9 / #0A0908", got.BackgroundLight, got.BackgroundDark)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=300" {
		t.Errorf("Cache-Control = %q, want the same short max-age as /theme.css", cc)
	}
}

// With no bundle the endpoint must still return a coherent identity, from the
// SAME defaults /client-config uses, so the no-bundle and sparse-bundle
// experiences cannot drift.
func TestBrandMeta_NoBundleMatchesClientConfigDefaults(t *testing.T) {
	w, got := getBrandMeta(t, &Server{})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	d := clientconfig.DefaultBranding()
	if got.AppName != d.AppName {
		t.Errorf("app_name = %q, want the shared default %q", got.AppName, d.AppName)
	}
	if got.LoginTitle != d.LoginTitle {
		t.Errorf("login_title = %q, want %q", got.LoginTitle, d.LoginTitle)
	}
	if got.ShareTitle != d.ShareTitle {
		t.Errorf("share_title = %q, want %q", got.ShareTitle, d.ShareTitle)
	}
	if got.ShareDescription != d.ShareDescription {
		t.Errorf("share_description = %q, want %q", got.ShareDescription, d.ShareDescription)
	}
	// No bundle means no palette: the web must fall back to its own literals
	// rather than be handed an empty string to put in a <meta> tag.
	if got.BackgroundLight != "" || got.BackgroundDark != "" {
		t.Errorf("backgrounds = %q / %q, want both empty", got.BackgroundLight, got.BackgroundDark)
	}
}

// A sparse manifest gets the same defaults, per-field.
func TestBrandMeta_SparseBundleFallsBackPerField(t *testing.T) {
	s := &Server{
		clientConfig: &clientconfig.Bundle{
			// Mirrors the post-load shape: a manifest with only app_name set has
			// its remaining fields filled by applyBrandingDefaults before it is
			// ever served, and ShareTitle defaults to AppName.
			Branding: clientconfig.Branding{
				AppName:          "Acme",
				LoginTitle:       clientconfig.DefaultBranding().LoginTitle,
				LoginTagline:     clientconfig.DefaultBranding().LoginTagline,
				ShareTitle:       "Acme",
				ShareDescription: clientconfig.DefaultBranding().ShareDescription,
			},
		},
	}
	_, got := getBrandMeta(t, s)
	d := clientconfig.DefaultBranding()
	if got.AppName != "Acme" {
		t.Errorf("app_name = %q, want Acme", got.AppName)
	}
	if got.ShareTitle != "Acme" {
		t.Errorf("share_title = %q, want it to default to app_name", got.ShareTitle)
	}
	if got.LoginTitle != d.LoginTitle {
		t.Errorf("login_title = %q, want the generic default %q", got.LoginTitle, d.LoginTitle)
	}
}

// A background fleet's own theme renderer would refuse must not be handed to the
// web either: <meta name="theme-color"> and the PWA manifest take a bare color,
// and one definition of "a color fleet emits" (colorValueRe) governs both.
func TestBrandMeta_DropsUnrenderableBackgrounds(t *testing.T) {
	cases := []struct {
		name  string
		value string
	}{
		{"css injection", "#fff;}html{display:none"},
		{"named color", "rebeccapurple"},
		{"empty", ""},
		{"whitespace only", "   "},
		{"url()", "url(https://evil.example/x.png)"},
		{"var reference", "var(--color-bg)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{clientConfig: &clientconfig.Bundle{
				Branding: clientconfig.Branding{Colors: clientconfig.BrandColors{
					Dark: map[string]string{"background": tc.value},
				}},
			}}
			_, got := getBrandMeta(t, s)
			if got.BackgroundDark != "" {
				t.Errorf("background_dark = %q, want it dropped", got.BackgroundDark)
			}
		})
	}
}

// The functional color forms /theme.css accepts must survive here too, so a
// bundle that themes in rgb()/hsl() is not silently downgraded to fleet's purple
// in the browser chrome.
func TestBrandMeta_AcceptsFunctionalColorForms(t *testing.T) {
	for _, v := range []string{"#abc", "#0A0908", "#0A0908FF", "rgb(10, 9, 8)", "rgba(10,9,8,0.5)", "hsl(210 40% 12%)"} {
		s := &Server{clientConfig: &clientconfig.Bundle{
			Branding: clientconfig.Branding{Colors: clientconfig.BrandColors{
				Dark: map[string]string{"background": v},
			}},
		}}
		_, got := getBrandMeta(t, s)
		if got.BackgroundDark != v {
			t.Errorf("background_dark for %q = %q, want it preserved", v, got.BackgroundDark)
		}
	}
}

// Token-gated but identity-less: reachable without X-User-Email (the login page
// has no session), refused without the shared secret (the browser must not be
// able to reach chat-server directly).
func TestBrandMeta_TokenGatedIdentityLess(t *testing.T) {
	s := &Server{sharedToken: "topsecret"}
	h := s.tokenOnlyMiddleware(http.HandlerFunc(s.brandMeta))

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/brand/meta", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("no token: status = %d, want 403", w.Code)
	}

	req = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/brand/meta", nil)
	req.Header.Set("X-Chat-Server-Token", "wrong")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("wrong token: status = %d, want 403", w.Code)
	}

	// Correct token, NO X-User-Email — the pre-auth case.
	req = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/brand/meta", nil)
	req.Header.Set("X-Chat-Server-Token", "topsecret")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("token, no identity: status = %d, want 200", w.Code)
	}
}

func TestBrandMeta_RejectsNonGET(t *testing.T) {
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequestWithContext(context.Background(), m, "/brand/meta", nil)
		w := httptest.NewRecorder()
		(&Server{}).brandMeta(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", m, w.Code)
		}
	}
}

// /brand/meta must not become a second, un-gated /client-config: the empty-state
// catalog is workspace content, not public identity.
func TestBrandMeta_CarriesNoWorkspaceContent(t *testing.T) {
	s := &Server{clientConfig: &clientconfig.Bundle{
		Branding: clientconfig.Branding{AppName: "Acme"},
		EmptyState: clientconfig.EmptyState{
			Cards:         []map[string]any{{"title": "internal prompt card"}},
			ProtocolPills: []map[string]any{{"label": "internal protocol"}},
		},
	}}
	w, _ := getBrandMeta(t, s)
	body := w.Body.String()
	for _, leaked := range []string{"internal prompt card", "internal protocol", "cards", "protocol_pills"} {
		if strings.Contains(body, leaked) {
			t.Errorf("/brand/meta leaked %q: %s", leaked, body)
		}
	}
}
