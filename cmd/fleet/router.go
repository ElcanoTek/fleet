package main

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/ElcanoTek/fleet/internal/webui"
)

// The one HTTP surface (#2050 phase 3, ADR-0110): one chi tree on the public
// listener (FLEET_WEB_ADDR) serves every family, browser and API traffic
// alike, so a front proxy can send the whole origin to one port.
//
//	/v1, /v1/*                      orchestrator: the public API (ADR-0053),
//	                                byte for byte what the orchestrator
//	                                listener answers
//	/api-info, /.well-known/agent-card.json, /a2a, /a2a/*, /triggers/*
//	                                orchestrator: the unversioned-forever paths
//	/webhooks/*                     chat: the signed inbound webhooks, what
//	                                the chat listener answers
//	/api/orchestrator, /api/orchestrator/*
//	                                orchestrator, the browser alias: the SAME
//	                                router as /v1, behind the gateway's browser
//	                                boundary (webui/orchestrator_mount.go)
//	/*                              webui: the SPA, sign-in, and the chat
//	                                browser routes
//
// Each family keeps its exact middleware chain. A mounted family routes the
// request afresh (familyMount), exactly as it does on its own listener: its
// chain, its metric labels and its path-based exemptions see the same path on
// every mount, and the parent tree adds no middleware of its own. Nothing here
// sets a read or write deadline (see buildWebServer), so the chat SSE streams
// and long polls keep the no-timeout behaviour they have on every listener.
//
// The orchestrator and chat listeners stay up on their loopback ports,
// serving the very same handlers: the box Caddyfile (scripts/lib/caddyfile.sh)
// still sends /v1 and /webhooks to them, and `fleet chat`, `fleet acp`,
// fleet-bench and the admin CLI dial them. Retiring them is a front-proxy
// change, not a router change (ADR-0110).

// httpSurface is what the public tree is assembled from.
type httpSurface struct {
	// gateway is the browser tier (internal/webui).
	gateway http.Handler
	// orch is the orchestrator router, walked for the contract and mounted
	// for the browser.
	orch chi.Routes
	// orchListener and chatListener are what the orchestrator and chat
	// listeners serve (the router behind the health probes and
	// apiversion.Router, and for chat the security headers). The public paths
	// of each family are served by exactly these handlers.
	orchListener http.Handler
	chatListener http.Handler
}

// orchestratorPublicPaths are the unversioned-forever orchestrator paths
// (docs/api-versioning.md) a front proxy exposes besides /v1 (ADR-0053).
var orchestratorPublicPaths = []string{"/api-info", "/.well-known/agent-card.json", "/a2a", "/a2a/*", "/triggers/*"}

// chatPublicPaths are the chat paths a front proxy exposes.
var chatPublicPaths = []string{"/webhooks/*"}

// orchestratorV1Prefix is the public API's mount point (Family.Prefix).
const orchestratorV1Prefix = "/v1"

// buildPublicRouter assembles the one tree.
func buildPublicRouter(s httpSurface) *chi.Mux {
	r := chi.NewRouter()
	gateway := freshRouting(s.gateway)

	r.Mount(orchestratorV1Prefix, familyMount{routes: s.orch, h: s.orchListener})
	for _, p := range orchestratorPublicPaths {
		r.Handle(p, freshRouting(s.orchListener))
	}
	for _, p := range chatPublicPaths {
		r.Handle(p, freshRouting(s.chatListener))
	}
	// The browser alias: the gateway gates it (session, CSRF, the backend
	// contract) and serves it from the same router.
	r.Mount(webui.OrchestratorPrefix, familyMount{routes: s.orch, h: s.gateway})
	r.Handle("/*", gateway)
	// A method chi does not know is the gateway's to answer, as it was when
	// the gateway was the listener's handler.
	r.MethodNotAllowed(gateway.ServeHTTP)
	r.NotFound(gateway.ServeHTTP)
	return r
}

// familyMount mounts a family: it lists the family's routes to chi.Walk (so
// the contract generator and the alias test see them under the mount point)
// and serves with the family's own handler, routed afresh.
type familyMount struct {
	routes chi.Routes
	h      http.Handler
}

var _ chi.Routes = familyMount{}

func (m familyMount) Routes() []chi.Route          { return m.routes.Routes() }
func (m familyMount) Middlewares() chi.Middlewares { return m.routes.Middlewares() }
func (m familyMount) Match(rctx *chi.Context, method, path string) bool {
	return m.routes.Match(rctx, method, path)
}
func (m familyMount) Find(rctx *chi.Context, method, path string) string {
	return m.routes.Find(rctx, method, path)
}

func (m familyMount) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.h.ServeHTTP(w, r.WithContext(withoutRouteContext(r.Context())))
}

// freshRouting serves h with the parent tree's routing state hidden.
func freshRouting(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r.WithContext(withoutRouteContext(r.Context())))
	})
}

// withoutRouteContext hides a parent chi router's routing state, so a chi
// router served with the returned context routes the request from scratch
// on its own path, exactly as it does as a listener's handler.
func withoutRouteContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, chi.RouteCtxKey, (*chi.Context)(nil))
}
