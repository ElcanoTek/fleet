package clientconfig

import (
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// resolveBranding applies branding defaults and validates branding assets and
// optional support contact configuration.
func (b *Bundle) resolveBranding() error {
	applyBrandingDefaults(&b.Branding)
	if err := b.resolveBrandLogo(); err != nil {
		return err
	}
	if err := b.resolveBrandShareImage(); err != nil {
		return err
	}
	return b.resolveBrandSupport()
}

// applyBrandingDefaults fills neutral generic strings for any unset branding
// field so a sparse manifest still renders a coherent UI.
func applyBrandingDefaults(br *Branding) {
	if br.AppName == "" {
		br.AppName = "Fleet"
	}
	if br.LoginTitle == "" {
		br.LoginTitle = "Welcome aboard."
	}
	if br.LoginTagline == "" {
		br.LoginTagline = "Sign in to your workspace and pick up where you left off."
	}
	if br.ShareTitle == "" {
		br.ShareTitle = br.AppName
	}
	if br.ShareDescription == "" {
		br.ShareDescription = "An AI workspace with real tool use."
	}
}

// brandLogoContentTypes maps the image extensions a bundle logo may use to the
// content type /brand/logo serves it as. An allowlist rather than
// mime.TypeByExtension: the response type is what makes the browser render the
// bytes, so the set stays deliberately small and image-only — a bundle cannot
// turn the logo route into a general file server for HTML or scripts by naming
// a different extension.
var brandLogoContentTypes = map[string]string{
	".svg":  "image/svg+xml",
	".png":  "image/png",
	".webp": "image/webp",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".ico":  "image/x-icon",
}

// BrandLogoContentType returns the content type fleet serves a bundle logo
// under, or "" when the extension is not one it knows. The HTTP layer and the
// load-time validator share this one map so a bundle can never pass validation
// with a path the route would then refuse to serve.
func BrandLogoContentType(p string) string {
	return brandLogoContentTypes[strings.ToLower(filepath.Ext(p))]
}

// BrandLogoExtensions returns the supported logo extensions, sorted, for error
// messages and docs.
func BrandLogoExtensions() []string {
	out := make([]string, 0, len(brandLogoContentTypes))
	for ext := range brandLogoContentTypes {
		out = append(out, ext)
	}
	slices.Sort(out)
	return out
}

// resolveBrandLogo validates branding.logo and records the absolute file the
// HTTP layer serves. Empty is the norm (fleet's own mark stands) and is not an
// error.
//
// The path arrives from a manifest, so it is operator-authored rather than user
// input — but it is still resolved defensively, because the failure modes are
// silent and permanent: a "../../etc/passwd" would otherwise be served under an
// image content type on every page load, and a path that merely doesn't exist
// would render a broken mark on every page of a deployment that believes it is
// branded. Both fail at startup instead.
//
// filepath.IsLocal rejects an absolute path and any ".." escape lexically;
// EvalSymlinks then re-checks containment, so a symlink inside the bundle
// pointing outward cannot widen the reach either.
func (b *Bundle) resolveBrandLogo() error {
	rel := strings.TrimSpace(b.Branding.Logo)
	b.Branding.Logo = rel
	resolved, err := b.resolveBrandImage("branding.logo", rel)
	if err != nil {
		return err
	}
	b.BrandLogoPath = resolved
	return nil
}

// resolveBrandShareImage validates branding.share_image the same way, and
// records the absolute file /brand/share-image serves. Empty is the norm
// (fleet's own generic share card stands) and is not an error.
//
// This field exists because the OG image was the last un-themable brand surface:
// a checked-in web/public/share.png was the og:image for EVERY deployment, so
// pasting a link to a white-labeled instance into Slack, iMessage, Discord or
// Teams unfurled with another company's logo — served from the client's own
// domain, so nothing looked amiss to the unfurler.
func (b *Bundle) resolveBrandShareImage() error {
	rel := strings.TrimSpace(b.Branding.ShareImage)
	b.Branding.ShareImage = rel
	resolved, err := b.resolveBrandImage("branding.share_image", rel)
	if err != nil {
		return err
	}
	// Unlike the logo, the share image is only ever consumed by link-unfurl
	// scrapers, and none of them render SVG (or ICO) unfurls — the web proxy
	// passes through PNG/WebP/JPEG only and redirects anything else to fleet's
	// generic card. Rejecting the logo-only extensions HERE (after the shared
	// path/containment checks, so their errors keep precedence) keeps the
	// branding contract's promise that a bad value fails at startup instead of
	// every unfurl silently showing the wrong brand.
	if rel != "" {
		switch strings.ToLower(filepath.Ext(rel)) {
		case ".png", ".webp", ".jpg", ".jpeg":
		default:
			return fmt.Errorf("branding.share_image %q: unsupported extension (unfurl scrapers only render .png, .webp, .jpg, .jpeg)", rel)
		}
	}
	b.BrandShareImagePath = resolved
	return nil
}

// resolveBrandSupport validates and normalizes branding.support when declared.
// An omitted or all-blank block resolves to nil (no support button rendered).
func (b *Bundle) resolveBrandSupport() error {
	if b.Branding.Support == nil {
		return nil
	}
	sc := b.Branding.Support
	sc.Label = strings.TrimSpace(sc.Label)
	sc.MobileLabel = strings.TrimSpace(sc.MobileLabel)
	sc.Email = strings.TrimSpace(sc.Email)
	sc.Title = strings.TrimSpace(sc.Title)
	sc.Message = strings.TrimSpace(sc.Message)
	sc.Subject = strings.TrimSpace(sc.Subject)
	if sc.Label == "" && sc.MobileLabel == "" && sc.Email == "" && sc.Title == "" && sc.Message == "" && sc.Subject == "" {
		b.Branding.Support = nil
		return nil
	}
	if sc.Label == "" {
		return fmt.Errorf("branding.support: label is required")
	}
	if sc.Email == "" {
		return fmt.Errorf("branding.support: email is required")
	}
	if strings.ContainsAny(sc.Email, " \t\r\n?&#/:%") {
		return fmt.Errorf("branding.support.email %q: must be a bare email address", sc.Email)
	}
	addr, err := mail.ParseAddress(sc.Email)
	if err != nil || addr.Address != sc.Email {
		return fmt.Errorf("branding.support.email %q: invalid email address", sc.Email)
	}
	if sc.Title == "" {
		sc.Title = sc.Label
	}
	return nil
}

// resolveBrandImage is the shared validator for every bundle-relative image path
// in branding:. field names it for error messages ("branding.logo"). An empty
// rel returns ("", nil) — declaring no image is the norm, not an error.
//
// The path arrives from a manifest, so it is operator-authored rather than user
// input — but it is still resolved defensively, because the failure modes are
// silent and permanent: a "../../etc/passwd" would otherwise be served under an
// image content type on every page load, and a path that merely doesn't exist
// would render a broken image on every page of a deployment that believes it is
// branded. Both fail at startup instead.
//
// filepath.IsLocal rejects an absolute path and any ".." escape lexically;
// EvalSymlinks then re-checks containment, so a symlink inside the bundle
// pointing outward cannot widen the reach either.
func (b *Bundle) resolveBrandImage(field, rel string) (string, error) {
	if rel == "" {
		return "", nil
	}
	if !filepath.IsLocal(rel) {
		return "", fmt.Errorf("%s %q: must be a bundle-relative path (no absolute path, no \"..\")", field, rel)
	}
	if BrandLogoContentType(rel) == "" {
		return "", fmt.Errorf("%s %q: unsupported extension (want one of %s)", field, rel, strings.Join(BrandLogoExtensions(), ", "))
	}
	root, err := filepath.EvalSymlinks(b.Dir)
	if err != nil {
		return "", fmt.Errorf("%s %q: resolve bundle dir: %w", field, rel, err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(b.Dir, rel))
	if err != nil {
		return "", fmt.Errorf("%s %q: %w", field, rel, err)
	}
	if resolved != root && !strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("%s %q: resolves outside the bundle (%s)", field, rel, resolved)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("%s %q: %w", field, rel, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s %q: not a regular file", field, rel)
	}
	return resolved, nil
}

// DefaultBranding returns the neutral generic branding strings fleet renders
// when no client bundle is wired. It is the SAME source of truth
// applyBrandingDefaults uses for a sparse manifest, so the no-bundle and
// sparse-bundle UIs match (one source of truth rather than a hardcoded literal
// in the HTTP layer).
func DefaultBranding() Branding {
	var b Branding
	applyBrandingDefaults(&b)
	return b
}
