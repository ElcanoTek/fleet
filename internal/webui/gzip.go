// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

// gzip for the JSON reads of the chat table (docs/CHAT-HISTORY-WINDOW.md).
//
// A conversation, the conversation list and the sidebar are JSON that
// compresses to a fraction of its size, and the single-box Caddyfile already
// encodes them (`encode zstd gzip`). An install whose proxy does not — the
// Helm chart's Ingress, a bare `fleet serve` — sent them as they were. So the
// gateway compresses them itself when the browser accepts gzip. A proxy that
// encodes leaves an already-encoded response alone, so nothing is compressed
// twice.
//
// It is deliberately narrow: GET rows of the chat table whose response is
// JSON and carries no validator (textJSON, jsonForced). Streams (SSE), file
// downloads, uploads and the ETag-carrying passthrough rows are untouched, and
// so is any response that is not a 200 of JSON, or is too small to be worth a
// gzip header.

// gzipMinBytes is the smallest body worth compressing: below it the gzip
// framing costs about what it saves.
const gzipMinBytes = 1024

var gzipWriters = sync.Pool{New: func() any {
	zw, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed)
	return zw
}}

// acceptsGzip reports whether the request's Accept-Encoding offers gzip with
// a non-zero quality.
func acceptsGzip(r *http.Request) bool {
	for _, v := range r.Header.Values("Accept-Encoding") {
		for part := range strings.SplitSeq(v, ",") {
			coding, params, _ := strings.Cut(strings.TrimSpace(part), ";")
			if !strings.EqualFold(strings.TrimSpace(coding), "gzip") {
				continue
			}
			q := strings.TrimSpace(params)
			if raw, ok := strings.CutPrefix(strings.ToLower(q), "q="); ok {
				if f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil && f <= 0 {
					return false
				}
			}
			return true
		}
	}
	return false
}

// gzipResponseWriter compresses a 200 JSON body once it has grown past
// gzipMinBytes. Until then it holds the bytes back, so a small response goes
// out as it is, with no Content-Encoding.
type gzipResponseWriter struct {
	w      http.ResponseWriter
	status int
	// buf holds the body until the decision is made.
	buf     []byte
	decided bool
	zw      *gzip.Writer
}

// withGzip returns the writer a compressible route writes to, and the func
// that finishes the response. A response that is not compressed leaves with
// exactly the headers it had before this existed; a compressed one says what
// it varied on. (Every row this wraps is served no-store, so no cache holds
// either form: Vary is there for correctness, not because one could mix them.)
func withGzip(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, func()) {
	if !acceptsGzip(r) {
		return w, func() {}
	}
	gw := &gzipResponseWriter{w: w}
	return gw, gw.finish
}

func (g *gzipResponseWriter) Header() http.Header { return g.w.Header() }

func (g *gzipResponseWriter) WriteHeader(code int) {
	if g.status == 0 {
		g.status = code
	}
}

// compressible reports whether this response is one to compress: a 200 of
// JSON that nothing has encoded already.
func (g *gzipResponseWriter) compressible() bool {
	h := g.w.Header()
	if g.status != http.StatusOK || h.Get("Content-Encoding") != "" {
		return false
	}
	ct, _, _ := strings.Cut(h.Get("Content-Type"), ";")
	return strings.EqualFold(strings.TrimSpace(ct), "application/json")
}

// decide sends the header and, when compress is set, starts the gzip stream.
func (g *gzipResponseWriter) decide(compress bool) {
	g.decided = true
	if g.status == 0 {
		g.status = http.StatusOK
	}
	if compress {
		h := g.w.Header()
		h.Set("Content-Encoding", "gzip")
		h.Add("Vary", "Accept-Encoding")
		h.Del("Content-Length")
		zw, _ := gzipWriters.Get().(*gzip.Writer)
		zw.Reset(g.w)
		g.zw = zw
	}
	g.w.WriteHeader(g.status)
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	if g.status == 0 {
		g.status = http.StatusOK
	}
	if !g.decided {
		if g.compressible() && len(g.buf)+len(b) < gzipMinBytes {
			g.buf = append(g.buf, b...)
			return len(b), nil
		}
		g.decide(g.compressible())
		if len(g.buf) > 0 {
			if _, err := g.body(g.buf); err != nil {
				return 0, err
			}
			g.buf = nil
		}
	}
	return g.body(b)
}

func (g *gzipResponseWriter) body(b []byte) (int, error) {
	if g.zw != nil {
		return g.zw.Write(b)
	}
	return g.w.Write(b)
}

// Flush sends what has been written so far. A handler that flushes wants its
// bytes on the wire now, so the wait for gzipMinBytes ends here.
func (g *gzipResponseWriter) Flush() {
	if !g.decided {
		g.decide(false)
		if len(g.buf) > 0 {
			_, _ = g.w.Write(g.buf)
			g.buf = nil
		}
	}
	if g.zw != nil {
		_ = g.zw.Flush()
	}
	_ = http.NewResponseController(g.w).Flush()
}

// Unwrap lets http.ResponseController reach the connection.
func (g *gzipResponseWriter) Unwrap() http.ResponseWriter { return g.w }

// finish ends the response: a body that never reached gzipMinBytes goes out
// uncompressed, and a gzip stream is closed and its writer returned.
func (g *gzipResponseWriter) finish() {
	if !g.decided {
		g.decide(false)
		if len(g.buf) > 0 {
			_, _ = g.w.Write(g.buf)
			g.buf = nil
		}
	}
	if g.zw != nil {
		_ = g.zw.Close()
		g.zw.Reset(nil)
		gzipWriters.Put(g.zw)
		g.zw = nil
	}
}
