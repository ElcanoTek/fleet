package httpapi

import (
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/httpmw"
	"github.com/ElcanoTek/fleet/internal/metrics"
)

// IP access control (#314).
//
// A config-driven, network-level allow/deny filter applied as defense-in-depth
// IN FRONT OF the shared-token auth — it does NOT replace it. The order of the
// checks below encodes the precedence rule the operator relies on:
//
//	denylist  → deny overrides everything (always blocked)
//	allowlist → if set, the client IP must match (else blocked); empty = allow all
//
// This is deliberately the OUTERMOST application-layer filter (it sits just
// inside recoverMiddleware, before body parsing in Routes) so a blocked request
// is dropped before any handler, body read, or auth comparison runs. /healthz is
// exempt so load-balancer / uptime probes from outside the allowlist keep
// working.
//
// X-Forwarded-For is honored ONLY when the immediate TCP peer is a configured
// trusted proxy (FLEET_TRUSTED_PROXIES). The chain is then walked from the
// RIGHT, skipping hops that are themselves trusted proxies, and the first
// untrusted address is the client. A chain of only trusted addresses, or one
// with an unparseable entry (the walk stops there: nothing left of garbage is
// trusted), falls back to the TCP peer. With no trusted proxies configured
// the header is never read, so an untrusted client cannot spoof an
// allowlisted source by setting X-Forwarded-For itself. (The leftmost-entry
// convention is attacker-controlled: a client sends `X-Forwarded-For:
// <spoof>` and a correctly-appending proxy forwards `<spoof>, <real>` — see
// #1111.) The resolution is httpmw.TrustedProxies — the same code the
// orchestrator API keys its rate limits on — not a copy of it.

// logIPFilter emits the active filter state once at construction so an operator
// can confirm their config loaded. Silent when neither list is set — silence
// means the default open behavior, so there is nothing to surface.
func logIPFilter(cfg ipFilterConfig) {
	if len(cfg.allow) == 0 && len(cfg.deny) == 0 {
		return
	}
	log.Printf("IP allowlist: %s. IP denylist: %s.", formatCIDRs(cfg.allow), formatCIDRs(cfg.deny))
}

// ipFilterConfig is the slice of *config.Config the filter needs. Reading it off
// a small struct (rather than the whole Config) keeps the filter self-contained
// and trivially testable.
type ipFilterConfig struct {
	allow          []*net.IPNet
	deny           []*net.IPNet
	trustedProxies []net.IP
}

// ipFilterMiddleware drops requests whose client IP is denied (#314). It is a
// no-op when no allow/deny lists are configured (the default), so default
// behavior is unchanged. /healthz is always exempt.
func (s *Server) ipFilterMiddleware(next http.Handler) http.Handler {
	cfg := ipFilterConfig{}
	if s.cfg != nil {
		cfg.allow = s.cfg.IPAllowlist
		cfg.deny = s.cfg.IPDenylist
		cfg.trustedProxies = s.cfg.TrustedProxies
	}
	// Nothing configured → return next unwrapped so the hot path adds no work.
	if len(cfg.allow) == 0 && len(cfg.deny) == 0 {
		return next
	}
	trust := httpmw.NewTrustedProxies(cfg.trustedProxies)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /healthz is exempt: probes from a load balancer / uptime monitor may
		// originate outside the operator's allowlist.
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		ip := clientIP(r, trust)
		switch {
		case ip == nil:
			// Unparseable RemoteAddr is treated as untrusted: fail closed when an
			// allowlist is in force, otherwise (deny-only config) let it through.
			if len(cfg.allow) > 0 {
				blockIP(w, "allowlist")
				return
			}
		case matchesCIDRList(ip, cfg.deny):
			// Deny overrides allow — checked first.
			blockIP(w, "denylist")
			return
		case len(cfg.allow) > 0 && !matchesCIDRList(ip, cfg.allow):
			blockIP(w, "allowlist")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// blockIP writes the uniform 403. The body is deliberately generic ("Access
// denied"): it does NOT distinguish an allowlist miss from a denylist hit so an
// attacker cannot probe the filter geometry to enumerate allowlisted ranges. The
// reason label is recorded only in the (operator-facing) metric.
func blockIP(w http.ResponseWriter, reason string) {
	metrics.RecordIPBlocked(reason)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte("Access denied\n"))
}

// clientIP resolves the real client IP for filtering through the shared
// resolver (httpmw.TrustedProxies.ClientIP — see the contract above), as a
// net.IP for the CIDR lists. Returns nil when RemoteAddr cannot be parsed,
// which the filter treats as untrusted (fail closed under an allowlist).
func clientIP(r *http.Request, trust httpmw.TrustedProxies) net.IP {
	ip := trust.ClientIP(r)
	if !ip.IsValid() {
		return nil
	}
	return net.IP(ip.AsSlice())
}

// matchesCIDRList reports whether ip falls inside any network in list.
func matchesCIDRList(ip net.IP, list []*net.IPNet) bool {
	for _, n := range list {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// formatCIDRs renders a network list for the startup log, e.g.
// "[192.168.1.0/24, 10.0.0.0/8]" or "[]" when empty.
func formatCIDRs(list []*net.IPNet) string {
	return "[" + strings.Join(cidrStrings(list), ", ") + "]"
}

// cidrStrings renders a network list as a string slice (for JSON surfaces like
// the health summary). Returns an empty (non-nil) slice when list is empty so
// the JSON is [] rather than null.
func cidrStrings(list []*net.IPNet) []string {
	out := make([]string, 0, len(list))
	for _, n := range list {
		out = append(out, n.String())
	}
	return out
}

// ipStrings renders an IP list as a string slice (for JSON surfaces).
func ipStrings(list []net.IP) []string {
	out := make([]string, 0, len(list))
	for _, ip := range list {
		out = append(out, ip.String())
	}
	return out
}
