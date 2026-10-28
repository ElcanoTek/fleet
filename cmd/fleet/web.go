package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/config"
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
	v := strings.TrimSpace(os.Getenv("FLEET_WEB_ADDR"))
	switch strings.ToLower(v) {
	case "":
		return defaultWebAddr, true
	case "off", "none", "disabled", "0":
		return "", false
	}
	return v, true
}

// buildWebServer assembles the browser tier over the in-process chat and
// orchestrator handlers. It returns nil when FLEET_WEB_ADDR=off.
func buildWebServer(cfg *config.Config, chat, orch http.Handler) (*http.Server, error) {
	addr, enabled := webAddr()
	if !enabled {
		log.Printf("web: browser tier disabled (FLEET_WEB_ADDR=off)")
		return nil, nil
	}
	wc := webui.ConfigFromEnv()
	// The in-process channel uses the server's own shared secret, which
	// config.Validate guarantees is set — for BOTH backends, unconditionally:
	// they verify the header-trust token against that same secret, so any
	// other value (a leftover ORCHESTRATOR_SERVER_TOKEN from the retired Next
	// tier's env, which an old bootstrap even set to ADMIN_API_KEY) would make
	// every orchestrator call a 403.
	wc.ChatToken = cfg.SharedToken
	wc.OrchestratorToken = cfg.SharedToken
	// Decided once by config.Load so the gateway and both backends agree.
	wc.PasswordLoginDisabled = cfg.PasswordLoginDisabled
	wc.AccountsManagedByAuth = cfg.AccountsManagedByAuth
	if wc.SessionSecret == "" {
		log.Printf("WARNING: APP_SESSION_SECRET is not set — password and SSO sign-in are disabled (the session cookie cannot be signed). Set it in the env file; `fleet doctor` repairs it.")
	}
	g, err := webui.New(wc, webui.Options{Chat: chat, Orchestrator: orch})
	if err != nil {
		return nil, err
	}
	if !g.BundlePresent() {
		log.Printf("WARNING: this fleet binary carries no web bundle — pages answer 503 until it is rebuilt with `make build` (the API still works)")
	}
	log.Printf("web: bundle %s", g.BuildID())
	return &http.Server{
		Addr:              addr,
		Handler:           g,
		ReadHeaderTimeout: 30 * time.Second,
		// Idle keep-alive connections are closed after two minutes; an open
		// SSE stream is never idle, so it is unaffected.
		IdleTimeout: 120 * time.Second,
		// Deliberately no ReadTimeout or WriteTimeout: uploads (bounded by
		// FLEET_UPLOAD_MAX_BYTES in the backend handlers) and SSE turns / large
		// downloads stream for as long as they take, exactly as on the chat
		// listener. Every body the gateway itself parses is capped where it
		// is read (login: 64 KiB; back-channel logout: maxBackchannelBody);
		// every forwarded body is capped by the backend handler that reads it.
	}, nil
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
		//nolint:gosec // G706: name is a constant and Addr is operator config (a listen address), not request input; %q quotes it.
		log.Printf("%s listening on %q", name, srv.Addr)
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
