package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/ElcanoTek/fleet/internal/accountevents"
)

// WithAccountEvents injects the account-events recorder (docs/ACCOUNT-EVENTS.md).
// nil (the default) leaves the feed off: nothing is queued.
func WithAccountEvents(rec *accountevents.Recorder) Option {
	return func(s *Server) { s.accountEvents = rec }
}

// accountEventTimeout bounds each half of the feed's work on a request: the
// snapshot before the write and the read-back + enqueue after it. Both run
// synchronously on the admin's request, so a stalled database must cost that
// request a bounded wait, never hold its goroutine indefinitely.
const accountEventTimeout = 10 * time.Second

// beginAccountChange snapshots email for the account-events feed and returns
// the func that publishes the resulting state when it changed. Handlers defer
// the returned func right before their first write, so every exit path (a
// partial failure included) reports what the two planes actually hold.
//
// The snapshot runs on the request's own context (bounded): before the write,
// a client that hung up has changed nothing, and a failed snapshot only makes
// Commit decline to guess. The publish runs detached from the request's
// cancellation — a client that hangs up after the write must not make the feed
// miss it — but under its own timeout.
func (s *Server) beginAccountChange(r *http.Request, email, source, actor string) func() {
	if s.accountEvents == nil {
		return func() {}
	}
	beginCtx, cancelBegin := context.WithTimeout(r.Context(), accountEventTimeout)
	change := s.accountEvents.Begin(beginCtx, email)
	cancelBegin()
	return func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), accountEventTimeout)
		defer cancel()
		change.CommitLogged(ctx, source, actor)
	}
}
