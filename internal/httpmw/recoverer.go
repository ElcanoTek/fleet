// Adapted from github.com/go-chi/chi/v5 v5.3.2, middleware/recoverer.go
// (itself derived from Goji's): the recover, re-panic and 500 logic is kept;
// the coloured "pretty stack" printer and the request-log-entry hook are
// replaced by internal/safe's structured panic event. Copyright (c)
// 2015-present Peter Kieltyka, Google Inc.; see LICENSE.
// SPDX-License-Identifier: MIT

package httpmw

import (
	"net/http"

	"github.com/ElcanoTek/fleet/internal/safe"
)

// Recoverer is the orchestrator's panic middleware: RecoverWith with a plain
// 500.
func Recoverer(next http.Handler) http.Handler {
	return RecoverWith("httpmw.handler", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})(next)
}

// RecoverWith is THE HTTP panic middleware; every router's recoverer is built
// from it. A panic in a handler is handed to safe.EmitPanicWithMetadata —
// one structured "panic recovered" event through the configured log handler
// (format, redactor, FLEET_LOG_FILE), with the incident id, the panic class
// and the route, never the panic value or the stack, which can carry request
// data or credentials — plus safe's counters and hooks. respond then answers
// the request (a 500) unless the connection is being upgraded.
// http.ErrAbortHandler is re-panicked, so the server aborts the response as
// it intends and logs nothing.
func RecoverWith(boundary string, respond func(w http.ResponseWriter, r *http.Request)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rvr := recover()
				if rvr == nil {
					return
				}
				if rvr == http.ErrAbortHandler { //nolint:errorlint // identity, as net/http's own check
					panic(rvr)
				}
				safe.EmitPanicWithMetadata(safe.PanicMetadata{
					Location: boundary + " " + r.Method + " " + r.URL.Path,
					Boundary: boundary,
				}, rvr, nil)
				if r.Header.Get("Connection") != "Upgrade" {
					respond(w, r)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
