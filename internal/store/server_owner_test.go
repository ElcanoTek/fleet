// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

// openSecondStore opens another store on dsn, as a second process would.
func openSecondStore(t *testing.T, dsn string) *Store {
	t.Helper()
	s, err := Open(dsn, testPool())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// holdAdvisoryLock takes key on a connection of its own (shared or
// exclusive) and returns the connection; closing it lets go, as a process
// that dies does.
func holdAdvisoryLock(t *testing.T, s *Store, key int64, shared bool) *sql.Conn {
	t.Helper()
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fn := "pg_try_advisory_lock"
	if shared {
		fn = "pg_try_advisory_lock_shared"
	}
	var got bool
	if err := conn.QueryRowContext(ctx, `SELECT `+fn+`($1)`, key).Scan(&got); err != nil || !got {
		discardSQLConn(conn)
		t.Fatalf("take lock: got %v, %v", got, err)
	}
	return conn
}

// A second server refuses while the first owns the database, naming it; one
// that waits gets the database as soon as the first lets go. A CLI opening
// the database meanwhile (as fleet migrate, backup or the admin CLI do) is
// not held up.
func TestServerOwnershipRefusesASecondServer(t *testing.T) {
	t.Parallel()
	a, dsn := newTestStoreDSN(t)
	b := openSecondStore(t, dsn)
	ctx := context.Background()
	ownerA, err := a.ClaimServerOwnership(ctx, ServerOwnershipOptions{Name: "fleet serve box-a pid 1"})
	if err != nil {
		t.Fatal(err)
	}
	defer ownerA.Release()

	_, err = b.ClaimServerOwnership(ctx, ServerOwnershipOptions{Name: "fleet serve box-b pid 2"})
	var owned *DatabaseOwnedError
	if !errors.Is(err, ErrDatabaseOwned) || !errors.As(err, &owned) {
		t.Fatalf("second server: err %v, want a DatabaseOwnedError", err)
	}
	if !strings.Contains(owned.Holder, "fleet serve box-a pid 1") {
		t.Fatalf("the refusal names %q, want the first server", owned.Holder)
	}

	cli := openSecondStore(t, dsn)
	if _, err := cli.ListUsers(ctx); err != nil {
		t.Fatalf("a CLI store while a server owns the database: %v", err)
	}

	type claim struct {
		o   *ServerOwnership
		err error
	}
	got := make(chan claim, 1)
	go func() {
		o, err := b.ClaimServerOwnership(ctx, ServerOwnershipOptions{Wait: 20 * time.Second, Name: "fleet serve box-b pid 2"})
		got <- claim{o, err}
	}()
	select {
	case c := <-got:
		if c.o != nil {
			c.o.Release()
		}
		t.Fatalf("a waiting server got the database (err %v) while the first owned it", c.err)
	case <-time.After(1500 * time.Millisecond):
	}
	ownerA.Release()
	select {
	case c := <-got:
		if c.err != nil {
			t.Fatalf("the waiting server after the first let go: %v", c.err)
		}
		c.o.Release()
	case <-time.After(15 * time.Second):
		t.Fatal("the waiting server did not get the database after the first let go")
	}
}

// A server that crashed lets go of the owner lock with its connection; the
// heartbeat row it left stays fresh, but it no longer moves, so the next
// server takes the database after watching it for the grace.
func TestServerOwnershipAfterTheOwnerCrashed(t *testing.T) {
	t.Parallel()
	s, dsn := newTestStoreDSN(t)
	next := openSecondStore(t, dsn)
	ctx := context.Background()
	crashed := holdAdvisoryLock(t, s, serverOwnerLockKey, false)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO core.server_presence (instance_id, last_seen) VALUES ('crashed', now())`); err != nil {
		t.Fatal(err)
	}
	type claim struct {
		o   *ServerOwnership
		err error
	}
	got := make(chan claim, 1)
	go func() {
		o, err := next.ClaimServerOwnership(ctx, ServerOwnershipOptions{Wait: 20 * time.Second, grace: 500 * time.Millisecond})
		got <- claim{o, err}
	}()
	time.Sleep(time.Second)
	discardSQLConn(crashed) // the crash
	start := time.Now()
	select {
	case c := <-got:
		if c.err != nil {
			t.Fatal(c.err)
		}
		c.o.Release()
		if time.Since(start) < 400*time.Millisecond {
			t.Fatalf("took the database %v after the crash, without watching the fresh row", time.Since(start))
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the next server did not take the database after the owner crashed")
	}
}

// A server whose connections dropped (a database outage, here longer than a
// heartbeat stays fresh) writes its row while it takes its owner lock back:
// a server starting meanwhile that finds the lock free sees the row move and
// lets go, however old the row was. Once the row stops moving it takes the
// database.
func TestServerOwnershipYieldsToAReconnectingServer(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO core.server_presence (instance_id, last_seen) VALUES ('reconnecting', now() - interval '2 minutes')`); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	writing := make(chan struct{})
	go func() {
		defer close(writing)
		// Starts after the starting server's first look, which finds the
		// row two minutes old.
		time.Sleep(300 * time.Millisecond)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				_, _ = s.db.ExecContext(ctx, `UPDATE core.server_presence SET last_seen = now() WHERE instance_id = 'reconnecting'`)
			}
		}
	}()
	_, err := s.ClaimServerOwnership(ctx, ServerOwnershipOptions{Wait: 3 * time.Second, grace: time.Second})
	close(stop)
	<-writing
	if !errors.Is(err, ErrDatabaseOwned) || !strings.Contains(err.Error(), "reconnecting") {
		t.Fatalf("with a moving heartbeat: err %v, want ErrDatabaseOwned naming a reconnecting server", err)
	}
	o, err := s.ClaimServerOwnership(ctx, ServerOwnershipOptions{Wait: 10 * time.Second, grace: time.Second})
	if err != nil {
		t.Fatalf("with the heartbeat stopped: %v", err)
	}
	o.Release()
}

// A running server without the owner lock — an earlier release, which holds
// only the presence lock — keeps a new server out too.
func TestServerOwnershipYieldsToAServerWithoutTheOwnerLock(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	earlier := holdAdvisoryLock(t, s, serverPresenceLockKey, true)
	_, err := s.ClaimServerOwnership(ctx, ServerOwnershipOptions{})
	if !errors.Is(err, ErrDatabaseOwned) || !strings.Contains(err.Error(), "does not hold the owner lock") {
		discardSQLConn(earlier)
		t.Fatalf("with a server holding only the presence lock: err %v", err)
	}
	discardSQLConn(earlier)
	o, err := s.ClaimServerOwnership(ctx, ServerOwnershipOptions{Wait: 10 * time.Second})
	if err != nil {
		t.Fatalf("after it stopped: %v", err)
	}
	o.Release()
}

// The owner lock and the presence lock compose: a CLI account deletion
// (presence exclusive) does not keep a server from taking ownership — the
// server then waits for the deletion before it marks itself running, as
// before — and once it runs the CLI deletion is refused.
func TestServerOwnershipWithACLIDeletion(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	unlockCLI, err := s.ExcludeServers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	o, err := s.ClaimServerOwnership(ctx, ServerOwnershipOptions{})
	if err != nil {
		unlockCLI()
		t.Fatalf("taking ownership during a CLI deletion: %v", err)
	}
	defer o.Release()
	type held struct {
		release func()
		err     error
	}
	started := make(chan held, 1)
	go func() {
		r, err := s.HoldServerPresence(ctx)
		started <- held{r, err}
	}()
	select {
	case h := <-started:
		if h.release != nil {
			h.release()
		}
		unlockCLI()
		t.Fatalf("the server marked itself running (err %v) during a CLI deletion", h.err)
	case <-time.After(1500 * time.Millisecond):
	}
	unlockCLI()
	var release func()
	select {
	case h := <-started:
		if h.err != nil {
			t.Fatal(h.err)
		}
		release = h.release
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not mark itself running after the CLI deletion")
	}
	defer release()
	if _, err := s.ExcludeServers(ctx); !errors.Is(err, ErrServerRunning) {
		t.Fatalf("a CLI deletion with the server running: err %v, want ErrServerRunning", err)
	}
}

// ownerSession is the one session holding the owner lock.
func ownerSession(t *testing.T, s *Store) lockHolder {
	t.Helper()
	hs, err := lockHolders(context.Background(), s.db, serverOwnerLockKey, "ExclusiveLock")
	if err != nil || len(hs) != 1 {
		t.Fatalf("owner lock holders: %+v, %v; want one", hs, err)
	}
	return hs[0]
}

// waitOwnerSessionReplaced ends the session holding the owner lock and waits
// until a new session named name holds it.
func waitOwnerSessionReplaced(t *testing.T, s *Store, name string) {
	t.Helper()
	old := ownerSession(t, s)
	terminateOwnerSessions(t, s)
	deadline := time.Now().Add(15 * time.Second)
	for {
		time.Sleep(100 * time.Millisecond)
		hs, err := lockHolders(context.Background(), s.db, serverOwnerLockKey, "ExclusiveLock")
		if err == nil && len(hs) == 1 && hs[0].name == name && hs[0].pid != old.pid {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the owner did not take its lock back on a new session (holders %+v, %v)", hs, err)
		}
	}
}

// terminateOwnerSessions ends the sessions holding the owner lock, as a
// database restart does.
func terminateOwnerSessions(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), `
		SELECT pg_terminate_backend(pid) FROM pg_locks
		 WHERE locktype = 'advisory' AND granted AND objsubid = 1
		   AND classid::bigint = $1 AND objid::bigint = $2
		   AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`,
		serverOwnerLockKey>>32, serverOwnerLockKey&0xffffffff); err != nil {
		t.Fatal(err)
	}
}

// An owner whose lock connection drops takes the lock back; if another
// session takes it first and keeps it, the claim is lost, and Lost says so.
func TestServerOwnershipTakenBackOrLost(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	const name = "fleet serve owner pid 7"
	o, err := s.ClaimServerOwnership(ctx, ServerOwnershipOptions{Name: name, grace: 200 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Release()

	waitOwnerSessionReplaced(t, s, name)
	if err := o.Err(); err != nil {
		t.Fatalf("after taking the lock back: Err %v", err)
	}

	// Another session takes the lock the moment the owner's drops, and keeps it.
	other := grabOwnerLock(t, s, "")
	defer discardSQLConn(other)
	select {
	case <-o.Lost():
		if !errors.Is(o.Err(), ErrDatabaseOwned) {
			t.Fatalf("lost claim: Err %v, want ErrDatabaseOwned", o.Err())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the claim was not reported lost while another session kept the lock")
	}
}

// Every connection holding a server lock is named and carries the lock
// session settings: the owner's, the presence lock's and the CLI account
// deletion's, so Postgres lets go of a vanished host's lock within about
// half a minute.
func TestServerLockSessionsAreNamedAndKeptAlive(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer discardSQLConn(conn)
	if err := configureLockSession(ctx, conn, "fleet serve test pid 1"); err != nil {
		t.Fatal(err)
	}
	var name string
	var tcp bool
	if err := conn.QueryRowContext(ctx, `SELECT current_setting('application_name'), inet_client_addr() IS NOT NULL`).Scan(&name, &tcp); err != nil {
		t.Fatal(err)
	}
	if name != "fleet serve test pid 1" {
		t.Fatalf("application_name = %q", name)
	}
	if tcp { // Postgres ignores (and reads 0 for) these on a Unix socket
		for _, kv := range lockSessionSettings {
			var got string
			// pg_settings shows this session's value, in the setting's base unit.
			if err := conn.QueryRowContext(ctx, `SELECT setting FROM pg_settings WHERE name = $1`, kv[0]).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != kv[1] {
				t.Errorf("%s = %q, want %s", kv[0], got, kv[1])
			}
		}
	}

	o, err := s.ClaimServerOwnership(ctx, ServerOwnershipOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Release()
	release, err := s.HoldServerPresence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	instance, err := s.serverInstanceID()
	if err != nil {
		t.Fatal(err)
	}
	named := func(role string) string {
		t.Helper()
		n, err := s.lockSessionName(role)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(n, role+" "+instance+" ") {
			t.Errorf("%q does not start with its role and instance id", n)
		}
		return truncateName(n)
	}
	if h := ownerSession(t, s); h.name != named("fleet serve") {
		t.Errorf("owner session named %q, want %q", h.name, named("fleet serve"))
	}
	presence, err := lockHolders(ctx, s.db, serverPresenceLockKey, "ShareLock")
	if err != nil || len(presence) != 1 || presence[0].name != named("fleet presence") {
		t.Errorf("presence sessions %+v, %v; want one named %q", presence, err, named("fleet presence"))
	}
	release()
	unlock, err := s.ExcludeServers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	cli, err := lockHolders(ctx, s.db, serverPresenceLockKey, "ExclusiveLock")
	if err != nil || len(cli) != 1 || cli[0].name != named("fleet account deletion") {
		t.Errorf("CLI deletion sessions %+v, %v; want one named %q", cli, err, named("fleet account deletion"))
	}
}

// grabOwnerLock ends the owner's lock session and takes the lock the moment
// it is free on a connection of its own named name ("" leaves it unnamed).
func grabOwnerLock(t *testing.T, s *Store, name string) *sql.Conn {
	t.Helper()
	ctx := context.Background()
	for attempt := 0; attempt < 5; attempt++ {
		terminateOwnerSessions(t, s)
		until := time.Now().Add(2 * time.Second)
		for time.Now().Before(until) {
			conn, err := s.db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if name != "" {
				if _, err := conn.ExecContext(ctx, `SELECT set_config('application_name', $1, false)`, name); err != nil {
					t.Fatal(err)
				}
			}
			var got bool
			if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, serverOwnerLockKey).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got {
				return conn
			}
			discardSQLConn(conn)
			time.Sleep(10 * time.Millisecond)
		}
	}
	t.Fatal("never took the owner lock from the owner")
	return nil
}

// While the owner takes its lock back, a holder it cannot see (the read
// failed, or the holder just let go) never counts toward the takeover: the
// claim is not lost, and the owner keeps trying until it has the lock.
func TestServerOwnershipUnseenHolderDoesNotLoseTheClaim(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	const name = "fleet serve unseen pid 8"
	o, err := s.ClaimServerOwnership(ctx, ServerOwnershipOptions{
		Name: name, grace: 100 * time.Millisecond,
		holders: func(context.Context) ([]lockHolder, error) { return nil, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Release()
	other := grabOwnerLock(t, s, "")
	select {
	case <-o.Lost():
		discardSQLConn(other)
		t.Fatalf("claim lost to a holder the owner could not see: %v", o.Err())
	case <-time.After(3 * time.Second): // ten takeovers
	}
	discardSQLConn(other)
	deadline := time.Now().Add(15 * time.Second)
	for {
		hs, err := lockHolders(ctx, s.db, serverOwnerLockKey, "ExclusiveLock")
		if err == nil && len(hs) == 1 && hs[0].name == name {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the owner did not take its lock back (holders %+v, %v)", hs, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := o.Err(); err != nil {
		t.Fatal(err)
	}
}

// sessionOf is the backend pid and start time of conn's session.
func sessionOf(t *testing.T, conn *sql.Conn) lockSession {
	t.Helper()
	var sess lockSession
	if err := conn.QueryRowContext(context.Background(),
		`SELECT pid, backend_start FROM pg_stat_activity WHERE pid = pg_backend_pid()`).Scan(&sess.pid, &sess.start); err != nil {
		t.Fatal(err)
	}
	return sess
}

// The owner's own earlier session — discarded after a ping timed out, but
// not yet ended by Postgres — still holding the lock is not another server:
// the owner ends it and takes the lock back instead of stopping. The session
// is known as its own by backend pid and start time.
func TestServerOwnershipEndsItsOwnLingeringSession(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	const name = "fleet serve self pid 9"
	o, err := s.ClaimServerOwnership(ctx, ServerOwnershipOptions{Name: name, grace: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Release()
	lingering := grabOwnerLock(t, s, "")
	defer discardSQLConn(lingering)
	sess := sessionOf(t, lingering)
	o.own.add(sess) // as if this process had held the lock on it
	deadline := time.Now().Add(15 * time.Second)
	for {
		hs, err := lockHolders(ctx, s.db, serverOwnerLockKey, "ExclusiveLock")
		if err == nil && len(hs) == 1 && hs[0].name == name && hs[0].pid != sess.pid {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the owner did not end its own lingering session and take the lock back (holders %+v, %v)", hs, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(time.Second) // several takeovers
	if err := o.Err(); err != nil {
		t.Fatalf("claim lost to the owner's own session: %v", err)
	}
}

// A session carrying the owner's very name that the owner never held the
// lock on — a clone running as the same pid on a host of the same name — is
// another server: it is not ended, and the claim is lost.
func TestServerOwnershipDoesNotEndAForeignSessionOfTheSameName(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	const name = "fleet serve clone pid 1"
	o, err := s.ClaimServerOwnership(ctx, ServerOwnershipOptions{Name: name, grace: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer o.Release()
	clone := grabOwnerLock(t, s, name)
	defer discardSQLConn(clone)
	select {
	case <-o.Lost():
		if !errors.Is(o.Err(), ErrDatabaseOwned) {
			t.Fatalf("lost claim: Err %v, want ErrDatabaseOwned", o.Err())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the claim was not reported lost to a session of the same name")
	}
	if err := clone.PingContext(ctx); err != nil {
		t.Fatalf("the owner ended a session it never held the lock on: %v", err)
	}
	if h := ownerSession(t, s); h.pid != sessionOf(t, clone).pid {
		t.Fatalf("owner lock held by %+v, want the clone", h)
	}
}

func TestTruncateOwnerName(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("a", 62) + "é" // é straddles byte 63
	if got := truncateName(long); got != strings.Repeat("a", 62) {
		t.Fatalf("truncateName = %q", got)
	}
	if got := truncateName("fleet serve h pid 1"); got != "fleet serve h pid 1" {
		t.Fatalf("truncateName = %q", got)
	}
}
