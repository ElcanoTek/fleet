// Package netguard holds the single SSRF IP classifier the network-facing code
// paths share. fleet makes outbound HTTP requests to hosts that untrusted input
// can influence — a URL the model hands web_fetch/download_url, or a hosted
// MCP-server URL a user types (whose discovery documents then name further
// hosts). Every such request is dialed through a guard that resolves the target
// and refuses any internal address, checked at DIAL time (not save time) so a
// hostname that later re-resolves to an internal IP (DNS rebinding) is still
// blocked.
//
// This package exists so that guard consults ONE blocklist. The classifier
// previously lived duplicated in internal/tools and internal/mcpoauth, and the
// two copies drifted — each blocked a range the other missed (tools covered the
// RFC test/benchmark/reserved nets; mcpoauth covered all-multicast + fail-closed
// nil). Centralizing it keeps the SSRF guard — a security invariant — honest:
// one definition, one test, no silent under-blocking. It imports only the
// standard net package, so it introduces no import cycle with either caller.
package netguard

import "net"

// ssrfBlockedNets are special-purpose ranges that net.IP's built-in classifiers
// do NOT cover but the SSRF guard must still refuse:
//
//   - 100.64.0.0/10  — RFC 6598 carrier-grade NAT. Some clouds serve their
//     instance-metadata endpoint here (Alibaba/Oracle 100.100.100.x), so it is
//     credential-bearing exactly like 169.254.169.254.
//   - 192.0.2.0/24   — RFC 5737 TEST-NET-1 (documentation-only).
//   - 198.18.0.0/15  — RFC 2544 benchmarking.
//   - 240.0.0.0/4    — RFC 1112 reserved, incl. 255.255.255.255 broadcast.
//   - 0.0.0.0/8      — RFC 1122 "this network"; 0.x.y.z connects to the local
//     host on some stacks, and only 0.0.0.0 itself is caught by IsUnspecified.
//   - 64:ff9b::/96 and 64:ff9b:1::/48 — RFC 6052 / RFC 8215 NAT64 prefixes.
//     `64:ff9b::7f00:1` IS 127.0.0.1 on a host with a NAT64 gateway, and none
//     of the v4 classifiers see it because To4() is nil for these.
//   - ::/96          — IPv4-compatible IPv6 (deprecated, RFC 4291 §2.5.5.1):
//     `::7f00:1` is loopback by the same trick.
//
// Parsed once at package init; net.IPNet.Contains handles IPv4-mapped IPv6
// forms (e.g. ::ffff:100.100.100.200) via its internal To4 conversion.
var ssrfBlockedNets = func() []*net.IPNet {
	cidrs := []string{
		"100.64.0.0/10",
		"192.0.2.0/24",
		"198.18.0.0/15",
		"240.0.0.0/4",
		"0.0.0.0/8",
		"64:ff9b::/96",
		"64:ff9b:1::/48",
		"::/96",
	}
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, cidr := range cidrs {
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			panic("netguard: bad SSRF CIDR " + cidr + ": " + err.Error()) // static list; unreachable
		}
		nets = append(nets, ipNet)
	}
	return nets
}()

// IsBlockedIP reports whether ip is an address an SSRF-guarded dialer must
// refuse to connect to: loopback, private (RFC 1918 + fc00::/7 ULA), link-local
// (incl. the 169.254.169.254 cloud-metadata endpoint), any multicast, the
// unspecified address, and the special-purpose ranges in ssrfBlockedNets (CGNAT
// cloud-metadata, test/benchmark, reserved). It fails CLOSED: a nil ip (a host
// that would not parse/resolve) is treated as blocked. IPv4-in-IPv6 is
// normalized first so a mapped address cannot slip past the v4 checks.
func IsBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	switch {
	case ip.IsLoopback(): // 127.0.0.0/8, ::1
		return true
	case ip.IsPrivate(): // RFC 1918 + ULA fc00::/7
		return true
	case ip.IsLinkLocalUnicast(): // 169.254.0.0/16 (incl. metadata 169.254.169.254), fe80::/10
		return true
	case ip.IsLinkLocalMulticast(), ip.IsInterfaceLocalMulticast(), ip.IsMulticast():
		return true
	case ip.IsUnspecified(): // 0.0.0.0, ::
		return true
	}
	for _, n := range ssrfBlockedNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
