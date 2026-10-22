// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/websession"
)

// The two backends are the chat-server and orchestrator handlers of the SAME
// process. The Next.js tier reached them over loopback HTTP; the gateway calls
// them in-process, which keeps their contract exactly as it was:
//
//   - a FRESH header set per call — the shared-secret channel header, the
//     verified identity, and only the few request headers a route forwarded
//     (Last-Event-ID, If-None-Match, a multipart Content-Type). Nothing the
//     browser sent is copied, so a browser can never present
//     X-Chat-Server-Token or X-User-Email to a backend;
//   - RemoteAddr 127.0.0.1 and no X-Forwarded-For, which is what the backends
//     saw from Next (it did not forward the header), so their per-IP filters
//     and rate limits behave exactly as before;
//   - the response headers a route used to re-emit, and no others.

// backendRemoteAddr is the peer address an in-process call presents.
const backendRemoteAddr = "127.0.0.1:0"

// identity is what a backend call asserts about the caller.
type identity struct {
	Email   string
	Epoch   string
	Source  websession.Source
	Issuer  string
	Subject string
}

func identityOf(s *websession.Session) identity {
	return identity{Email: s.Email, Epoch: s.Epoch, Source: s.Source, Issuer: s.Issuer, Subject: s.Subject}
}

// setIdentity writes X-User-Email, the session-epoch claim, and — only for a
// central-Auth session — the issuer/subject the epoch is keyed by.
func setIdentity(h http.Header, id identity) {
	h.Set("X-User-Email", id.Email)
	if id.Epoch != "" {
		h.Set("X-User-Session-Epoch", id.Epoch)
	}
	if id.Source == websession.SourceOIDC && id.Issuer != "" && id.Subject != "" {
		h.Set("X-User-Session-Source", "oidc")
		h.Set("X-External-Issuer", id.Issuer)
		h.Set("X-External-Subject", id.Subject)
	}
}

// chatHeaders is chatServerHeaders: the shared secret plus the identity.
func (g *Gateway) chatHeaders(id identity) http.Header {
	h := http.Header{}
	h.Set("X-Chat-Server-Token", g.cfg.ChatToken)
	setIdentity(h, id)
	return h
}

// orchAuth is the credential an orchestrator call carries: a presented bearer
// (forwarded verbatim; the orchestrator owns and validates it) or the cookie
// session's identity over the X-Orchestrator-Server-Token channel.
type orchAuth struct {
	bearer string
	id     identity
}

func (g *Gateway) orchHeaders(a orchAuth) http.Header {
	h := http.Header{}
	if a.bearer != "" {
		h.Set("Authorization", "Bearer "+a.bearer)
		return h
	}
	if g.cfg.OrchestratorToken != "" {
		h.Set("X-Orchestrator-Server-Token", g.cfg.OrchestratorToken)
	}
	setIdentity(h, a.id)
	return h
}

// call is one in-process backend request.
type call struct {
	method string
	// target is the backend path plus query, already encoded.
	target string
	header http.Header
	body   io.Reader
	// contentLength is the body length when known (-1 otherwise).
	contentLength int64
	// detach: the backend keeps running when the browser goes away. The Next
	// routes that did not pass request.signal upstream behaved this way.
	detach bool
}

// out describes how a backend response is re-emitted to the browser.
type out struct {
	// headers are the upstream response headers copied through.
	headers []string
	// ctFallback is the Content-Type when upstream sent none; ctForce replaces it.
	ctFallback string
	ctForce    string
	// okOn204 turns an upstream 204 into 200 {"ok":true}.
	okOn204 bool
	// statusOnly forwards only the status (`new NextResponse(null, {status})`).
	statusOnly bool
	// plainErrors re-emits a non-2xx response as its bare body with Next's
	// default text/plain content type and no other upstream header (the
	// streaming and file routes' `new NextResponse(text, {status})`).
	plainErrors bool
	// onSuccess adjusts the browser headers of a 2xx response; dst already holds
	// the copied headers, src is what upstream set.
	onSuccess func(dst, src http.Header)
	// onError adjusts the browser headers of a non-2xx response.
	onError func(dst http.Header)
	// status200 rewrites a 2xx to 200 (the file and SSE routes answered 200).
	status200 bool
}

// Header allowlists the Next funnels re-emitted.
var (
	// contentTypeOnly: the hand-written `new NextResponse(text, {headers:
	// {"Content-Type": …}})` routes.
	contentTypeOnly = []string{"Content-Type"}
	// forwardedHeaders: chatServerPassthrough and proxyToOrchestrator
	// (web/src/app/lib/proxyHeaders.ts). Content-Length deliberately absent.
	forwardedHeaders = []string{"Content-Type", "Content-Disposition", "Cache-Control", "Last-Modified", "ETag"}
)

// textJSON is the common shape: Content-Type only, application/json fallback.
var textJSON = out{headers: contentTypeOnly, ctFallback: "application/json"}

// passthroughJSON is chatServerPassthrough / proxyToOrchestrator.
var passthroughJSON = out{headers: forwardedHeaders, ctFallback: "application/json"}

// dispatch runs c against backend h and re-emits the response per o. A nil
// backend (the browser-test harness boots the gateway alone) answers the same
// 502 the Next tier gave when chat-server was unreachable.
func (g *Gateway) dispatch(w http.ResponseWriter, r *http.Request, h http.Handler, name string, c call, o out) {
	if h == nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": name + " unreachable: no backend"})
		return
	}
	ctx := r.Context()
	if c.detach {
		ctx = context.WithoutCancel(ctx)
	}
	body := c.body
	if body == nil {
		body = http.NoBody
	}
	req, err := http.NewRequestWithContext(ctx, c.method, "http://127.0.0.1"+c.target, body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": name + " unreachable: " + err.Error()})
		return
	}
	req.RequestURI = c.target
	req.RemoteAddr = backendRemoteAddr
	req.Host = "127.0.0.1"
	if c.body == nil {
		req.ContentLength = 0
	} else {
		req.ContentLength = c.contentLength
	}
	if c.header != nil {
		req.Header = c.header
	}
	pw := &proxyWriter{w: w, hdr: http.Header{}, o: &o, revoke: func() { g.clearSessionCookie(w, r) }}
	h.ServeHTTP(pw, req)
	if !pw.wrote {
		pw.WriteHeader(http.StatusOK)
	}
}

// clearSessionCookie deletes elcano_session — the browser-side consequence of
// a backend's X-Session-Revoked verdict. Without it the user is trapped: the
// cookie's MAC is still valid, so the gate keeps treating it as a session.
func (g *Gateway) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	setCookie(w, websession.CookieName, "", 0, g.secureRequest(r), "")
}

// proxyWriter is the ResponseWriter a backend handler writes into. It decides
// the browser-facing status and headers at WriteHeader time and then streams
// the body straight through, flushing when the handler flushes (SSE).
type proxyWriter struct {
	w       http.ResponseWriter
	hdr     http.Header
	o       *out
	revoke  func()
	wrote   bool
	discard bool
}

func (p *proxyWriter) Header() http.Header { return p.hdr }

func (p *proxyWriter) WriteHeader(code int) {
	if p.wrote || code < 200 {
		return
	}
	p.wrote = true
	o := p.o
	if code == http.StatusUnauthorized && p.hdr.Get("X-Session-Revoked") == "1" {
		p.revoke()
	}
	dst := p.w.Header()
	// A 204 has no body. Routes that answered it as {"ok":true} still do;
	// every other route re-emitted a bare 204 (`new NextResponse(null, {status:
	// 204})`), which is also what the reattach stream's "nothing to replay" is.
	if code == http.StatusNoContent {
		if o.okOn204 {
			dst.Set("Content-Type", "application/json")
			p.w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(p.w, `{"ok":true}`)
		} else {
			p.w.WriteHeader(http.StatusNoContent)
		}
		p.discard = true
		return
	}
	if o.statusOnly {
		p.w.WriteHeader(code)
		p.discard = true
		return
	}
	ok := code >= 200 && code < 300
	if !ok && o.plainErrors {
		dst.Set("Content-Type", "text/plain;charset=UTF-8")
		secureContentType(dst)
		p.w.WriteHeader(code)
		return
	}
	for _, name := range o.headers {
		if v := p.hdr.Values(name); len(v) > 0 {
			if http.CanonicalHeaderKey(name) == "Content-Type" {
				v = v[:1] // see secureContentType
			}
			dst[http.CanonicalHeaderKey(name)] = append([]string(nil), v...)
		}
	}
	if o.ctForce != "" {
		dst.Set("Content-Type", o.ctForce)
	} else if dst.Get("Content-Type") == "" && o.ctFallback != "" {
		dst.Set("Content-Type", o.ctFallback)
	}
	if ok && o.onSuccess != nil {
		o.onSuccess(dst, p.hdr)
	}
	if !ok && o.onError != nil {
		o.onError(dst)
	}
	if ok && o.status200 {
		code = http.StatusOK
	}
	secureContentType(dst)
	p.w.WriteHeader(code)
}

// secureContentType is the last word on what a forwarded body may be taken
// for. The bytes come from the backend handlers, which echo request values
// into JSON and text; the browser must never treat them as a document:
//
//   - nosniff, always (decorate sets it too; this keeps the guarantee local);
//   - no Content-Type at all becomes text/plain — otherwise net/http would
//     sniff one, and a body starting with "<html" would be served as HTML;
//   - an active type (HTML, SVG, XML) that a route forwards on purpose — a
//     chat export, a workspace file — additionally gets a sandboxing CSP, so
//     even opened inline it cannot run script on this origin.
func secureContentType(dst http.Header) {
	dst.Set("X-Content-Type-Options", "nosniff")
	// One Content-Type only: browsers resolve several by taking the last
	// valid one, while this check (and fileOut's) reads the first.
	// (WriteHeader already collapsed forwarded values; this covers the rest.)
	if v := dst.Values("Content-Type"); len(v) > 1 {
		dst.Set("Content-Type", v[0])
	}
	ct := strings.TrimSpace(dst.Get("Content-Type"))
	if ct == "" {
		dst.Set("Content-Type", "text/plain; charset=utf-8")
		return
	}
	if isActiveContent(ct) && !strings.Contains(strings.Join(dst.Values("Content-Security-Policy"), ","), "sandbox") {
		dst.Add("Content-Security-Policy", "sandbox")
	}
}

func (p *proxyWriter) Write(b []byte) (int, error) {
	if !p.wrote {
		p.WriteHeader(http.StatusOK)
	}
	if p.discard {
		return len(b), nil
	}
	return p.w.Write(b)
}

// Flush forwards a backend flush (the SSE writers flush per frame); without it
// the browser would see nothing until the handler returned.
func (p *proxyWriter) Flush() {
	if !p.wrote {
		p.WriteHeader(http.StatusOK)
	}
	if p.discard {
		return
	}
	_ = http.NewResponseController(p.w).Flush()
}

// Unwrap lets http.ResponseController reach the connection (write deadlines).
func (p *proxyWriter) Unwrap() http.ResponseWriter { return p.w }

// bodyOrNil returns r's body as a reader, or nil when it is empty — the Next
// routes' `body.length > 0 ? body : undefined`. It peeks one byte rather than
// buffering, so a large body still streams.
func bodyOrNil(r *http.Request) (io.Reader, int64) {
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		return nil, 0
	}
	br := bufio.NewReader(r.Body)
	if _, err := br.Peek(1); err != nil {
		return nil, 0
	}
	return br, r.ContentLength
}

// jsonBody is chatServerFetch's body rule: the browser body forwarded as-is,
// labelled application/json when non-empty, absent when empty.
func jsonBody(r *http.Request, h http.Header) (io.Reader, int64) {
	b, n := bodyOrNil(r)
	if b != nil {
		h.Set("Content-Type", "application/json")
	}
	return b, n
}

// isMultipart reports whether the browser sent a multipart body.
func isMultipart(r *http.Request) bool {
	return strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "multipart/form-data")
}

func stringReader(s string) io.Reader { return strings.NewReader(s) }
