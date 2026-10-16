// Ported from github.com/go-chi/chi/v5 v5.3.2, middleware/recoverer_test.go
// (modified: the pretty-stack and colour assertions became plain-stack ones).
// Copyright (c) 2015-present Peter Kieltyka, Google Inc.; see LICENSE.
// SPDX-License-Identifier: MIT

package httpmw

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

func panickingHandler(http.ResponseWriter, *http.Request) { panic("foo") }

func TestRecoverer(t *testing.T) {
	r := chi.NewRouter()

	oldRecovererErrorWriter := recovererErrorWriter
	defer func() { recovererErrorWriter = oldRecovererErrorWriter }()
	buf := &bytes.Buffer{}
	recovererErrorWriter = buf

	r.Use(Recoverer)
	r.Get("/", panickingHandler)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "panic: foo\n") || !strings.Contains(out, "panickingHandler") {
		t.Fatalf("report should name the panic and its handler:\n%s", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatal("report should carry no ANSI colour codes")
	}
}

// An upgraded connection gets no 500 written on it.
func TestRecovererUpgradeWritesNoStatus(t *testing.T) {
	oldRecovererErrorWriter := recovererErrorWriter
	defer func() { recovererErrorWriter = oldRecovererErrorWriter }()
	recovererErrorWriter = &bytes.Buffer{}

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
