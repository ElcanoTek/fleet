package mcpoauth

import (
	"net"
	"testing"
)

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1",              // loopback
		"::1",                    // loopback v6
		"10.0.0.5",               // RFC 1918
		"172.16.0.1",             // RFC 1918
		"192.168.1.1",            // RFC 1918
		"169.254.169.254",        // link-local / cloud metadata
		"169.254.0.1",            // link-local
		"fe80::1",                // link-local v6
		"fc00::1",                // ULA
		"fd12:3456::1",           // ULA
		"0.0.0.0",                // unspecified
		"::",                     // unspecified v6
		"100.64.0.1",             // CGNAT
		"224.0.0.1",              // multicast
		"::ffff:127.0.0.1",       // IPv4-mapped loopback (must not slip through)
		"::ffff:169.254.169.254", // IPv4-mapped metadata
	}
	for _, s := range blocked {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test IP %q", s)
		}
		if !isBlockedIP(ip) {
			t.Errorf("isBlockedIP(%s) = false, want true (should be blocked)", s)
		}
	}

	allowed := []string{
		"8.8.8.8",
		"1.1.1.1",
		"93.184.216.34",      // example.com
		"2606:2800:220:1::1", // public v6
	}
	for _, s := range allowed {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test IP %q", s)
		}
		if isBlockedIP(ip) {
			t.Errorf("isBlockedIP(%s) = true, want false (public address)", s)
		}
	}

	if !isBlockedIP(nil) {
		t.Error("isBlockedIP(nil) should be blocked (fail closed)")
	}
}

func TestSafeHTTPClientBlocksLoopback(t *testing.T) {
	c := SafeHTTPClient(0)
	// Dial loopback directly — the dialer must refuse before connecting.
	resp, err := c.Get("http://127.0.0.1:1/")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("SafeHTTPClient connected to loopback")
	}
}
