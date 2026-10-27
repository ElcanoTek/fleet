// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// The browser API. Every /api/* path the Next.js tier served is registered
// here with the SAME method set, upstream path, query forwarding, body rule
// and response shape it had (the per-route .ts handlers this replaced are
// recorded in ADR-0091's inventory). An unregistered /api path is a JSON 404,
// never index.html; a registered path with the wrong method is a 405.
//
// Most routes are table rows: chatRoute for chat-server, orchRoute for the
// orchestrator. The ones with logic of their own (login, SSO, streams, file
// downloads, the model catalogs, the brand assets) are handlers in the
// neighbouring files.

// bodyRule is how a chat route forwards the browser's request body.
type bodyRule int

const (
	// noBody: the route sent no body upstream (and ignored the browser's).
	noBody bodyRule = iota
	// jsonBodyIfAny: the browser body, labelled application/json when present.
	jsonBodyIfAny
	// jsonBodyAlways: the browser body with Content-Type: application/json set
	// even when empty (routes that set the header explicitly).
	jsonBodyAlways
)

// chatRoute is one table-driven chat-server passthrough.
type chatRoute struct {
	pattern string
	target  func(r *http.Request) string
	body    bodyRule
	out     out
}

// p returns the encoded path value — the route handlers' encodeURIComponent of
// a decoded dynamic segment.
func p(r *http.Request, name string) string { return encodeURIComponent(r.PathValue(name)) }

// fixed is a target with no dynamic part.
func fixed(path string) func(*http.Request) string {
	return func(*http.Request) string { return path }
}

// withRawQuery appends the browser's query string verbatim (the routes that
// forwarded request.nextUrl.search).
func withRawQuery(path string, r *http.Request) string {
	if r.URL.RawQuery == "" {
		return path
	}
	return path + "?" + r.URL.RawQuery
}

// passThroughQuery forwards only the allow-listed params, every value of a
// repeated one, empty values dropped, in allow-list order — the
// orchestrator proxy's helper (api/orchestrator/_lib/proxy.ts).
func passThroughQuery(r *http.Request, allowed ...string) string {
	q := r.URL.Query()
	out := url.Values{}
	var keys []string
	for _, k := range allowed {
		for _, v := range q[k] {
			if v != "" {
				if _, seen := out[k]; !seen {
					keys = append(keys, k)
				}
				out[k] = append(out[k], v)
			}
		}
	}
	if len(keys) == 0 {
		return ""
	}
	// url.Values.Encode sorts keys; the TS helper emitted allow-list order.
	s := ""
	for _, k := range keys {
		for _, v := range out[k] {
			if s != "" {
				s += "&"
			}
			s += url.QueryEscape(k) + "=" + url.QueryEscape(v)
		}
	}
	return "?" + s
}

func isMutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// handleChatRoute is the session-gated chat passthrough: CSRF on a mutating
// verb, then the session, then the in-process call.
func (g *Gateway) handleChatRoute(rt chatRoute) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if isMutating(r.Method) && !g.verifyOrigin(w, r) {
			return
		}
		s := g.session(r)
		if s == nil {
			unauthorized(w)
			return
		}
		h := g.chatHeaders(identityOf(s))
		c := call{method: r.Method, target: rt.target(r), header: h, detach: true}
		switch rt.body {
		case noBody:
		case jsonBodyIfAny:
			c.body, c.contentLength = jsonBody(r, h)
		case jsonBodyAlways:
			c.body, c.contentLength = bodyOrNil(r)
			h.Set("Content-Type", "application/json")
		}
		g.dispatch(w, r, g.chat, "chat-server", c, rt.out)
	}
}

// orchRoute is one proxyToOrchestrator row.
type orchRoute struct {
	pattern string
	target  func(r *http.Request) string
}

// resolveOrchAuth is the orchestrator credential: an explicit bearer wins,
// else the cookie session.
func (g *Gateway) resolveOrchAuth(r *http.Request) (orchAuth, bool) {
	if tok := bearerToken(r); tok != "" {
		return orchAuth{bearer: tok}, true
	}
	if s := g.session(r); s != nil {
		return orchAuth{id: identityOf(s)}, true
	}
	return orchAuth{}, false
}

// handleOrchRoute is proxyToOrchestrator: CSRF on a mutating verb (bearer
// clients included — they must send an Origin), the credential, then the
// body and its Content-Type on a mutating verb, the forwarded-header response.
func (g *Gateway) handleOrchRoute(rt orchRoute) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		c := call{method: r.Method, target: rt.target(r), header: h}
		if mutating {
			if r.Body != nil && r.Body != http.NoBody {
				c.body, c.contentLength = r.Body, r.ContentLength
			}
			if ct := r.Header.Get("Content-Type"); ct != "" {
				h.Set("Content-Type", ct)
			}
		}
		g.dispatch(w, r, g.orch, "orchestrator", c, passthroughJSON)
	}
}

// notFoundAPI answers an /api path no route claimed: 405 (with Allow) when the
// path exists under another method, else a JSON 404 — never index.html. The
// catch-all outranks ServeMux's own 405, so the method probe happens here.
func (g *Gateway) notFoundAPI(w http.ResponseWriter, r *http.Request) {
	var allow []string
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if m == r.Method {
			continue
		}
		probe := r.Clone(r.Context())
		probe.Method = m
		if _, pat := g.api.Handler(probe); pat != "" && pat != "/api/" {
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

var projectFocusID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (g *Gateway) registerAPI(mux *http.ServeMux) {
	mux.HandleFunc("/api/", g.notFoundAPI)

	conv := func(r *http.Request) string { return "/conversations/" + p(r, "id") }
	textOK := out{headers: contentTypeOnly, ctFallback: "application/json", okOn204: true}
	jsonForced := out{headers: contentTypeOnly, ctForce: "application/json"}
	jsonForcedOK := out{headers: contentTypeOnly, ctForce: "application/json", okOn204: true}
	textPlainFallback := out{headers: contentTypeOnly, ctFallback: "text/plain"}

	chat := []chatRoute{
		// ── admin ──
		{"GET /api/admin/decision-models", fixed("/admin/decision-models"), noBody, textJSON},
		{"GET /api/admin/doctor", func(r *http.Request) string {
			if r.URL.Query().Get("deep") == "1" {
				return "/admin/doctor?deep=1"
			}
			return "/admin/doctor"
		}, noBody, textJSON},
		{"POST /api/admin/guardrail/test", fixed("/admin/guardrail/test"), noBody, passthroughJSON},
		{"GET /api/admin/health-summary", fixed("/admin/health-summary"), noBody, textJSON},
		{"GET /api/admin/llm-providers", fixed("/admin/llm-providers"), noBody, textJSON},
		{"POST /api/admin/llm-providers", fixed("/admin/llm-providers"), jsonBodyIfAny, textJSON},
		{"PUT /api/admin/llm-providers/{id}", func(r *http.Request) string { return "/admin/llm-providers/" + p(r, "id") }, jsonBodyIfAny, textJSON},
		{"DELETE /api/admin/llm-providers/{id}", func(r *http.Request) string { return "/admin/llm-providers/" + p(r, "id") }, noBody, textJSON},
		{"POST /api/admin/llm-providers/{id}/test", func(r *http.Request) string { return "/admin/llm-providers/" + p(r, "id") + "/test" }, noBody, textJSON},
		{"GET /api/admin/notify-settings", fixed("/admin/notify-settings"), noBody, passthroughJSON},
		{"PUT /api/admin/notify-settings", fixed("/admin/notify-settings"), jsonBodyIfAny, passthroughJSON},
		{"DELETE /api/admin/notify-settings", fixed("/admin/notify-settings"), noBody, passthroughJSON},
		{"POST /api/admin/notify-settings/test", fixed("/admin/notify-settings/test"), jsonBodyIfAny, passthroughJSON},
		{"GET /api/admin/server-stats", fixed("/admin/server-stats"), noBody, textJSON},
		{"GET /api/admin/settings", fixed("/admin/settings"), noBody, passthroughJSON},
		{"PUT /api/admin/settings/{key}", func(r *http.Request) string { return "/admin/settings/" + p(r, "key") }, jsonBodyIfAny, passthroughJSON},
		{"DELETE /api/admin/settings/{key}", func(r *http.Request) string { return "/admin/settings/" + p(r, "key") }, noBody, passthroughJSON},
		{"GET /api/admin/stats", fixed("/admin/stats"), noBody, textJSON},
		{"GET /api/admin/storage", fixed("/admin/storage"), noBody, passthroughJSON},
		{"POST /api/admin/storage/cleanup", fixed("/admin/storage/cleanup"), jsonBodyAlways, passthroughJSON},
		{"POST /api/admin/teams/rename", fixed("/admin/teams/rename"), jsonBodyAlways, textJSON},
		{"GET /api/admin/users", fixed("/admin/users"), noBody, textJSON},
		{"POST /api/admin/users", fixed("/admin/users"), jsonBodyIfAny, textJSON},
		{"PATCH /api/admin/users/{email}", func(r *http.Request) string { return "/admin/users/" + p(r, "email") }, jsonBodyIfAny, textJSON},
		{"DELETE /api/admin/users/{email}", func(r *http.Request) string { return "/admin/users/" + p(r, "email") }, noBody, textJSON},
		{"PUT /api/admin/users/{email}/password", func(r *http.Request) string { return "/admin/users/" + p(r, "email") + "/password" }, jsonBodyIfAny, textJSON},

		// ── workspace-wide reads ──
		{"GET /api/client-config", fixed("/client-config"), noBody, textJSON},
		{"GET /api/server-config", fixed("/server-config"), noBody, textJSON},
		{"GET /api/personas", fixed("/personas"), noBody, textJSON},
		{"GET /api/mcp-servers", fixed("/mcp-servers"), noBody, textJSON},
		{"GET /api/mcp-catalog", fixed("/mcp-catalog"), noBody, textJSON},
		{"GET /api/llm-provider-models", fixed("/llm-provider-models"), noBody, textJSON},
		{"GET /api/search", func(r *http.Request) string { return withRawQuery("/search", r) }, noBody, textJSON},
		{"GET /api/skills", fixed("/skills"), noBody, textJSON},
		{"GET /api/skills/{name}", func(r *http.Request) string { return "/skills/" + p(r, "name") }, noBody, textJSON},
		{"GET /api/user-skills", fixed("/user-skills"), noBody, textJSON},
		{"POST /api/user-skills", fixed("/user-skills"), jsonBodyAlways, textJSON},
		{"PUT /api/user-skills/{id}", func(r *http.Request) string { return "/user-skills/" + p(r, "id") }, jsonBodyAlways, textJSON},
		{"DELETE /api/user-skills/{id}", func(r *http.Request) string { return "/user-skills/" + p(r, "id") }, noBody, textJSON},
		{"GET /api/connector-prefs", fixed("/connector-prefs"), noBody, textJSON},
		{"PUT /api/connector-prefs", fixed("/connector-prefs"), jsonBodyAlways, textJSON},
		{"DELETE /api/connector-prefs", func(r *http.Request) string {
			q := r.URL.Query()
			return "/connector-prefs?kind=" + encodeURIComponent(q.Get("kind")) + "&id=" + encodeURIComponent(q.Get("id"))
		}, noBody, textJSON},
		{"GET /api/me/team", fixed("/me/team"), noBody, textJSON},
		{"PUT /api/me/team", fixed("/me/team"), jsonBodyIfAny, textJSON},

		// ── conversations ──
		{"GET /api/conversations", func(r *http.Request) string {
			q := r.URL.Query()
			switch {
			case q.Get("scope") == "team":
				return "/conversations?scope=team"
			case q.Get("archived") == "true":
				return "/conversations?archived=true"
			}
			return "/conversations"
		}, noBody, textJSON},
		{"POST /api/conversations", fixed("/conversations"), jsonBodyIfAny, textJSON},
		{"DELETE /api/conversations", fixed("/conversations"), jsonBodyIfAny, textJSON},
		{"PATCH /api/conversations/bulk", fixed("/conversations/bulk"), jsonBodyIfAny, textJSON},
		{"GET /api/conversations/{id}", conv, noBody, textJSON},
		{"DELETE /api/conversations/{id}", conv, noBody, textOK},
		{"POST /api/conversations/{id}/approval-groups/{groupId}", func(r *http.Request) string {
			return conv(r) + "/approval-groups/" + p(r, "groupId")
		}, jsonBodyIfAny, textJSON},
		{"GET /api/conversations/{id}/approvals/{approvalId}", func(r *http.Request) string {
			return conv(r) + "?approval_id=" + p(r, "approvalId")
		}, noBody, textJSON},
		{"POST /api/conversations/{id}/approvals/{approvalId}", func(r *http.Request) string {
			return conv(r) + "/approvals/" + p(r, "approvalId")
		}, jsonBodyIfAny, textJSON},
		{"POST /api/conversations/{id}/approvals/{approvalId}/stop", func(r *http.Request) string {
			return conv(r) + "/approvals/" + p(r, "approvalId") + "/stop"
		}, noBody, textJSON},
		{"POST /api/conversations/{id}/archive", func(r *http.Request) string { return conv(r) + "/archive" }, jsonBodyIfAny, textOK},
		{"POST /api/conversations/{id}/auto-approve", func(r *http.Request) string { return conv(r) + "/auto-approve" }, jsonBodyIfAny, textJSON},
		{"POST /api/conversations/{id}/branch", func(r *http.Request) string { return conv(r) + "/branch" }, jsonBodyIfAny, textJSON},
		{"POST /api/conversations/{id}/cancel", func(r *http.Request) string { return conv(r) + "/cancel" }, noBody, textOK},
		{"GET /api/conversations/{id}/inflight", func(r *http.Request) string { return conv(r) + "/inflight" }, noBody, textJSON},
		{"GET /api/conversations/{id}/mcp-servers", func(r *http.Request) string { return conv(r) + "/mcp-servers" }, noBody, textJSON},
		{"POST /api/conversations/{id}/mcp-servers", func(r *http.Request) string { return conv(r) + "/mcp-servers" }, jsonBodyIfAny, textJSON},
		{"POST /api/conversations/{id}/model", func(r *http.Request) string { return conv(r) + "/model" }, jsonBodyIfAny, textOK},
		{"GET /api/conversations/{id}/outputs", func(r *http.Request) string { return conv(r) + "/outputs" }, noBody, passthroughJSON},
		{"POST /api/conversations/{id}/outputs/share", func(r *http.Request) string { return conv(r) + "/outputs/share" }, jsonBodyIfAny, passthroughJSON},
		{"POST /api/conversations/{id}/pin", func(r *http.Request) string { return conv(r) + "/pin" }, jsonBodyIfAny, textOK},
		{"POST /api/conversations/{id}/project", func(r *http.Request) string { return conv(r) + "/project" }, jsonBodyIfAny, passthroughJSON},
		{"POST /api/conversations/{id}/promote-to-task", func(r *http.Request) string { return conv(r) + "/promote-to-task" }, noBody, textJSON},
		{"GET /api/conversations/{id}/queue", func(r *http.Request) string { return conv(r) + "/queue" }, noBody, jsonForced},
		{"DELETE /api/conversations/{id}/queue/{inputId}", func(r *http.Request) string {
			return conv(r) + "/queue/" + p(r, "inputId")
		}, noBody, jsonForcedOK},
		{"POST /api/conversations/{id}/queue/{inputId}/send-now", func(r *http.Request) string {
			return conv(r) + "/queue/" + p(r, "inputId") + "/send-now"
		}, noBody, jsonForcedOK},
		{"POST /api/conversations/{id}/rename", func(r *http.Request) string { return conv(r) + "/rename" }, jsonBodyIfAny, textJSON},
		{"POST /api/conversations/{id}/share", func(r *http.Request) string { return conv(r) + "/share" }, jsonBodyIfAny, textJSON},
		{"DELETE /api/conversations/{id}/share", func(r *http.Request) string { return conv(r) + "/share" }, noBody, textOK},
		{"POST /api/conversations/{id}/share-with-team", func(r *http.Request) string { return conv(r) + "/share-with-team" }, jsonBodyIfAny, textJSON},
		{"GET /api/conversations/{id}/subagents/{childId}", func(r *http.Request) string {
			return conv(r) + "/subagents/" + p(r, "childId")
		}, noBody, textJSON},
		{"POST /api/conversations/{id}/suggest-prompt", func(r *http.Request) string { return conv(r) + "/suggest-prompt" }, noBody, textJSON},
		{"GET /api/conversations/{id}/team-link", func(r *http.Request) string { return conv(r) + "/team-link" }, noBody, passthroughJSON},
		{"POST /api/conversations/{id}/truncate", func(r *http.Request) string { return withRawQuery(conv(r)+"/truncate", r) }, noBody, textOK},
		{"GET /api/conversations/{id}/turns/{turnId}", func(r *http.Request) string {
			return conv(r) + "/turns/" + p(r, "turnId")
		}, noBody, textJSON},

		// ── memories ──
		{"GET /api/memories", fixed("/memories"), noBody, textJSON},
		{"POST /api/memories", fixed("/memories"), jsonBodyIfAny, textJSON},
		{"GET /api/memories/graph", func(r *http.Request) string { return withRawQuery("/memories/graph", r) }, noBody, textJSON},
		{"PATCH /api/memories/{memoryId}", func(r *http.Request) string { return "/memories/" + p(r, "memoryId") }, jsonBodyIfAny, textJSON},
		{"DELETE /api/memories/{memoryId}", func(r *http.Request) string { return "/memories/" + p(r, "memoryId") }, noBody, textPlainFallback},
		{"POST /api/memories/{memoryId}/accept", func(r *http.Request) string { return "/memories/" + p(r, "memoryId") + "/accept" }, jsonBodyIfAny, textJSON},

		// ── projects ──
		{"GET /api/projects", fixed("/projects"), noBody, textJSON},
		{"POST /api/projects", fixed("/projects"), jsonBodyIfAny, textJSON},
		{"GET /api/projects/{pid}", func(r *http.Request) string { return "/projects/" + p(r, "pid") }, noBody, textJSON},
		{"PATCH /api/projects/{pid}", func(r *http.Request) string { return "/projects/" + p(r, "pid") }, jsonBodyIfAny, textJSON},
		{"DELETE /api/projects/{pid}", func(r *http.Request) string { return "/projects/" + p(r, "pid") }, noBody, textJSON},
		{"GET /api/projects/{pid}/conversations", func(r *http.Request) string { return "/projects/" + p(r, "pid") + "/conversations" }, noBody, passthroughJSON},
		{"GET /api/projects/{pid}/export", func(r *http.Request) string { return "/projects/" + p(r, "pid") + "/export" }, noBody, passthroughJSON},
		{"GET /api/projects/{pid}/impact", func(r *http.Request) string { return "/projects/" + p(r, "pid") + "/impact" }, noBody, passthroughJSON},
		{"GET /api/projects/{pid}/members", func(r *http.Request) string { return "/projects/" + p(r, "pid") + "/members" }, noBody, passthroughJSON},
		{"GET /api/projects/{pid}/memories", func(r *http.Request) string { return "/projects/" + p(r, "pid") + "/memories" }, noBody, textJSON},
		{"POST /api/projects/{pid}/memories", func(r *http.Request) string { return "/projects/" + p(r, "pid") + "/memories" }, jsonBodyIfAny, textJSON},
		{"PATCH /api/projects/{pid}/memories/{memoryId}", func(r *http.Request) string {
			return "/projects/" + p(r, "pid") + "/memories/" + p(r, "memoryId")
		}, jsonBodyIfAny, textJSON},
		{"DELETE /api/projects/{pid}/memories/{memoryId}", func(r *http.Request) string {
			return "/projects/" + p(r, "pid") + "/memories/" + p(r, "memoryId")
		}, noBody, textPlainFallback},
		{"GET /api/projects/{pid}/my-state", func(r *http.Request) string { return "/projects/" + p(r, "pid") + "/my-state" }, noBody, passthroughJSON},
		{"PUT /api/projects/{pid}/my-state", func(r *http.Request) string { return "/projects/" + p(r, "pid") + "/my-state" }, jsonBodyIfAny, passthroughJSON},
		{"GET /api/projects/{pid}/team-conversations", func(r *http.Request) string {
			return "/projects/" + p(r, "pid") + "/team-conversations"
		}, noBody, passthroughJSON},
		{"POST /api/projects/{pid}/transfer", func(r *http.Request) string { return "/projects/" + p(r, "pid") + "/transfer" }, jsonBodyIfAny, textJSON},

		// ── browser push ──
		{"POST /api/push/renew", fixed("/push/renew"), jsonBodyIfAny, textJSON},
		{"POST /api/push/status", fixed("/push/status"), jsonBodyIfAny, textJSON},
		{"POST /api/push/subscribe", fixed("/push/subscribe"), jsonBodyIfAny, textJSON},
		{"DELETE /api/push/unsubscribe", fixed("/push/unsubscribe"), jsonBodyIfAny, textJSON},
		{"GET /api/push/vapid-public-key", fixed("/push/vapid-public-key"), noBody, textJSON},

		// ── per-user remote MCP servers ──
		{"GET /api/remote-mcp-servers", fixed("/remote-mcp-servers"), noBody, textJSON},
		{"POST /api/remote-mcp-servers", fixed("/remote-mcp-servers"), jsonBodyIfAny, textJSON},
		{"DELETE /api/remote-mcp-servers/{id}", func(r *http.Request) string { return "/remote-mcp-servers/" + p(r, "id") }, noBody, textOK},
		{"PUT /api/remote-mcp-servers/{id}/account", func(r *http.Request) string { return "/remote-mcp-servers/" + p(r, "id") + "/account" }, jsonBodyAlways, textJSON},
		{"POST /api/remote-mcp-servers/{id}/authorize", func(r *http.Request) string { return "/remote-mcp-servers/" + p(r, "id") + "/authorize" }, noBody, textJSON},
		{"POST /api/remote-mcp-servers/{id}/default", func(r *http.Request) string { return "/remote-mcp-servers/" + p(r, "id") + "/default" }, noBody, textJSON},
		{"PUT /api/remote-mcp-servers/{id}/key", func(r *http.Request) string { return "/remote-mcp-servers/" + p(r, "id") + "/key" }, jsonBodyAlways, textJSON},
		{"POST /api/remote-mcp-servers/{id}/shares", func(r *http.Request) string { return "/remote-mcp-servers/" + p(r, "id") + "/shares" }, jsonBodyAlways, textOK},
		{"DELETE /api/remote-mcp-servers/{id}/shares/{grantee}", func(r *http.Request) string {
			return "/remote-mcp-servers/" + p(r, "id") + "/shares/" + p(r, "grantee")
		}, noBody, textOK},
		{"POST /api/remote-mcp-servers/{id}/signout", func(r *http.Request) string { return "/remote-mcp-servers/" + p(r, "id") + "/signout" }, noBody, out{statusOnly: true}},

		// ── shared file library ──
		{"GET /api/shared-files", fixed("/shared-files"), noBody, passthroughJSON},
		{"PATCH /api/shared-files/{id}", func(r *http.Request) string { return "/shared-files/" + p(r, "id") }, jsonBodyIfAny, passthroughJSON},
		{"DELETE /api/shared-files/{id}", func(r *http.Request) string { return "/shared-files/" + p(r, "id") }, noBody, passthroughJSON},
	}
	for _, rt := range chat {
		mux.HandleFunc(rt.pattern, g.handleChatRoute(rt))
	}

	// GET /api/projects/{id}/files validates ?focus before forwarding it: an
	// id the panel highlights, never a free-form string.
	mux.HandleFunc("GET /api/projects/{pid}/files", func(w http.ResponseWriter, r *http.Request) {
		s := g.session(r)
		if s == nil {
			unauthorized(w)
			return
		}
		target := "/projects/" + p(r, "pid") + "/files"
		if r.URL.Query().Has("focus") {
			focus := r.URL.Query().Get("focus")
			if !projectFocusID.MatchString(focus) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid focus"})
				return
			}
			target += "?focus=" + encodeURIComponent(focus)
		}
		g.dispatch(w, r, g.chat, "chat-server", call{method: http.MethodGet, target: target, header: g.chatHeaders(identityOf(s)), detach: true}, passthroughJSON)
	})

	// GET /api/conversations/{id}/team-view forwards If-None-Match so a
	// teammate's poll can be answered 304.
	mux.HandleFunc("GET /api/conversations/{id}/team-view", func(w http.ResponseWriter, r *http.Request) {
		s := g.session(r)
		if s == nil {
			unauthorized(w)
			return
		}
		h := g.chatHeaders(identityOf(s))
		if v := r.Header.Get("If-None-Match"); v != "" {
			h.Set("If-None-Match", v)
		}
		g.dispatch(w, r, g.chat, "chat-server", call{method: http.MethodGet, target: conv(r) + "/team-view", header: h, detach: true}, passthroughJSON)
	})

	g.registerStreams(mux)
	g.registerFiles(mux)
	g.registerAuth(mux)
	g.registerBrand(mux)
	g.registerModels(mux)
	g.registerOrchestrator(mux)
}

func (g *Gateway) registerOrchestrator(mux *http.ServeMux) {
	task := func(r *http.Request) string { return "/tasks/" + p(r, "taskId") }
	ds := func(r *http.Request) string { return "/datasets/" + p(r, "datasetId") }
	routes := []orchRoute{
		{"GET /api/orchestrator/admin/budgets", fixed("/admin/budgets")},
		{"POST /api/orchestrator/admin/budgets", fixed("/admin/budgets")},
		{"DELETE /api/orchestrator/admin/budgets/{budgetId}", func(r *http.Request) string { return "/admin/budgets/" + p(r, "budgetId") }},
		{"GET /api/orchestrator/admin/usage", func(r *http.Request) string {
			return "/admin/usage" + passThroughQuery(r, "group_by", "from", "to", "format")
		}},
		{"GET /api/orchestrator/admin/usage/adoption", func(r *http.Request) string {
			return "/admin/usage/adoption" + passThroughQuery(r, "from", "to", "format")
		}},
		{"GET /api/orchestrator/config", fixed("/api/config")},
		{"GET /api/orchestrator/datasets", fixed("/datasets")},
		{"POST /api/orchestrator/datasets", fixed("/datasets")},
		{"GET /api/orchestrator/datasets/{datasetId}", ds},
		{"DELETE /api/orchestrator/datasets/{datasetId}", ds},
		{"POST /api/orchestrator/datasets/{datasetId}/approve", func(r *http.Request) string { return ds(r) + "/approve" }},
		{"GET /api/orchestrator/datasets/{datasetId}/export", func(r *http.Request) string { return ds(r) + "/export" }},
		{"POST /api/orchestrator/datasets/{datasetId}/pause", func(r *http.Request) string { return ds(r) + "/pause" }},
		{"POST /api/orchestrator/datasets/{datasetId}/rerun", func(r *http.Request) string { return ds(r) + "/rerun" }},
		{"GET /api/orchestrator/datasets/{datasetId}/rows", func(r *http.Request) string {
			return ds(r) + "/rows" + passThroughQuery(r, "status", "limit", "offset")
		}},
		{"POST /api/orchestrator/datasets/{datasetId}/rows", func(r *http.Request) string { return ds(r) + "/rows" }},
		{"POST /api/orchestrator/datasets/{datasetId}/run", func(r *http.Request) string { return ds(r) + "/run" }},
		{"GET /api/orchestrator/logs/{taskId}", func(r *http.Request) string { return "/logs/" + p(r, "taskId") }},
		{"GET /api/orchestrator/logs/{taskId}/subagents/{childId}", func(r *http.Request) string {
			return "/logs/" + p(r, "taskId") + "/subagents/" + p(r, "childId")
		}},
		{"GET /api/orchestrator/mcp-servers", fixed("/mcp-servers")},
		{"GET /api/orchestrator/mcp-servers/{server}/accounts", func(r *http.Request) string { return "/mcp-servers/" + p(r, "server") + "/accounts" }},
		{"POST /api/orchestrator/mcp-servers/{server}/accounts", func(r *http.Request) string { return "/mcp-servers/" + p(r, "server") + "/accounts" }},
		{"PUT /api/orchestrator/mcp-servers/{server}/accounts/{account}", func(r *http.Request) string {
			return "/mcp-servers/" + p(r, "server") + "/accounts/" + p(r, "account")
		}},
		{"DELETE /api/orchestrator/mcp-servers/{server}/accounts/{account}", func(r *http.Request) string {
			return "/mcp-servers/" + p(r, "server") + "/accounts/" + p(r, "account")
		}},
		{"GET /api/orchestrator/me", fixed("/api/me")},
		{"GET /api/orchestrator/prompts", fixed("/prompts")},
		{"POST /api/orchestrator/prompts", fixed("/prompts")},
		{"PUT /api/orchestrator/prompts/{promptId}", func(r *http.Request) string { return "/prompts/" + p(r, "promptId") }},
		{"DELETE /api/orchestrator/prompts/{promptId}", func(r *http.Request) string { return "/prompts/" + p(r, "promptId") }},
		{"GET /api/orchestrator/sla-report", func(r *http.Request) string { return "/sla-report" + passThroughQuery(r, "days") }},
		{"GET /api/orchestrator/stats", fixed("/stats")},
		{"GET /api/orchestrator/task-templates", fixed("/task-templates")},
		{"GET /api/orchestrator/tasks", func(r *http.Request) string {
			return "/tasks" + passThroughQuery(r, "limit", "offset", "status", "q", "scheduled_only",
				"completed_today", "completed_status", "created_by", "tag")
		}},
		{"POST /api/orchestrator/tasks", fixed("/tasks")},
		{"POST /api/orchestrator/tasks/estimate", fixed("/tasks/estimate")},
		{"GET /api/orchestrator/tasks/recipients", fixed("/tasks/recipients")},
		{"GET /api/orchestrator/tasks/tags", fixed("/tasks/tags")},
		{"GET /api/orchestrator/tasks/upcoming", func(r *http.Request) string { return "/tasks/upcoming" + passThroughQuery(r, "limit") }},
		{"GET /api/orchestrator/tasks/{taskId}", task},
		{"PUT /api/orchestrator/tasks/{taskId}", task},
		{"DELETE /api/orchestrator/tasks/{taskId}", task},
		{"POST /api/orchestrator/tasks/{taskId}/feedback", func(r *http.Request) string { return task(r) + "/feedback" }},
		{"GET /api/orchestrator/tasks/{taskId}/learned-instructions", func(r *http.Request) string { return task(r) + "/learned-instructions" }},
		{"DELETE /api/orchestrator/tasks/{taskId}/learned-instructions/active", func(r *http.Request) string {
			return task(r) + "/learned-instructions/active"
		}},
		{"POST /api/orchestrator/tasks/{taskId}/learned-instructions/{version}/activate", func(r *http.Request) string {
			return task(r) + "/learned-instructions/" + p(r, "version") + "/activate"
		}},
		{"DELETE /api/orchestrator/tasks/{taskId}/permanent", func(r *http.Request) string { return task(r) + "/permanent" }},
		{"POST /api/orchestrator/tasks/{taskId}/rerun", func(r *http.Request) string { return task(r) + "/rerun" }},
		{"POST /api/orchestrator/tasks/{taskId}/wake", func(r *http.Request) string { return task(r) + "/wake" }},
		{"POST /api/orchestrator/upload", fixed("/upload")},
	}
	for _, rt := range routes {
		mux.HandleFunc(rt.pattern, g.handleOrchRoute(rt))
	}
	mux.HandleFunc("POST /api/orchestrator/tasks/{taskId}/discuss", g.handleDiscuss)
	mux.HandleFunc("POST /api/orchestrator/auth/login", g.handleOrchLogin)
	mux.HandleFunc("POST /api/orchestrator/auth/logout", g.handleOrchLogout)
	mux.HandleFunc("GET /api/orchestrator/auth/elcano-login", func(w http.ResponseWriter, r *http.Request) {
		g.elcanoLoginRedirect(w, r, "/orchestrator")
	})
}
