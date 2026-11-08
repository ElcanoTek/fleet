// Ported from github.com/go-chi/chi/v5 v5.3.2, middleware/recoverer_test.go
// (modified: the report assertions check internal/safe's structured event).
// Copyright (c) 2015-present Peter Kieltyka, Google Inc.; see LICENSE.
// SPDX-License-Identifier: MIT

package httpmw

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/ElcanoTek/fleet/internal/safe"
)

func panickingHandler(http.ResponseWriter, *http.Request) { panic("foo") }

// captureRecovered routes safe's panic records to a buffer for the test.
func captureRecovered(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	safe.SetPanicHandler(slog.NewJSONHandler(buf, nil))
	t.Cleanup(func() { safe.SetPanicHandler(nil) })
	return buf
}

// A handler panic is answered 500 and reported as safe's structured event
// (through the configured handler), naming the route — never the panic value
// or a stack, which can carry request data (#2074 review: this used to be
// fmt.Fprintf(os.Stderr, "panic: %v\n\n%s", value, stack)).
func TestRecoverer(t *testing.T) {
	r := chi.NewRouter()
	buf := captureRecovered(t)

	r.Use(Recoverer)
	r.Get("/", func(http.ResponseWriter, *http.Request) { panic("secret-bearing panic value foo") })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	out := buf.String()
	if !strings.Contains(out, `"msg":"panic recovered"`) || !strings.Contains(out, `"goroutine":"httpmw.handler GET /"`) || !strings.Contains(out, `"panic_class":"string"`) {
		t.Fatalf("report should be safe's structured event naming the route:\n%s", out)
	}
	if strings.Contains(out, "secret-bearing") || strings.Contains(out, "goroutine 1") {
		t.Fatalf("the panic value or a stack was logged:\n%s", out)
	}
}

// RecoverWith answers through the caller's responder.
func TestRecoverWithUsesTheResponder(t *testing.T) {
	_ = captureRecovered(t)
	w := httptest.NewRecorder()
	RecoverWith("unit", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})(http.HandlerFunc(panickingHandler)).ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
	if w.Code != http.StatusTeapot {
		t.Fatalf("status = %d", w.Code)
	}
}

// An upgraded connection gets no 500 written on it.
func TestRecovererUpgradeWritesNoStatus(t *testing.T) {
	_ = captureRecovered(t)

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Connection", "Upgrade")
	Recoverer(http.HandlerFunc(panickingHandler)).ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Body.Len() != 0 { // the recorder's untouched default
		t.Fatalf("status = %d, body %q: want nothing written", w.Code, w.Body.String())
	}
}

func TestRecovererAbortHandler(t *testing.T) {
	defer func() {
		rcv := recover()
		if rcv != http.ErrAbortHandler { //nolint:errorlint // identity, as upstream
			t.Fatalf("http.ErrAbortHandler should not be recovered")
		}
	}()

	w := httptest.NewRecorder()

	r := chi.NewRouter()
	r.Use(Recoverer)

	r.Get("/", func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	})

	req, err := http.NewRequest("GET", "/", nil)
	if err != nil {
		t.Fatal(err)
	}

	r.ServeHTTP(w, req)
}
