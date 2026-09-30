package httpapi

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/accountevents"
	"github.com/ElcanoTek/fleet/internal/store"
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
// teamMemberLister is the store's view of who is in a team, for a rename.
type teamMemberLister interface {
	TeamMemberEmails(ctx context.Context, team string) ([]string, error)
}

// beginTeamRename is beginAccountChange for every member of team before a
// rename: each member's change is snapshotted and published on its own, with
// the admin as actor. A member list that cannot be read publishes nothing and
// says so (a resync repairs it), rather than guessing at who moved.
func (s *Server) beginTeamRename(r *http.Request, team string) func() {
	lister, ok := s.store.(teamMemberLister)
	if s.accountEvents == nil || !ok {
		return func() {}
	}
	listCtx, cancel := context.WithTimeout(r.Context(), accountEventTimeout)
	members, err := lister.TeamMemberEmails(listCtx, strings.TrimSpace(team))
	cancel()
	if err != nil {
		log.Printf("account events: read the members of team %q before a rename: %s (run `fleet account-events resync` to republish)",
			team, logSafe(err.Error()))
		return func() {}
	}
	actor := userFromCtx(r.Context())
	commits := make([]func(), 0, len(members))
	for _, email := range members {
		commits = append(commits, s.beginAccountChange(r, email, store.AccountEventSourceAdminUI, actor))
	}
	return func() {
		for _, commit := range commits {
			commit()
		}
	}
}

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
