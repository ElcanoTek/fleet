package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ElcanoTek/fleet/internal/netguard"
)

// slirpHostGateway is the address at which a rootless-Podman container reaches a
// service bound to the host's loopback, when the container is launched with
// `--network=slirp4netns:allow_host_loopback=true` (see networkArgs). The egress
// proxy binds 127.0.0.1 on the host; the container dials it here.
const slirpHostGateway = "10.0.2.2"

// EgressProxy is the host-side HTTP CONNECT proxy that enforces the sandbox
// network egress allowlist (#211). It is the enforcement point for "allowlisted"
// network mode: the sandbox runs on slirp4netns (full transport) with
// HTTPS_PROXY/HTTP_PROXY pointed here, and this proxy permits a CONNECT tunnel
// only to a host on the per-turn allowlist.
//
// IMPORTANT — this is a BEST-EFFORT control, not a hard jail. It constrains
// tools that honor the proxy environment (pip, curl, most HTTP libraries). A
// process inside the sandbox that opens a raw socket, or ignores the proxy env,
// still has full slirp4netns egress. For the adversarial-exfiltration threat
// model, lockdown (`--network=none`) remains the hard seal. See
// docs/adr/0012-sandbox-egress-allowlist.md.
//
// Per-turn tokens prevent cross-turn tunneling: each TakeContainerWithEgress
// registers a fresh random token bound to that turn's allowlist; the container's
// proxy URL carries the token as basic-auth userinfo, so a container can only
// reach the domains its own turn was granted, and a stale/guessed token is
// rejected.
type EgressProxy struct {
	srv  *http.Server
	port int

	// dial and requirePort are the production egress guards, held as fields
	// ONLY so tests can tunnel to an httptest server (loopback, random port)
	// without weakening the shipped defaults: dialPublic refuses blocked
	// address ranges and "443" pins CONNECT to HTTPS.
	dial        func(ctx context.Context, host, port string) (net.Conn, error)
	requirePort string

	// drainTimeout bounds how long the SECOND copy direction of a tunnel may
	// keep running after the first finished (#1124 follow-up). Waiting for
	// both directions is what preserves half-close semantics, but with no
	// bound a silent peer that ignores the propagated FIN and never
	// responds/closes would park the remaining io.Copy in Read forever —
	// pinning the tunnel's goroutines and both FDs for the process lifetime
	// (the old first-EOF teardown never leaked, it just truncated). A field
	// only as a test seam (like dial); zero means defaultTunnelDrainTimeout.
	drainTimeout time.Duration

	mu     sync.RWMutex
	tokens map[string][]string // token -> allowlist (lowercased domain patterns)
}

// defaultTunnelDrainTimeout is how long the surviving copy direction may keep
// running after the other finished. Generous on purpose: a cooperating peer
// that received the FIN finishes its response well inside it, and 60s is the
// same order as the dial/read timeouts elsewhere in this file; the point is
// only that a silent peer cannot pin the tunnel forever.
const defaultTunnelDrainTimeout = 60 * time.Second

// NewEgressProxy returns a not-yet-listening proxy. Call Start to bind + serve.
func NewEgressProxy() *EgressProxy {
	return &EgressProxy{
		tokens:      make(map[string][]string),
		dial:        dialPublic,
		requirePort: "443",
	}
}

// tunnelDrainTimeout resolves the drain-deadline seam (zero → default).
func (p *EgressProxy) tunnelDrainTimeout() time.Duration {
	if p.drainTimeout > 0 {
		return p.drainTimeout
	}
	return defaultTunnelDrainTimeout
}

// Start binds the proxy to 127.0.0.1 on an ephemeral port and serves in a
// background goroutine. The bind is synchronous so Port is valid on return.
func (p *EgressProxy) Start() error {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("egress proxy listen: %w", err)
	}
	p.port = ln.Addr().(*net.TCPAddr).Port
	p.srv = &http.Server{
		Handler:           http.HandlerFunc(p.handle),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := p.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("sandbox: egress proxy serve error: %v", err)
		}
	}()
	return nil
}

// Port is the host loopback port the proxy listens on (0 until Start succeeds).
func (p *EgressProxy) Port() int { return p.port }

// Shutdown stops the proxy, bounded by ctx.
func (p *EgressProxy) Shutdown(ctx context.Context) error {
	if p.srv == nil {
		return nil
	}
	return p.srv.Shutdown(ctx)
}

// Register binds a fresh random token to allowlist for one turn and returns the
// token plus a release func that drops it. The allowlist is copied + lowercased
// so later caller mutation can't change what the proxy enforces.
func (p *EgressProxy) Register(allowlist []string) (token string, release func(), err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", nil, fmt.Errorf("egress proxy token: %w", err)
	}
	token = hex.EncodeToString(b)

	norm := make([]string, 0, len(allowlist))
	for _, a := range allowlist {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
			norm = append(norm, a)
		}
	}
	p.mu.Lock()
	p.tokens[token] = norm
	p.mu.Unlock()

	return token, func() {
		p.mu.Lock()
		delete(p.tokens, token)
		p.mu.Unlock()
	}, nil
}

// ProxyURLForToken is the HTTP(S)_PROXY value injected into a container for the
// given turn token. The token rides as basic-auth userinfo so standard HTTP
// clients send it as Proxy-Authorization automatically. The host is the
// slirp4netns host-loopback gateway, reachable from the sandbox.
func (p *EgressProxy) ProxyURLForToken(token string) string {
	return fmt.Sprintf("http://%s:@%s:%d", token, slirpHostGateway, p.port)
}

// errBlockedUpstream marks a CONNECT target that resolved only to blocked
// (loopback/private/link-local/metadata) address space.
var errBlockedUpstream = errors.New("egress proxy: destination resolves to a blocked address range")

// dialPublic resolves host, rejects blocked ranges (netguard's list), and
// dials the exact validated IP — so a concurrent re-resolution (DNS rebinding)
// can't swap a blocked address in between check and connect. The proxy dials
// from the HOST network namespace, which is precisely why a private-range
// target must never be reachable through it.
func dialPublic(ctx context.Context, host, port string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if ip := net.ParseIP(host); ip != nil {
		if netguard.IsBlockedIP(ip) {
			return nil, errBlockedUpstream
		}
		return dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", host, err)
	}
	var lastErr error
	for _, ipa := range ips {
		if netguard.IsBlockedIP(ipa.IP) {
			lastErr = errBlockedUpstream
			continue
		}
		conn, derr := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ipa.IP.String(), port))
		if derr != nil {
			lastErr = derr
			continue
		}
		return conn, nil
	}
	if lastErr == nil {
		lastErr = errBlockedUpstream
	}
	return nil, lastErr
}

// handle implements the CONNECT proxy: authenticate the per-turn token, check
// the CONNECT target host against that token's allowlist, then tunnel raw bytes.
func (p *EgressProxy) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		// This proxy only does HTTPS CONNECT tunneling; plain-HTTP forward
		// proxying would terminate at us and is intentionally unsupported.
		http.Error(w, "only CONNECT is supported", http.StatusMethodNotAllowed)
		return
	}

	token, ok := proxyAuthToken(r.Header.Get("Proxy-Authorization"))
	if !ok {
		w.Header().Set("Proxy-Authenticate", `Basic realm="fleet-egress"`)
		http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
		return
	}
	p.mu.RLock()
	allowlist, known := p.tokens[token]
	p.mu.RUnlock()
	if !known {
		http.Error(w, "unrecognized proxy token", http.StatusForbidden)
		return
	}

	host := r.Host
	port := "443"
	if h, pt, err := net.SplitHostPort(r.Host); err == nil {
		host, port = h, pt
	}
	// The allowlist names DOMAINS, not services: without pinning the port,
	// `CONNECT allowed.example.com:22` rides an HTTPS allowlist entry to
	// arbitrary TCP. This proxy exists for HTTPS tunneling only.
	if p.requirePort != "" && port != p.requirePort {
		http.Error(w, "egress proxy tunnels HTTPS (port 443) only", http.StatusForbidden)
		return
	}
	if !domainAllowed(host, allowlist) {
		http.Error(w, "destination not permitted by this sandbox's egress allowlist", http.StatusForbidden)
		return
	}

	upstream, err := p.dial(r.Context(), host, port)
	if err != nil {
		if errors.Is(err, errBlockedUpstream) {
			// An allowlist entry resolving to loopback/private/link-local space
			// would otherwise turn this proxy — which dials from the HOST
			// netns — into a tunnel to 127.0.0.1 and the LAN.
			http.Error(w, "destination resolves to a blocked address range", http.StatusForbidden)
			return
		}
		http.Error(w, "upstream dial failed", http.StatusBadGateway)
		return
	}
	defer upstream.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "proxy does not support hijacking", http.StatusInternalServerError)
		return
	}
	client, _, err := hj.Hijack()
	if err != nil {
		return
	}
	defer client.Close()

	// After hijack we own the raw conn: acknowledge the tunnel, then splice.
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	// Splice both directions and wait for BOTH to finish (#1124). Returning on
	// the first EOF used to let the deferred Closes tear down the whole tunnel,
	// truncating the response of a client that legally half-closes its write
	// side after sending its request. Instead, each finished copy direction
	// propagates the half-close to its destination (closeWrite), so the peer
	// sees the FIN while the opposite direction keeps flowing; the deferred
	// Closes then only reap fully-drained conns. An abrupt failure (RST, dead
	// upstream) still unblocks both copies: each ends when its source errors or
	// its destination refuses the write.
	//
	// The surviving direction is BOUNDED: when one direction finishes, a drain
	// deadline is armed on both conns (read+write — the survivor may be parked
	// in either; only the survivor is affected, the finisher is done). Without
	// it, a peer that ignores the propagated FIN and never responds/closes
	// (e.g. an LB-held keep-alive) would park the remaining Read forever,
	// pinning this handler's goroutines and both FDs for the process lifetime
	// — container teardown closes only the CLIENT side, which ends the
	// client-read direction but not an upstream-read parked against a silent
	// upstream. Cooperating peers are unaffected (they finish well inside the
	// window); a silent one is reaped when the deadline fires and its blocked
	// Read/Write returns.
	armDrainDeadline := func() {
		dl := time.Now().Add(p.tunnelDrainTimeout())
		_ = client.SetDeadline(dl)
		_ = upstream.SetDeadline(dl)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(upstream, client); closeWrite(upstream); armDrainDeadline() }()
	go func() { defer wg.Done(); _, _ = io.Copy(client, upstream); closeWrite(client); armDrainDeadline() }()
	wg.Wait()
}

// closeWrite half-closes conn's write side when the concrete type supports it —
// *net.TCPConn (both tunnel ends in production) and *tls.Conn expose
// CloseWrite() error — so the peer of a finished copy direction sees EOF
// without the other direction being torn down. A conn type with no CloseWrite
// cannot represent a half-close, and leaving its peer waiting for an EOF that
// never comes would hang the tunnel, so it is fully closed instead (the
// pre-#1124 teardown semantics for that end).
func closeWrite(conn net.Conn) {
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = conn.Close()
}

// proxyAuthToken extracts the token (the basic-auth username) from a
// Proxy-Authorization header value. The password half is unused.
func proxyAuthToken(header string) (string, bool) {
	const prefix = "Basic "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header[len(prefix):]))
	if err != nil {
		return "", false
	}
	user, _, _ := strings.Cut(string(dec), ":")
	if user == "" {
		return "", false
	}
	return user, true
}

// domainAllowed reports whether host matches any allowlist entry. Entries are
// exact ("api.github.com") or "*."-prefixed wildcards ("*.github.com", which
// matches the apex github.com AND any subdomain). Matching is case-insensitive
// and a single trailing dot on host is ignored. The leading "." in the computed
// suffix is the label boundary, so "*.github.com" does NOT match "evilgithub.com".
// An empty allowlist denies everything (an allowlisted turn with no entries can
// reach nothing — equivalent to lockdown for cooperating clients).
func domainAllowed(host string, allowlist []string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" {
		return false
	}
	for _, entry := range allowlist {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if strings.HasPrefix(entry, "*.") {
			base := entry[2:]
			if host == base || strings.HasSuffix(host, "."+base) {
				return true
			}
			continue
		}
		if host == entry {
			return true
		}
	}
	return false
}
