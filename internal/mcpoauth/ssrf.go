package mcpoauth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// SSRF defense for user-supplied remote-MCP URLs.
//
// Users type arbitrary hosted-server URLs into the GUI, and the host-side fleet
// process then makes outbound HTTP requests to them — for discovery, for the
// OAuth token endpoints they advertise, and (with a bearer attached) for the
// MCP tool calls themselves. Without a guard a user could point fleet at
// http://169.254.169.254/ (cloud metadata) or an internal service and have the
// agent stream the response back, or smuggle a bearer to an internal host.
//
// The guard is a custom DialContext that resolves the target host and refuses to
// connect to any private / loopback / link-local / metadata address — checked at
// DIAL time, not save time, so a hostname that later re-resolves to an internal
// IP (DNS rebinding) is still blocked. We dial the exact validated IP to close
// the resolve→connect TOCTOU window. The data-plane client additionally refuses
// to follow redirects so a 30x can't bounce a bearer to another origin.

// errBlockedAddress is returned by the dialer when a resolved address is in a
// blocked range. It deliberately does not echo back internal IPs to the caller's
// surfaced error in a way that aids scanning beyond "blocked".
var errBlockedAddress = errors.New("connection to a private, loopback, or link-local address is not allowed")

// errRedirectBlocked is returned to disable redirect-following on the data-plane
// client (a redirect must never carry the bearer to a new origin).
var errRedirectBlocked = errors.New("redirects are disabled for remote MCP connections")

// cgnatRange is RFC 6598 carrier-grade NAT space (100.64.0.0/10), commonly used
// for internal infrastructure, which net.IP.IsPrivate does not cover.
var cgnatRange = func() *net.IPNet {
	_, n, _ := net.ParseCIDR("100.64.0.0/10")
	return n
}()

// isBlockedIP reports whether ip is in a range we must never connect to for a
// user-supplied URL. IPv4-in-IPv6 is normalized first so a mapped address can't
// slip past the v4 checks.
func isBlockedIP(ip net.IP) bool {
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
	case cgnatRange.Contains(ip): // 100.64.0.0/10
		return true
	}
	return false
}

// safeDialContext returns a DialContext that resolves the host, rejects any
// blocked resolved IP, and dials a validated address directly.
func safeDialContext() func(ctx context.Context, network, addr string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("split host:port: %w", err)
		}
		// A literal IP needs no resolution — validate and dial it directly.
		if ip := net.ParseIP(host); ip != nil {
			if isBlockedIP(ip) {
				return nil, errBlockedAddress
			}
			return dialer.DialContext(ctx, network, addr)
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve %q: %w", host, err)
		}
		var lastErr error
		for _, ipa := range ips {
			if isBlockedIP(ipa.IP) {
				lastErr = errBlockedAddress
				continue
			}
			// Dial the exact validated IP so a concurrent re-resolution can't
			// swap in a blocked address between check and connect.
			conn, derr := dialer.DialContext(ctx, network, net.JoinHostPort(ipa.IP.String(), port))
			if derr != nil {
				lastErr = derr
				continue
			}
			return conn, nil
		}
		if lastErr == nil {
			lastErr = errBlockedAddress
		}
		return nil, lastErr
	}
}

func safeTransport() *http.Transport {
	return &http.Transport{
		DialContext:           safeDialContext(),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          20,
		IdleConnTimeout:       60 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// SafeHTTPClient builds an HTTP client for talking to user-supplied remote MCP
// servers and their authorization servers. The dialer blocks internal
// addresses on every connection (including redirect hops). Redirects are
// disabled so a 30x cannot relay a bearer to a different origin — MCP JSON-RPC
// and the OAuth metadata/token endpoints are not expected to redirect.
func SafeHTTPClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: safeTransport(),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errRedirectBlocked
		},
	}
}
