package httpapi

import (
	"log"
	"net"
	"net/http"
	"strings"

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
// trusted proxy (FLEET_TRUSTED_PROXIES). With no trusted proxies configured the
// header is never read, so an untrusted client cannot spoof an allowlisted
// source by setting X-Forwarded-For itself.

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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /healthz is exempt: probes from a load balancer / uptime monitor may
		// originate outside the operator's allowlist.
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		ip := clientIP(r, cfg.trustedProxies)
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

// clientIP resolves the real client IP for filtering. It starts from the
// immediate TCP peer (r.RemoteAddr). Only when that peer is a configured trusted
// proxy does it consult X-Forwarded-For and take the LEFTMOST entry (the
// originating client per the XFF convention). With no trusted proxies, the
// header is ignored entirely — closing the spoofing bypass where an untrusted
// client sets X-Forwarded-For to an allowlisted address. Returns nil when
// RemoteAddr cannot be parsed.
func clientIP(r *http.Request, trustedProxies []net.IP) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// RemoteAddr may already be a bare IP (e.g. in tests / unix sockets).
		host = r.RemoteAddr
	}
	peer := net.ParseIP(strings.TrimSpace(host))
	if peer == nil {
		return nil
	}
	if !isTrustedProxy(peer, trustedProxies) {
		return peer
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return peer
	}
	// Leftmost entry is the client the chain claims to have originated from.
	first := strings.TrimSpace(strings.SplitN(xff, ",", 2)[0])
	if client := net.ParseIP(first); client != nil {
		return client
	}
	return peer
}

// isTrustedProxy reports whether peer is one of the configured trusted proxy IPs.
func isTrustedProxy(peer net.IP, trusted []net.IP) bool {
	for _, t := range trusted {
		if t.Equal(peer) {
			return true
		}
	}
	return false
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
