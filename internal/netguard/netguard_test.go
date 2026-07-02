package netguard

import (
	"net"
	"testing"
)

// TestIsBlockedIP is the single regression matrix for the shared SSRF IP
// classifier: every internal / special-purpose range an SSRF-guarded dialer
// must refuse, plus ordinary public addresses that must be allowed. It also
// pins the two behaviours that the previously-drifted copies disagreed on —
// all-multicast (only mcpoauth had it) and the RFC test/benchmark/reserved
// nets (only tools had them) — and the fail-closed nil case.
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
		"100.64.0.1",             // CGNAT (RFC 6598)
		"100.100.100.200",        // CGNAT cloud-metadata (Alibaba/Oracle)
		"224.0.0.1",              // link-local multicast
		"233.1.1.1",              // global multicast (drift: only mcpoauth blocked it)
		"ff02::1",                // interface/link-local multicast v6
		"192.0.2.1",              // TEST-NET-1 (drift: only tools blocked it)
		"198.18.0.1",             // RFC 2544 benchmark (drift: only tools blocked it)
		"240.0.0.1",              // reserved (drift: only tools blocked it)
		"255.255.255.255",        // broadcast (in 240.0.0.0/4)
		"::ffff:127.0.0.1",       // IPv4-mapped loopback (must not slip through)
		"::ffff:169.254.169.254", // IPv4-mapped metadata
		"::ffff:100.100.100.200", // IPv4-mapped CGNAT metadata
	}
	for _, s := range blocked {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test IP %q", s)
		}
		if !IsBlockedIP(ip) {
			t.Errorf("IsBlockedIP(%s) = false, want true (should be blocked)", s)
		}
	}

	allowed := []string{
		"8.8.8.8",
		"1.1.1.1",
		"93.184.216.34",                      // example.com
		"2606:2800:220:1:248:1893:25c8:1946", // public v6
	}
	for _, s := range allowed {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test IP %q", s)
		}
		if IsBlockedIP(ip) {
			t.Errorf("IsBlockedIP(%s) = true, want false (public address)", s)
		}
	}

	if !IsBlockedIP(nil) {
		t.Error("IsBlockedIP(nil) should be blocked (fail closed)")
	}
}
