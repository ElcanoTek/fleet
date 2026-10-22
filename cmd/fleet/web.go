package main

import (
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/config"
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
	// config.Validate guarantees is set.
	wc.ChatToken = cfg.SharedToken
	if wc.OrchestratorToken == "" {
		wc.OrchestratorToken = cfg.SharedToken
	}
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
		// No WriteTimeout: SSE turns and large downloads stream for as long as
		// they take, exactly as on the chat listener.
	}, nil
}
