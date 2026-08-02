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

// logoBundle builds a Bundle rooted at a temp dir containing `assets/mark.svg`,
// with Branding.Logo set to rel.
func logoBundle(t *testing.T, rel string) *Bundle {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "mark.svg"), []byte("<svg/>"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return &Bundle{Dir: dir, Branding: Branding{Logo: rel}}
}

func TestResolveBrandLogo_AcceptsBundleRelativeFile(t *testing.T) {
	b := logoBundle(t, "assets/mark.svg")
	if err := b.resolveBrandLogo(); err != nil {
		t.Fatalf("valid logo rejected: %v", err)
	}
	if b.BrandLogoPath == "" {
		t.Fatal("BrandLogoPath not recorded — the HTTP layer would have nothing to serve")
	}
	if !strings.HasSuffix(b.BrandLogoPath, filepath.Join("assets", "mark.svg")) {
		t.Errorf("BrandLogoPath = %q, want it to end at the declared file", b.BrandLogoPath)
	}
}

// Absent is the normal case (fleet's own mark stands) and must not error.
func TestResolveBrandLogo_EmptyIsValid(t *testing.T) {
	b := &Bundle{Dir: t.TempDir()}
	if err := b.resolveBrandLogo(); err != nil {
		t.Fatalf("absent logo: %v", err)
	}
	if b.BrandLogoPath != "" {
		t.Errorf("BrandLogoPath = %q, want empty", b.BrandLogoPath)
	}
	// Whitespace-only is the same as absent, not a path of blanks.
	b = &Bundle{Dir: t.TempDir(), Branding: Branding{Logo: "   "}}
	if err := b.resolveBrandLogo(); err != nil || b.BrandLogoPath != "" || b.Branding.Logo != "" {
		t.Errorf("blank logo: err=%v path=%q logo=%q", err, b.BrandLogoPath, b.Branding.Logo)
	}
}

// Every rejection here would otherwise be a silent, permanent defect: a path
// outside the bundle served under an image content type on every page load, or
// a broken mark on every page of a deployment that believes it is branded.
func TestResolveBrandLogo_Rejects(t *testing.T) {
	cases := []struct {
		name string
		rel  string
		want string
	}{
		{"absolute path", "/etc/passwd", "bundle-relative"},
		{"parent escape", "../../etc/hosts", "bundle-relative"},
		{"nested escape", "assets/../../secrets.svg", "bundle-relative"},
		{"unsupported extension", "assets/mark.txt", "unsupported extension"},
		{"no extension", "assets/mark", "unsupported extension"},
		{"missing file", "assets/nope.svg", "no such file"},
		{"directory", "assets", "unsupported extension"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := logoBundle(t, tc.rel)
			err := b.resolveBrandLogo()
			if err == nil {
				t.Fatalf("accepted %q", tc.rel)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q must mention %q", err, tc.want)
			}
			if b.BrandLogoPath != "" {
				t.Errorf("BrandLogoPath set to %q on a rejected logo", b.BrandLogoPath)
			}
		})
	}
}

// A path that is lexically local can still escape through a symlink, so
// containment is re-checked after resolution.
func TestResolveBrandLogo_RejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privilege on Windows")
	}
	outside := filepath.Join(t.TempDir(), "outside.svg")
	if err := os.WriteFile(outside, []byte("<svg/>"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	b := logoBundle(t, "assets/link.svg")
	if err := os.Symlink(outside, filepath.Join(b.Dir, "assets", "link.svg")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	err := b.resolveBrandLogo()
	if err == nil {
		t.Fatal("accepted a symlink pointing outside the bundle")
	}
	if !strings.Contains(err.Error(), "outside the bundle") {
		t.Errorf("error %q must say the path escapes", err)
	}
}

// A symlink that stays inside the bundle is legitimate (a bundle may keep one
// canonical asset and link to it).
func TestResolveBrandLogo_AllowsInternalSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privilege on Windows")
	}
	b := logoBundle(t, "assets/link.svg")
	if err := os.Symlink(filepath.Join(b.Dir, "assets", "mark.svg"), filepath.Join(b.Dir, "assets", "link.svg")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := b.resolveBrandLogo(); err != nil {
		t.Fatalf("internal symlink rejected: %v", err)
	}
}

// The validator and the HTTP layer must agree: anything that passes validation
// has a content type the route can serve, or the rail renders a broken image.
func TestBrandLogoContentType_MatchesValidatedExtensions(t *testing.T) {
	for _, ext := range BrandLogoExtensions() {
		if BrandLogoContentType("mark"+ext) == "" {
			t.Errorf("extension %s is advertised as supported but has no content type", ext)
		}
		if BrandLogoContentType("MARK"+strings.ToUpper(ext)) == "" {
			t.Errorf("extension %s must match case-insensitively", ext)
		}
	}
	if BrandLogoContentType("mark.html") != "" {
		t.Error("html must not be servable — the route would become a file server for markup")
	}
	if BrandLogoContentType("mark") != "" {
		t.Error("an extensionless path must not resolve to a content type")
	}
}
