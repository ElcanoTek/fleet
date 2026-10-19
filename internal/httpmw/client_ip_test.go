// Copied from github.com/go-chi/chi/v5 v5.4.0, middleware/client_ip_test.go (modified: tests of the dropped middlewares removed).
// Copyright (c) 2015-present Peter Kieltyka, Google Inc.; see LICENSE.
// SPDX-License-Identifier: MIT

package httpmw

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestClientIPFromXFF_SingleTrustedHop(t *testing.T) {
	tt := []struct {
		name string
		xff  []string
		out  string
	}{
		{"missing", nil, ""},
		{"empty", []string{""}, ""},
		{"single", []string{"100.100.100.100"}, "100.100.100.100"},
		{"comma_space", []string{"1.1.1.1, 2.2.2.2"}, "2.2.2.2"},
		{"comma_no_space", []string{"1.1.1.1,2.2.2.2"}, "2.2.2.2"},
		{"multi_header_merged", []string{"1.1.1.1", "2.2.2.2"}, "2.2.2.2"},
		{"multi_header_with_commas", []string{"5.5.5.5, 6.6.6.6", "7.7.7.7, 4.4.4.4"}, "4.4.4.4"},
		{"ipv6", []string{"2001:db8::1"}, "2001:db8::1"},
		{"mixed_v4_v6_rightmost_wins", []string{"203.0.113.1, 2001:db8::1"}, "2001:db8::1"},

		// Normalization at the entry point: v4-mapped IPv6 folds to v4, zone stripped.
		{"v4_mapped_rightmost_folded", []string{"::ffff:1.2.3.4"}, "1.2.3.4"},
		{"zone_stripped_rightmost", []string{"2001:db8::1%eth0"}, "2001:db8::1"},

		// A header like "oh, hi,,127.0.0.1,,,," can be injected by the client.
		// The walker reaches 127.0.0.1 from the right before encountering the
		// unparseable tokens, so fail-closed never trips here.
		// See https://adam-p.ca/blog/2022/03/x-forwarded-for/ for more details.
		{"weird_with_empties_then_valid_rightmost", []string{"oh, hi,,127.0.0.1,,,,"}, "127.0.0.1"},
	}
	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			got := run(t, ClientIPFromXFF(testPeerPrefix), func(r *http.Request) {
				for _, v := range tc.xff {
					r.Header.Add("X-Forwarded-For", v)
				}
			})
			if got != tc.out {
				t.Errorf("want %q, got %q", tc.out, got)
			}
		})
	}
}

func TestClientIPFromXFF_TrustedPrefixes(t *testing.T) {
	tt := []struct {
		name     string
		prefixes []string
		xff      []string
		out      string
	}{
		{
			name:     "single_trusted_proxy_skipped",
			prefixes: []string{"203.0.113.0/24"},
			xff:      []string{"100.100.100.100, 203.0.113.50"},
			out:      "100.100.100.100",
		},
		{
			name:     "multiple_trusted_proxies_skipped",
			prefixes: []string{"203.0.113.0/24", "198.51.100.0/24"},
			xff:      []string{"1.1.1.1, 203.0.113.10, 198.51.100.5"},
			out:      "1.1.1.1",
		},
		{
			name:     "all_trusted_returns_empty",
			prefixes: []string{"203.0.113.0/24"},
			xff:      []string{"203.0.113.10, 203.0.113.20"},
			out:      "",
		},
		{
			name:     "ipv6_trusted_range",
			prefixes: []string{"2606:4700::/32"},
			xff:      []string{"2001:db8::1, 2606:4700::1"},
			out:      "2001:db8::1",
		},
		{
			name:     "mixed_v4_v6_trust_list",
			prefixes: []string{"2606:4700::/32", "203.0.113.0/24"},
			xff:      []string{"8.8.8.8, 2606:4700::1, 203.0.113.5"},
			out:      "8.8.8.8",
		},
		// Adam-P's blog and rezmoss's advisory: we must NOT filter private/loopback
		// values when they sit between trusted proxies. K8s nginx-ingress legitimately
		// produces this shape.
		{
			name:     "private_between_trusted_is_client",
			prefixes: []string{"10.244.0.0/24"},
			xff:      []string{"10.244.1.50, 10.244.0.10"},
			out:      "10.244.1.50",
		},
		// /24 boundary tests (Saku0512's PR #1087).
		{
			name:     "boundary_first_addr_in_prefix",
			prefixes: []string{"203.0.113.0/24"},
			xff:      []string{"100.100.100.100, 203.0.113.0"},
			out:      "100.100.100.100",
		},
		{
			name:     "boundary_last_addr_in_prefix",
			prefixes: []string{"203.0.113.0/24"},
			xff:      []string{"100.100.100.100, 203.0.113.255"},
			out:      "100.100.100.100",
		},
		{
			name:     "ip_just_outside_prefix_is_client",
			prefixes: []string{"203.0.113.0/24"},
			xff:      []string{"203.0.114.1, 203.0.113.1"},
			out:      "203.0.114.1",
		},
	}
	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			got := run(t, ClientIPFromXFF(tc.prefixes...), func(r *http.Request) {
				r.RemoteAddr = peerIn(tc.prefixes[0])
				for _, v := range tc.xff {
					r.Header.Add("X-Forwarded-For", v)
				}
			})
			if got != tc.out {
				t.Errorf("want %q, got %q", tc.out, got)
			}
		})
	}
}

func TestClientIPFromXFF_PanicsOnBadPrefix(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on invalid CIDR")
		}
	}()
	ClientIPFromXFF("not-a-cidr")
}

func TestClientIPFromXFF_PanicsOnNoPrefixes(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on no trusted prefixes")
		}
	}()
	ClientIPFromXFF()
}

// TestClientIP_TrustedPeer pins that r.RemoteAddr is the rightmost hop:
// headers are read only when the TCP peer is within a trusted prefix.
func TestClientIP_TrustedPeer(t *testing.T) {
	tt := []struct {
		name       string
		remoteAddr string
		out        string
	}{
		{"trusted_peer_reads_header", "10.0.0.5:1234", "203.0.113.7"},
		{"untrusted_peer_is_client", "198.51.100.9:1234", "198.51.100.9"},
		{"v4_mapped_trusted_peer", "[::ffff:10.0.0.5]:1234", "203.0.113.7"},
		{"v4_mapped_untrusted_peer_folded", "[::ffff:198.51.100.9]:1234", "198.51.100.9"},
		{"zoned_trusted_peer", "[fe80::1%eth0]:1234", "203.0.113.7"},
		{"untrusted_ipv6_peer", "[2001:db8::9]:1234", "2001:db8::9"},
		{"bare_ip_peer", "10.0.0.5", "203.0.113.7"},
		{"unparseable_peer_fails_closed", "@", ""},
		{"empty_peer_fails_closed", "", ""},
	}
	prefixes := []string{"10.0.0.0/8", "fe80::/10"}
	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			xff := run(t, ClientIPFromXFF(prefixes...), func(r *http.Request) {
				r.RemoteAddr = tc.remoteAddr
				r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
			})
			if xff != tc.out {
				t.Errorf("ClientIPFromXFF: want %q, got %q", tc.out, xff)
			}
		})
	}
}

func TestGetClientIPAddr_Unset(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	if got := GetClientIP(req.Context()); got != "" {
		t.Errorf("GetClientIP on empty ctx: want %q, got %q", "", got)
	}
	if got := GetClientIPAddr(req.Context()); got.IsValid() {
		t.Errorf("GetClientIPAddr on empty ctx: want zero, got %v", got)
	}
}

// Upstream's TestGetClientIPAddr_RoundTrip, through ClientIPFromXFF (fleet
// does not carry ClientIPFromHeader) from a trusted peer.
func TestGetClientIPAddr_RoundTripXFF(t *testing.T) {
	var gotStr string
	var gotAddr netip.Addr
	h := ClientIPFromXFF(testPeerPrefix)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotStr = GetClientIP(r.Context())
		gotAddr = GetClientIPAddr(r.Context())
	}))
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.42")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if gotStr != "203.0.113.42" || !gotAddr.IsValid() || gotAddr.String() != "203.0.113.42" || gotAddr.Is6() {
		t.Errorf("got %q / %v", gotStr, gotAddr)
	}
}

// GHSA-3fxj-6jh8-hvhx (Saku0512), the XFF half (the other half exercised
// ClientIPFromRemoteAddr, which fleet does not carry): an admin endpoint gates
// on the client IP, and an attacker prepends a spoofed IP to X-Forwarded-For.
// Behind a trusted proxy the proxy APPENDS the attacker's real IP, so the
// rightmost-untrusted entry is the genuine attacker IP, never the prepended
// spoof.
func TestAdvisory_GHSA_3fxj_6jh8_hvhx(t *testing.T) {
	got := run(t, ClientIPFromXFF("10.0.0.0/8"), func(r *http.Request) {
		r.RemoteAddr = peerIn("10.0.0.0/8")
		r.Header.Set("X-Forwarded-For", "1.2.3.4, 99.99.99.99")
	})
	if got == "1.2.3.4" {
		t.Fatal("VULNERABLE: returned attacker-supplied admin IP")
	}
	if got != "99.99.99.99" {
		t.Errorf("want 99.99.99.99 (rightmost-untrusted), got %q", got)
	}
}

// GHSA-9g5q-2w5x-hmxf (convto): RealIP set r.RemoteAddr from the leftmost XFF
// IP. With ClientIPFromXFF the rightmost-untrusted IP is selected, and we
// never mutate r.RemoteAddr.
func TestAdvisory_GHSA_9g5q_2w5x_hmxf(t *testing.T) {
	originalRemoteAddr := "203.0.113.99:1234"

	var capturedClientIP, capturedRemoteAddr string
	r := chi.NewRouter()
	r.Use(ClientIPFromXFF("203.0.113.99/32"))
	r.Get("/", func(_ http.ResponseWriter, r *http.Request) {
		capturedClientIP = GetClientIP(r.Context())
		capturedRemoteAddr = r.RemoteAddr
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-For", "192.0.2.2, 192.0.2.1") // convto's PoC values.
	req.RemoteAddr = originalRemoteAddr
	r.ServeHTTP(httptest.NewRecorder(), req)

	// The rightmost value is the one added by our (single) trusted hop.
	if capturedClientIP != "192.0.2.1" {
		t.Errorf("client IP: want 192.0.2.1 (rightmost), got %q", capturedClientIP)
	}
	if capturedClientIP == "192.0.2.2" {
		t.Errorf("VULNERABLE: returned attacker-supplied leftmost IP")
	}
	// Critical: r.RemoteAddr is never mutated, unlike middleware.RealIP.
	if capturedRemoteAddr != originalRemoteAddr {
		t.Errorf("r.RemoteAddr was mutated: want %q, got %q", originalRemoteAddr, capturedRemoteAddr)
	}
}

// GHSA-rjr7-jggh-pgcp (rezmoss), the XFF half (the other half exercised
// ClientIPFromHeader, which fleet does not carry): "X-Forwarded-For:
// 127.0.0.1" must not impersonate loopback. The trusted proxy appends the
// attacker's actual IP, so the rightmost-untrusted entry is never the spoof.
func TestAdvisory_GHSA_rjr7_jggh_pgcp(t *testing.T) {
	got := run(t, ClientIPFromXFF("10.0.0.0/8"), func(r *http.Request) {
		r.RemoteAddr = peerIn("10.0.0.0/8")
		r.Header.Set("X-Forwarded-For", "127.0.0.1, 99.99.99.99")
	})
	if got == "127.0.0.1" {
		t.Fatal("VULNERABLE: returned attacker-supplied loopback")
	}
	if got != "99.99.99.99" {
		t.Errorf("want 99.99.99.99 (rightmost-untrusted), got %q", got)
	}
}

// GHSA-j6qf-3m9c-vwj3 (tenbbughunters): ClientIPFromXFF never checked that
// the TCP peer was a trusted proxy, so a client reaching the origin directly
// could pick its own IP. Now an untrusted peer is the client IP. (Upstream's
// ClientIPFromHeader half is dropped: fleet does not carry that middleware.)
func TestAdvisory_GHSA_j6qf_3m9c_vwj3(t *testing.T) {
	const attacker = "127.0.0.1:54417"
	for _, xff := range []string{
		"10.0.0.7, 198.51.100.42", // reporter's PoC: forged trusted hop appended.
		"10.0.0.7",
	} {
		t.Run("xff/"+xff, func(t *testing.T) {
			got := run(t, ClientIPFromXFF("198.51.100.0/24"), func(r *http.Request) {
				r.RemoteAddr = attacker
				r.Header.Set("X-Forwarded-For", xff)
			})
			if got == "10.0.0.7" {
				t.Fatal("VULNERABLE: returned attacker-supplied allowlisted IP")
			}
			if got != "127.0.0.1" {
				t.Errorf("want 127.0.0.1 (real TCP peer), got %q", got)
			}
		})
	}
}

// "Multiple XFF headers" attack — Go's http.Header.Get returns only the first
// value, but the security-relevant rightmost IP is in the LAST instance after
// the proxy appends. We must merge all values; otherwise an attacker can pick
// which header the server sees by sending a duplicate.
//
// See https://adam-p.ca/blog/2022/03/x-forwarded-for/ "Multiple headers".
func TestXFF_MultipleHeadersMerged(t *testing.T) {
	// Attacker sends two XFF headers; the trusted proxy appends to the second
	// one (per RFC 2616). The genuine client is at the right of the merged
	// chain. A naive http.Header.Get() implementation would only see the first
	// header and return the attacker's prepended value.
	got := run(t, ClientIPFromXFF("198.51.100.0/24"), func(r *http.Request) {
		r.RemoteAddr = peerIn("198.51.100.0/24")
		r.Header.Add("X-Forwarded-For", "127.0.0.1")             // attacker-injected.
		r.Header.Add("X-Forwarded-For", "8.8.8.8, 198.51.100.1") // appended by trusted proxy.
	})
	if got != "8.8.8.8" {
		t.Errorf("want 8.8.8.8, got %q", got)
	}
}

// TestXFF_V4MappedIPv6BypassesTrustedV4Prefix demonstrates a gap in the
// rightmost-untrusted walker: netip.Prefix.Contains returns false when an
// IPv4-mapped IPv6 address (::ffff:a.b.c.d) is checked against a plain v4
// prefix. So an attacker who can inject the v4-mapped form of an internal
// address into XFF — and whose own connecting IP happens to be in the
// trusted range (e.g. a compromised host inside an internal VPC) — gets a
// trusted-looking value returned as "the client IP". Any downstream code
// that treats internal IPs as privileged (rate-limit exemptions, internal
// admin allowlists, audit pipelines) then mis-classifies the request.
//
// Pinned post-fix behavior: parseHeaderAddr calls Unmap() before the prefix
// check, so ::ffff:10.0.0.5 is recognized as 10.0.0.5 and skipped along with
// the rest of the trusted chain — leaving no untrusted IP and an empty
// GetClientIP.
func TestXFF_V4MappedIPv6BypassesTrustedV4Prefix(t *testing.T) {
	tt := []struct {
		name     string
		prefixes []string
		xff      string
		out      string
	}{
		{
			name:     "internal_address_via_v4_mapped",
			prefixes: []string{"10.0.0.0/8"},
			xff:      "::ffff:10.0.0.5, 10.0.0.1",
			out:      "",
		},
		{
			name:     "loopback_via_v4_mapped",
			prefixes: []string{"127.0.0.0/8", "10.0.0.0/8"},
			xff:      "::ffff:127.0.0.1, 10.0.0.1",
			out:      "",
		},
	}
	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			got := run(t, ClientIPFromXFF(tc.prefixes...), func(r *http.Request) {
				r.RemoteAddr = peerIn(tc.prefixes[0])
				r.Header.Set("X-Forwarded-For", tc.xff)
			})
			if got != tc.out {
				t.Errorf("VULNERABLE: v4-mapped IPv6 escaped v4 trusted prefix — want %q, got %q", tc.out, got)
			}
		})
	}
}

// TestXFF_IPv6ZoneIDBypassesTrustedV6Prefix demonstrates the same shape of
// gap for IPv6 trust prefixes: netip.Prefix.Contains returns false for any
// zoned IPv6 address, so an attacker-injected zone suffix escapes the trust
// check and the value is returned as "the client IP". Zone IDs are
// link-local concepts; they have no meaning on the wire and should never
// appear in a real XFF chain.
//
// Pinned post-fix behavior: parseHeaderAddr strips the zone before the
// prefix check, so 2606:4700::1%attacker is recognized as in 2606:4700::/32
// and skipped.
func TestXFF_IPv6ZoneIDBypassesTrustedV6Prefix(t *testing.T) {
	got := run(t, ClientIPFromXFF("2606:4700::/32"), func(r *http.Request) {
		r.RemoteAddr = peerIn("2606:4700::/32")
		r.Header.Set("X-Forwarded-For", "2606:4700::1%attacker, 2606:4700::5")
	})
	if got != "" {
		t.Errorf("VULNERABLE: zoned IPv6 escaped v6 trusted prefix — want %q, got %q", "", got)
	}
}

// TestXFF_FailClosedOnUnparseable pins the fail-closed contract for the
// rightmost-untrusted XFF walk (PR #967 review issue #6, adam-p): if the
// walker encounters an entry that does not parse as an IP, it MUST abandon
// the walk and leave no client IP set. Rightmost-untrusted relies on being
// able to read every hop to the right of the client; an unparseable hop is
// indistinguishable from a hostile or missing one, so we cannot safely keep
// walking past it.
//
// Empty / whitespace-only entries are NOT parse failures — they are dropped
// by trim before parsing, and do not trip fail-closed.
func TestXFF_FailClosedOnUnparseable(t *testing.T) {
	tt := []struct {
		name     string
		prefixes []string
		xff      []string
		out      string
	}{
		// Rightmost entry is garbage — walker fails closed before reaching the
		// otherwise-valid 1.1.1.1 to the left. Without fail-closed, garbage
		// to the right of a real client could be silently elided.
		{
			name:     "garbage_rightmost",
			prefixes: []string{"10.0.0.0/8"},
			xff:      []string{"1.1.1.1, garbage"},
			out:      "",
		},
		// Garbage between the client and a trusted proxy hop. Without fail-
		// closed, the walker would skip the trusted hop, silently skip the
		// garbage, and return 203.0.113.7 — a spoofable result.
		{
			name:     "garbage_between_client_and_trusted_proxy",
			prefixes: []string{"10.0.0.0/8"},
			xff:      []string{"203.0.113.7, garbage, 10.0.0.1"},
			out:      "",
		},
		// Garbage past a multi-hop trusted chain. Same shape with more
		// trusted hops in front; behaviour must be identical.
		{
			name:     "garbage_past_trusted_chain",
			prefixes: []string{"10.0.0.0/8"},
			xff:      []string{"203.0.113.7, garbage, 10.0.0.1, 10.0.0.2"},
			out:      "",
		},
		// Negative case: garbage sits in a left-hand header that the walker
		// never reaches because it returns at a valid untrusted entry first.
		// Fail-closed must NOT over-trigger here.
		{
			name:     "garbage_in_unreachable_left_header",
			prefixes: []string{"10.0.0.0/8"},
			xff:      []string{"garbage", "203.0.113.7, 10.0.0.1"},
			out:      "203.0.113.7",
		},
	}
	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			got := run(t, ClientIPFromXFF(tc.prefixes...), func(r *http.Request) {
				r.RemoteAddr = peerIn(tc.prefixes[0])
				for _, v := range tc.xff {
					r.Header.Add("X-Forwarded-For", v)
				}
			})
			if got != tc.out {
				t.Errorf("want %q, got %q", tc.out, got)
			}
		})
	}
}

// testPeerPrefix covers httptest.NewRequest's default RemoteAddr.
const testPeerPrefix = "192.0.2.1/32"

// peerIn returns a RemoteAddr inside prefix.
func peerIn(prefix string) string {
	return netip.AddrPortFrom(netip.MustParsePrefix(prefix).Addr().Next(), 1234).String()
}

// run invokes mw with the request constructed by buildReq and returns the
// client IP captured inside the handler (or "" if none was set).
func run(t *testing.T, mw func(http.Handler) http.Handler, buildReq func(*http.Request)) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/", nil)
	buildReq(req)

	var got string
	r := chi.NewRouter()
	r.Use(mw)
	r.Get("/", func(_ http.ResponseWriter, r *http.Request) {
		got = GetClientIP(r.Context())
	})
	r.ServeHTTP(httptest.NewRecorder(), req)
	return got
}
