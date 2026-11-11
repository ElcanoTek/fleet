package playwrightrunner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestGuardProxyExplainsRefusals: the guard proxy tells a refused address
// from an unreachable one, in the page a plain-HTTP navigation shows and in
// the note explain adds to the ERR_TUNNEL_CONNECTION_FAILED error Chromium
// reports for a refused HTTPS tunnel. No browser needed.
func TestGuardProxyExplainsRefusals(t *testing.T) {
	t.Parallel()
	dial := func(_ context.Context, _, addr string) (net.Conn, error) {
		host, _, _ := net.SplitHostPort(addr)
		switch host {
		case "blocked.test", "img.test", "127.0.0.2":
			return nil, errBlockedAddress
		default:
			return nil, fmt.Errorf("resolve %q: %w", host, &net.DNSError{Err: "no such host", Name: host, Server: "127.0.0.53:53", IsNotFound: true})
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	refused := &proxyRefusals{}
	addr, closeProxy, err := startSSRFGuardProxy(ctx, dial, refused)
	if err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	defer closeProxy()
	proxyURL, _ := url.Parse("http://" + addr)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: 5 * time.Second}
	get := func(target string) (int, string) {
		t.Helper()
		resp, err := client.Get(target)
		if err != nil {
			return 0, err.Error()
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	// Plain HTTP: the browser shows the proxy's answer as the page.
	if code, body := get("http://blocked.test/"); code != http.StatusForbidden ||
		body != "Blocked by Fleet: blocked.test:80 resolves to a private or internal network address, which Fleet's browser does not open.\n" {
		t.Errorf("blocked plain-HTTP page = %d %q", code, body)
	}
	if code, body := get("http://missing.test/"); code != http.StatusBadGateway ||
		body != "Fleet's browser could not reach missing.test:80: the name does not resolve.\n" {
		t.Errorf("unreachable plain-HTTP page = %d %q", code, body)
	}

	// HTTPS: the note goes after the first line of the browser's error and
	// names the failing URL's host, not the other refusals around it.
	_, _ = get("https://img.test/a.png")
	mark := refused.mark()
	_, _ = get("https://blocked.test/")
	_, _ = get("https://127.0.0.2/x")
	navErr := "Page.goto: net::ERR_TUNNEL_CONNECTION_FAILED at https://blocked.test/\nCall log:\n  - navigating to \"https://blocked.test/\""
	want := "Page.goto: net::ERR_TUNNEL_CONNECTION_FAILED at https://blocked.test/ (blocked.test:443 resolves to a private or internal network address, which Fleet's browser does not open)\nCall log:\n  - navigating to \"https://blocked.test/\""
	if got := refused.explain(navErr, mark); got != want {
		t.Errorf("explain =\n%q\nwant\n%q", got, want)
	}
	if got := refused.explain("Page.goto: net::ERR_TUNNEL_CONNECTION_FAILED at https://127.0.0.2/x", 0); !strings.HasSuffix(got, "(127.0.0.2:443 is a private or internal network address, which Fleet's browser does not open)") {
		t.Errorf("explain for an IP literal = %q", got)
	}
	// A URL that matches no refusal (it failed after a redirect) gets the
	// most recent one since the mark.
	if got := refused.explain("Page.goto: net::ERR_TUNNEL_CONNECTION_FAILED at https://start.test/", mark); !strings.Contains(got, "(127.0.0.2:443 is a private") {
		t.Errorf("explain fallback = %q", got)
	}
	// Refusals before the mark are not this command's.
	if got := refused.explain("net::ERR_TUNNEL_CONNECTION_FAILED at https://img.test/a.png", refused.mark()); strings.Contains(got, "(") {
		t.Errorf("explain with nothing refused since the mark = %q", got)
	}
	if got := refused.explain("waiting for selector \"#pw\" failed: timeout", 0); got != "waiting for selector \"#pw\" failed: timeout" {
		t.Errorf("explain changed an error that is not a refused tunnel: %q", got)
	}
	// An unresolvable host is "could not reach", with the reason in words and
	// never the resolver's address.
	_, _ = get("https://missing.test/")
	if got := refused.explain("net::ERR_TUNNEL_CONNECTION_FAILED at https://missing.test/", 0); !strings.HasSuffix(got, "(Fleet's browser could not reach missing.test:443: the name does not resolve)") || strings.Contains(got, "127.0.0.53") {
		t.Errorf("explain for an unresolvable host = %q", got)
	}

	var nilLog *proxyRefusals
	if got := nilLog.explain(navErr, nilLog.mark()); got != navErr {
		t.Errorf("a nil log changed the message: %q", got)
	}
}

// TestProxyRefusalsKeepTheMostRecentPerHost: the log holds the latest
// maxProxyRefusals hosts, one entry each, so a page that keeps retrying
// blocked hosts cannot crowd out the refusal of the URL that actually failed.
func TestProxyRefusalsKeepTheMostRecentPerHost(t *testing.T) {
	t.Parallel()
	p := &proxyRefusals{}
	for i := 0; i < 50; i++ {
		p.add("retry.test:443", errBlockedAddress)
	}
	for i := 0; i < 2*maxProxyRefusals; i++ {
		p.add(fmt.Sprintf("noise%d.test:443", i), errBlockedAddress)
	}
	mark := p.mark()
	p.add("target.test:443", errBlockedAddress)
	if len(p.list) != maxProxyRefusals {
		t.Fatalf("log holds %d entries, want %d", len(p.list), maxProxyRefusals)
	}
	got := p.explain("net::ERR_TUNNEL_CONNECTION_FAILED at https://target.test/", mark)
	if !strings.HasSuffix(got, "(target.test:443 resolves to a private or internal network address, which Fleet's browser does not open)") {
		t.Errorf("explain after the log filled up = %q", got)
	}
	for _, r := range p.list {
		if r.host == "retry.test:443" {
			t.Errorf("the oldest host is still in a full log")
		}
	}
}

func TestDialFailureReasonsAreWords(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("resolve %q: %w", "x", &net.DNSError{Err: "no such host", Server: "10.96.0.10:53", IsNotFound: true}), "the name does not resolve"},
		{fmt.Errorf("resolve %q: %w", "x", &net.DNSError{Err: "i/o timeout", Server: "10.96.0.10:53", IsTimeout: true}), "looking the name up timed out"},
		{&net.OpError{Op: "dial", Net: "tcp", Err: &net.AddrError{Err: "boom"}}, "the connection failed"},
		{errBlockedAddress, ""},
	} {
		if got := dialFailureReason(tc.err); got != tc.want {
			t.Errorf("dialFailureReason(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// TestGuardProxyDoesNotRecordAbandonedRequests: a request the browser drops
// before the upstream answers (it navigated away) is not a refusal. The
// client here is a raw connection that sends the request and closes, so the
// proxy sees it cancelled, as it does when Chromium cancels a subresource.
// The upstream dial fails on its own after a moment (a CONNECT's dial sees
// the cancellation earlier; a plain request's RoundTrip returns as soon as
// its context ends), and neither may be recorded.
func TestGuardProxyDoesNotRecordAbandonedRequests(t *testing.T) {
	t.Parallel()
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		select {
		case <-ctx.Done():
		case <-time.After(300 * time.Millisecond):
		}
		return nil, errors.New("connect: connection refused")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	refused := &proxyRefusals{}
	addr, closeProxy, err := startSSRFGuardProxy(ctx, dial, refused)
	if err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	defer closeProxy()
	for _, req := range []string{
		"CONNECT slow.test:443 HTTP/1.1\r\nHost: slow.test:443\r\n\r\n",
		"GET http://slow.test/ HTTP/1.1\r\nHost: slow.test\r\n\r\n",
	} {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial proxy: %v", err)
		}
		if _, err := conn.Write([]byte(req)); err != nil {
			t.Fatalf("write %q: %v", req, err)
		}
		time.Sleep(50 * time.Millisecond)
		_ = conn.Close()
	}
	// Both handlers are done well within this: the dial gives up at 300 ms.
	time.Sleep(time.Second)
	if got := refused.mark(); got != 0 {
		t.Errorf("abandoned requests were recorded (mark = %d)", got)
	}
}
