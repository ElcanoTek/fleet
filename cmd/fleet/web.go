package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/config/envreg"
	"github.com/ElcanoTek/fleet/internal/httpmw"
	"github.com/ElcanoTek/fleet/internal/ms365mcp"
	"github.com/ElcanoTek/fleet/internal/netguard"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/webui"
)

// defaultWebAddr is where the browser tier listens: loopback, the port the
// Next.js tier used, so an existing Caddyfile keeps working unchanged across
// the upgrade (ADR-0091). Caddy (or another TLS proxy) fronts it.
const defaultWebAddr = "127.0.0.1:3000"

// webAddr resolves FLEET_WEB_ADDR: empty → the loopback default, "off" →
// no browser tier (an API-only deployment). Like the two backend listeners
// it never widens to every interface by default.
func webAddr() (addr string, enabled bool) {
	v := strings.TrimSpace(envreg.Get("FLEET_WEB_ADDR"))
	switch strings.ToLower(v) {
	case "":
		return defaultWebAddr, true
	case "off", "none", "disabled", "0":
		return "", false
	}
	return v, true
}

// buildWebServer assembles the public listener: the one chi tree
// (buildPublicRouter) over the browser tier and the in-process chat and
// orchestrator handlers. It returns nil when FLEET_WEB_ADDR=off.
func buildWebServer(cfg *config.Config, b backendHandlers) (*http.Server, error) {
	ms365Handler := wireMS365MCP(cfg)
	addr, enabled := webAddr()
	if !enabled {
		slog.Info("web: browser tier disabled (FLEET_WEB_ADDR=off)")
		return nil, nil
	}
	wc := gatewayConfig(cfg)
	if wc.SessionSecret == "" {
		slog.Warn("WARNING: APP_SESSION_SECRET is not set — password and SSO sign-in are disabled (the session cookie cannot be signed). Set it in the env file; `fleet doctor` repairs it.")
	}
	g, err := webui.New(wc, webui.Options{Chat: b.chat, Orchestrator: b.orch})
	if err != nil {
		return nil, err
	}
	if !g.BundlePresent() {
		slog.Warn("WARNING: this fleet binary carries no web bundle — pages answer 503 until it is rebuilt with `make build` (the API still works)")
	}
	slog.Info("web: bundle loaded", "build_id", g.BuildID())
	surface := b.surface(g)
	surface.ms365 = ms365Handler
	return &http.Server{
		Addr:              addr,
		Handler:           buildPublicRouter(surface),
		ReadHeaderTimeout: 30 * time.Second,
		// Idle keep-alive connections are closed after two minutes; an open
		// SSE stream is never idle, so it is unaffected.
		IdleTimeout: 120 * time.Second,
		// Deliberately no ReadTimeout or WriteTimeout: uploads (bounded by
		// FLEET_UPLOAD_MAX_BYTES in the backend handlers) and SSE turns / large
		// downloads stream for as long as they take, exactly as on the chat
		// and orchestrator listeners, whose handlers this listener serves too.
		// Every body the gateway itself parses is capped where it is read
		// (login: 64 KiB; back-channel logout: maxBackchannelBody); every
		// forwarded body is capped by the backend handler that reads it.
	}, nil
}

// backendHandlers are the chat and orchestrator handlers, and what their own
// listeners serve.
type backendHandlers struct {
	// chat is the chat server's routes (the gateway's in-process backend).
	chat http.Handler
	// orch is the orchestrator router.
	orch http.Handler
	// chatListener and orchListener are the handlers of the chat and
	// orchestrator listeners; nil means chat and orch themselves.
	chatListener, orchListener http.Handler
}

// surface is the public tree's input once the gateway exists.
func (b backendHandlers) surface(gateway http.Handler) httpSurface {
	s := httpSurface{gateway: gateway, orchListener: b.orchListener, chatListener: b.chatListener}
	if s.orchListener == nil {
		s.orchListener = b.orch
	}
	if s.chatListener == nil {
		s.chatListener = b.chat
	}
	if s.orchListener == nil {
		s.orchListener = http.NotFoundHandler()
	}
	if s.chatListener == nil {
		s.chatListener = http.NotFoundHandler()
	}
	if routes, ok := b.orch.(chi.Routes); ok {
		s.orch = routes
	} else {
		// A test double: nothing to walk.
		s.orch = chi.NewRouter()
	}
	return s
}

// gatewayConfig is the gateway's environment configuration plus what the
// server itself decided.
func gatewayConfig(cfg *config.Config) webui.Config {
	wc := webui.ConfigFromEnv()
	// The in-process calls carry their caller as a typed principal in the
	// request context (#2050, ADR-0108): no shared secret is involved.
	// Decided once by config.Load so the gateway and both backends agree.
	wc.PasswordLoginDisabled = cfg.PasswordLoginDisabled
	wc.AccountsManagedByAuth = cfg.AccountsManagedByAuth
	// The same client-address resolver the backends use.
	wc.TrustedProxies = httpmw.NewTrustedProxies(cfg.TrustedProxies)
	return wc
}

// wireMS365MCP constructs the optional built-in Microsoft 365 OAuth + Graph
// MCP handler when MS365_MCP_CLIENT_ID is configured and registers (or clears)
// the in-process netguard dispatch hook.
func wireMS365MCP(cfg *config.Config) http.Handler {
	ms365Cfg := ms365mcp.ConfigFromEnv(cfg)
	if !ms365Cfg.Enabled() {
		netguard.RegisterInProcessHandler("ms365mcp", nil, nil)
		return nil
	}
	h := ms365mcp.NewHandler(ms365Cfg)
	netguard.RegisterInProcessHandler("ms365mcp", ms365Cfg.MatchesInProcessURL, h)
	slog.Info("web: built-in Microsoft 365 MCP enabled",
		"tenant_id", safe.LogSafe(ms365Cfg.TenantID),
		"public_url", safe.LogSafe(ms365Cfg.PublicURL),
		"aliases", len(ms365Cfg.Aliases),
	)
	return h
}

// serveInBackground runs one plain-HTTP listener (the orchestrator's, the
// browser tier's) in the background; nil is a no-op. A listen failure or a
// panic lands on errCh, which triggers the graceful shutdown. Extracted to
// keep run() within the cyclomatic budget.
func serveInBackground(name string, srv *http.Server, errCh chan<- error) {
	if srv == nil {
		return
	}
	go func() {
		defer safe.Recover("cmd."+name, func(any) { errCh <- errors.New(name + ": panicked") })
		slog.Info("server listening", "name", safe.LogSafe(name), "addr", safe.LogSafe(srv.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("%s: %w", name, err)
		}
	}()
}

// printWebBundle prints the embedded web bundle's build id and returns 0, or
// says the binary has none and returns 1 (`fleet version --web`).
func printWebBundle(w io.Writer) int {
	id := webui.EmbeddedBuildID()
	if id == "" {
		_, _ = fmt.Fprintln(w, "web bundle: missing (build with `make build`, which runs `make web` first)")
		return 1
	}
	_, _ = fmt.Fprintln(w, "web bundle: "+id)
	return 0
}
