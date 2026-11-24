// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package webui

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAcceptsGzip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		header string
		want   bool
	}{
		{"gzip, deflate, br", true},
		{"br;q=1.0, GZIP;q=0.8", true},
		{"gzip;q=0", false},
		{"gzip; q=0.0", false},
		{"deflate, br", false},
		{"", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if c.header != "" {
			r.Header.Set("Accept-Encoding", c.header)
		}
		if got := acceptsGzip(r); got != c.want {
			t.Errorf("Accept-Encoding %q: acceptsGzip = %v, want %v", c.header, got, c.want)
		}
	}
}

// gzipProbe runs handler behind withGzip for a request that accepts gzip (or
// not) and returns the recorded response.
func gzipProbe(t *testing.T, accept string, handler func(http.ResponseWriter)) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if accept != "" {
		r.Header.Set("Accept-Encoding", accept)
	}
	rec := httptest.NewRecorder()
	w, finish := withGzip(rec, r)
	handler(w)
	finish()
	return rec
}

func gunzip(t *testing.T, body io.Reader) string {
	t.Helper()
	zr, err := gzip.NewReader(body)
	if err != nil {
		t.Fatalf("response is not gzip: %v", err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip body: %v", err)
	}
	return string(b)
}

func TestGzipCompressesALargeJSONBody(t *testing.T) {
	t.Parallel()
	body := `{"history":"` + strings.Repeat("row,row,row;", 4000) + `"}`
	rec := gzipProbe(t, "gzip", func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "123")
		w.WriteHeader(http.StatusOK)
		// Written in pieces, as json.Encoder and a proxied handler do.
		for i := 0; i < len(body); i += 700 {
			if _, err := io.WriteString(w, body[i:min(i+700, len(body))]); err != nil {
				t.Fatalf("write: %v", err)
			}
		}
	})
	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := rec.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Errorf("Vary = %q, want Accept-Encoding", got)
	}
	if got := rec.Header().Get("Content-Length"); got != "" {
		t.Errorf("Content-Length %q survived compression", got)
	}
	if rec.Body.Len() >= len(body)/4 {
		t.Errorf("compressed body is %d bytes of %d", rec.Body.Len(), len(body))
	}
	if got := gunzip(t, rec.Body); got != body {
		t.Error("the decompressed body differs from what the handler wrote")
	}
}

func TestGzipLeavesOtherResponsesAlone(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("x", 4*gzipMinBytes)
	cases := []struct {
		name, accept, contentType, encoding, body string
		status                                    int
	}{
		{name: "the browser does not accept gzip", contentType: "application/json", body: big, status: 200},
		{name: "a small body", accept: "gzip", contentType: "application/json", body: `{"ok":true}`, status: 200},
		{name: "not JSON", accept: "gzip", contentType: "text/html; charset=utf-8", body: big, status: 200},
		{name: "an error", accept: "gzip", contentType: "application/json", body: big, status: 500},
		{name: "already encoded", accept: "gzip", contentType: "application/json", encoding: "br", body: big, status: 200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rec := gzipProbe(t, c.accept, func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", c.contentType)
				if c.encoding != "" {
					w.Header().Set("Content-Encoding", c.encoding)
				}
				w.WriteHeader(c.status)
				_, _ = io.WriteString(w, c.body)
			})
			if rec.Code != c.status {
				t.Errorf("status = %d, want %d", rec.Code, c.status)
			}
			if got := rec.Header().Get("Content-Encoding"); got != c.encoding {
				t.Errorf("Content-Encoding = %q, want %q", got, c.encoding)
			}
			if rec.Body.String() != c.body {
				t.Error("the body was changed")
			}
			// A response left alone keeps exactly the headers it had.
			if got := rec.Header().Get("Vary"); got != "" {
				t.Errorf("Vary = %q on a response that was not compressed", got)
			}
		})
	}
}

func TestGzipKeepsABodylessStatus(t *testing.T) {
	t.Parallel()
	rec := gzipProbe(t, "gzip", func(w http.ResponseWriter) { w.WriteHeader(http.StatusNoContent) })
	if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 || rec.Header().Get("Content-Encoding") != "" {
		t.Errorf("204 became %d with %d body bytes, encoding %q", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Encoding"))
	}
}

// The conversation read is compressed end to end through the gateway, and a
// mutation or a browser that does not ask is not.
func TestConversationReadIsGzippedThroughTheGateway(t *testing.T) {
	e := newTestEnv(t)
	payload := `{"history":"` + strings.Repeat("0123456789", 2000) + `"}`
	e.chat.respond = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, payload)
	}
	w := e.do(signedIn(t, req("GET", "/api/conversations/c1?limit=60", "", "Accept-Encoding", "gzip, br")))
	if w.StatusCode != http.StatusOK || w.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("status %d, Content-Encoding %q", w.StatusCode, w.Header.Get("Content-Encoding"))
	}
	if got := gunzip(t, w.Body); got != payload {
		t.Error("the decompressed conversation differs from the backend's")
	}
	if w.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("the compressed response lost nosniff")
	}

	w = e.do(signedIn(t, req("GET", "/api/conversations/c1", "")))
	plain, _ := io.ReadAll(w.Body)
	if w.Header.Get("Content-Encoding") != "" || string(plain) != payload {
		t.Error("a browser that did not ask for gzip got an encoded or altered body")
	}
	w = e.do(signedIn(t, req("POST", "/api/conversations/c1/rename", `{"title":"t"}`, "Origin", "http://fleet.example.com", "Accept-Encoding", "gzip")))
	if w.Header.Get("Content-Encoding") != "" {
		t.Error("a POST response was compressed")
	}
}
