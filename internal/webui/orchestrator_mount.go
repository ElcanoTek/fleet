// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
)

// OrchestratorPrefix is where the browser reaches the Operations Center: the
// SAME orchestrator router external clients reach at /v1 (ADR-0109 §2,
// ADR-0110). Every route the router registers answers here too; there is no
// per-route table to keep in step with it.
const OrchestratorPrefix = "/api/orchestrator"

// routeFinder is the part of a chi router the mount needs: the pattern a
// method and path resolve to, or "" when none does (chi.Mux.Find).
type routeFinder interface {
	Find(rctx *chi.Context, method, path string) string
}

// orchQueryPolicy is the query string the browser alias forwards, per
// orchestrator route ("METHOD /pattern" as the router registers it). It is the
// part of the retired passthrough table that was not a plain prefix strip:
//
//   - a listed route forwards only its allow-listed parameters (passThroughQuery);
//   - a route listed with nil forwards no query at all, as its row did;
//   - a route not listed forwards the query verbatim, exactly as /v1 does. These
//     are the routes the browser never had a row for.
//
// TestOrchestratorQueryPolicyNamesRealRoutes (cmd/fleet) holds every key to a
// route the real router registers, so a rename cannot silently drop a policy.
var orchQueryPolicy = map[string][]string{
	"GET /admin/budgets":                                nil,
	"POST /admin/budgets":                               nil,
	"DELETE /admin/budgets/{budget_id}":                 nil,
	"GET /admin/usage":                                  {"group_by", "from", "to", "format"},
	"GET /admin/usage/adoption":                         {"from", "to", "format"},
	"GET /api/config":                                   nil,
	"GET /api/me":                                       nil,
	"GET /logs/{task_id}":                               nil,
	"GET /logs/{task_id}/subagents/{child_session_id}":  nil,
	"GET /mcp-servers":                                  nil,
	"GET /prompts":                                      nil,
	"POST /prompts":                                     nil,
	"PUT /prompts/{prompt_id}":                          nil,
	"DELETE /prompts/{prompt_id}":                       nil,
	"GET /sla-report":                                   {"days"},
	"GET /stats":                                        nil,
	"GET /task-templates":                               nil,
	"GET /tasks":                                        {"limit", "offset", "status", "q", "scheduled_only", "completed_today", "completed_status", "created_by", "tag"},
	"POST /tasks":                                       nil,
	"POST /tasks/estimate":                              nil,
	"GET /tasks/recipients":                             nil,
	"GET /tasks/tags":                                   nil,
	"GET /tasks/upcoming":                               {"limit"},
	"GET /tasks/{task_id}":                              nil,
	"PUT /tasks/{task_id}":                              nil,
	"DELETE /tasks/{task_id}":                           nil,
	"DELETE /tasks/{task_id}/permanent":                 nil,
	"POST /tasks/{task_id}/rerun":                       nil,
	"POST /tasks/{task_id}/wake":                        nil,
	"POST /upload":                                      nil,
}

// OrchestratorQueryPolicyRoutes lists the orchestrator routes the browser
// alias filters the query of (for the cross-package drift test).
func OrchestratorQueryPolicyRoutes() []string {
	out := make([]string, 0, len(orchQueryPolicy))
	for k := range orchQueryPolicy {
		out = append(out, k)
	}
	return out
}

// orchRenames are the browser paths that are not the router's path under the
// prefix. Both keep answering at their router path under the prefix too.
var orchRenames = map[string]string{
	"GET " + OrchestratorPrefix + "/config": "/api/config",
	"GET " + OrchestratorPrefix + "/me":     "/api/me",
}

func (g *Gateway) registerOrchestrator(mux *http.ServeMux) {
	// The mount: everything under the prefix is the orchestrator router.
	mux.HandleFunc(OrchestratorPrefix+"/", func(w http.ResponseWriter, r *http.Request) {
		sub := strings.TrimPrefix(r.URL.Path, OrchestratorPrefix)
		g.serveOrchestrator(w, r, sub)
	})
	// The bare prefix is no route (and must not be redirected to the subtree).
	mux.HandleFunc(OrchestratorPrefix, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	})
	for pattern, sub := range orchRenames {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) { g.serveOrchestrator(w, r, sub) })
	}
	// The gateway's own routes under the prefix.
	mux.HandleFunc("POST "+OrchestratorPrefix+"/tasks/{taskId}/discuss", g.handleDiscuss)
	mux.HandleFunc("GET "+OrchestratorPrefix+"/auth/elcano-login", func(w http.ResponseWriter, r *http.Request) {
		g.elcanoLoginRedirect(w, r, "/orchestrator")
	})
}

// serveOrchestrator serves one browser request from the orchestrator router at
// sub (the router's own path). The router is called directly with the
// browser's request, its body streaming through untouched; what the gateway
// adds is the browser boundary the passthrough rows had, unchanged:
//
//  1. a path the router does not register is the gateway's JSON 404 (405 with
//     Allow when another method is registered), and reaches nothing;
//  2. CSRF on a mutating verb (bearer clients included — they must send an
//     Origin), then the credential: an explicit bearer, else the cookie
//     session as a typed principal; neither is a 401;
//  3. the backend contract of backend.go: a fresh header set (the bearer, and
//     the Content-Type of a mutating request), a body only on a mutating verb,
//     the loopback peer, and the per-route query policy (orchQueryPolicy);
//  4. the forwarded-header response (passthroughJSON), including clearing the
//     cookie on a revoked-session verdict.
//
// The router routes the request afresh on sub, exactly as the orchestrator
// listener and the /v1 mount do, so its middleware chain, metric labels and
// path-based exemptions see the same path on every mount.
func (g *Gateway) serveOrchestrator(w http.ResponseWriter, r *http.Request, sub string) {
	if g.orch == nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "orchestrator unreachable: no backend"})
		return
	}
	escaped := sub
	if raw := r.URL.RawPath; raw != "" {
		if rest, ok := strings.CutPrefix(raw, OrchestratorPrefix); ok && sub == strings.TrimPrefix(r.URL.Path, OrchestratorPrefix) {
			escaped = rest
		}
	}
	pattern, found := g.findOrchestratorRoute(r.Method, escaped)
	if !found {
		g.orchestratorNotFound(w, r, escaped)
		return
	}
	mutating := isMutating(r.Method)
	if mutating && !g.verifyOrigin(w, r) {
		return
	}
	auth, ok := g.resolveOrchAuth(r)
	if !ok {
		unauthorized(w)
		return
	}

	h := g.orchHeaders(auth)
	var body io.ReadCloser = http.NoBody
	var n int64
	if mutating {
		if r.Body != nil && r.Body != http.NoBody {
			body, n = r.Body, r.ContentLength
		}
		if ct := r.Header.Get("Content-Type"); ct != "" {
			h.Set("Content-Type", ct)
		}
	}
	query := r.URL.RawQuery
	if allowed, listed := orchQueryPolicy[r.Method+" "+pattern]; listed && pattern != "" {
		query = strings.TrimPrefix(passThroughQuery(r, allowed...), "?")
	}

	u := &url.URL{Scheme: "http", Host: "127.0.0.1", Path: sub, RawQuery: query}
	if escaped != sub {
		u.RawPath = escaped
	}
	ctx := backendContext(r.Context(), auth.who)
	// A fresh routing context: the router routes on sub, not on whatever a
	// parent router left in the request.
	ctx = withoutRouteContext(ctx)
	req := &http.Request{
		Method:        r.Method,
		URL:           u,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        h,
		Body:          body,
		ContentLength: n,
		Host:          "127.0.0.1",
		RemoteAddr:    backendRemoteAddr,
		RequestURI:    u.RequestURI(),
	}
	if body == http.NoBody {
		req.ContentLength = 0
	}
	g.serveBackend(w, r, g.orch, req.WithContext(ctx), passthroughJSON)
}

// findOrchestratorRoute reports the router pattern method+sub resolves to. A
// router that cannot be searched (a test double) is assumed to serve it.
func (g *Gateway) findOrchestratorRoute(method, sub string) (string, bool) {
	f, ok := g.orch.(routeFinder)
	if !ok {
		return "", true
	}
	if p := f.Find(chi.NewRouteContext(), method, sub); p != "" {
		return p, true
	}
	// ServeMux matched GET rows for HEAD; the router answers HEAD itself.
	if method == http.MethodHead {
		if p := f.Find(chi.NewRouteContext(), http.MethodGet, sub); p != "" {
			return p, true
		}
	}
	return "", false
}

// orchestratorNotFound is notFoundAPI for the mount: 405 with Allow when the
// browser path answers another method, else the JSON 404. A browser path
// under the prefix answers a method when either the router registers sub
// for it or one of the gateway's own patterns under the prefix does (the
// renames, discuss, the live stream, the workspace download, elcano-login).
// Asking only the router would turn a wrong method on a gateway-owned path
// into a 404; asking only the gateway would do the same to every router path.
func (g *Gateway) orchestratorNotFound(w http.ResponseWriter, r *http.Request, sub string) {
	f, _ := g.orch.(routeFinder)
	var allow []string
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if m == r.Method {
			continue
		}
		probe := m
		if m == http.MethodHead {
			probe = http.MethodGet
		}
		if (f != nil && f.Find(chi.NewRouteContext(), probe, sub) != "") || g.gatewayOwnsUnderPrefix(r, m) {
			allow = append(allow, m)
		}
	}
	if len(allow) > 0 {
		w.Header().Set("Allow", strings.Join(allow, ", "))
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
}

// gatewayOwnsUnderPrefix reports whether one of the gateway's own patterns
// under the prefix (not the mount itself) serves r's path for method.
func (g *Gateway) gatewayOwnsUnderPrefix(r *http.Request, method string) bool {
	probe := r.Clone(r.Context())
	probe.Method = method
	_, pat := g.api.Handler(probe)
	switch pat {
	case "", "/api/", OrchestratorPrefix, OrchestratorPrefix + "/":
		return false
	}
	return true
}

// withoutRouteContext hides a parent chi router's routing state, so a chi
// router served with the returned context routes the request from scratch.
func withoutRouteContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, chi.RouteCtxKey, (*chi.Context)(nil))
}
