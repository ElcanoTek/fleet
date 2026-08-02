// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package clientconfig

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// branding.share_image goes through the SAME validator as branding.logo
// (resolveBrandImage), so these tests exist to prove the new field is actually
// wired to it rather than getting its own weaker path — a share image resolving
// outside the bundle would be served under an image content type to any
// anonymous link-unfurl scraper that asks.

// shareBundle builds a Bundle rooted at a temp dir containing
// `assets/share.png`, with Branding.ShareImage set to rel.
func shareBundle(t *testing.T, rel string) *Bundle {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "share.png"), []byte("\x89PNG"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return &Bundle{Dir: dir, Branding: Branding{ShareImage: rel}}
}

func TestResolveBrandShareImage_AcceptsBundleRelativeFile(t *testing.T) {
	b := shareBundle(t, "assets/share.png")
	if err := b.resolveBrandShareImage(); err != nil {
		t.Fatalf("valid share_image rejected: %v", err)
	}
	if b.BrandShareImagePath == "" {
		t.Fatal("BrandShareImagePath not recorded — the HTTP layer would have nothing to serve")
	}
	if !strings.HasSuffix(b.BrandShareImagePath, filepath.Join("assets", "share.png")) {
		t.Errorf("BrandShareImagePath = %q, want it to end at the declared file", b.BrandShareImagePath)
	}
}

// Absent is the normal case (fleet's own neutral card stands) and must not error.
func TestResolveBrandShareImage_EmptyIsValid(t *testing.T) {
	b := &Bundle{Dir: t.TempDir()}
	if err := b.resolveBrandShareImage(); err != nil {
		t.Fatalf("absent share_image: %v", err)
	}
	if b.BrandShareImagePath != "" {
		t.Errorf("BrandShareImagePath = %q, want empty", b.BrandShareImagePath)
	}
	// Whitespace-only is the same as absent, not a path of blanks.
	b = &Bundle{Dir: t.TempDir(), Branding: Branding{ShareImage: "   "}}
	if err := b.resolveBrandShareImage(); err != nil || b.BrandShareImagePath != "" || b.Branding.ShareImage != "" {
		t.Errorf("blank share_image: err=%v path=%q field=%q", err, b.BrandShareImagePath, b.Branding.ShareImage)
	}
}

func TestResolveBrandShareImage_Rejects(t *testing.T) {
	cases := []struct {
		name string
		rel  string
		want string
	}{
		{"absolute path", "/etc/passwd", "bundle-relative"},
		{"parent escape", "../../etc/passwd", "bundle-relative"},
		{"nested parent escape", "assets/../../outside.png", "bundle-relative"},
		{"unknown extension", "assets/share.txt", "unsupported extension"},
		{"no extension", "assets/share", "unsupported extension"},
		{"missing file", "assets/missing.png", "no such file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := shareBundle(t, tc.rel)
			err := b.resolveBrandShareImage()
			if err == nil {
				t.Fatalf("accepted %q; want an error", tc.rel)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
			// The error must name the field so an operator knows which key to fix.
			if !strings.Contains(err.Error(), "branding.share_image") {
				t.Errorf("error = %q, want it to name branding.share_image", err)
			}
			if b.BrandShareImagePath != "" {
				t.Errorf("BrandShareImagePath = %q, want it unset on failure", b.BrandShareImagePath)
			}
		})
	}
}

// Logo-legal but unfurl-illegal types: no scraper renders an SVG/ICO unfurl
// and the web proxy would silently redirect one to fleet's generic card, so a
// share_image with a logo-only extension must fail at load — even when the
// file exists and would pass every shared path check.
func TestResolveBrandShareImage_RejectsLogoOnlyExtensions(t *testing.T) {
	for _, ext := range []string{".svg", ".ico"} {
		t.Run(ext, func(t *testing.T) {
			b := shareBundle(t, "assets/share"+ext)
			if err := os.WriteFile(filepath.Join(b.Dir, "assets", "share"+ext), []byte("x"), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
			err := b.resolveBrandShareImage()
			if err == nil || !strings.Contains(err.Error(), "unsupported extension") {
				t.Fatalf("err = %v, want an unsupported-extension error", err)
			}
			if b.BrandShareImagePath != "" {
				t.Errorf("BrandShareImagePath = %q, want it unset on failure", b.BrandShareImagePath)
			}
		})
	}
}

// A directory is not servable content.
func TestResolveBrandShareImage_RejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "share.png"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	b := &Bundle{Dir: dir, Branding: Branding{ShareImage: "share.png"}}
	err := b.resolveBrandShareImage()
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("err = %v, want a not-a-regular-file rejection", err)
	}
}

// A symlink inside the bundle pointing outward must not widen the reach: lexical
// checks alone would pass it, which is why containment is re-checked after
// EvalSymlinks.
func TestResolveBrandShareImage_RejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privilege on Windows")
	}
	outside := t.TempDir()
	target := filepath.Join(outside, "secret.png")
	if err := os.WriteFile(target, []byte("\x89PNG"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	dir := t.TempDir()
	link := filepath.Join(dir, "share.png")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	b := &Bundle{Dir: dir, Branding: Branding{ShareImage: "share.png"}}
	err := b.resolveBrandShareImage()
	if err == nil || !strings.Contains(err.Error(), "outside the bundle") {
		t.Fatalf("err = %v, want an outside-the-bundle rejection", err)
	}
}

// The two fields are independent: declaring one must not resolve or clobber the
// other.
func TestBrandImages_AreIndependent(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, n := range []string{"mark.svg", "share.png"} {
		if err := os.WriteFile(filepath.Join(dir, "assets", n), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}

	only := &Bundle{Dir: dir, Branding: Branding{Logo: "assets/mark.svg"}}
	if err := only.resolveBrandLogo(); err != nil {
		t.Fatalf("logo: %v", err)
	}
	if err := only.resolveBrandShareImage(); err != nil {
		t.Fatalf("share_image absent: %v", err)
	}
	if only.BrandLogoPath == "" {
		t.Error("logo not resolved")
	}
	if only.BrandShareImagePath != "" {
		t.Errorf("BrandShareImagePath = %q, want empty when only logo is declared", only.BrandShareImagePath)
	}

	both := &Bundle{Dir: dir, Branding: Branding{Logo: "assets/mark.svg", ShareImage: "assets/share.png"}}
	if err := both.resolveBrandLogo(); err != nil {
		t.Fatalf("logo: %v", err)
	}
	if err := both.resolveBrandShareImage(); err != nil {
		t.Fatalf("share_image: %v", err)
	}
	if both.BrandLogoPath == both.BrandShareImagePath {
		t.Errorf("both paths resolved to %q — the fields are crossed", both.BrandLogoPath)
	}
}
