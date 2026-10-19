// Copied from github.com/go-chi/chi/v5 v5.4.0, middleware/client_ip.go (modified: only ClientIPFromXFF and its readers kept; its per-request walk is lifted into xffClientIP so trusted_proxies.go shares it).
// Copyright (c) 2015-present Peter Kieltyka, Google Inc.; see LICENSE.
// SPDX-License-Identifier: MIT

package httpmw

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
)

// clientIPCtxKey stores the client IP set by ClientIPFromXFF.
var clientIPCtxKey = &contextKey{"clientIP"}

// contextKey is a pointer-typed context key (from chi's middleware.go).
type contextKey struct{ name string }

func (k *contextKey) String() string { return "httpmw context value " + k.name }

// xForwardedForHeader is the canonical form of the X-Forwarded-For header
// name, used by the XFF-based middlewares.
const xForwardedForHeader = "X-Forwarded-For"

// ClientIPFromXFF stores the client IP read from the X-Forwarded-For header,
// walking the chain right-to-left and skipping any IP that falls within one
// of the given trusted CIDR prefixes. The first IP that is not trusted is
// the client. Read it with [GetClientIP].
//
// An unparseable entry mid-chain aborts the walk and leaves no client IP
// set (fail-closed) — we can't safely trust anything left of garbage.
//
// Use this when you sit behind one or more reverse proxies whose IP ranges
// you can enumerate as CIDRs. Most CDNs publish their IPs:
//
//	Cloudflare:   https://www.cloudflare.com/ips/
//	AWS:          https://ip-ranges.amazonaws.com/ip-ranges.json
//	Fastly:       https://api.fastly.com/public-ip-list
//	Google Cloud: https://www.gstatic.com/ipranges/cloud.json
//
// Example (CloudFront):
//
//	r.Use(httpmw.ClientIPFromXFF(
//	    "13.32.0.0/15",   // CloudFront IPv4
//	    "52.46.0.0/18",   // CloudFront IPv4
//	    "2600:9000::/28", // CloudFront IPv6
//	))
//
// The TCP peer in r.RemoteAddr is treated as the rightmost hop: if it is
// not within a trusted prefix, it is the client IP and X-Forwarded-For is
// ignored. A client reaching this server directly therefore cannot spoof
// its IP. If r.RemoteAddr doesn't parse, no client IP is set (fail-closed).
//
// At least one prefix is required. For a single trusted hop directly in
// front of this server (e.g., nginx on localhost), pass its address:
//
//	r.Use(httpmw.ClientIPFromXFF("127.0.0.1/32", "::1/128"))
//
// v4-mapped IPv6 (::ffff:a.b.c.d) folds to plain v4 and IPv6 zones are
// stripped before the prefix check and storage; otherwise an attacker
// could use either notation to alias a trusted IP past the check.
//
// Panics at startup if no prefix is given or any prefix is invalid.
func ClientIPFromXFF(trustedIPPrefixes ...string) func(http.Handler) http.Handler {
	if len(trustedIPPrefixes) == 0 {
		panic("httpmw.ClientIPFromXFF: at least one trusted IP prefix is required")
	}
	prefixes := mustParsePrefixes(trustedIPPrefixes)
	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if found := xffClientIP(r, prefixes); found.IsValid() {
				r = r.WithContext(context.WithValue(r.Context(), clientIPCtxKey, found))
			}
			h.ServeHTTP(w, r)
		})
	}
}

// xffClientIP is ClientIPFromXFF's per-request resolution (upstream inlines
// it in the handler; fleet lifts it out so TrustedProxies.ClientIP runs the
// very same walk): the untrusted TCP peer, else the first untrusted
// X-Forwarded-For hop from the right, else the zero Addr (an unparseable
// RemoteAddr or entry, or a chain of only trusted hops).
func xffClientIP(r *http.Request, prefixes []netip.Prefix) netip.Addr {
	var found netip.Addr
	if peer, untrusted := untrustedPeer(r, prefixes); untrusted {
		found = peer
	} else {
		walkXFF(r.Header[xForwardedForHeader], func(v string) bool {
			ip, ok := parseHeaderAddr(v)
			if !ok {
				return true // fail-closed; leave found unset
			}
			if inAnyPrefix(ip, prefixes) {
				return false // trusted hop; keep walking left
			}
			found = ip
			return true
		})
	}
	return found
}

// GetClientIP returns the client IP as a string, as set by
// ClientIPFromXFF. Returns "" if no valid IP was set.
// Convenient for logging, rate-limit keys, etc.
func GetClientIP(ctx context.Context) string {
	ip := GetClientIPAddr(ctx)
	if !ip.IsValid() {
		return ""
	}
	return ip.String()
}

// GetClientIPAddr returns the client IP as a [netip.Addr], as set by
// ClientIPFromXFF. The returned Addr is the zero value if not
// set; use [netip.Addr.IsValid] to check. Useful when you need typed work —
// prefix containment, Is4/Is6, etc. — without re-parsing the string.
func GetClientIPAddr(ctx context.Context) netip.Addr {
	ip, _ := ctx.Value(clientIPCtxKey).(netip.Addr)
	return ip
}

// walkXFF walks the entries of the merged X-Forwarded-For chain
// RIGHT-TO-LEFT, invoking visit on each trimmed non-empty entry. visit
// returns true to stop the walk. Lazy walk, zero allocations (entries
// are substrings of the input headers).
//
// Multiple XFF headers are merged per RFC 2616 — each header's
// comma-separated entries in order received — so an attacker cannot pick
// which value security logic sees by sending a duplicate header.
func walkXFF(headers []string, visit func(entry string) bool) {
	for _, h := range slices.Backward(headers) {
		for h != "" {
			var v string
			if i := strings.LastIndexByte(h, ','); i >= 0 {
				v, h = h[i+1:], h[:i]
			} else {
				v, h = h, ""
			}
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			if visit(v) {
				return
			}
		}
	}
}

// mustParsePrefixes parses CIDR prefixes, panicking on the first invalid one.
func mustParsePrefixes(cidrs []string) []netip.Prefix {
	prefixes := make([]netip.Prefix, len(cidrs))
	for i, p := range cidrs {
		prefixes[i] = netip.MustParsePrefix(p)
	}
	return prefixes
}

// untrustedPeer reports whether the TCP peer in r.RemoteAddr is outside
// prefixes. If so, peer is the client IP, or invalid if RemoteAddr doesn't
// parse. With no prefixes, every peer is trusted.
func untrustedPeer(r *http.Request, prefixes []netip.Prefix) (peer netip.Addr, untrusted bool) {
	if len(prefixes) == 0 {
		return netip.Addr{}, false
	}
	peer, ok := parseRemoteAddr(r.RemoteAddr)
	if !ok {
		return netip.Addr{}, true
	}
	// Zoned addresses never match a prefix, so check a link-local peer without its zone.
	return peer, !inAnyPrefix(peer.WithZone(""), prefixes)
}

// parseRemoteAddr parses the host of r.RemoteAddr. v4-mapped IPv6 folds to
// plain v4; the IPv6 zone is preserved (link-local connections may
// legitimately have one).
func parseRemoteAddr(remoteAddr string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr // RemoteAddr may already be a bare IP (e.g. in tests).
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}

// inAnyPrefix reports whether ip falls within any of the given prefixes.
func inAnyPrefix(ip netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// parseHeaderAddr parses s and normalizes for storage: v4-mapped IPv6
// (::ffff:a.b.c.d) folds to plain v4, IPv6 zone is stripped. Both defend the
// trust-prefix check against attacker-injected aliases — [netip.Prefix.Contains]
// returns false for v4-mapped addresses vs v4 prefixes and for any zoned
// address, so without folding/stripping an attacker could escape an
// otherwise valid trust list.
//
// Header-sourced IPs only. r.RemoteAddr goes through parseRemoteAddr
// (Unmap, but zone preserved for legitimate link-local connections).
func parseHeaderAddr(s string) (netip.Addr, bool) {
	ip, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return ip.Unmap().WithZone(""), true
}
