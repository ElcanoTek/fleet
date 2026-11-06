package netguard

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

var errTestBlocked = errors.New("blocked")

// listenLoopback opens a TCP listener on 127.0.0.1 that accepts and closes
// connections, and returns its port.
func listenLoopback(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

// TestDialContext pins the resolve-then-dial guard every SSRF-sensitive dialer
// is built from (#1938): a blocked literal IP or a hostname whose every address
// is blocked returns the CALLER's error (callers match it with errors.Is), an
// allowed address is dialed, and a blocked address among several is skipped in
// favour of an allowed one. isBlocked is injected so loopback can stand in for
// "public" here; production passes IsBlockedIP.
func TestDialContext(t *testing.T) {
	port := listenLoopback(t)
	d := &net.Dialer{Timeout: 2 * time.Second}
	blockAll := func(net.IP) bool { return true }
	allowAll := func(net.IP) bool { return false }
	// Allow only IPv4 loopback, so a "localhost" that also resolves to ::1 has
	// to skip the blocked address and dial the allowed one.
	allowV4Loopback := func(ip net.IP) bool { return !ip.Equal(net.IPv4(127, 0, 0, 1)) }

	cases := []struct {
		name      string
		isBlocked func(net.IP) bool
		addr      string
		wantErr   error // nil = must connect
	}{
		{"blocked literal IP", blockAll, net.JoinHostPort("127.0.0.1", port), errTestBlocked},
		{"allowed literal IP", allowAll, net.JoinHostPort("127.0.0.1", port), nil},
		{"hostname resolving only to blocked addresses", blockAll, net.JoinHostPort("localhost", port), errTestBlocked},
		{"blocked address skipped for an allowed one", allowV4Loopback, net.JoinHostPort("localhost", port), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dial := DialContext(d, tc.isBlocked, errTestBlocked)
			conn, err := dial(context.Background(), "tcp", tc.addr)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if conn != nil {
					t.Fatal("got a connection to a blocked address")
				}
				return
			}
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			_ = conn.Close()
		})
	}

	t.Run("malformed address", func(t *testing.T) {
		if _, err := DialContext(d, allowAll, errTestBlocked)(context.Background(), "tcp", "no-port"); err == nil {
			t.Fatal("want an error for an address without a port")
		}
	})
}
