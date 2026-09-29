package httpapi

import (
	"context"
	"net/http"

	"github.com/ElcanoTek/fleet/internal/accountevents"
)

// WithAccountEvents injects the account-events recorder (docs/ACCOUNT-EVENTS.md).
// nil (the default) leaves the feed off: nothing is queued.
func WithAccountEvents(rec *accountevents.Recorder) Option {
	return func(s *Server) { s.accountEvents = rec }
}

// beginAccountChange snapshots email for the account-events feed and returns
// the func that publishes the resulting state when it changed. Handlers defer
// the returned func right before their first write, so every exit path (a
// partial failure included) reports what the two planes actually hold. The
// publish runs detached from the request's cancellation: a client that hangs
// up after the write must not make the feed miss it.
func (s *Server) beginAccountChange(r *http.Request, email, source, actor string) func() {
	if s.accountEvents == nil {
		return func() {}
	}
	ctx := context.WithoutCancel(r.Context())
	change := s.accountEvents.Begin(ctx, email)
	return func() { change.CommitLogged(ctx, source, actor) }
}
