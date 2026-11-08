// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/safe"
)

// verifyOrigin is the CSRF defense for every mutating browser route (ported
// from web/src/app/lib/csrf.ts; the cases in csrf_test.go are that file's).
//
// SameSite=Lax on the session cookie blocks most cross-site POSTs but permits
// top-level form submits, so a mutating request must carry an Origin that
// names this deployment. Accepted shapes, in order:
//
//  1. The canonical origin's host (PublicOrigin) when configured.
//  2. A loopback Origin (localhost / 127.0.0.1 / [::1], any port) that exactly
//     matches the CONNECTION's own Host header — the SSH-tunnel shape, which a
//     canonical-only check would 403 on every POST including login. Safe
//     because a victim's browser talking to the real deployment sends the
//     deployment's Host, never loopback; X-Forwarded-Host is deliberately
//     ignored here.
//  3. With no canonical origin (local dev): X-Forwarded-Host, then Host.
//
// Missing or malformed Origin is rejected. It returns false after writing the
// 403 {"error":"forbidden"} the Next tier answered with (no reason leaked; the
// reason is logged).
func (g *Gateway) verifyOrigin(w http.ResponseWriter, r *http.Request) bool {
	reason := g.originRejection(r)
	if reason == "" {
		return true
	}
	logging.From(r.Context()).Warn("webui: csrf rejected", "reason", safe.LogSafe(reason))
	writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
	return false
}

func (g *Gateway) originRejection(r *http.Request) string {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return "missing Origin header"
	}
	var canonicalHost string
	if g.cfg.PublicOrigin != nil {
		canonicalHost = g.cfg.PublicOrigin.Host
	}
	expected := canonicalHost
	if expected == "" {
		expected = requestHost(r)
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "malformed Origin header"
	}
	if u.Host == expected {
		return ""
	}
	if canonicalHost != "" && u.Host == r.Host && isLoopbackHost(r.Host) {
		return ""
	}
	return "origin " + u.Host + " does not match expected host " + expected
}

// isLoopbackHost reports whether host[:port] names the loopback interface in
// one of the exact shapes a tunneled browser produces. Not a DNS lookup and
// not a suffix match: localhost.evil.example must not qualify.
func isLoopbackHost(host string) bool {
	hostname := host
	if strings.HasPrefix(host, "[") {
		if i := strings.IndexByte(host, ']'); i >= 0 {
			hostname = host[:i+1]
		}
	} else if i := strings.IndexByte(host, ':'); i >= 0 {
		hostname = host[:i]
	}
	return hostname == "localhost" || hostname == "127.0.0.1" || hostname == "[::1]"
}
