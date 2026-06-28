package httpapi

import (
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
)

// mustCIDR parses an IP or CIDR into a network for table setup, coercing a bare
// host to /32 (IPv4) or /128 (IPv6) — mirroring config.parseCIDRList.
func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	if _, n, err := net.ParseCIDR(s); err == nil {
		return n
	}
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("mustCIDR: cannot parse %q", s)
	}
	bits := 32
	if ip.To4() == nil {
		bits = 128
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}
}

func cidrs(t *testing.T, ss ...string) []*net.IPNet {
	t.Helper()
	out := make([]*net.IPNet, 0, len(ss))
	for _, s := range ss {
		out = append(out, mustCIDR(t, s))
	}
	return out
}

// okHandler is the downstream handler; it writes 200 so a test can tell an
// allowed request (200) from a blocked one (403) and assert the body never leaks.
var okHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("downstream"))
})

func TestIPFilterMiddleware(t *testing.T) {
	tests := []struct {
		name       string
		allow      []*net.IPNet
		deny       []*net.IPNet
		trusted    []net.IP
		remoteAddr string
		xff        string // X-Forwarded-For header (empty = unset)
		path       string
		wantCode   int
	}{
		{
			name:       "no lists: allow all (default, backward compatible)",
			remoteAddr: "203.0.113.9:5000",
			wantCode:   http.StatusOK,
		},
		{
			name:       "allowlist: matching IP passes",
			allow:      cidrs(t, "192.168.1.0/24"),
			remoteAddr: "192.168.1.50:1234",
			wantCode:   http.StatusOK,
		},
		{
			name:       "allowlist: non-matching IP blocked",
			allow:      cidrs(t, "192.168.1.0/24"),
			remoteAddr: "10.0.0.5:1234",
			wantCode:   http.StatusForbidden,
		},
		{
			name:       "allowlist: bare host (/32) exact match passes",
			allow:      cidrs(t, "203.0.113.7"),
			remoteAddr: "203.0.113.7:443",
			wantCode:   http.StatusOK,
		},
		{
			name:       "allowlist: bare host (/32) neighbor blocked",
			allow:      cidrs(t, "203.0.113.7"),
			remoteAddr: "203.0.113.8:443",
			wantCode:   http.StatusForbidden,
		},
		{
			name:       "denylist: listed IP blocked",
			deny:       cidrs(t, "45.33.32.0/24"),
			remoteAddr: "45.33.32.156:1234",
			wantCode:   http.StatusForbidden,
		},
		{
			name:       "denylist: unlisted IP passes (deny-only, no allowlist)",
			deny:       cidrs(t, "45.33.32.0/24"),
			remoteAddr: "8.8.8.8:1234",
			wantCode:   http.StatusOK,
		},
		{
			name:       "deny overrides allow: IP in both is blocked",
			allow:      cidrs(t, "10.0.0.0/8"),
			deny:       cidrs(t, "10.0.0.66"),
			remoteAddr: "10.0.0.66:1234",
			wantCode:   http.StatusForbidden,
		},
		{
			name:       "XFF trusted: real client read from header, allowed",
			allow:      cidrs(t, "192.168.1.0/24"),
			trusted:    []net.IP{net.ParseIP("127.0.0.1")},
			remoteAddr: "127.0.0.1:9999",
			xff:        "192.168.1.42",
			wantCode:   http.StatusOK,
		},
		{
			name:       "XFF trusted: real client read from header, blocked",
			allow:      cidrs(t, "192.168.1.0/24"),
			trusted:    []net.IP{net.ParseIP("127.0.0.1")},
			remoteAddr: "127.0.0.1:9999",
			xff:        "8.8.8.8",
			wantCode:   http.StatusForbidden,
		},
		{
			name:       "XFF trusted: leftmost entry of a chain is used",
			allow:      cidrs(t, "192.168.1.0/24"),
			trusted:    []net.IP{net.ParseIP("127.0.0.1")},
			remoteAddr: "127.0.0.1:9999",
			xff:        "192.168.1.42, 10.9.9.9, 127.0.0.1",
			wantCode:   http.StatusOK,
		},
		{
			name:  "XFF untrusted: header ignored, real peer NOT allowlisted, blocked (anti-spoof)",
			allow: cidrs(t, "192.168.1.0/24"),
			// No trusted proxies: a client claiming to be 192.168.1.42 via XFF must
			// NOT be trusted; the real peer 8.8.8.8 is judged instead → blocked.
			remoteAddr: "8.8.8.8:1234",
			xff:        "192.168.1.42",
			wantCode:   http.StatusForbidden,
		},
		{
			name:       "XFF from a non-trusted peer is ignored even with trusted proxies set",
			allow:      cidrs(t, "192.168.1.0/24"),
			trusted:    []net.IP{net.ParseIP("127.0.0.1")},
			remoteAddr: "8.8.8.8:1234", // peer is NOT the trusted proxy
			xff:        "192.168.1.42",
			wantCode:   http.StatusForbidden,
		},
		{
			name:       "healthz exempt: blocked-range IP still reaches /healthz",
			allow:      cidrs(t, "192.168.1.0/24"),
			remoteAddr: "8.8.8.8:1234",
			path:       "/healthz",
			wantCode:   http.StatusOK,
		},
		{
			name:       "IPv6 allowlist match passes",
			allow:      cidrs(t, "2001:db8::/32"),
			remoteAddr: "[2001:db8::1]:1234",
			wantCode:   http.StatusOK,
		},
		{
			name:       "IPv6 allowlist miss blocked",
			allow:      cidrs(t, "2001:db8::/32"),
			remoteAddr: "[2001:dead::1]:1234",
			wantCode:   http.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{cfg: &config.Config{
				IPAllowlist:    tc.allow,
				IPDenylist:     tc.deny,
				TrustedProxies: tc.trusted,
			}}
			h := s.ipFilterMiddleware(okHandler)

			path := tc.path
			if path == "" {
				path = "/chat"
			}
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)

			if rr.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d (body %q)", rr.Code, tc.wantCode, rr.Body.String())
			}
			if tc.wantCode == http.StatusForbidden {
				// Uniform, generic body + plain text content type — no info about WHY.
				if got := rr.Body.String(); got != "Access denied\n" {
					t.Errorf("blocked body = %q, want \"Access denied\\n\"", got)
				}
				if ct := rr.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
					t.Errorf("blocked content-type = %q, want text/plain; charset=utf-8", ct)
				}
			}
		})
	}
}

// TestIPFilterMiddleware_NoConfigReturnsUnwrapped verifies the default open case
// is a pass-through (the middleware returns next unwrapped), so the hot path adds
// no per-request work when no lists are configured.
func TestIPFilterMiddleware_NoConfigReturnsUnwrapped(t *testing.T) {
	s := &Server{cfg: &config.Config{}}
	got := s.ipFilterMiddleware(okHandler)
	if reflect.ValueOf(got).Pointer() != reflect.ValueOf(okHandler).Pointer() {
		t.Errorf("with no lists configured, ipFilterMiddleware should return next unwrapped")
	}
}

// TestClientIP exercises the IP-resolution seam directly: trust gating, leftmost
// XFF selection, and unparseable input.
func TestClientIP(t *testing.T) {
	trusted := []net.IP{net.ParseIP("127.0.0.1")}
	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		trusted    []net.IP
		want       string // "" means nil
	}{
		{name: "bare peer, no proxies", remoteAddr: "8.8.8.8:1234", want: "8.8.8.8"},
		{name: "trusted peer reads leftmost XFF", remoteAddr: "127.0.0.1:80", xff: "1.2.3.4, 5.6.7.8", trusted: trusted, want: "1.2.3.4"},
		{name: "untrusted peer ignores XFF", remoteAddr: "8.8.8.8:1234", xff: "1.2.3.4", trusted: trusted, want: "8.8.8.8"},
		{name: "trusted peer, empty XFF falls back to peer", remoteAddr: "127.0.0.1:80", xff: "", trusted: trusted, want: "127.0.0.1"},
		{name: "trusted peer, garbage XFF falls back to peer", remoteAddr: "127.0.0.1:80", xff: "not-an-ip", trusted: trusted, want: "127.0.0.1"},
		{name: "bare-IP RemoteAddr (no port)", remoteAddr: "9.9.9.9", want: "9.9.9.9"},
		{name: "unparseable RemoteAddr returns nil", remoteAddr: "garbage", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/chat", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				req.Header.Set("X-Forwarded-For", tc.xff)
			}
			got := clientIP(req, tc.trusted)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("clientIP = %v, want nil", got)
				}
				return
			}
			if got == nil || !got.Equal(net.ParseIP(tc.want)) {
				t.Fatalf("clientIP = %v, want %s", got, tc.want)
			}
		})
	}
}
