// Adapted from github.com/go-chi/chi/v5 v5.3.2, middleware/recoverer.go
// (itself derived from Goji's): the recover, re-panic and 500 logic is kept;
// the coloured "pretty stack" printer and the request-log-entry hook are
// replaced by a plain panic line and Go's own stack. Copyright (c)
// 2015-present Peter Kieltyka, Google Inc.; see LICENSE.
// SPDX-License-Identifier: MIT

package httpmw

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime/debug"
)

// recovererErrorWriter is where a recovered panic is reported; a var so tests
// can capture it.
var recovererErrorWriter io.Writer = os.Stderr

// Recoverer is a middleware that recovers from panics, writes the panic value
// and its stack to stderr, and answers 500 (Internal Server Error) when the
// response can still carry one. http.ErrAbortHandler is re-panicked, so the
// server aborts the response as it intends and logs nothing.
func Recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rvr := recover()
			if rvr == nil {
				return
			}
			if rvr == http.ErrAbortHandler { //nolint:errorlint // identity, as net/http's own check
				panic(rvr)
			}
			_, _ = fmt.Fprintf(recovererErrorWriter, "panic: %v\n\n%s", rvr, debug.Stack())
			if r.Header.Get("Connection") != "Upgrade" {
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
