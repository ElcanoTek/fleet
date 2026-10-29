package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
)

func TestRenderThemeCSS_EmitsValidTokensInStableOrder(t *testing.T) {
	css := renderThemeCSS(clientconfig.BrandColors{
		Dark: map[string]string{
			"accent":     "#9da7ef",
			"primary":    "#7272ab",
			"background": "#1a0b1e",
		},
	})
	// Selector out-specifies globals.css so order-of-load can't lose.
	if !strings.Contains(css, `html:root[data-theme="dark"]{`) {
		t.Fatalf("missing dark selector: %q", css)
	}
	// Declared in themeTokenOrder order (primary before accent before background),
	// not map-iteration order.
	pi, ai, bi := strings.Index(css, "--color-primary:"), strings.Index(css, "--color-accent:"), strings.Index(css, "--color-bg:")
	if pi < 0 || ai <= pi || bi <= ai {
		t.Errorf("tokens out of stable order: primary=%d accent=%d bg=%d in %q", pi, ai, bi, css)
	}
	if strings.Contains(css, `data-theme="light"`) {
		t.Errorf("emitted a light block for a dark-only palette: %q", css)
	}
}

func TestRenderThemeCSS_DropsInvalidAndUnknown(t *testing.T) {
	css := renderThemeCSS(clientconfig.BrandColors{
		Light: map[string]string{
			"primary":     "#7272ab",                  // valid hex
			"accent":      "rgb(157, 167, 239)",       // valid functional
			"secondary":   "red; } body{display:none", // INVALID — injection attempt
			"text_muted":  "javascript:alert(1)",      // INVALID
			"not_a_token": "#a1b2c3",                  // unknown key — ignored
		},
	})
	if !strings.Contains(css, "--color-primary:#7272ab;") {
		t.Errorf("valid hex dropped: %q", css)
	}
	if !strings.Contains(css, "--color-accent:rgb(157, 167, 239);") {
		t.Errorf("valid functional color dropped: %q", css)
	}
	if strings.Contains(css, "display:none") || strings.Contains(css, "body{") {
		t.Errorf("injection survived sanitation: %q", css)
	}
	if strings.Contains(css, "javascript") {
		t.Errorf("invalid value emitted: %q", css)
	}
	if strings.Contains(css, "#a1b2c3") {
		t.Errorf("unknown token emitted: %q", css)
	}
}

// #12345 and #1234567 are not CSS colors; only 3/4/6/8-digit hex is. The
// browser would drop the declaration in /theme.css anyway, but the same
// validator feeds <meta name="theme-color"> and the PWA manifest, where an
// invalid value ships as-is — so the regex itself must reject odd lengths.
func TestRenderThemeCSS_RejectsOddHexLengths(t *testing.T) {
	css := renderThemeCSS(clientconfig.BrandColors{
		Dark: map[string]string{
			"primary":   "#12345",    // 5 digits — invalid
			"secondary": "#1234567",  // 7 digits — invalid
			"accent":    "#1234",     // 4 digits (RGBA) — valid
			"border":    "#12345678", // 8 digits (RRGGBBAA) — valid
		},
	})
	if strings.Contains(css, "#12345;") || strings.Contains(css, "#1234567;") {
		t.Errorf("odd-length hex emitted: %q", css)
	}
	if !strings.Contains(css, "--color-accent:#1234;") {
		t.Errorf("4-digit hex dropped: %q", css)
	}
	if !strings.Contains(css, "--color-border:#12345678;") {
		t.Errorf("8-digit hex dropped: %q", css)
	}
}

func TestRenderThemeCSS_EmptyPaletteEmitsNoRules(t *testing.T) {
	css := renderThemeCSS(clientconfig.BrandColors{})
	if strings.Contains(css, "{") {
		t.Errorf("expected no rules for empty palette, got %q", css)
	}
}

func TestThemeCSS_TokenGatedButIdentityless(t *testing.T) {
	s := &Server{sharedToken: "topsecret"}
	h := s.tokenOnlyMiddleware(http.HandlerFunc(s.themeCSS))

	// No token -> 403.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/theme.css", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("no token: status %d want 403", w.Code)
	}

	// Valid token, NO X-User-Email -> 200 (identity-less, unlike authMiddleware).
	req = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/theme.css", nil)
	req.Header.Set("X-Chat-Server-Token", "topsecret")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("valid token: status %d want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("content-type %q want text/css", ct)
	}
}

func TestThemeCSS_ServesBundlePalette(t *testing.T) {
	s := &Server{
		sharedToken: "topsecret",
		clientConfig: &clientconfig.Bundle{
			Branding: clientconfig.Branding{
				Colors: clientconfig.BrandColors{
					Dark: map[string]string{"primary": "#e6007e"},
				},
			},
		},
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/theme.css", nil)
	req.Header.Set("X-Chat-Server-Token", "topsecret")
	w := httptest.NewRecorder()
	s.tokenOnlyMiddleware(http.HandlerFunc(s.themeCSS)).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "--color-primary:#e6007e;") {
		t.Errorf("bundle color not served: %q", w.Body.String())
	}
}

// ── /brand/logo ─────────────────────────────────────────────────────────────

// newLogoServer builds a Server whose bundle declares a logo file on disk.
func newLogoServer(t *testing.T, name string, body []byte) *Server {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write logo: %v", err)
	}
	return &Server{
		sharedToken: "topsecret",
		clientConfig: &clientconfig.Bundle{
			Dir:           dir,
			Branding:      clientconfig.Branding{Logo: name},
			BrandLogoPath: path,
		},
	}
}

func getLogo(t *testing.T, s *Server) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/brand/logo", nil)
	req.Header.Set("X-Chat-Server-Token", "topsecret")
	w := httptest.NewRecorder()
	s.tokenOnlyMiddleware(http.HandlerFunc(s.brandLogo)).ServeHTTP(w, req)
	return w
}

func TestBrandLogo_ServesBundleFileWithHardenedHeaders(t *testing.T) {
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="28" height="28"></svg>`)
	w := getLogo(t, newLogoServer(t, "mark.svg", svg))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d want 200", w.Code)
	}
	if got := w.Body.String(); got != string(svg) {
		t.Errorf("body = %q want the bundle file verbatim", got)
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/svg+xml" {
		t.Errorf("content-type %q want image/svg+xml", ct)
	}
	// An SVG is a parsed document and this route is directly reachable, so the
	// type must be pinned and scripts inside it must not execute.
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff — the browser could sniff past the declared type")
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP %q must deny by default", csp)
	}
}

func TestBrandLogo_404WithoutBundleOrLogo(t *testing.T) {
	for name, s := range map[string]*Server{
		"no bundle": {sharedToken: "topsecret"},
		"bundle without a logo": {
			sharedToken:  "topsecret",
			clientConfig: &clientconfig.Bundle{Dir: t.TempDir()},
		},
	} {
		if w := getLogo(t, s); w.Code != http.StatusNotFound {
			t.Errorf("%s: status %d want 404 so the web falls back to fleet's mark", name, w.Code)
		}
	}
}

// A file that vanished under a running process must not 500 — the rail just
// falls back, because a missing mark is never worth failing a page over.
func TestBrandLogo_404WhenFileDisappeared(t *testing.T) {
	s := newLogoServer(t, "mark.png", []byte("\x89PNG\r\n\x1a\n"))
	if err := os.Remove(s.clientConfig.BrandLogoPath); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if w := getLogo(t, s); w.Code != http.StatusNotFound {
		t.Errorf("status %d want 404", w.Code)
	}
}

func TestBrandLogo_404OverCap(t *testing.T) {
	s := newLogoServer(t, "huge.png", make([]byte, brandLogoMaxBytes+1))
	if w := getLogo(t, s); w.Code != http.StatusNotFound {
		t.Errorf("status %d want 404 — an oversize mark should degrade, not ship megabytes per page", w.Code)
	}
}

func TestBrandLogo_TokenGated(t *testing.T) {
	s := newLogoServer(t, "mark.svg", []byte("<svg/>"))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/brand/logo", nil)
	w := httptest.NewRecorder()
	s.tokenOnlyMiddleware(http.HandlerFunc(s.brandLogo)).ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("no token: status %d want 403", w.Code)
	}
}

// The logo is advertised on /client-config only when a file actually backed it,
// so the web never points an <img> at a route that 404s.
func TestClientConfig_AdvertisesLogoOnlyWhenResolved(t *testing.T) {
	s := newLogoServer(t, "mark.svg", []byte("<svg/>"))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/client-config", nil)
	w := httptest.NewRecorder()
	s.clientConfigHandler(w, req)
	if !strings.Contains(w.Body.String(), `"logo_url":"`+brandLogoWebPath+`"`) {
		t.Errorf("body %s must advertise %s", w.Body.String(), brandLogoWebPath)
	}

	s.clientConfig.BrandLogoPath = ""
	w = httptest.NewRecorder()
	s.clientConfigHandler(w, req)
	if strings.Contains(w.Body.String(), "logo_url") {
		t.Errorf("body %s must omit logo_url when no file backed it", w.Body.String())
	}
}

func TestClientConfig_CarriesSupportContact(t *testing.T) {
	s := newLogoServer(t, "mark.svg", []byte("<svg/>"))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/client-config", nil)

	w := httptest.NewRecorder()
	s.clientConfigHandler(w, req)
	if strings.Contains(w.Body.String(), `"support"`) {
		t.Errorf("body %s must omit support when not configured", w.Body.String())
	}

	s.clientConfig.Branding.Support = &clientconfig.SupportContact{
		Label:   "Direct Support",
		Email:   "support@example.com",
		Title:   "Direct Support Line",
		Message: "Reach out directly with questions or feedback.",
		Subject: "Workspace support",
	}
	w = httptest.NewRecorder()
	s.clientConfigHandler(w, req)
	want := `"support":{"label":"Direct Support","email":"support@example.com","title":"Direct Support Line","message":"Reach out directly with questions or feedback.","subject":"Workspace support"}`
	if !strings.Contains(w.Body.String(), want) {
		t.Errorf("body %s must carry %s", w.Body.String(), want)
	}
}

// /client-config carries the workspace's effective model tiers (#1187): the
// live agentcore holders, so an admin override is visible to the next shell
// mount without any rebuild — and the compiled-in pair before any override.
func TestClientConfig_CarriesLiveModelTiers(t *testing.T) {
	t.Cleanup(func() {
		agentcore.SetDefaultModel("")
		agentcore.SetAdvancedModel("")
		agentcore.SetTaskModel("")
	})
	s := newLogoServer(t, "mark.svg", []byte("<svg/>"))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/client-config", nil)

	// With no task-tier override, task_model is the default tier.
	w := httptest.NewRecorder()
	s.clientConfigHandler(w, req)
	want := `"models":{"default_model":"` + agentcore.DefaultCoreModel + `","advanced_model":"` + agentcore.DefaultMaxModel + `","task_model":"` + agentcore.DefaultCoreModel + `"}`
	if !strings.Contains(w.Body.String(), want) {
		t.Errorf("body %s must carry the compiled-in tier pair %s", w.Body.String(), want)
	}

	agentcore.SetDefaultModel("acme/frontier-1")
	agentcore.SetAdvancedModel("acme/frontier-1-pro")
	w = httptest.NewRecorder()
	s.clientConfigHandler(w, req)
	want = `"models":{"default_model":"acme/frontier-1","advanced_model":"acme/frontier-1-pro","task_model":"acme/frontier-1"}`
	if !strings.Contains(w.Body.String(), want) {
		t.Errorf("body %s must carry the overridden tiers %s", w.Body.String(), want)
	}

	agentcore.SetTaskModel("acme/long-runner")
	w = httptest.NewRecorder()
	s.clientConfigHandler(w, req)
	want = `"task_model":"acme/long-runner"`
	if !strings.Contains(w.Body.String(), want) {
		t.Errorf("body %s must carry the task-tier override %s", w.Body.String(), want)
	}

	// The operator's scheduler fallback rides along for the task-create form;
	// unset, the key is omitted so the web keeps its compiled-in fallback.
	if s.cfg == nil {
		s.cfg = &config.Config{}
	}
	s.cfg.TaskFallbackModel = " acme/cheap-1 "
	w = httptest.NewRecorder()
	s.clientConfigHandler(w, req)
	want = `"task_fallback_model":"acme/cheap-1"`
	if !strings.Contains(w.Body.String(), want) {
		t.Errorf("body %s must carry the operator's task fallback %s", w.Body.String(), want)
	}
}

// /client-config carries the operator's pinned picker rows (FLEET_PINNED_MODELS)
// verbatim, and omits the key entirely when none are configured so an older web
// build sees the payload it always saw.
func TestClientConfig_CarriesPinnedModels(t *testing.T) {
	s := newLogoServer(t, "mark.svg", []byte("<svg/>"))
	if s.cfg == nil {
		s.cfg = &config.Config{}
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/client-config", nil)

	w := httptest.NewRecorder()
	s.clientConfigHandler(w, req)
	if strings.Contains(w.Body.String(), `"pinned"`) {
		t.Errorf("body %s must omit pinned when none are configured", w.Body.String())
	}

	s.cfg.PinnedModels = []config.PinnedModel{
		{Slug: "acme/open-70b", Label: "Elcano Nova", Tag: "alpha"},
		{Slug: "acme/open-8b", OutputPricePerMtok: func() *float64 { v := 0.0; return &v }()},
	}
	w = httptest.NewRecorder()
	s.clientConfigHandler(w, req)
	want := `"pinned":[{"slug":"acme/open-70b","label":"Elcano Nova","tag":"alpha"},{"slug":"acme/open-8b","output_price_per_mtok":0}]`
	if !strings.Contains(w.Body.String(), want) {
		t.Errorf("body %s must carry the pinned rows %s", w.Body.String(), want)
	}
}

// A rebranded palette repaints the Tools picker's tokens from its own colors
// (Reklaim's picker was fleet purple), and a pinned picker token wins.
func TestRenderThemeCSS_PickerTokensFollowPalette(t *testing.T) {
	css := renderThemeCSS(clientconfig.BrandColors{
		Dark: map[string]string{
			"primary":       "#FFDF03",
			"on_primary":    "#0C0A09",
			"accent":        "#5BA7FF",
			"border_strong": "rgba(255, 223, 3, 0.32)",
			"rail_active":   "rgba(255, 223, 3, 0.16)",
			"link":          "#123456", // pinned: not derived
		},
		Light: map[string]string{"primary": "#FFDF03"},
	})
	for _, want := range []string{
		"--color-accent-strong:var(--color-primary);",
		"--color-on-accent-strong:var(--color-on-primary);",
		"--color-switch-knob-on:var(--color-on-primary);",
		"--color-selected-bg:var(--rail-active);",
		"--color-selected-border:var(--color-border-strong);",
		"--color-match-bg:color-mix(in srgb, var(--color-primary) 22%, transparent);",
		"--color-icon-tile:color-mix(in srgb, var(--color-accent) 14%, transparent);",
		"--color-link:#123456;",
		"--color-match-bg:color-mix(in srgb, var(--color-primary) 40%, transparent);",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("missing %q in %q", want, css)
		}
	}
	// Nothing derives from a key the bundle did not set.
	for _, unwanted := range []string{"--color-text-subtle:", "--color-divider:", "--color-scrim:"} {
		if strings.Contains(css, unwanted) {
			t.Errorf("derived %q without its source key: %q", unwanted, css)
		}
	}
	if strings.Count(css, "--color-link:") != 1 {
		t.Errorf("light block derived a link without accent: %q", css)
	}
}

// on_primary alone repaints the picker's foregrounds, and a malformed picker
// pin falls back to its default instead of being replaced by a derived value.
func TestRenderThemeCSS_PickerDerivationEdges(t *testing.T) {
	css := renderThemeCSS(clientconfig.BrandColors{
		Dark: map[string]string{"on_primary": "#0C0A09"},
		Light: map[string]string{
			"primary":       "#FFDF03",
			"accent_strong": "red; } body{display:none", // malformed pin
		},
	})
	_, dark, ok := strings.Cut(css, `data-theme="dark"`)
	if !ok {
		t.Fatalf("no dark block: %q", css)
	}
	for _, want := range []string{"--color-on-accent-strong:var(--color-on-primary);", "--color-switch-knob-on:var(--color-on-primary);"} {
		if !strings.Contains(dark, want) {
			t.Errorf("on_primary alone did not derive %q: %q", want, css)
		}
	}
	if strings.Contains(css, "--color-accent-strong:") {
		t.Errorf("malformed accent_strong pin was replaced, want the default: %q", css)
	}
	if strings.Contains(css, "display:none") {
		t.Errorf("malformed value emitted: %q", css)
	}
}
