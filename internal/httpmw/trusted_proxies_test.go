// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package httpmw

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestNewTrustedProxies(t *testing.T) {
	got := NewTrustedProxies([]net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1"), net.ParseIP("::ffff:10.0.0.1"), {1, 2, 3}})
	want := []string{"127.0.0.1/32", "::1/128", "10.0.0.1/32"}
	if fmt.Sprint(got.Prefixes()) != fmt.Sprint(want) {
		t.Errorf("Prefixes() = %v, want %v (v4-mapped folds to /32; a malformed IP is skipped)", got.Prefixes(), want)
	}
	if got.Empty() {
		t.Error("Empty() = true for three trusted proxies")
	}
	if z := NewTrustedProxies(nil); !z.Empty() || len(z.Prefixes()) != 0 {
		t.Errorf("NewTrustedProxies(nil): Empty=%v Prefixes=%v, want true, []", z.Empty(), z.Prefixes())
	}
}

// trustCases is the FLEET_TRUSTED_PROXIES contract as a table: what ClientIP
// returns, which the orchestrator's Middleware path must agree with.
var trustCases = []struct {
	name       string
	trusted    []string
	remoteAddr string
	xff        []string
	want       string // "" = no IP (unparseable RemoteAddr)
}{
	{"no proxies: peer, header ignored", nil, "203.0.113.9:1", []string{"1.2.3.4"}, "203.0.113.9"},
	{"untrusted peer: header ignored", []string{"127.0.0.1"}, "203.0.113.9:1", []string{"1.2.3.4"}, "203.0.113.9"},
	{"trusted peer, no header: peer", []string{"127.0.0.1"}, "127.0.0.1:1", nil, "127.0.0.1"},
	{"trusted peer: rightmost untrusted hop", []string{"127.0.0.1"}, "127.0.0.1:1", []string{"6.6.6.6, 203.0.113.7"}, "203.0.113.7"},
	{"walks past trusted hops", []string{"127.0.0.1", "10.0.0.1"}, "127.0.0.1:1", []string{"6.6.6.6, 203.0.113.7, 10.0.0.1"}, "203.0.113.7"},
	{"all-trusted chain: peer", []string{"127.0.0.1", "10.0.0.1"}, "127.0.0.1:1", []string{"10.0.0.1, 127.0.0.1"}, "127.0.0.1"},
	{"unparseable entry stops the walk: peer", []string{"127.0.0.1"}, "127.0.0.1:1", []string{"203.0.113.7, not-an-ip"}, "127.0.0.1"},
	{"duplicate headers merge in order", []string{"127.0.0.1"}, "127.0.0.1:1", []string{"6.6.6.6", "203.0.113.7"}, "203.0.113.7"},
	{"v4-mapped peer is trusted and folds", []string{"127.0.0.1"}, "[::ffff:127.0.0.1]:1", []string{"203.0.113.7"}, "203.0.113.7"},
	{"v4-mapped hop folds", []string{"127.0.0.1"}, "127.0.0.1:1", []string{"::ffff:203.0.113.7"}, "203.0.113.7"},
	{"IPv6 loopback proxy", []string{"127.0.0.1", "::1"}, "[::1]:1", []string{"2001:db8::7"}, "2001:db8::7"},
	{"bare-IP RemoteAddr", nil, "198.51.100.4", nil, "198.51.100.4"},
	{"unparseable RemoteAddr, no proxies", nil, "garbage", nil, ""},
	{"unparseable RemoteAddr, proxies set", []string{"127.0.0.1"}, "garbage", []string{"203.0.113.7"}, ""},
}

func trustFrom(ss []string) TrustedProxies {
	ips := make([]net.IP, 0, len(ss))
	for _, s := range ss {
		ips = append(ips, net.ParseIP(s))
	}
	return NewTrustedProxies(ips)
}

func trustRequest(remoteAddr string, xff []string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = remoteAddr
	for _, v := range xff {
		req.Header.Add("X-Forwarded-For", v)
	}
	return req
}

func TestTrustedProxiesClientIP(t *testing.T) {
	for _, tc := range trustCases {
		t.Run(tc.name, func(t *testing.T) {
			got := trustFrom(tc.trusted).ClientIP(trustRequest(tc.remoteAddr, tc.xff))
			if tc.want == "" {
				if got.IsValid() {
					t.Fatalf("ClientIP = %v, want none", got)
				}
				return
			}
			if got != netip.MustParseAddr(tc.want) {
				t.Fatalf("ClientIP = %v, want %s", got, tc.want)
			}
		})
	}
}

// TestTrustedProxiesMiddlewareAgrees — what an orchestrator handler sees
// behind Middleware (the stored IP, else the TCP peer, as getClientIP reads
// it) is exactly ClientIP, for every case, including no proxies (pass-through).
func TestTrustedProxiesMiddlewareAgrees(t *testing.T) {
	for _, tc := range trustCases {
		t.Run(tc.name, func(t *testing.T) {
			trust := trustFrom(tc.trusted)
			var stored netip.Addr
			trust.Middleware()(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				stored = GetClientIPAddr(r.Context())
			})).ServeHTTP(httptest.NewRecorder(), trustRequest(tc.remoteAddr, tc.xff))
			if !stored.IsValid() {
				stored, _ = parseRemoteAddr(tc.remoteAddr)
			}
			if want := trust.ClientIP(trustRequest(tc.remoteAddr, tc.xff)); stored != want {
				t.Fatalf("middleware path = %v, ClientIP = %v", stored, want)
			}
		})
	}
}
