package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
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
			"not_a_token": "#ffffff",                  // unknown key — ignored
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
	if strings.Contains(css, "#ffffff") {
		t.Errorf("unknown token emitted: %q", css)
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
