package tools

import (
	"bytes"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// Interstitial handling for download_url: an email click-tracker or a file
// host often answers a download link with a small HTML page that moves on to
// the file itself, by a meta refresh or a single download link. These helpers
// find that next URL; download_url fetches it through the same SSRF-guarded
// client.

// refreshURLRE pulls the URL out of a meta refresh's content attribute
// ("5; url=https://…", quotes optional).
var refreshURLRE = regexp.MustCompile(`(?i)^\s*\d*\s*[;,]?\s*url\s*=\s*['"]?([^'"\s]+)`)

// dataFileExts are extensions of the report, data and attachment files a
// download link usually names.
var dataFileExts = map[string]bool{
	".csv": true, ".tsv": true, ".txt": true, ".json": true, ".xml": true,
	".xlsx": true, ".xls": true, ".xlsm": true, ".parquet": true,
	".zip": true, ".gz": true, ".tgz": true, ".bz2": true, ".7z": true,
	".pdf": true, ".doc": true, ".docx": true, ".ppt": true, ".pptx": true,
	".rtf": true, ".odt": true, ".ods": true, ".odp": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".tif": true, ".tiff": true,
	".eml": true, ".msg": true, ".ics": true,
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
// The page is tokenized, not pattern-matched, so markup inside a comment, a
// script or an attribute value is never followed.
func interstitialTarget(page []byte, base *url.URL) string {
	z := html.NewTokenizer(bytes.NewReader(page))
	refresh, found := "", ""
	several := false
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			continue
		}
		tok := z.Token()
		switch tok.DataAtom {
		case atom.Meta:
			if refresh == "" && strings.EqualFold(attr(tok, "http-equiv"), "refresh") {
				if m := refreshURLRE.FindStringSubmatch(attr(tok, "content")); m != nil {
					refresh = resolveHTTP(base, m[1])
				}
			}
		case atom.A:
			u := resolveHTTP(base, attr(tok, "href"))
			if u == "" {
				continue
			}
			parsed, err := url.Parse(u)
			if err != nil || !looksLikeDataFile(parsed.Path) {
				continue
			}
			if found != "" && found != u {
				several = true // several files: not an interstitial, let the agent choose
			}
			found = u
		default:
			// Only meta refreshes and links move an interstitial on.
		}
	}
	if refresh != "" {
		return refresh
	}
	if several {
		return ""
	}
	return found
}

func attr(tok html.Token, name string) string {
	for _, a := range tok.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}

// resolveHTTP resolves an HTML attribute value against base and keeps it only
// if it is an http(s) URL.
func resolveHTTP(base *url.URL, ref string) string {
	ref = strings.TrimSpace(ref) // the tokenizer already decoded entities
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
