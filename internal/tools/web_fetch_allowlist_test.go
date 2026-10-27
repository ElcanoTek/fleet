package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/sandbox"
)

// A web_fetch held to an egress allowlist refuses an unlisted host before any
// request is made, and refuses a redirect from a listed host to an unlisted
// one (ADR-0094). The SSRF dialer is swapped for the test server's client so
// the loopback test servers are reachable; the host policy under test is the
// one production installs.
func TestWebFetchHonorsEgressAllowlist(t *testing.T) {
	var unlistedHits int
	unlisted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		unlistedHits++
		_, _ = w.Write([]byte("secret"))
	}))
	defer unlisted.Close()
	// "localhost" is a different HOST from "127.0.0.1" for the allowlist.
	unlistedURL := strings.Replace(unlisted.URL, "127.0.0.1", "localhost", 1)

	listed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bounce" {
			http.Redirect(w, r, unlistedURL+"/x", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("hello"))
	}))
	defer listed.Close()

	// The plumbing under test is "the policy is consulted on the first URL
	// and on every redirect hop"; the loopback test servers speak plain HTTP
	// on random ports, so this test's policy checks the host only. The real
	// policy's scheme/port/host rule is pinned by TestEgressURLPolicy.
	allow := []string{"127.0.0.1"}
	tool := &webFetchTool{
		client:      listed.Client(),
		rateLimiter: newRateLimiter(0),
		allowURL: func(u *neturl.URL) error {
			if !sandbox.DomainAllowed(u.Hostname(), allow) {
				return fmt.Errorf("%s refused: the host is not on this run's egress allowlist", u.Hostname())
			}
			return nil
		},
	}
	tool.client.CheckRedirect = tool.checkRedirect

	if _, err := tool.run(context.Background(), unlistedURL+"/x"); err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("unlisted host: err = %v, want an allowlist refusal", err)
	}

	raw, err := tool.run(context.Background(), listed.URL+"/ok")
	if err != nil {
		t.Fatalf("listed host: %v", err)
	}
	var res webFetchResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil || res.Stdout != "hello" {
		t.Fatalf("listed host result = %+v (%v), want hello", res, err)
	}

	raw, err = tool.run(context.Background(), listed.URL+"/bounce")
	if err != nil {
		t.Fatalf("bounce: unexpected hard error: %v", err)
	}
	if err := json.Unmarshal([]byte(raw), &res); err != nil || !strings.Contains(res.Error, "allowlist") {
		t.Fatalf("redirect to an unlisted host = %+v (%v), want an allowlist refusal", res, err)
	}
	if unlistedHits != 0 {
		t.Fatalf("the unlisted host was contacted %d time(s)", unlistedHits)
	}
}

// The exported constructor wires the same policy: an empty allowlist refuses
// everything, through the tool's public Run path.
func TestNewWebFetchToolForHostsEmptyAllowlistRefuses(t *testing.T) {
	tool := NewWebFetchToolForHosts(nil)
	if tool.Info().Name != WebFetchToolName {
		t.Fatalf("name = %q", tool.Info().Name)
	}
	resp, err := tool.Run(context.Background(), fantasy.ToolCall{Input: `{"url":"https://example.com/"}`})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.IsError || !strings.Contains(resp.Content, "allowlist") {
		t.Fatalf("resp = %+v, want an allowlist refusal", resp)
	}
}

// The allowlisted-mode policy matches the egress proxy: HTTPS only, port 443
// only (explicit or implied), and an allowlisted host. The proxy tunnels
// nothing else, so the host-side fetch must not either — otherwise an
// allowlisted domain would be reachable over plain HTTP or on any port.
func TestEgressURLPolicy(t *testing.T) {
	policy := egressURLPolicy([]string{"*.example.com"})
	for raw, wantOK := range map[string]bool{
		"https://api.example.com/x":      true,
		"https://api.example.com:443/x":  true,
		"HTTPS://example.com/":           true,
		"http://api.example.com/x":       false, // scheme
		"http://api.example.com:443/x":   false, // scheme, even on 443
		"https://api.example.com:8443/x": false, // port
		"https://api.example.com:22/":    false, // port
		"ftp://api.example.com/":         false,
		"https://evilexample.com/":       false, // host
		"https://example.net/":           false,
	} {
		u, err := neturl.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := policy(u); (err == nil) != wantOK {
			t.Errorf("policy(%s) = %v, want ok=%v", raw, err, wantOK)
		}
	}
}

// A redirect from an allowed HTTPS URL to the same host over plain HTTP, or
// to another port, is refused by the real policy on the redirect hop.
func TestWebFetchRedirectHeldToSchemeAndPort(t *testing.T) {
	tool := &webFetchTool{allowURL: egressURLPolicy([]string{"api.example.com"})}
	via := []*http.Request{{URL: &neturl.URL{Scheme: "https", Host: "api.example.com"}}}
	for _, raw := range []string{"http://api.example.com/x", "https://api.example.com:8443/x"} {
		u, _ := neturl.Parse(raw)
		if err := tool.checkRedirect(&http.Request{URL: u}, via); err == nil {
			t.Errorf("redirect to %s allowed, want refused", raw)
		}
	}
	u, _ := neturl.Parse("https://api.example.com/next")
	if err := tool.checkRedirect(&http.Request{URL: u}, via); err != nil {
		t.Errorf("redirect to an allowed HTTPS URL refused: %v", err)
	}
}
