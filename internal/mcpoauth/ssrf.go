package mcpoauth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/ElcanoTek/fleet/internal/netguard"
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

// isBlockedIP reports whether ip is in a range we must never connect to for a
// user-supplied URL. It delegates to the shared SSRF classifier (see
// internal/netguard) so the remote-MCP control plane blocks exactly the same
// ranges as every other outbound path — one source of truth for this invariant.
func isBlockedIP(ip net.IP) bool {
	return netguard.IsBlockedIP(ip)
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

// SafeStreamingHTTPClient is SafeHTTPClient's posture — the resolve-then-dial
// guard and the unconditional redirect refusal — for callers that READ A
// LONG-LIVED RESPONSE BODY (an SSE subscription). http.Client.Timeout bounds
// the whole exchange, request through the last body byte, so it would cut a
// stream off mid-read; this client sets none and bounds the connection
// phases individually instead (dial 10s and TLS 10s from safeTransport, plus
// 30s for the response headers so a server that accepts and then stalls is
// still bounded). The caller owns the overall deadline through the request
// context — every caller MUST pass one.
func SafeStreamingHTTPClient() *http.Client {
	tr := safeTransport()
	tr.ResponseHeaderTimeout = 30 * time.Second
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errRedirectBlocked
		},
	}
}
