// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package httpmw

import (
	"net"
	"net/http"
	"net/netip"
)

// TrustedProxies is FLEET_TRUSTED_PROXIES (config.Config.TrustedProxies) in
// the form both of fleet's HTTP servers resolve the client IP from. It exists
// so there is ONE resolver: the orchestrator API installs Middleware
// (ClientIPFromXFF over Prefixes) and the chat server's IP allow/deny filter
// calls ClientIP, and both run the same walk (xffClientIP in client_ip.go).
// Two hand-written copies of that contract drifted once already — the chat
// filter skipped an unparseable X-Forwarded-For entry while ClientIPFromXFF
// stops at it — so the two servers could name different clients for one
// request.
//
// The zero value trusts no proxy: X-Forwarded-For is never read and the
// client is always the TCP peer.
type TrustedProxies struct {
	prefixes []netip.Prefix
}

// NewTrustedProxies builds the host prefixes (/32 or /128) for the given
// proxy IPs. IPv4 addresses parsed into 16-byte form are unmapped first so
// they become /32 prefixes, matching how ClientIPFromXFF folds v4-mapped
// peers and hops. An entry that is not a valid IP (impossible from
// config.Load, which rejects one at startup) is skipped.
func NewTrustedProxies(ips []net.IP) TrustedProxies {
	var t TrustedProxies
	for _, ip := range ips {
		addr, ok := netip.AddrFromSlice(ip)
		if !ok {
			continue
		}
		addr = addr.Unmap()
		t.prefixes = append(t.prefixes, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return t
}

// Empty reports whether no proxy is trusted. ClientIPFromXFF panics without
// a prefix, so callers install it only when this is false.
func (t TrustedProxies) Empty() bool { return len(t.prefixes) == 0 }

// Prefixes returns the trusted prefixes as the strings ClientIPFromXFF takes.
func (t TrustedProxies) Prefixes() []string {
	out := make([]string, 0, len(t.prefixes))
	for _, p := range t.prefixes {
		out = append(out, p.String())
	}
	return out
}

// Middleware is the orchestrator's wiring of t: ClientIPFromXFF over
// t.Prefixes(), or a pass-through when no proxy is trusted (ClientIPFromXFF
// requires a prefix, and with none there is nothing to store — readers fall
// back to the TCP peer, which is what ClientIP returns then too).
func (t TrustedProxies) Middleware() func(http.Handler) http.Handler {
	if t.Empty() {
		return func(h http.Handler) http.Handler { return h }
	}
	return ClientIPFromXFF(t.Prefixes()...)
}

// ClientIP returns the client IP of r, the same address an orchestrator
// handler reads behind t.Middleware() with its fallback to the TCP peer:
//
//   - no trusted proxies, or a TCP peer that is not one: the peer;
//     X-Forwarded-For is never read, so a direct client cannot pick its own
//     address by sending the header;
//   - a trusted peer: the first X-Forwarded-For hop, walking right-to-left,
//     that is not itself a trusted proxy;
//   - a trusted peer whose chain is empty, all-trusted, or hits an
//     unparseable entry (the walk stops there — nothing left of garbage is
//     trusted): the peer.
//
// v4-mapped addresses fold to plain IPv4. The zero Addr is returned only when
// r.RemoteAddr itself does not parse.
func (t TrustedProxies) ClientIP(r *http.Request) netip.Addr {
	peer, ok := parseRemoteAddr(r.RemoteAddr)
	if !ok {
		return netip.Addr{}
	}
	if t.Empty() {
		return peer
	}
	if ip := xffClientIP(r, t.prefixes); ip.IsValid() {
		return ip
	}
	return peer
}
