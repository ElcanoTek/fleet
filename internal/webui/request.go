// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// requestProto is the scheme the browser used: X-Forwarded-Proto (Caddy sets
// it) or the connection's own.
func requestProto(r *http.Request) string {
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		return p
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// requestHost is X-Forwarded-Host, then Host.
func requestHost(r *http.Request) string {
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		return h
	}
	return r.Host
}

// redirectURL resolves pathAndQuery against the configured canonical origin,
// falling back to the forwarded headers only when none is configured (local
// dev). X-Forwarded-* is client-supplied unless a proxy overwrites it, so it
// must never pick the redirect host on a deployment that knows its origin.
func (g *Gateway) redirectURL(r *http.Request, pathAndQuery string) string {
	base := g.cfg.PublicOrigin
	if base == nil {
		base = &url.URL{Scheme: requestProto(r), Host: requestHost(r)}
	}
	ref, err := url.Parse(pathAndQuery)
	if err != nil {
		return pathAndQuery
	}
	return base.ResolveReference(ref).String()
}

// secureRequest decides the Secure cookie attribute: the configured origin's
// scheme when there is one, else the forwarded/connection scheme.
func (g *Gateway) secureRequest(r *http.Request) bool {
	if g.cfg.PublicOrigin != nil {
		return g.cfg.PublicOrigin.Scheme == "https"
	}
	return requestProto(r) == "https"
}

// writeJSON writes v with status. JSON-only responses from the gateway itself
// (401s, 502s, the CSRF 403) all go through here.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func unauthorized(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Unauthorized"})
}

// seeOther is NextResponse.redirect(url, {status: 303}).
func seeOther(w http.ResponseWriter, location string) {
	w.Header().Set("Location", location)
	w.WriteHeader(http.StatusSeeOther)
}

// temporaryRedirect is the request gate's NextResponse.redirect default (307).
func temporaryRedirect(w http.ResponseWriter, location string) {
	w.Header().Set("Location", location)
	w.WriteHeader(http.StatusTemporaryRedirect)
}

// encodeURIComponent matches JavaScript's encodeURIComponent, which the route
// handlers used to build every upstream path segment and query value: it
// leaves A–Z a–z 0–9 - _ . ! ~ * ' ( ) unescaped and percent-encodes the rest
// as UTF-8.
func encodeURIComponent(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&15])
	}
	return b.String()
}

// setCookie appends a Set-Cookie header in the attribute order the Next tier
// emitted (Path, Max-Age, HttpOnly, SameSite, Secure).
func setCookie(w http.ResponseWriter, name, value string, maxAge int64, secure bool, domain string) {
	var b strings.Builder
	b.WriteString(name)
	b.WriteByte('=')
	b.WriteString(value)
	if domain != "" {
		b.WriteString("; Domain=")
		b.WriteString(domain)
	}
	b.WriteString("; Path=/; Max-Age=")
	b.WriteString(strconv.FormatInt(maxAge, 10))
	b.WriteString("; HttpOnly; SameSite=Lax")
	if secure {
		b.WriteString("; Secure")
	}
	w.Header().Add("Set-Cookie", b.String())
}

func cookieValue(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}
