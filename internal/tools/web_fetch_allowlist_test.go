package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

	allow := []string{"127.0.0.1"}
	tool := &webFetchTool{
		client:      listed.Client(),
		rateLimiter: newRateLimiter(0),
		allowHost:   func(h string) bool { return sandbox.DomainAllowed(h, allow) },
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
