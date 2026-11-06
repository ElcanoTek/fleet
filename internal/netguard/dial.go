package netguard

import (
	"context"
	"fmt"
	"net"
)

// DialContext returns a dial function that resolves the target host, refuses
// every resolved address isBlocked rejects, and dials the exact address it
// validated. Checking at DIAL time (not when a URL is saved) and dialing the
// validated IP rather than the hostname is what defeats DNS rebinding: a name
// that re-resolves to an internal address between check and connect cannot
// slip through.
//
// A literal IP is validated and dialed directly. A hostname is tried address by
// address, skipping blocked ones. If no address was usable the result is the
// last dial error, or blockedErr when every address was blocked (or none
// resolved to an allowed one).
//
// Every outbound path that dials a host untrusted input can influence builds
// its dialer here, so a fix to the guard lands once. Callers pass their own
// d (timeouts and keep-alive differ by path) and their own blockedErr (callers
// match it with errors.Is and surface its text); isBlocked is a parameter so a
// test can admit a loopback httptest origin. Production passes IsBlockedIP.
func DialContext(d *net.Dialer, isBlocked func(net.IP) bool, blockedErr error) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("split host:port: %w", err)
		}
		if ip := net.ParseIP(host); ip != nil {
			if isBlocked(ip) {
				return nil, blockedErr
			}
			return d.DialContext(ctx, network, addr)
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, fmt.Errorf("resolve %q: %w", host, err)
		}
		var lastErr error
		for _, ipa := range ips {
			if isBlocked(ipa.IP) {
				lastErr = blockedErr
				continue
			}
			conn, derr := d.DialContext(ctx, network, net.JoinHostPort(ipa.IP.String(), port))
			if derr != nil {
				lastErr = derr
				continue
			}
			return conn, nil
		}
		if lastErr == nil {
			lastErr = blockedErr
		}
		return nil, lastErr
	}
}
