package mcpoauth

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/netguard"
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
		if !netguard.IsBlockedIP(ip) {
			t.Errorf("netguard.IsBlockedIP(%s) = false, want true (should be blocked)", s)
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
		if netguard.IsBlockedIP(ip) {
			t.Errorf("netguard.IsBlockedIP(%s) = true, want false (public address)", s)
		}
	}

	if !netguard.IsBlockedIP(nil) {
		t.Error("netguard.IsBlockedIP(nil) should be blocked (fail closed)")
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

// TestSafeMCPDataPlaneHTTPClientIsBoundedByTheCallNotAClock: the remote MCP
// data-plane client must let a slow tool run for its whole per-call budget.
// A remote vendor that answered tools/call only after 30s was cut off by the
// control-plane client's fixed Timeout ("Client.Timeout exceeded while
// awaiting headers") whatever the call was allowed. The request context is
// the only bound: a slow answer inside it succeeds, one past it fails.
func TestSafeMCPDataPlaneHTTPClientIsBoundedByTheCallNotAClock(t *testing.T) {
	// Global registry: not parallel.
	const host = "slow-vendor.dataplane.test"
	netguard.RegisterInProcessHandler("dataplane-test", func(u *url.URL) bool { return u.Host == host },
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(200 * time.Millisecond): // headers only after the "control" timeout
			case <-r.Context().Done():
				return
			}
			_, _ = w.Write([]byte("ok"))
		}))
	t.Cleanup(func() { netguard.RegisterInProcessHandler("dataplane-test", nil, nil) })

	get := func(c *http.Client, budget time.Duration) error {
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+host+"/mcp", nil)
		if err != nil {
			return err
		}
		resp, err := c.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, err = io.ReadAll(resp.Body)
		return err
	}

	// The control plane keeps its clock: the same slow answer is refused.
	if err := get(SafeHTTPClient(50*time.Millisecond), 5*time.Second); err == nil {
		t.Fatal("control-plane client waited past its own timeout")
	}
	dp := SafeMCPDataPlaneHTTPClient()
	if dp.Timeout != 0 {
		t.Fatalf("data-plane client Timeout = %v, want none (the call's context bounds it)", dp.Timeout)
	}
	if err := get(dp, 5*time.Second); err != nil {
		t.Fatalf("data-plane client gave up on a slow answer inside the call budget: %v", err)
	}
	if err := get(dp, 50*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("data-plane client past the call budget: err = %v, want context.DeadlineExceeded", err)
	}
}

func TestSafeMCPDataPlaneHTTPClientKeepsTheGuards(t *testing.T) {
	c := SafeMCPDataPlaneHTTPClient()
	resp, err := c.Get("http://127.0.0.1:1/")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("data-plane client connected to loopback")
	}
	if c.CheckRedirect == nil || c.CheckRedirect(nil, nil) == nil {
		t.Fatal("data-plane client must refuse every redirect")
	}
}
