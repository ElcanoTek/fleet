package httpapi

import "testing"

func TestIsActiveContentTypeCoversEveryXMLType(t *testing.T) {
	for ct, want := range map[string]bool{
		"text/html; charset=utf-8": true,
		"image/svg+xml":            true,
		"application/xhtml+xml":    true,
		"text/xml":                 true,
		"application/xml":          true,
		"application/rss+xml":      true,
		"application/atom+xml":     true,
		"application/xslt+xml":     true,
		"text/xsl":                 true,
		"TEXT/HTML":                true,
		"application/json":         false,
		"text/plain":               false,
		"image/png":                false,
		"":                         false,
	} {
		if got := isActiveContentType(ct); got != want {
			t.Errorf("isActiveContentType(%q) = %v, want %v", ct, got, want)
		}
	}
}
