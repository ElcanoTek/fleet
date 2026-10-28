package tools

import (
	"html"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
)

// Interstitial handling for download_url: an email click-tracker or a file
// host often answers a download link with a small HTML page that moves on to
// the file itself, by a meta refresh or a single download link. These helpers
// find that next URL; download_url fetches it through the same SSRF-guarded
// client.

var (
	metaRefreshRE    = regexp.MustCompile(`(?is)<meta[^>]+http-equiv\s*=\s*["']?refresh["']?[^>]*content\s*=\s*["']\s*\d*\s*;?\s*url\s*=\s*['"]?([^"'>\s]+)`)
	metaRefreshAltRE = regexp.MustCompile(`(?is)<meta[^>]+content\s*=\s*["']\s*\d*\s*;?\s*url\s*=\s*['"]?([^"'>\s]+)[^>]*http-equiv\s*=\s*["']?refresh`)
	anchorHrefRE     = regexp.MustCompile(`(?is)<a\b[^>]*?\bhref\s*=\s*["']([^"']+)["']`)
)

// dataFileExts are extensions of the report and data files a download link
// usually names.
var dataFileExts = map[string]bool{
	".csv": true, ".tsv": true, ".txt": true, ".json": true, ".xml": true,
	".xlsx": true, ".xls": true, ".xlsm": true, ".parquet": true,
	".zip": true, ".gz": true, ".tgz": true, ".bz2": true, ".7z": true,
	".pdf": true, ".docx": true, ".pptx": true,
}

func isHTMLResponse(resp *http.Response) bool {
	mt, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return err == nil && (mt == "text/html" || mt == "application/xhtml+xml")
}

// looksLikeDataFile reports whether a URL path names a data or report file.
func looksLikeDataFile(p string) bool {
	return dataFileExts[strings.ToLower(path.Ext(p))]
}

// interstitialTarget returns the URL an interstitial page moves on to: its
// meta refresh, else its only link to a data file. "" when the page names no
// single file (an ordinary page, a login form, a list of several downloads).
func interstitialTarget(page []byte, base *url.URL) string {
	text := string(page)
	for _, re := range []*regexp.Regexp{metaRefreshRE, metaRefreshAltRE} {
		if m := re.FindStringSubmatch(text); m != nil {
			if u := resolveHTTP(base, m[1]); u != "" {
				return u
			}
		}
	}
	found := ""
	for _, m := range anchorHrefRE.FindAllStringSubmatch(text, -1) {
		u := resolveHTTP(base, m[1])
		if u == "" {
			continue
		}
		parsed, err := url.Parse(u)
		if err != nil || !looksLikeDataFile(parsed.Path) {
			continue
		}
		if found != "" && found != u {
			return "" // several files: not an interstitial, let the agent choose
		}
		found = u
	}
	return found
}

// resolveHTTP resolves an HTML attribute value against base and keeps it only
// if it is an http(s) URL.
func resolveHTTP(base *url.URL, ref string) string {
	ref = strings.TrimSpace(html.UnescapeString(ref))
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return u.String()
}
