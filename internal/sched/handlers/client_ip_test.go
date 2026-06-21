// Copyright (c) 2025 ElcanoTek
// All rights reserved. This is a private repository.

package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5/middleware"
)

// TestGetClientIP exercises getClientIP behind the same ClientIPFromXFF
// configuration wired up in main.go: loopback (Caddy) is the only trusted
// hop, and client-supplied X-Forwarded-For entries must not win.
func TestGetClientIP(t *testing.T) {
	clientIPMiddleware := middleware.ClientIPFromXFF("127.0.0.1/32", "::1/128")

	resolve := func(remoteAddr string, xff ...string) string {
		var got string
		handler := clientIPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
