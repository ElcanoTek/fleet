package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIsPrivateIP covers the SSRF allow/deny classifier directly: every
// internal range the network tools must refuse, plus a couple of public
// addresses that must be allowed.
func TestIsPrivateIP(t *testing.T) {
	cases := []struct {
		ip      string
		private bool
	}{
		// Blocked: loopback, private RFC1918, link-local, the cloud
		// metadata endpoint, and the unspecified address.
		{"127.0.0.1", true},
		{"::1", true},
		{"10.0.0.5", true},
		{"172.16.0.1", true},
		{"192.168.1.1", true},
		{"169.254.0.1", true},
		{"169.254.169.254", true}, // cloud metadata
		{"0.0.0.0", true},
		{"fc00::1", true}, // IPv6 unique-local (IsPrivate)
		{"fe80::1", true}, // IPv6 link-local
		// Allowed: ordinary public addresses.
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"93.184.216.34", false}, // example.com
		{"2606:2800:220:1:248:1893:25c8:1946", false},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("ParseIP(%q) returned nil", c.ip)
		}
		if got := isPrivateIP(ip); got != c.private {
			t.Errorf("isPrivateIP(%s) = %v, want %v", c.ip, got, c.private)
		}
	}
}

// TestSSRFGuardedDialerControl exercises the dialer's Control hook in
// isolation — no network is touched. The hook is what blocks a hostname
// that resolves (or DNS-rebinds) to an internal IP: it runs against the
// resolved address, so a private resolution is rejected and a public one
// is allowed.
func TestSSRFGuardedDialerControl(t *testing.T) {
	dialer := newSSRFGuardedDialer()
	if dialer.Control == nil {
		t.Fatal("guarded dialer has no Control hook")
	}

	// A hostname that resolves to a loopback/private IP must be blocked.
	if err := dialer.Control("tcp", "127.0.0.1:80", nil); err == nil {
		t.Error("expected Control to block loopback address, got nil")
	} else if !strings.Contains(err.Error(), "access to private IP denied") {
		t.Errorf("expected SSRF denial error, got %v", err)
	}

	// The cloud metadata endpoint must be blocked.
	if err := dialer.Control("tcp", "169.254.169.254:80", nil); err == nil {
		t.Error("expected Control to block cloud metadata endpoint, got nil")
	}

	// A public address must be allowed through.
	if err := dialer.Control("tcp", "8.8.8.8:443", nil); err != nil {
		t.Errorf("expected Control to allow public address, got %v", err)
	}
}

// TestWebFetchSSRFBlocksLoopback proves the guard, not a connection
// failure, is what blocks the fetch: the SAME loopback httptest server
// is reachable with a plain client but refused by the SSRF-guarded
// client NewWebFetchTool builds.
func TestWebFetchSSRFBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "sensitive internal data")
	}))
	defer srv.Close()

	// Guarded client (production wiring): must be blocked.
	guarded := &webFetchTool{
		client: &http.Client{
			Timeout:   DefaultTimeout,
			Transport: &http.Transport{DialContext: newSSRFGuardedDialer().DialContext},
		},
		cache:       newFetchCache(),
		rateLimiter: newRateLimiter(0),
	}
	raw, err := guarded.run(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("unexpected hard error: %v", err)
	}
	var blocked webFetchResult
	if err := json.Unmarshal([]byte(raw), &blocked); err != nil {
		t.Fatalf("guarded result not JSON: %v", err)
	}
	if blocked.Error == "" {
		t.Fatalf("expected SSRF error, got successful fetch with body: %s", blocked.Stdout)
	}
	if !strings.Contains(blocked.Error, "access to private IP denied") {
		t.Errorf("expected SSRF denial message, got: %s", blocked.Error)
	}

	// Unguarded client (no SSRF Control): the same server is reachable,
	// confirming the block above was the guard and not a dead port.
	allowed := &webFetchTool{
		client:      &http.Client{Timeout: DefaultTimeout},
		cache:       newFetchCache(),
		rateLimiter: newRateLimiter(0),
	}
	raw, err = allowed.run(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("unguarded fetch hard error: %v", err)
	}
	var ok webFetchResult
	if err := json.Unmarshal([]byte(raw), &ok); err != nil {
		t.Fatalf("unguarded result not JSON: %v", err)
	}
	if ok.Error != "" {
		t.Fatalf("expected unguarded fetch to succeed, got error: %s", ok.Error)
	}
	if !strings.Contains(ok.Stdout, "sensitive internal data") {
		t.Errorf("expected body from reachable server, got: %q", ok.Stdout)
	}
}

// TestDownloadURLSSRFBlocksLoopback proves download_url enforces the same
// guard: with the production dialer the loopback server is refused; with
// a plain dialer the same server downloads fine.
func TestDownloadURLSSRFBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "sensitive internal data")
	}))
	defer srv.Close()

	// downloadCtx swaps in a plain dialer; reinstate the production guard
	// for the blocked half so we test the real wiring.
	ctx, _ := downloadCtx(t)

	prev := downloadURLDialContext
	downloadURLDialContext = newSSRFGuardedDialer().DialContext
	blocked := runDownloadURL(ctx, DownloadURLParams{URL: srv.URL + "/secret.txt"})
	downloadURLDialContext = prev // restore the unguarded dialer downloadCtx installed

	if blocked.Status != downloadStatusError {
		t.Fatalf("expected SSRF error status, got %q (saved_to=%s)", blocked.Status, blocked.SavedTo)
	}
	if !strings.Contains(blocked.Error, "access to private IP denied") {
		t.Errorf("expected SSRF denial message, got: %s", blocked.Error)
	}

	// Now with the unguarded dialer (downloadCtx default): same server works.
	allowed := runDownloadURL(ctx, DownloadURLParams{URL: srv.URL + "/secret.txt"})
	if allowed.Status != downloadStatusSuccess {
		t.Fatalf("expected unguarded download to succeed, got status=%q err=%q", allowed.Status, allowed.Error)
	}
}
