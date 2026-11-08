package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/logging"
)

// TLS termination for the chat server (the user-facing HTTP/SSE API).
//
// The standard deployment fronts the Next.js app — the ONLY public entrypoint —
// with Caddy (auto-HTTPS) or Tailscale, and the Go chat/orchestrator servers
// bind loopback. FLEET_TLS_MODE=off (the default) preserves that. manual/auto
// let an operator instead terminate TLS directly at the Fleet chat process. The
// orchestrator stays loopback HTTP — it is impersonation-load-bearing and MUST
// stay on 127.0.0.1 (see buildOrchestratorMux), so it is never TLS-served here.
//
// Tailscale Funnel is a distinct architecture (tsnet.Server provides its own
// ListenTLS + cert renewal); it is documented in the README rather than driven
// by FLEET_TLS_MODE.

// tlsActive reports whether the chat server terminates TLS itself.
func tlsActive(cfg *config.Config) bool {
	return cfg.TLSMode == "manual" || cfg.TLSMode == "auto"
}

// securityHeadersMiddleware adds baseline security headers to chat responses.
// HSTS is sent only when TLS is active — over plain HTTP it is a no-op per spec
// and confusing in logs.
func securityHeadersMiddleware(next http.Handler, hsts bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		if hsts {
			w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// serveChat starts the chat HTTP server, terminating TLS when FLEET_TLS_MODE is
// manual/auto. It blocks until the server stops; the caller treats
// http.ErrServerClosed (graceful shutdown) as a clean exit.
func serveChat(server *http.Server, cfg *config.Config) error {
	switch cfg.TLSMode {
	case "manual":
		// Fail fast before binding if the cert/key are missing or unreadable.
		if _, err := tls.LoadX509KeyPair(cfg.TLSCertFile, cfg.TLSKeyFile); err != nil {
			return fmt.Errorf("TLS manual: load cert/key: %w", err)
		}
		server.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		go startRedirectServer(cfg.TLSHTTPAddr, nil)
		slog.Info("chat-server listening (TLS manual)", "addr", server.Addr)
		return server.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
	case "auto":
		warnIfPrivateDomain(cfg.TLSDomain)
		if err := os.MkdirAll(cfg.TLSACMEDir, 0o700); err != nil {
			return fmt.Errorf("TLS auto: create acme cache dir %q: %w", cfg.TLSACMEDir, err)
		}
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(cfg.TLSDomain),
			Cache:      autocert.DirCache(cfg.TLSACMEDir),
			Email:      cfg.TLSACMEEmail,
		}
		server.TLSConfig = m.TLSConfig()
		// :80 serves the ACME HTTP-01 challenge AND redirects everything else.
		go startRedirectServer(cfg.TLSHTTPAddr, m)
		slog.Info("chat-server listening (TLS auto)", "addr", server.Addr, "domain", cfg.TLSDomain)
		return server.ListenAndServeTLS("", "")
	default: // off
		slog.Info("chat-server listening", "addr", server.Addr)
		return server.ListenAndServe()
	}
}

// startRedirectServer runs the plain-HTTP listener that 301-redirects to HTTPS.
// When m is non-nil (auto mode) it also serves the ACME HTTP-01 challenge.
func startRedirectServer(addr string, m *autocert.Manager) {
	if addr == "" {
		addr = ":80"
	}
	var h http.Handler = http.HandlerFunc(redirectToHTTPS)
	if m != nil {
		h = m.HTTPHandler(h)
	}
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("TLS redirect listener failed", "addr", addr, "err", err)
	}
}

// redirectToHTTPS sends a 301 to the https:// form of the requested URL.
func redirectToHTTPS(w http.ResponseWriter, r *http.Request) {
	target := "https://" + stripPort(r.Host) + r.URL.RequestURI()
	//nolint:gosec // G710: standard HTTP→HTTPS upgrade to the SAME Host (scheme-only); not an open redirect to an arbitrary origin.
	// nosemgrep: go.lang.security.injection.open-redirect.open-redirect -- same reasoning as the G710 waiver above: the target is built from r.Host with the scheme forced to https, so it can only ever point back at the host the client already asked for. Not attacker-chosen.
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

// stripPort drops a :port suffix from a Host header (the redirect targets the
// standard HTTPS port).
func stripPort(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// warnIfPrivateDomain logs a prominent warning when an auto-mode domain resolves
// to a private/loopback address — the public ACME HTTP-01 challenge will fail.
// It still proceeds (split-horizon DNS, operator knows best).
func warnIfPrivateDomain(domain string) {
	// domain is operator config (FLEET_TLS_DOMAIN), not request input — a
	// one-shot startup diagnostic lookup, bounded by a short timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(ctx, domain)
	if err != nil || len(addrs) == 0 {
		return // let autocert surface DNS errors naturally
	}
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && (ip.IsPrivate() || ip.IsLoopback()) {
			logging.From(ctx).Warn("tls: FLEET_TLS_MODE=auto but the domain resolves to a private/loopback address; the ACME HTTP-01 challenge will likely fail — use FLEET_TLS_MODE=manual for internal deployments", "domain", domain, "resolved", a)
			return
		}
	}
}
