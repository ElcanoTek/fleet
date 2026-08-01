package sandbox

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestDomainAllowed(t *testing.T) {
	cases := []struct {
		host      string
		allowlist []string
		want      bool
	}{
		{"api.github.com", []string{"api.github.com"}, true},
		{"api.github.com", []string{"*.github.com"}, true},
		{"github.com", []string{"*.github.com"}, true}, // wildcard covers the apex
		{"deep.api.github.com", []string{"*.github.com"}, true},
		{"github.com", []string{"github.com"}, true},
		{"API.GitHub.com", []string{"api.github.com"}, true},  // case-insensitive
		{"api.github.com.", []string{"api.github.com"}, true}, // trailing dot ignored
		{"evil.com", []string{"*.github.com", "pypi.org"}, false},
		{"evilgithub.com", []string{"*.github.com"}, false}, // label-boundary guard
		{"notgithub.com", []string{"*.github.com"}, false},
		{"api.github.com", nil, false},        // empty allowlist denies all
		{"api.github.com", []string{}, false}, // ditto
		{"api.github.com", []string{"  ", ""}, false},
		{"", []string{"github.com"}, false},
	}
	for _, c := range cases {
		if got := domainAllowed(c.host, c.allowlist); got != c.want {
			t.Errorf("domainAllowed(%q, %v) = %v, want %v", c.host, c.allowlist, got, c.want)
		}
	}
}

func TestProxyAuthToken(t *testing.T) {
	mk := func(user string) string {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"))
	}
	if tok, ok := proxyAuthToken(mk("abc123")); !ok || tok != "abc123" {
		t.Errorf("valid basic auth: got (%q,%v)", tok, ok)
	}
	for _, bad := range []string{"", "Bearer x", "Basic !!!notbase64", "Basic " + base64.StdEncoding.EncodeToString([]byte(":nopass"))} {
		if _, ok := proxyAuthToken(bad); ok {
			t.Errorf("proxyAuthToken(%q) accepted, want reject", bad)
		}
	}
}

// TestEgressProxyTunnel exercises the CONNECT proxy end-to-end over localhost
// (no podman): an in-allowlist target tunnels; out-of-allowlist, unknown token,
// and missing auth all fail closed.
func TestEgressProxyTunnel(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "reached-upstream")
	}))
	defer target.Close()
	tu, _ := url.Parse(target.URL)
	targetHost := tu.Hostname() // "127.0.0.1"

	p := NewEgressProxy()
	// Test seams: the httptest upstream is loopback on a random port, which
	// production guards (port-443 pin + blocked-range dial) rightly refuse.
	p.requirePort = ""
	p.dial = func(ctx context.Context, host, port string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	}
	if err := p.Start(); err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	clientThrough := func(token string) *http.Client {
		pu, _ := url.Parse(fmt.Sprintf("http://%s:@127.0.0.1:%d", token, p.Port()))
		return &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				Proxy:           http.ProxyURL(pu),
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		}
	}

	t.Run("allowed host tunnels", func(t *testing.T) {
		tok, release, err := p.Register([]string{targetHost})
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		resp, err := clientThrough(tok).Get(target.URL)
		if err != nil {
			t.Fatalf("allowed request failed: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if string(body) != "reached-upstream" {
			t.Errorf("body = %q, want reached-upstream", body)
		}
	})

	t.Run("out-of-allowlist host blocked", func(t *testing.T) {
		tok, release, err := p.Register([]string{"example.com"}) // not targetHost
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		resp, err := clientThrough(tok).Get(target.URL)
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("expected error for blocked destination, got nil")
		}
	})

	t.Run("unknown token blocked", func(t *testing.T) {
		resp, err := clientThrough("deadbeefnotregistered").Get(target.URL)
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("expected error for unknown token, got nil")
		}
	})

	t.Run("released token blocked", func(t *testing.T) {
		tok, release, err := p.Register([]string{targetHost})
		if err != nil {
			t.Fatal(err)
		}
		release() // drop it before use
		resp, err := clientThrough(tok).Get(target.URL)
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("expected error after token release, got nil")
		}
	})

	t.Run("missing proxy auth blocked", func(t *testing.T) {
		pu, _ := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", p.Port())) // no userinfo
		c := &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				Proxy:           http.ProxyURL(pu),
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		}
		resp, err := c.Get(target.URL)
		if err == nil {
			_ = resp.Body.Close()
			t.Fatal("expected error for missing proxy auth, got nil")
		}
	})

	t.Run("non-CONNECT rejected", func(t *testing.T) {
		// A plain GET directly to the proxy (not via CONNECT) must be refused.
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", p.Port()))
		if err != nil {
			t.Fatalf("dial proxy: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("plain GET status = %d, want 405", resp.StatusCode)
		}
	})

	t.Run("ProxyURLForToken targets slirp gateway", func(t *testing.T) {
		if got := p.ProxyURLForToken("tok"); !strings.Contains(got, slirpHostGateway) || !strings.HasPrefix(got, "http://tok:@") {
			t.Errorf("ProxyURLForToken = %q", got)
		}
	})
}

// The allowlist names DOMAINS, not services: without the port pin, a CONNECT
// to allowed.example.com:22 rides an HTTPS allowlist entry to arbitrary TCP.
// And because the proxy dials from the HOST netns, an allowlist entry that
// resolves to loopback/private space would tunnel the sandbox into 127.0.0.1
// and the LAN — dialPublic must refuse those ranges.
func TestEgressProxyProductionGuards(t *testing.T) {
	p := NewEgressProxy() // production defaults: requirePort=443, dial=dialPublic
	if err := p.Start(); err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	tok, release, err := p.Register([]string{"127.0.0.1", "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	connect := func(target string) int {
		conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", p.Port()))
		if err != nil {
			t.Fatalf("dial proxy: %v", err)
		}
		defer conn.Close()
		auth := base64.StdEncoding.EncodeToString([]byte(tok + ":"))
		fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\n\r\n", target, target, auth)
		resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
		if err != nil {
			t.Fatalf("read CONNECT response: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	t.Run("non-443 port refused even for an allowlisted host", func(t *testing.T) {
		if code := connect("example.com:22"); code != http.StatusForbidden {
			t.Errorf("CONNECT :22 = %d, want 403", code)
		}
	})
	t.Run("loopback target refused even when allowlisted", func(t *testing.T) {
		if code := connect("127.0.0.1:443"); code != http.StatusForbidden {
			t.Errorf("CONNECT 127.0.0.1:443 = %d, want 403", code)
		}
	})
	t.Run("private-range literal refused", func(t *testing.T) {
		if code := connect("10.1.2.3:443"); code != http.StatusForbidden {
			t.Errorf("CONNECT 10.1.2.3:443 = %d, want 403", code)
		}
	})
}
