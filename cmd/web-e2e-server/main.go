// Command web-e2e-server boots fleet's browser tier (internal/webui) ALONE,
// with no chat-server or orchestrator behind it, for the mocked Playwright
// suite (web/e2e/mocked). Every /api/* call the specs care about is
// intercepted in the browser by Playwright; what this binary contributes is
// the real thing on the other side: the embedded SPA, the request gate, both
// session cookies, CSP and the security headers, the server-side redirects.
// An /api route that does reach it answers 502 "unreachable", the same as the
// Next.js harness did with its backend URLs pointing at nothing.
//
// A test harness, like cmd/fake-llm: never deployed, and nothing in the
// release path builds it.
package main

import (
	"errors"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/ElcanoTek/fleet/internal/webui"
)

func main() {
	addr := os.Getenv("FLEET_WEB_ADDR")
	if addr == "" {
		addr = "127.0.0.1:3100"
	}
	cfg := webui.ConfigFromEnv()
	g, err := webui.New(cfg, webui.Options{})
	if err != nil {
		log.Fatalf("web-e2e-server: %v", err)
	}
	if !g.BundlePresent() {
		log.Fatalf("web-e2e-server: no web bundle embedded — run `make web` (cd web && npm run build) before building this harness")
	}
	srv := &http.Server{Addr: addr, Handler: g, ReadHeaderTimeout: 30 * time.Second}
	//nolint:gosec // G706: addr is the harness's own config (a listen address), not request input; %q quotes it.
	log.Printf("web-e2e-server: bundle %s listening on %q", g.BuildID(), addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
