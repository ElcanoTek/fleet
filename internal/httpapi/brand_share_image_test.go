package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
)

// shareImageFixture writes a file into a temp bundle dir and returns a Server
// whose bundle points branding.share_image at it.
func shareImageFixture(t *testing.T, name string, content []byte) *Server {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return &Server{
		sharedToken: "topsecret",
		clientConfig: &clientconfig.Bundle{
			Dir:                 dir,
			Branding:            clientconfig.Branding{ShareImage: name},
			BrandShareImagePath: path,
		},
	}
}

func getShareImage(t *testing.T, s *Server, method string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), method, "/brand/share-image", nil)
	w := httptest.NewRecorder()
	s.brandShareImage(w, req)
	return w
}

// The bundle's card must actually be served — this is the whole point of #893.
func TestBrandShareImage_ServesBundleCard(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\nfake-card-bytes")
	s := shareImageFixture(t, "acme-share.png", png)

	w := getShareImage(t, s, http.MethodGet)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Body.Bytes(); string(got) != string(png) {
		t.Errorf("body = %q, want the bundle file's bytes", got)
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("Content-Type = %q, want image/png", ct)
	}
	// Same defensive delivery as /brand/logo: bundle content is operator-authored,
	// but this route is directly reachable so the type is pinned and scripts are
	// inert even if someone opens the URL.
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff")
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP = %q, want a locked-down policy", csp)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "public, max-age=300" {
		t.Errorf("Cache-Control = %q", cc)
	}
}

// No bundle, or a bundle declaring no share image, must 404 so the web falls back
// to fleet's own neutral card rather than serving a broken og:image.
func TestBrandShareImage_404WithoutADeclaredImage(t *testing.T) {
	for name, s := range map[string]*Server{
		"no bundle":      {},
		"no share_image": {clientConfig: &clientconfig.Bundle{Dir: t.TempDir()}},
	} {
		t.Run(name, func(t *testing.T) {
			if w := getShareImage(t, s, http.MethodGet); w.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", w.Code)
			}
		})
	}
}

// A file that vanished under a running process must fail soft, not 500.
func TestBrandShareImage_404WhenFileVanished(t *testing.T) {
	s := shareImageFixture(t, "card.png", []byte("x"))
	if err := os.Remove(s.clientConfig.BrandShareImagePath); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if w := getShareImage(t, s, http.MethodGet); w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// The share-image cap is deliberately larger than the logo cap, and both are
// enforced. An oversized card degrades to fleet's own rather than making every
// unfurl scraper wait on megabytes.
func TestBrandShareImage_EnforcesItsOwnLargerCap(t *testing.T) {
	if brandShareImageMaxBytes <= brandLogoMaxBytes {
		t.Fatalf("share-image cap %d must exceed the logo cap %d", brandShareImageMaxBytes, brandLogoMaxBytes)
	}
	// Just over the share-image cap.
	s := shareImageFixture(t, "huge.png", make([]byte, brandShareImageMaxBytes+1))
	if w := getShareImage(t, s, http.MethodGet); w.Code != http.StatusNotFound {
		t.Errorf("oversized: status = %d, want 404", w.Code)
	}

	// A card that would BUST the logo cap is still fine here — that is the point
	// of giving the two assets separate caps.
	ok := shareImageFixture(t, "big-but-fine.png", make([]byte, brandLogoMaxBytes+1))
	if w := getShareImage(t, ok, http.MethodGet); w.Code != http.StatusOK {
		t.Errorf("card larger than the LOGO cap: status = %d, want 200", w.Code)
	}
}

func TestBrandShareImage_HeadAndMethodGating(t *testing.T) {
	s := shareImageFixture(t, "card.png", []byte("\x89PNGdata"))
	if w := getShareImage(t, s, http.MethodHead); w.Code != http.StatusOK {
		t.Errorf("HEAD: status = %d, want 200", w.Code)
	}
	for _, m := range []string{http.MethodPost, http.MethodDelete} {
		if w := getShareImage(t, s, m); w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", m, w.Code)
		}
	}
}

// Token-gated but identity-less: an unfurl scraper has no session, and the
// browser must still not reach chat-server directly.
func TestBrandShareImage_TokenGatedIdentityLess(t *testing.T) {
	s := shareImageFixture(t, "card.png", []byte("\x89PNGdata"))
	h := s.tokenOnlyMiddleware(http.HandlerFunc(s.brandShareImage))

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/brand/share-image", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("no token: status = %d, want 403", w.Code)
	}

	req = httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/brand/share-image", nil)
	req.Header.Set("X-Chat-Server-Token", "topsecret")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("token, no identity: status = %d, want 200", w.Code)
	}
}

// /brand/meta advertises the URL only when a file backed the field, mirroring
// logo_url, so the web never emits an og:image pointing at a 404.
func TestBrandMeta_AdvertisesShareImageOnlyWhenBacked(t *testing.T) {
	backed := shareImageFixture(t, "card.png", []byte("\x89PNGdata"))
	_, got := getBrandMeta(t, backed)
	if got.ShareImageURL != brandShareImageWebPath {
		t.Errorf("share_image_url = %q, want %q", got.ShareImageURL, brandShareImageWebPath)
	}

	unbacked := &Server{clientConfig: &clientconfig.Bundle{Dir: t.TempDir()}}
	w, got := getBrandMeta(t, unbacked)
	if got.ShareImageURL != "" {
		t.Errorf("share_image_url = %q, want empty for a bundle with no share_image", got.ShareImageURL)
	}
	// omitempty: the key must be absent, not present-and-empty, so an older web
	// build sees exactly what it saw before.
	if strings.Contains(w.Body.String(), "share_image_url") {
		t.Errorf("share_image_url should be omitted entirely: %s", w.Body.String())
	}
}
