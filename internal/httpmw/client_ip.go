// Copied from github.com/go-chi/chi/v5 v5.3.2, middleware/client_ip.go (modified: only ClientIPFromXFF and its readers kept).
// Copyright (c) 2015-present Peter Kieltyka, Google Inc.; see LICENSE.
// SPDX-License-Identifier: MIT

package httpmw

import (
	"context"
	"net/http"
	"net/netip"
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
// Calling with no arguments returns the rightmost XFF entry, or no IP if
// that entry doesn't parse (fail-closed) — safe only if you have exactly
// one trusted hop directly in front of this server (e.g., nginx on localhost).
//
// v4-mapped IPv6 (::ffff:a.b.c.d) folds to plain v4 and IPv6 zones are
// stripped before the prefix check and storage; otherwise an attacker
// could use either notation to alias a trusted IP past the check.

// Panics at startup if any prefix is invalid.
func ClientIPFromXFF(trustedIPPrefixes ...string) func(http.Handler) http.Handler {
	prefixes := make([]netip.Prefix, len(trustedIPPrefixes))
	for i, p := range trustedIPPrefixes {
		prefixes[i] = netip.MustParsePrefix(p)
	}
	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var found netip.Addr
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
			if found.IsValid() {
				r = r.WithContext(context.WithValue(r.Context(), clientIPCtxKey, found))
			}
			h.ServeHTTP(w, r)
		})
	}
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
	for hi := len(headers) - 1; hi >= 0; hi-- {
		h := headers[hi]
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
func parseHeaderAddr(s string) (netip.Addr, bool) {
	ip, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, false
	}
	return ip.Unmap().WithZone(""), true
}
