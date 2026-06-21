package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsImageMIME(t *testing.T) {
	cases := map[string]bool{
		"image/png":  true,
		"image/jpeg": true,
		"IMAGE/PNG":  true,
		"image/jpg":  true,
		"image/gif":  true,
		"image/webp": true,
		"image/svg":  false, // intentionally excluded; vision models often choke on SVG
		"image/heic": false,
		"":           false,
		"text/plain": false,
	}
	for in, want := range cases {
		if got := IsImageMIME(in); got != want {
			t.Errorf("IsImageMIME(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestImageMIMEFromName(t *testing.T) {
	cases := map[string]string{
		"foo.png":      "image/png",
		"BAR.JPG":      "image/jpeg",
		"q.jpeg":       "image/jpeg",
		"animated.gif": "image/gif",
		"r.webp":       "image/webp",
		"x.txt":        "",
		"":             "",
	}
	for in, want := range cases {
		if got := ImageMIMEFromName(in); got != want {
			t.Errorf("ImageMIMEFromName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDecodeImageDataURI(t *testing.T) {
	payload := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	uri := "data:image/png;base64," + base64.StdEncoding.EncodeToString(payload)
	mt, data, err := decodeImageDataURI(uri)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if mt != "image/png" {
		t.Errorf("media = %q", mt)
	}
	if string(data) != string(payload) {
		t.Errorf("data mismatch")
	}
	if _, _, err := decodeImageDataURI("https://example.com/x.png"); err == nil {
		t.Error("expected error for non-data URI")
	}
	if _, _, err := decodeImageDataURI("data:image/png,plain"); err == nil {
		t.Error("expected error for non-base64 data URI")
	}
}

func TestParseImageGenResponseHappy(t *testing.T) {
	body := []byte(`{
	  "choices":[{"message":{"role":"assistant","content":"here","images":[
	    {"image_url":{"url":"data:image/png;base64,aGVsbG8="}}
	  ]}}],
	  "usage":{"cost":0.04}
	}`)
	mt, data, comment, cost, err := parseImageGenResponse(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if mt != "image/png" || string(data) != "hello" || comment != "here" || cost == nil {
		t.Errorf("unexpected: mt=%q comment=%q cost=%v", mt, comment, cost)
	}
}

func TestParseImageGenResponseRefusal(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"I cannot do that"}}]}`)
	if _, _, _, _, err := parseImageGenResponse(body); err == nil || !strings.Contains(err.Error(), "I cannot do that") {
		t.Errorf("expected error surfacing model text, got %v", err)
	}
}

// rewriteRoundTripper redirects every request to the same path on `target`,
// preserving method/body/headers. Lets the test exercise runGenerateImage's
// real network code path against a stub server.
type rewriteRoundTripper struct{ target string }

func (rw rewriteRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	parts := strings.SplitN(strings.TrimPrefix(rw.target, "http://"), "/", 2)
	r2.URL.Scheme = "http"
	r2.URL.Host = parts[0]
	r2.Host = parts[0]
	return http.DefaultTransport.RoundTrip(r2)
}

func TestCanonicalExtForImageMedia(t *testing.T) {
	cases := map[string]string{
		"image/png":                ".png",
		"image/jpeg":               ".jpg",
		"IMAGE/JPEG":               ".jpg",
		"image/jpg":                ".jpg",
		"image/webp":               ".webp",
		"image/gif":                ".gif",
		"":                         "",
		"application/octet-stream": "",
		"image/heic":               "",
	}
	for in, want := range cases {
		if got := canonicalExtForImageMedia(in); got != want {
			t.Errorf("canonicalExtForImageMedia(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeImageFilename(t *testing.T) {
	cases := []struct{ in, want string }{
		{"my-art", "my-art"},
		{"  spaced  ", "spaced"},
		{"banner.png", "banner"},         // recognized ext stripped
		{"banner.weird", "banner-weird"}, // unknown ext kept (replaced char-by-char)
		{"path/to/file.jpg", "file"},
		{"a/b\\c/d", "d"},
		{"..hidden", "hidden"},
		{"weird name!?", "weird-name"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := sanitizeImageFilename(tc.in); got != tc.want {
				t.Errorf("sanitizeImageFilename(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	// Empty / fully-stripped inputs default to image-<timestamp>.
	for _, in := range []string{"", "   ", "//\\\\", "----"} {
		got := sanitizeImageFilename(in)
		if !strings.HasPrefix(got, "image-") {
			t.Errorf("expected default slug for %q, got %q", in, got)
		}
	}
	// Length cap.
	long := strings.Repeat("a", 200)
	if got := sanitizeImageFilename(long); len(got) != 80 {
		t.Errorf("long slug truncated to %d chars, want 80", len(got))
	}
}

// TestRunGenerateImage_StubExtensionFromMIME asserts the saved file's
// extension matches the model's response MIME — even when the agent passed
// a filename ending in a different extension.
func TestRunGenerateImage_StubExtensionFromMIME(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "test-key")
	t.Setenv("FLEET_WORKSPACE_ROOT", t.TempDir())
	tmp := t.TempDir()
	t.Setenv("FLEET_ALLOWED_DIRS", tmp)
	t.Chdir(tmp)

	jpegBytes := []byte{0xff, 0xd8, 0xff, 0xe0}
	dataURI := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpegBytes)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("missing auth header")
		}
		var req struct {
			Modalities []string `json:"modalities"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Modalities) == 0 || req.Modalities[0] != "image" {
			t.Errorf("expected image modality")
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"","images":[{"image_url":{"url":"` + dataURI + `"}}]}}],"usage":{"cost":0.01}}`))
	}))
	defer srv.Close()

	client := &http.Client{Transport: rewriteRoundTripper{target: srv.URL}}
	res, err := runGenerateImage(context.Background(), client, GenerateImageParams{
		Prompt:   "a banner",
		Filename: "banner.png", // wrong-format hint must be ignored
	})
	if err != nil {
		t.Fatalf("runGenerateImage: %v", err)
	}
	if filepath.Ext(res.Path) != ".jpg" {
		t.Errorf("expected .jpg from response MIME, got %q", res.Path)
	}
	if filepath.Base(res.Path) != "banner.jpg" {
		t.Errorf("expected basename banner.jpg, got %q", filepath.Base(res.Path))
	}
	if res.Bytes != len(jpegBytes) {
		t.Errorf("bytes = %d, want %d", res.Bytes, len(jpegBytes))
	}
	got, err := os.ReadFile(res.Path)
	if err != nil || string(got) != string(jpegBytes) {
		t.Errorf("file mismatch: %v %q", err, string(got))
	}
}

// TestRunGenerateImage_DefaultFilename verifies that omitting the filename
// produces a timestamp-based slug rather than failing.
func TestRunGenerateImage_DefaultFilename(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "k")
	t.Setenv("FLEET_WORKSPACE_ROOT", t.TempDir())
	tmp := t.TempDir()
	t.Setenv("FLEET_ALLOWED_DIRS", tmp)
	t.Chdir(tmp)

	dataURI := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("PNG"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"images":[{"image_url":{"url":"` + dataURI + `"}}]}}]}`))
	}))
	defer srv.Close()

	client := &http.Client{Transport: rewriteRoundTripper{target: srv.URL}}
	res, err := runGenerateImage(context.Background(), client, GenerateImageParams{Prompt: "x"})
	if err != nil {
		t.Fatalf("runGenerateImage: %v", err)
	}
	base := filepath.Base(res.Path)
	if !strings.HasPrefix(base, "image-") || !strings.HasSuffix(base, ".png") {
		t.Errorf("expected default slug image-<ts>.png, got %q", base)
	}
}

func TestRunGenerateImage_RequiresAPIKey(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	if _, err := runGenerateImage(context.Background(), http.DefaultClient, GenerateImageParams{
		Prompt: "x",
	}); err == nil || !strings.Contains(err.Error(), "OPENROUTER_API_KEY") {
		t.Errorf("expected API key error, got %v", err)
	}
}

func TestRunGenerateImage_RequiresPrompt(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "k")
	if _, err := runGenerateImage(context.Background(), http.DefaultClient, GenerateImageParams{
		Prompt: "",
	}); err == nil {
		t.Error("expected prompt-required error")
	}
}
