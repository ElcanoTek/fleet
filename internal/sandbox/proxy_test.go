package sandbox

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
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

// TestEgressProxyTunnelSurvivesClientHalfClose pins the #1124 fix: a client
// that legally half-closes its WRITE side after sending its request must still
// receive the full response. The upstream here deliberately answers only after
// it has seen the client's FIN (io.ReadAll returns at EOF), so the response can
// only arrive through a proxy that (a) propagates the half-close upstream via
// CloseWrite and (b) keeps the upstream→client direction open after the
// client→upstream copy finished. The old tunnel loop returned on the FIRST
// finished direction and let the deferred Closes kill both, truncating exactly
// this response.
func TestEgressProxyTunnelSurvivesClientHalfClose(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("upstream listen: %v", err)
	}
	defer upstream.Close()
	const reply = "pong-after-half-close"
	go func() {
		conn, aerr := upstream.Accept()
		if aerr != nil {
			return
		}
		defer conn.Close()
		// Block until the client's half-close FIN traverses the tunnel.
		req, _ := io.ReadAll(conn)
		if string(req) == "ping" {
			_, _ = conn.Write([]byte(reply))
		}
	}()
	upstreamPort := upstream.Addr().(*net.TCPAddr).Port

	p := NewEgressProxy()
	// Same test seams as TestEgressProxyTunnel: loopback upstream on a random
	// port, which the production guards rightly refuse.
	p.requirePort = ""
	p.dial = func(ctx context.Context, host, port string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	}
	if err := p.Start(); err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()
	tok, release, err := p.Register([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", p.Port()))
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	// Bound the whole exchange so a regression fails instead of hanging.
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	auth := base64.StdEncoding.EncodeToString([]byte(tok + ":"))
	fmt.Fprintf(conn, "CONNECT 127.0.0.1:%d HTTP/1.1\r\nHost: 127.0.0.1:%d\r\nProxy-Authorization: Basic %s\r\n\r\n", upstreamPort, upstreamPort, auth)
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil || !strings.Contains(status, "200") {
		t.Fatalf("CONNECT status = %q, err = %v; want 200", status, err)
	}
	for { // drain the (empty) response headers up to the blank line
		line, herr := br.ReadString('\n')
		if herr != nil {
			t.Fatalf("read CONNECT headers: %v", herr)
		}
		if line == "\r\n" {
			break
		}
	}

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write request: %v", err)
	}
	// The legal half-close: request fully sent, read side stays open.
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("half-close: %v", err)
	}
	body, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("read response after half-close: %v", err)
	}
	if string(body) != reply {
		t.Fatalf("response after half-close = %q, want %q (truncated by tunnel teardown?)", body, reply)
	}
}

// TestEgressProxyTunnelDrainDeadlineReapsSilentUpstream pins the bound on the
// surviving copy direction: waiting for BOTH directions (the half-close fix)
// must not let a peer that ignores the propagated FIN and never
// responds/closes pin the tunnel forever. The upstream here consumes the
// request and the FIN, then goes silent while holding its conn open; the
// tunnel must still be reaped within the drain window (observed at the client
// as EOF: the fired deadline ends the upstream-read copy, which half-closes
// the client side on its way out).
func TestEgressProxyTunnelDrainDeadlineReapsSilentUpstream(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("upstream listen: %v", err)
	}
	defer upstream.Close()
	upstreamHeld := make(chan net.Conn, 1)
	go func() {
		conn, aerr := upstream.Accept()
		if aerr != nil {
			return
		}
		_, _ = io.ReadAll(conn) // consume "ping" + the propagated FIN…
		upstreamHeld <- conn    // …then go silent, HOLDING the conn open
	}()
	upstreamPort := upstream.Addr().(*net.TCPAddr).Port

	p := NewEgressProxy()
	p.requirePort = ""
	p.dial = func(ctx context.Context, host, port string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	}
	p.drainTimeout = 150 * time.Millisecond // test seam: reap in ms, not 60s
	if err := p.Start(); err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()
	tok, release, err := p.Register([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", p.Port()))
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	auth := base64.StdEncoding.EncodeToString([]byte(tok + ":"))
	fmt.Fprintf(conn, "CONNECT 127.0.0.1:%d HTTP/1.1\r\nHost: 127.0.0.1:%d\r\nProxy-Authorization: Basic %s\r\n\r\n", upstreamPort, upstreamPort, auth)
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil || !strings.Contains(status, "200") {
		t.Fatalf("CONNECT status = %q, err = %v; want 200", status, err)
	}
	for {
		line, herr := br.ReadString('\n')
		if herr != nil {
			t.Fatalf("read CONNECT headers: %v", herr)
		}
		if line == "\r\n" {
			break
		}
	}

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write request: %v", err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("half-close: %v", err)
	}
	// The silent upstream never answers; without the drain deadline this read
	// blocks until the 10s conn deadline (the pinned-forever regression).
	start := time.Now()
	body, err := io.ReadAll(br)
	elapsed := time.Since(start)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			t.Fatalf("tunnel was never reaped: client read hit its own deadline after %v", elapsed)
		}
		// A non-timeout error (e.g. RST from the proxy's Close) is also a reap.
	}
	if len(body) != 0 {
		t.Errorf("silent upstream produced body %q, want none", body)
	}
	if elapsed > 5*time.Second {
		t.Errorf("tunnel reaped after %v, want within the ~150ms drain window", elapsed)
	}
	select {
	case held := <-upstreamHeld:
		_ = held.Close()
	case <-time.After(5 * time.Second):
		t.Error("upstream never observed the FIN/teardown")
	}
}

// TestEgressProxyCloseWriteFallbackFullyCloses pins closeWrite's fallback
// branch: a conn type with no CloseWrite (here a net.Pipe end on the dial
// seam) cannot represent a half-close, so it must be FULLY closed — its peer
// unblocks with EOF — rather than left open waiting for an EOF that never
// comes. Every other proxy test uses *net.TCPConn on both ends, which would
// let a fallback regression (no-op) pass the suite unnoticed.
func TestEgressProxyCloseWriteFallbackFullyCloses(t *testing.T) {
	p := NewEgressProxy()
	p.requirePort = ""
	// Deliberately keep the default 60s drain window: the assertion below must
	// be satisfied by the FALLBACK's full Close, not by the drain deadline.
	peerResult := make(chan error, 1)
	p.dial = func(_ context.Context, _, _ string) (net.Conn, error) {
		us, peer := net.Pipe() // neither end implements CloseWrite
		go func() {
			buf := make([]byte, 4)
			if _, err := io.ReadFull(peer, buf); err != nil {
				peerResult <- fmt.Errorf("read request: %w", err)
				return
			}
			// The client has half-closed; the proxy cannot express that on a
			// pipe, so the fallback must fully close its end — surfacing here
			// as EOF. If the fallback were a no-op this Read parks forever.
			_, err := peer.Read(make([]byte, 1))
			peerResult <- err
			_ = peer.Close()
		}()
		return us, nil
	}
	if err := p.Start(); err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()
	tok, release, err := p.Register([]string{"upstream.test"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", p.Port()))
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	auth := base64.StdEncoding.EncodeToString([]byte(tok + ":"))
	fmt.Fprintf(conn, "CONNECT upstream.test:443 HTTP/1.1\r\nHost: upstream.test:443\r\nProxy-Authorization: Basic %s\r\n\r\n", auth)
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil || !strings.Contains(status, "200") {
		t.Fatalf("CONNECT status = %q, err = %v; want 200", status, err)
	}
	for {
		line, herr := br.ReadString('\n')
		if herr != nil {
			t.Fatalf("read CONNECT headers: %v", herr)
		}
		if line == "\r\n" {
			break
		}
	}

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write request: %v", err)
	}
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("half-close: %v", err)
	}

	select {
	case err := <-peerResult:
		if !errors.Is(err, io.EOF) {
			t.Errorf("pipe peer unblocked with %v, want io.EOF from the fallback's full Close", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pipe peer still blocked: closeWrite's no-CloseWrite fallback did not fully close the conn")
	}
	// The tunnel unwinds fully: the pipe close ends the upstream-read copy,
	// which half-closes the client — EOF here, no drain deadline involved.
	if body, err := io.ReadAll(br); err != nil || len(body) != 0 {
		t.Errorf("client teardown read = (%q, %v), want empty EOF", body, err)
	}
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

// TestEgressProxyTunnelDrainDeadlineIsIdleNotAbsolute pins that the drain
// bound is an IDLE bound: after the client half-closes (arming the drain), an
// upstream that keeps streaming for longer than the whole drain window must not
// be cut off. The pre-fix absolute deadline truncated any allowlisted download
// that outlived the window — silently, since the io.Copy error was discarded.
func TestEgressProxyTunnelDrainDeadlineIsIdleNotAbsolute(t *testing.T) {
	const chunks, chunkEvery = 12, 40 * time.Millisecond // ~480ms total, drain window 150ms
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("upstream listen: %v", err)
	}
	defer upstream.Close()
	go func() {
		conn, aerr := upstream.Accept()
		if aerr != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4)
		_, _ = io.ReadFull(conn, buf) // "ping"
		for i := 0; i < chunks; i++ {
			time.Sleep(chunkEvery)
			if _, werr := fmt.Fprintf(conn, "chunk-%02d\n", i); werr != nil {
				return
			}
		}
	}()
	upstreamPort := upstream.Addr().(*net.TCPAddr).Port

	p := NewEgressProxy()
	p.requirePort = ""
	p.dial = func(ctx context.Context, host, port string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	}
	p.drainTimeout = 150 * time.Millisecond // shorter than the transfer, longer than one chunk gap
	if err := p.Start(); err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()
	tok, release, err := p.Register([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", p.Port()))
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	auth := base64.StdEncoding.EncodeToString([]byte(tok + ":"))
	fmt.Fprintf(conn, "CONNECT 127.0.0.1:%d HTTP/1.1\r\nHost: 127.0.0.1:%d\r\nProxy-Authorization: Basic %s\r\n\r\n", upstreamPort, upstreamPort, auth)
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil || !strings.Contains(status, "200") {
		t.Fatalf("CONNECT status = %q, err = %v; want 200", status, err)
	}
	for {
		line, herr := br.ReadString('\n')
		if herr != nil {
			t.Fatalf("read CONNECT headers: %v", herr)
		}
		if line == "\r\n" {
			break
		}
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write request: %v", err)
	}
	// Half-close arms the drain deadline on the surviving (upstream→client)
	// direction while the upstream is still streaming.
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("half-close: %v", err)
	}
	body, err := io.ReadAll(br)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	got := strings.Count(string(body), "chunk-")
	if got != chunks {
		t.Fatalf("received %d of %d chunks — the drain deadline truncated a progressing transfer:\n%s", got, chunks, body)
	}
}
