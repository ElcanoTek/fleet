package httpapi

import (
	"context"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/store"
)

// teardownGrace bounds how long a fixture waits for cancelled turns. Generous
// enough for a real DB write to land, short enough that a genuine leak is a test
// failure rather than a hung package.
const teardownGrace = 20 * time.Second

// stopServerFixture tears a DB-backed fixture down in the only order that is
// safe, and fails the test if it cannot.
//
// The order is turns, then non-turn background work, then the store. Closing the
// store first — which is what these fixtures used to do — left detached
// goroutines writing into a dead pool, logging "sql: database is closed" into
// whatever test ran next. When the tests shared one database, the worse half
// was a goroutine from a finished test still writing to it while the next
// test's fixture truncated it — the lock-order collision that deadlocked
// TRUNCATE in CI. Every test now has a database of its own (internal/testdb,
// dropped WITH (FORCE) at cleanup), so that collision cannot happen, but a
// leaked turn is still a bug — it can log into a finished test, or fail on
// its dropped database — and this is where it gets fixed.
//
// This is deliberately the same sequence cmd/fleet runs on SIGTERM —
// BeginShutdown, cancel, drain, stop background — because the fixture needs the
// same guarantee the server does, and a teardown that invented its own order
// would be a second, weaker one to keep correct.
//
// BeginShutdown first is the step that is easy to miss and does the real work.
// Cancelling a turn is not enough on its own: a turn's completion tail drains the
// input queue and launches the next queued row, so a test that leaves rows queued
// (the depth-cap test leaves ten) turns one cancellation into a fresh turn, and
// the chain re-arms itself faster than any grace period can outlast it. The
// shuttingDown flag is what makes maybeDrainQueue decline to launch, which is
// exactly why production sets it before draining rather than after.
//
// Cancel rather than wait, because a test may deliberately park a turn — the
// queue tests gate one open to exercise queueing, so waiting for it to finish on
// its own would hang the package. Every turn context is cancellable and agentcore
// exits at its next checkpoint, so cancellation is the teardown signal.
//
// The failure is loud on purpose. A turn that ignores cancellation for the full
// grace is a leak of exactly the kind this function exists to prevent, and a
// silent timeout here would hand the next test a live writer again.
func stopServerFixture(t *testing.T, srv *Server, st *store.Store) {
	t.Helper()

	srv.BeginShutdown()
	srv.CancelInflightTurns()
	ctx, cancel := context.WithTimeout(context.Background(), teardownGrace)
	defer cancel()
	if !srv.DrainTurns(ctx) {
		t.Errorf("turn goroutines still running %s after cancellation — a leaked turn "+
			"will write into the next test's database (ActiveTurns=%d)", teardownGrace, srv.ActiveTurns())
	}
	srv.StopBackground()
	_ = st.Close()
}
