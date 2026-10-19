// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ElcanoTek/fleet/internal/httpmw"
)

// TestGetClientIP exercises getClientIP behind the ClientIPFromXFF
// configuration main.go wires up for FLEET_TRUSTED_PROXIES=127.0.0.1,::1:
// loopback (Caddy) is the only trusted hop, and client-supplied
// X-Forwarded-For entries must not win.
func TestGetClientIP(t *testing.T) {
	clientIPMiddleware := httpmw.ClientIPFromXFF("127.0.0.1/32", "::1/128")

	resolve := func(remoteAddr string, xff ...string) string {
		var got string
		handler := clientIPMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			got = getClientIP(r)
		}))
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = remoteAddr
		for _, v := range xff {
			req.Header.Add("X-Forwarded-For", v)
		}
		handler.ServeHTTP(httptest.NewRecorder(), req)
		return got
	}

	tests := []struct {
		name       string
		remoteAddr string
		xff        []string
		want       string
	}{
		{
			name:       "proxied request resolves XFF client IP",
			remoteAddr: "127.0.0.1:34567",
			xff:        []string{"203.0.113.7"},
			want:       "203.0.113.7",
		},
		{
			name:       "client-spoofed XFF entry is ignored in favor of the proxy-appended one",
			remoteAddr: "127.0.0.1:34567",
			xff:        []string{"1.2.3.4, 203.0.113.7"},
			want:       "203.0.113.7",
		},
		{
			name:       "loopback hops are walked past to the real client",
			remoteAddr: "127.0.0.1:34567",
			xff:        []string{"203.0.113.7, 127.0.0.1"},
			want:       "203.0.113.7",
		},
		{
			name:       "direct connection falls back to RemoteAddr",
			remoteAddr: "192.0.2.5:9999",
			want:       "192.0.2.5",
		},
		{
			name:       "direct connection cannot spoof via XFF (GHSA-j6qf-3m9c-vwj3)",
			remoteAddr: "192.0.2.5:9999",
			xff:        []string{"10.0.0.7, 127.0.0.1"},
			want:       "192.0.2.5",
		},
		{
			name:       "direct IPv6 connection falls back to bracketed RemoteAddr",
			remoteAddr: "[2001:db8::1]:9999",
			want:       "2001:db8::1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolve(tc.remoteAddr, tc.xff...); got != tc.want {
				t.Errorf("getClientIP() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGetClientIPMatchesSharedResolver — the orchestrator's real client-IP
// path (httpmw.TrustedProxies.Middleware, then getClientIP with its own
// RemoteAddr fallback) names the same address as TrustedProxies.ClientIP, the
// resolver the chat server's IP filter uses, with and without trusted proxies.
func TestGetClientIPMatchesSharedResolver(t *testing.T) {
	for _, trusted := range [][]net.IP{nil, {net.ParseIP("127.0.0.1"), net.ParseIP("::1")}} {
		trust := httpmw.NewTrustedProxies(trusted)
		for _, tc := range []struct{ remoteAddr, xff string }{
			{"203.0.113.9:1", ""},
			{"203.0.113.9:1", "1.2.3.4"},
			{"127.0.0.1:1", ""},
			{"127.0.0.1:1", "6.6.6.6, 203.0.113.7"},
			{"127.0.0.1:1", "203.0.113.7, 127.0.0.1"},
			{"127.0.0.1:1", "203.0.113.7, not-an-ip"},
			{"[::1]:1", "2001:db8::7"},
			{"[2001:db8::1]:1", "1.2.3.4"},
			{"[::ffff:127.0.0.1]:1", "203.0.113.7"},
		} {
			newReq := func() *http.Request {
				req := httptest.NewRequest("GET", "/", nil)
				req.RemoteAddr = tc.remoteAddr
				if tc.xff != "" {
					req.Header.Set("X-Forwarded-For", tc.xff)
				}
				return req
			}
			var got string
			trust.Middleware()(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got = getClientIP(r)
			})).ServeHTTP(httptest.NewRecorder(), newReq())
			want := trust.ClientIP(newReq())
			if !net.ParseIP(got).Equal(net.IP(want.AsSlice())) {
				t.Errorf("trusted=%v RemoteAddr=%s XFF=%q: getClientIP = %q, shared resolver = %v", trusted, tc.remoteAddr, tc.xff, got, want)
			}
		}
	}
}
