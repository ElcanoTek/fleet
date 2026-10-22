// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"net/http"
	"net/url"
	"strings"
)

// isActiveContent reports whether a browser would open this type as a
// document that can run script on our origin. Agent-written files of these
// types are a stored-XSS primitive (a prompt-injected agent writes report.html
// with a <script>, the user opens it in a tab, and it runs with the session's
// cookies), so the file routes force them to download.
//
// Every XML type counts, not just the common ones: browsers render any
// "…/xml" or "…+xml" document (RSS, Atom, XSLT, MathML …) as XML, and an
// XHTML-namespaced <script src="/api/…/x.js"> inside it is same-origin, so the
// page CSP's script-src 'self' would not stop it. mime.TypeByExtension on a
// host with /etc/mime.types maps .rss, .atom, .xsl and friends to such types.
func isActiveContent(contentType string) bool {
	ct, _, _ := strings.Cut(contentType, ";")
	ct = strings.ToLower(strings.TrimSpace(ct))
	return ct == "text/html" || ct == "text/xsl" ||
		strings.HasSuffix(ct, "/xml") || strings.HasSuffix(ct, "+xml")
}

func attachmentDisposition(filename string) string {
	return "attachment; filename*=UTF-8''" + encodeURIComponent(filename)
}

// catchAll re-derives a {path...} tail the way the Next catch-all route did:
// split the ESCAPED path on "/", decode each segment, and re-encode it with
// encodeURIComponent for the upstream. An encoded slash inside a filename
// therefore stays inside its segment. It also returns the last decoded
// segment (the download filename).
func catchAll(r *http.Request, skipSegments int) (encoded string, last string) {
	segs := strings.Split(strings.TrimPrefix(r.URL.EscapedPath(), "/"), "/")
	if len(segs) <= skipSegments {
		return "", "download"
	}
	tail := segs[skipSegments:]
	out := make([]string, 0, len(tail))
	for _, seg := range tail {
		dec, err := url.PathUnescape(seg)
		if err != nil {
			dec = seg
		}
		out = append(out, encodeURIComponent(dec))
		last = dec
	}
	if last == "" {
		last = "download"
	}
	return strings.Join(out, "/"), last
}

// fileOut is the workspace/team-file response: the listed upstream headers,
// nosniff (set by decorate on every response), active documents forced to
// download, status 200; a refusal is forwarded as its plain body.
func fileOut(headers []string, filename string, forceUnknown bool, extra func(dst http.Header)) out {
	return out{
		headers:     headers,
		plainErrors: true,
		status200:   true,
		onSuccess: func(dst, _ http.Header) {
			ct := dst.Get("Content-Type")
			if isActiveContent(ct) || (forceUnknown && ct == "") {
				dst.Set("Content-Disposition", attachmentDisposition(filename))
			}
			if extra != nil {
				extra(dst)
			}
		},
	}
}

func (g *Gateway) registerFiles(mux *http.ServeMux) {
	// GET /api/conversations/{id}/workspace/{path...} — a file from the
	// conversation's workspace (the markdown <img> path renders through here).
	// Auth and path validation live in chat-server (handleWorkspaceFile).
	mux.HandleFunc("GET /api/conversations/{id}/workspace/{path...}", func(w http.ResponseWriter, r *http.Request) {
		s := g.session(r)
		if s == nil {
			unauthorized(w)
			return
		}
		tail, name := catchAll(r, 4)
		g.dispatch(w, r, g.chat, "chat-server", call{
			method: http.MethodGet,
			target: "/conversations/" + p(r, "id") + "/workspace/" + tail,
			header: g.chatHeaders(identityOf(s)),
		}, fileOut([]string{"Content-Type", "Content-Length", "Cache-Control", "Last-Modified", "ETag"}, name, false, nil))
	})

	// GET|HEAD /api/conversations/{id}/team-files/{path...} — a teammate's
	// download of one shared output (docs/TEAM-SHARING.md, ADR-0079). Every
	// gate is in the Go handler; this re-applies the hardening because the
	// bytes were written by SOMEONE ELSE'S agent: a sandboxing CSP, and active
	// or untyped documents forced to download.
	teamFile := func(w http.ResponseWriter, r *http.Request) {
		s := g.session(r)
		if s == nil {
			unauthorized(w)
			return
		}
		tail, name := catchAll(r, 4)
		g.dispatch(w, r, g.chat, "chat-server", call{
			method: r.Method,
			target: "/conversations/" + p(r, "id") + "/team-files/" + tail,
			header: g.chatHeaders(identityOf(s)),
		}, fileOut([]string{"Content-Type", "Content-Length", "Cache-Control", "Last-Modified", "ETag", "Content-Disposition"}, name, true,
			func(dst http.Header) { dst.Set("Content-Security-Policy", "sandbox") }))
	}
	mux.HandleFunc("GET /api/conversations/{id}/team-files/{path...}", teamFile)
	mux.HandleFunc("HEAD /api/conversations/{id}/team-files/{path...}", teamFile)

	// GET /api/conversations/{id}/export — the download dialog's artifact
	// (?format, ?include forwarded; the Go server owns their meaning). The
	// Content-Disposition filename is the one the Go server chose.
	mux.HandleFunc("GET /api/conversations/{id}/export", func(w http.ResponseWriter, r *http.Request) {
		s := g.session(r)
		if s == nil {
			unauthorized(w)
			return
		}
		q := url.Values{}
		var parts []string
		for _, k := range []string{"format", "include"} {
			if v := r.URL.Query().Get(k); v != "" {
				q.Set(k, v)
				parts = append(parts, url.QueryEscape(k)+"="+url.QueryEscape(v))
			}
		}
		target := "/conversations/" + p(r, "id") + "/export"
		if len(parts) > 0 {
			target += "?" + strings.Join(parts, "&")
		}
		g.dispatch(w, r, g.chat, "chat-server", call{method: http.MethodGet, target: target, header: g.chatHeaders(identityOf(s))},
			out{headers: []string{"Content-Type", "Content-Disposition"}})
	})

	// GET /api/shared-files/{id}/download — one shared-library file.
	mux.HandleFunc("GET /api/shared-files/{id}/download", func(w http.ResponseWriter, r *http.Request) {
		s := g.session(r)
		if s == nil {
			unauthorized(w)
			return
		}
		g.dispatch(w, r, g.chat, "chat-server", call{
			method: http.MethodGet,
			target: "/shared-files/" + p(r, "id") + "/download",
			header: g.chatHeaders(identityOf(s)),
		}, out{headers: []string{"Content-Type", "Content-Disposition", "Content-Length"}, plainErrors: true, status200: true})
	})

	// POST /api/attachments and POST /api/shared-files — multipart uploads,
	// streamed straight through. The gateway applies NO size cap of its own:
	// chat-server enforces FLEET_UPLOAD_MAX_BYTES per file (and its own
	// whole-request cap), and a second, different limit here is exactly the
	// truncation bug the Next tier had (proxyClientMaxBodySize).
	upload := func(target string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if !g.verifyOrigin(w, r) {
				return
			}
			s := g.session(r)
			if s == nil {
				unauthorized(w)
				return
			}
			if !isMultipart(r) {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "expected multipart/form-data"})
				return
			}
			h := g.chatHeaders(identityOf(s))
			h.Set("Content-Type", r.Header.Get("Content-Type"))
			g.dispatch(w, r, g.chat, "chat-server", call{
				method: http.MethodPost, target: target, header: h,
				body: r.Body, contentLength: r.ContentLength,
			}, textJSON)
		}
	}
	mux.HandleFunc("POST /api/attachments", upload("/attachments"))
	mux.HandleFunc("POST /api/shared-files", upload("/shared-files"))

	// GET /api/orchestrator/tasks/{id}/workspace/{path...} — a scheduled run's
	// workspace file, with the chat workspace route's hardening.
	mux.HandleFunc("GET /api/orchestrator/tasks/{taskId}/workspace/{path...}", func(w http.ResponseWriter, r *http.Request) {
		auth, ok := g.resolveOrchAuth(r)
		if !ok {
			unauthorized(w)
			return
		}
		tail, name := catchAll(r, 5)
		g.dispatch(w, r, g.orch, "orchestrator", call{
			method: http.MethodGet,
			target: "/tasks/" + p(r, "taskId") + "/workspace/" + tail,
			header: g.orchHeaders(auth),
		}, fileOut([]string{"Content-Type", "Content-Length", "Cache-Control", "Last-Modified", "ETag"}, name, false, nil))
	})
}
