// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"net/http"
)

// ssePassthroughHeaders are the response headers chat-server sets on an SSE
// response that the browser must receive. The stream routes REBUILD the
// header set rather than pass chat-server's through, so a header added
// server-side never arrives unless it is listed here — which had already
// happened once: the keepalive cadence was dropped and the client's liveness
// watchdog silently degraded (#1584). The conversation id rides a header
// because a brand-new chat's socket can die before its `conversation` frame
// (#1591). scripts/check_sse_passthrough_headers_test.go pins this list
// against what the Go producer sets.
var ssePassthroughHeaders = []string{
	"X-Fleet-Heartbeat-Interval-Ms",
	"X-Fleet-Conversation-Id",
}

// sseOut re-emits a chat SSE stream: the fixed streaming header set plus the
// pass-through list, status 200; a non-2xx is forwarded as its plain body.
var sseOut = out{
	plainErrors: true,
	status200:   true,
	onSuccess: func(dst, src http.Header) {
		dst.Set("Content-Type", "text/event-stream; charset=utf-8")
		dst.Set("Cache-Control", "no-cache, no-transform")
		// Tells a buffering proxy (nginx) not to hold frames back.
		dst.Set("X-Accel-Buffering", "no")
		for _, name := range ssePassthroughHeaders {
			if v := src.Get(name); v != "" {
				dst.Set(name, v)
			}
		}
	},
}

func (g *Gateway) registerStreams(mux *http.ServeMux) {
	// POST /api/chat — one chat turn, streamed back as SSE. The body is
	// chat-server's /chat contract, forwarded verbatim.
	mux.HandleFunc("POST /api/chat", func(w http.ResponseWriter, r *http.Request) {
		if !g.verifyOrigin(w, r) {
			return
		}
		s := g.session(r)
		if s == nil {
			unauthorized(w)
			return
		}
		h := g.chatHeaders(identityOf(s))
		c := call{method: http.MethodPost, target: "/chat", header: h}
		c.body, c.contentLength = jsonBody(r, h)
		g.dispatch(w, r, g.chat, "chat-server", c, sseOut)
	})

	// GET /api/conversations/{id}/stream — reattach to an in-flight turn after
	// a reload or a phone's screen lock. Forwards Last-Event-ID and the whole
	// query (turn_id). A 204 means nothing to replay.
	mux.HandleFunc("GET /api/conversations/{id}/stream", func(w http.ResponseWriter, r *http.Request) {
		s := g.session(r)
		if s == nil {
			unauthorized(w)
			return
		}
		h := g.chatHeaders(identityOf(s))
		if v := r.Header.Get("Last-Event-ID"); v != "" {
			h.Set("Last-Event-ID", v)
		}
		target := withRawQuery("/conversations/"+p(r, "id")+"/stream", r)
		g.dispatch(w, r, g.chat, "chat-server", call{method: http.MethodGet, target: target, header: h}, sseOut)
	})

	// POST /api/conversations/{id}/summarize — "summarize and continue",
	// streamed (summary.delta / summary.completed / summary.error). The body
	// defaults to {} (the model falls back to the conversation's own).
	mux.HandleFunc("POST /api/conversations/{id}/summarize", func(w http.ResponseWriter, r *http.Request) {
		if !g.verifyOrigin(w, r) {
			return
		}
		s := g.session(r)
		if s == nil {
			unauthorized(w)
			return
		}
		h := g.chatHeaders(identityOf(s))
		h.Set("Content-Type", "application/json")
		c := call{method: http.MethodPost, target: "/conversations/" + p(r, "id") + "/summarize", header: h}
		c.body, c.contentLength = bodyOrNil(r)
		if c.body == nil {
			c.body, c.contentLength = stringReader("{}"), 2
		}
		g.dispatch(w, r, g.chat, "chat-server", c, out{
			headers:    contentTypeOnly,
			ctFallback: "text/event-stream",
			onSuccess:  func(dst, _ http.Header) { dst.Set("Cache-Control", "no-cache, no-transform") },
			onError:    func(dst http.Header) { dst.Set("Cache-Control", "no-cache, no-transform") },
		})
	})

	// GET /api/orchestrator/tasks/{id}/stream — a scheduled run's live log
	// (#508), cookie session or bearer, Last-Event-ID forwarded.
	mux.HandleFunc("GET /api/orchestrator/tasks/{taskId}/stream", func(w http.ResponseWriter, r *http.Request) {
		auth, ok := g.resolveOrchAuth(r)
		if !ok {
			unauthorized(w)
			return
		}
		h := g.orchHeaders(auth)
		h.Set("Accept", "text/event-stream")
		if v := r.Header.Get("Last-Event-ID"); v != "" {
			h.Set("Last-Event-ID", v)
		}
		g.dispatch(w, r, g.orch, "orchestrator", call{method: http.MethodGet, target: "/tasks/" + p(r, "taskId") + "/stream", header: h}, out{
			headers:    contentTypeOnly,
			ctFallback: "application/json",
			status200:  true,
			onSuccess: func(dst, _ http.Header) {
				dst.Set("Content-Type", "text/event-stream")
				dst.Set("Cache-Control", "no-cache, no-transform")
				dst.Set("X-Accel-Buffering", "no")
			},
		})
	})
}
