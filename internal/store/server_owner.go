// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ElcanoTek/fleet/internal/logging"
)

// One `fleet serve` owns a database at a time (ADR-0122). Its boot recovery
// treats every turn still 'running', every claimed input and every executing
// approval as left behind by a crashed predecessor, so a second server that
// reached it while the first still ran would end the first's live work. The
// owner is whoever holds serverOwnerLockKey, a session-level advisory lock
// taken exclusive on a connection of its own (ClaimServerOwnership) before
// boot recovery or any other write of the server's: Postgres lets go of it
// when that connection ends, so a crashed owner holds nothing.
//
// Only `fleet serve` takes it. The CLIs that run alongside a server (fleet
// migrate, backup, admin, task run, doctor, status) never do, and the CLI
// account deletion keeps to the presence lock (server_presence.go), a
// different key: a server takes the owner lock first and the presence lock
// second, the CLI only the presence lock, so the two never wait on each other
// in a cycle.
const serverOwnerLockKey int64 = 0x464c545f4f574e52 // "FLT_OWNR"

// ErrDatabaseOwned is ClaimServerOwnership's answer while another fleet
// server owns the database; the error it is wrapped in names that server.
var ErrDatabaseOwned = errors.New("another fleet server owns this database")

// DatabaseOwnedError is ErrDatabaseOwned with what is known of the server
// that owns the database.
type DatabaseOwnedError struct {
	// Holder describes the owner: the name its session carries (host and
	// pid), its address and when it connected, or why the database counts
	// as owned when no session holds the owner lock.
	Holder string
}

func (e *DatabaseOwnedError) Error() string {
	if e.Holder == "" {
		return ErrDatabaseOwned.Error()
	}
	return ErrDatabaseOwned.Error() + ": " + e.Holder
}

// Is makes errors.Is(err, ErrDatabaseOwned) hold.
func (e *DatabaseOwnedError) Is(target error) bool { return target == ErrDatabaseOwned }

var (
	// serverOwnerPoll is how often a server waiting for the owner lock tries
	// it again.
	serverOwnerPoll = time.Second
	// serverOwnerWaitLog is how often a waiting server says it waits.
	serverOwnerWaitLog = 10 * time.Second
	// serverHeartbeatKept is how old a heartbeat row may be and still be
	// watched by a starting server: the age past which heartbeat() deletes
	// rows.
	serverHeartbeatKept = time.Hour
)

// lockSessionSettings are set on every connection that holds one of the
// server locks: a server's owner and presence connections and the CLI
// account deletion's. They bound how long Postgres keeps the session (and
// the lock) of a holder whose host vanished without closing its connection —
// a lost node, say — to about half a minute, instead of the operating
// system's defaults (keepalive after two hours, retransmissions for about
// fifteen minutes); a process that dies on a host that stays up has its
// socket closed at once. Postgres ignores them on a Unix-socket connection,
// where the peer is on the same host.
var lockSessionSettings = [][2]string{
	{"tcp_keepalives_idle", "10"},
	{"tcp_keepalives_interval", "5"},
	{"tcp_keepalives_count", "3"},
	{"tcp_user_timeout", "25000"},
}

// configureLockSession names conn's session (application_name, which a
// refused server prints) and applies lockSessionSettings, each best effort.
// The settings would follow the connection back into the pool, so a
// connection configured here is discarded, never returned.
func configureLockSession(ctx context.Context, conn *sql.Conn, name string) error {
	if _, err := conn.ExecContext(ctx, `SELECT set_config('application_name', $1, false)`, truncateName(name)); err != nil {
		return err
	}
	for _, kv := range lockSessionSettings {
		if _, err := conn.ExecContext(ctx, `SELECT set_config($1, $2, false)`, kv[0], kv[1]); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			logging.From(ctx).Debug("server lock: session setting not applied", "setting", kv[0], "err", err)
		}
	}
	return nil
}

// ServerOwnershipOptions configure ClaimServerOwnership.
type ServerOwnershipOptions struct {
	// Wait is how long to wait for another owner to let go before refusing.
	Wait time.Duration
	// Name is the application_name of the owner connection, which a refused
	// server prints to name the owner; "fleet serve <instance> <host> pid
	// <pid>" when empty (lockSessionName).
	Name string

	// grace is how long a server that took the lock while a heartbeat row
	// exists watches the row before it trusts it is a crashed server's
	// (serverPresenceGrace when zero).
	grace time.Duration
	// takeover is how long the lock must stay held by another session while
	// the owner tries to take it back before the owner counts it lost
	// (3 × grace when zero).
	takeover time.Duration
	// holders lists the sessions holding the owner lock while the owner
	// takes it back (ownerLockHolders when nil); a test seam.
	holders func(ctx context.Context) ([]lockHolder, error)
}

// ServerOwnership is a held claim on the database (ClaimServerOwnership).
type ServerOwnership struct {
	lost    chan struct{}
	lostErr error
	release func()
	own     ownSessions
}

// lockSession identifies one Postgres session: its backend pid and start
// time, which together never repeat (a pid alone is reused).
type lockSession struct {
	pid   int
	start time.Time
}

// ownSessions are the sessions this process has held the owner lock on.
// The application_name alone cannot say a session is ours: every container
// runs fleet as pid 1, clones share host names, and a long name loses its
// tail to Postgres' 63-byte limit.
type ownSessions struct {
	mu   sync.Mutex
	seen []lockSession
}

func (o *ownSessions) add(sess lockSession) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.seen = append(o.seen, sess)
}

// has reports whether h is a session this process held the lock on.
func (o *ownSessions) has(h lockHolder) bool {
	if !h.since.Valid {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, sess := range o.seen {
		if sess.pid == h.pid && sess.start.Equal(h.since.Time) {
			return true
		}
	}
	return false
}

// Lost is closed when the claim is lost to another server: the owner
// connection dropped and another session holds the lock since. The server
// must stop; Err says why.
func (o *ServerOwnership) Lost() <-chan struct{} { return o.lost }

// Err is the reason the claim was lost, nil while it is held.
func (o *ServerOwnership) Err() error {
	select {
	case <-o.lost:
		return o.lostErr
	default:
		return nil
	}
}

// Release lets go of the lock and its connection; it is safe to call more
// than once.
func (o *ServerOwnership) Release() { o.release() }

// ClaimServerOwnership makes this process the database's one running fleet
// server, before it recovers or writes anything. It takes the owner lock
// exclusive on a new connection named opts.Name, then makes sure no other
// server runs without it:
//
//   - a session holding the presence lock shared is a running server that
//     does not hold the owner lock (an earlier release, or one whose owner
//     connection dropped and is reconnecting);
//   - a heartbeat row in core.server_presence (of the last
//     serverHeartbeatKept) is a server that ran. The row of one that stopped
//     cleanly is gone; one left behind is a crashed server's or that of a
//     server reconnecting after its connections dropped — however long the
//     outage, so the row's age says nothing — which writes its row twice a
//     second while it tries to take the lock back. So the rows are looked
//     at again opts.grace later, holding the lock: a row written meanwhile
//     is a running server. A clean restart, with no row, does not wait.
//
// While any of that says another server runs it lets go, waits and tries
// again, saying every serverOwnerWaitLog whom it waits for, and after
// opts.Wait answers a *DatabaseOwnedError naming that server, holding
// nothing. It returns ctx's error, holding nothing, when ctx ends first.
//
// Once held, the connection is checked every serverPresenceCheck and taken
// again when it drops (keepOwnerLock); Lost reports the claim lost when
// another server holds the lock meanwhile and keeps it.
func (s *Store) ClaimServerOwnership(ctx context.Context, opts ServerOwnershipOptions) (_ *ServerOwnership, err error) {
	if s == nil || s.db == nil {
		return &ServerOwnership{lost: make(chan struct{}), release: func() {}}, nil
	}
	if opts.Name == "" {
		if opts.Name, err = s.lockSessionName("fleet serve"); err != nil {
			return nil, err
		}
	}
	if opts.grace <= 0 {
		opts.grace = serverPresenceGrace
	}
	if opts.takeover <= 0 {
		opts.takeover = 3 * opts.grace
	}
	if opts.holders == nil {
		opts.holders = func(ctx context.Context) ([]lockHolder, error) {
			return lockHolders(ctx, s.db, serverOwnerLockKey, "ExclusiveLock")
		}
	}
	instance, err := s.serverInstanceID()
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(opts.Wait)
	var lastLog time.Time
	for {
		conn, sess, holder, err := s.tryServerOwnership(ctx, opts, instance)
		if err == nil && conn != nil {
			return s.keepServerOwnership(ctx, conn, sess, opts, instance), nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			logging.From(ctx).Warn("database ownership: taking the owner lock failed; retrying", "err", err)
		}
		if time.Now().After(deadline) {
			if holder == "" && err != nil {
				return nil, fmt.Errorf("database ownership: %w", err)
			}
			return nil, &DatabaseOwnedError{Holder: holder}
		}
		if holder != "" && time.Since(lastLog) >= serverOwnerWaitLog {
			logging.From(ctx).Warn("another fleet server owns this database; waiting for it to stop",
				"owner", holder, "gives_up_in", time.Until(deadline).Round(time.Second).String())
			lastLog = time.Now()
		}
		if !sleepCtx(ctx, serverOwnerPoll) {
			return nil, ctx.Err()
		}
	}
}

// tryServerOwnership is one try of ClaimServerOwnership: the owner
// connection, holding the lock, when no other server runs; otherwise no
// connection and holder naming why the database counts as owned. A read it
// cannot make is an error, never a free database.
func (s *Store) tryServerOwnership(ctx context.Context, opts ServerOwnershipOptions, instance string) (conn *sql.Conn, sess lockSession, holder string, err error) {
	conn, sess, got, err := s.tryOwnerLock(ctx, opts.Name)
	if err != nil {
		return nil, sess, "", err
	}
	if !got {
		hs, _ := lockHolders(ctx, s.db, serverOwnerLockKey, "ExclusiveLock")
		if holder = describeHolders(hs, "fleet serve"); holder == "" {
			holder = "a session holding the owner lock"
		}
		return nil, sess, holder, nil
	}
	presence, err := lockHolders(ctx, conn, serverPresenceLockKey, "ShareLock")
	if err != nil {
		s.unlockOwner(ctx, conn)
		return nil, sess, "", err
	}
	if len(presence) > 0 {
		s.unlockOwner(ctx, conn)
		return nil, sess, "a fleet server that does not hold the owner lock (an earlier release, or one reconnecting to the database): " + describeHolders(presence, "a session"), nil
	}
	first, err := heartbeatRows(ctx, conn, instance)
	if err != nil {
		s.unlockOwner(ctx, conn)
		return nil, sess, "", err
	}
	if len(first) == 0 {
		return conn, sess, "", nil
	}
	if !sleepCtx(ctx, opts.grace) {
		s.unlockOwner(ctx, conn)
		return nil, sess, "", ctx.Err()
	}
	second, err := heartbeatRows(ctx, conn, instance)
	if err != nil {
		s.unlockOwner(ctx, conn)
		return nil, sess, "", err
	}
	for id, seen := range second {
		if was, ok := first[id]; !ok || seen.After(was) {
			s.unlockOwner(ctx, conn)
			// Let a reconnecting server take its lock back before trying again.
			sleepCtx(ctx, opts.grace)
			return nil, sess, "a fleet server reconnecting to the database (its heartbeat is still being written)", nil
		}
	}
	return conn, sess, "", nil
}

// tryOwnerLock takes the owner lock exclusive without waiting, on a new
// dedicated connection named name (configureLockSession), and returns the
// session it holds it on; got is false (and no connection kept) while
// another session holds it.
func (s *Store) tryOwnerLock(ctx context.Context, name string) (*sql.Conn, lockSession, bool, error) {
	var sess lockSession
	conn, err := s.lockConn(ctx)
	if err != nil {
		return nil, sess, false, err
	}
	if err := configureLockSession(ctx, conn, name); err != nil {
		discardSQLConn(conn)
		return nil, sess, false, err
	}
	var got bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, serverOwnerLockKey).Scan(&got); err != nil {
		discardSQLConn(conn)
		return nil, sess, false, err
	}
	if !got {
		discardSQLConn(conn)
		return nil, sess, false, nil
	}
	if err := conn.QueryRowContext(ctx,
		`SELECT pid, backend_start FROM pg_catalog.pg_stat_activity WHERE pid = pg_catalog.pg_backend_pid()`).Scan(&sess.pid, &sess.start); err != nil {
		s.unlockOwner(ctx, conn)
		return nil, sess, false, err
	}
	return conn, sess, true, nil
}

// unlockOwner releases the owner lock and discards its connection.
func (s *Store) unlockOwner(ctx context.Context, conn *sql.Conn) {
	unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, _ = conn.ExecContext(unlockCtx, `SELECT pg_advisory_unlock($1)`, serverOwnerLockKey)
	discardSQLConn(conn)
}

// keepServerOwnership starts keepOwnerLock on conn and returns the claim.
func (s *Store) keepServerOwnership(ctx context.Context, conn *sql.Conn, sess lockSession, opts ServerOwnershipOptions, instance string) *ServerOwnership {
	o := &ServerOwnership{lost: make(chan struct{})}
	o.own.add(sess)
	life, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.keepOwnerLock(life, conn, opts, instance, o)
	}()
	var once sync.Once
	o.release = func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
	return o
}

// keepOwnerLock holds conn (and with it the lock) until ctx ends, then
// unlocks it. Its connection is checked every serverPresenceCheck; one found
// dead is discarded and the lock taken again (retakeOwnerLock). When another
// server holds it meanwhile and keeps it, the claim is lost (o.lost) and
// keepOwnerLock returns.
func (s *Store) keepOwnerLock(ctx context.Context, conn *sql.Conn, opts ServerOwnershipOptions, instance string, o *ServerOwnership) {
	t := time.NewTicker(serverPresenceCheck)
	defer t.Stop()
	wroteHeartbeat := false
	// The retake writes this server's heartbeat row. A server that stops
	// before it held the presence lock (stopped while it waited for a CLI
	// deletion) has no presence heartbeat to remove it, so it goes here; the
	// presence release (first, on a normal stop) removes the same row.
	defer func() {
		if wroteHeartbeat {
			delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_, _ = s.db.ExecContext(delCtx, `DELETE FROM core.server_presence WHERE instance_id = $1`, instance)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			if conn != nil {
				s.unlockOwner(ctx, conn)
			}
			return
		case <-t.C:
		}
		pingCtx, cancel := context.WithTimeout(ctx, serverPresencePing)
		err := conn.PingContext(pingCtx)
		cancel()
		if err == nil || ctx.Err() != nil {
			continue
		}
		logging.From(ctx).Warn("database ownership: the owner lock's connection dropped; taking it again", "err", err)
		discardSQLConn(conn)
		wroteHeartbeat = true
		var holder string
		if conn, holder = s.retakeOwnerLock(ctx, opts, instance, &o.own); conn == nil {
			if holder != "" {
				o.lostErr = &DatabaseOwnedError{Holder: holder}
				close(o.lost)
				logging.From(ctx).Error("database ownership: lost to another fleet server; this server must stop", "owner", holder)
			}
			return
		}
		logging.From(ctx).Info("database ownership: owner lock taken again")
	}
}

// retakeOwnerLock takes the owner lock again after its connection dropped,
// trying every serverPresenceRetry and writing the heartbeat row on every
// try, so a server starting meanwhile sees the row move and lets go
// (tryServerOwnership). It returns the connection, or nil when ctx ends
// (holder empty) or another server owns the database (holder names it).
//
// Each try the lock is refused, the holders are looked at:
//
//   - none listed (the holder just let go) or a failed read: it says
//     nothing, and the next try decides;
//   - only sessions this process held the lock on (own, matched by backend
//     pid and start time, never by name): its old session, discarded after a
//     ping timed out in a network stall but not yet ended by Postgres. It is
//     ended (pg_terminate_backend; the same role may) and the lock tried
//     again;
//   - anything else, through every try for opts.takeover — longer than a
//     starting server holds the lock while it looks — is another server
//     that owns the database.
func (s *Store) retakeOwnerLock(ctx context.Context, opts ServerOwnershipOptions, instance string, own *ownSessions) (*sql.Conn, string) {
	var heldSince time.Time
	lastLog := time.Now()
	for {
		tryCtx, cancel := context.WithTimeout(ctx, serverPresencePing)
		hbErr := s.heartbeat(tryCtx, instance)
		conn, sess, got, err := s.tryOwnerLock(tryCtx, opts.Name)
		var hs []lockHolder
		if !got && err == nil {
			hs, err = opts.holders(tryCtx)
		}
		mine := !got && err == nil && allOwn(hs, own)
		if mine {
			for _, h := range hs {
				_, _ = s.db.ExecContext(tryCtx, `SELECT pg_catalog.pg_terminate_backend($1)`, h.pid)
			}
		}
		cancel()
		if got {
			own.add(sess)
			return conn, ""
		}
		if ctx.Err() != nil {
			return nil, ""
		}
		switch {
		case err != nil || len(hs) == 0:
			heldSince = time.Time{}
		case mine:
			heldSince = time.Time{}
			logging.From(ctx).Warn("database ownership: ended this server's own earlier owner session, which still held the lock", "sessions", len(hs))
		case heldSince.IsZero():
			heldSince = time.Now()
		case time.Since(heldSince) >= opts.takeover:
			return nil, describeHolders(hs, "fleet serve")
		}
		if time.Since(lastLog) >= serverPresenceWaitLog {
			logging.From(ctx).Warn("database ownership: still taking the owner lock again", "err", errors.Join(err, hbErr))
			lastLog = time.Now()
		}
		if !sleepCtx(ctx, serverPresenceRetry) {
			return nil, ""
		}
	}
}

// queryer is what lockHolders and heartbeatRows read through: the pool or
// one connection.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// lockHolder is one session holding an advisory lock.
type lockHolder struct {
	pid   int
	name  string // application_name
	addr  string
	since sql.NullTime
}

// lockHolders lists the other sessions holding the advisory lock key in mode
// in this database, oldest first. Every role sees a session's pid and
// application_name; the address and connection time of another role's
// session read as empty (pg_stat_activity hides them).
func lockHolders(ctx context.Context, q queryer, key int64, mode string) ([]lockHolder, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT l.pid, COALESCE(a.application_name, ''), COALESCE(host(a.client_addr), ''), a.backend_start
		  FROM pg_catalog.pg_locks l
		  JOIN pg_catalog.pg_stat_activity a ON a.pid = l.pid
		 WHERE l.locktype = 'advisory' AND l.granted AND l.objsubid = 1
		   AND l.classid::bigint = $1 AND l.objid::bigint = $2 AND l.mode = $3
		   AND l.pid <> pg_catalog.pg_backend_pid()
		   AND l.database = (SELECT oid FROM pg_catalog.pg_database WHERE datname = pg_catalog.current_database())
		 ORDER BY a.backend_start`,
		key>>32, key&0xffffffff, mode)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []lockHolder
	for rows.Next() {
		var h lockHolder
		if err := rows.Scan(&h.pid, &h.name, &h.addr, &h.since); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// describeHolders names each holder by its application_name (fallback when
// it has none), address and connection time; "" for none.
func describeHolders(hs []lockHolder, fallback string) string {
	out := make([]string, 0, len(hs))
	for _, h := range hs {
		desc := h.name
		if desc == "" {
			desc = fallback
		}
		if h.addr != "" {
			desc += " from " + h.addr
		}
		if h.since.Valid {
			desc += ", connected since " + h.since.Time.UTC().Format(time.RFC3339)
		}
		out = append(out, desc)
	}
	return strings.Join(out, "; ")
}

// allOwn reports whether every holder is a session this process held the
// owner lock on.
func allOwn(hs []lockHolder, own *ownSessions) bool {
	for _, h := range hs {
		if !own.has(h) {
			return false
		}
	}
	return len(hs) > 0
}

// heartbeatRows returns the heartbeat rows of other servers written within
// serverHeartbeatKept, by instance.
func heartbeatRows(ctx context.Context, q queryer, instance string) (map[string]time.Time, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT instance_id, last_seen FROM core.server_presence
		  WHERE instance_id <> $1 AND last_seen > now() - make_interval(secs => $2)`,
		instance, serverHeartbeatKept.Seconds())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	seen := map[string]time.Time{}
	for rows.Next() {
		var id string
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		seen[id] = at
	}
	return seen, rows.Err()
}

// hostName is this host's name, for the lock sessions' application_name.
func hostName() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "unknown-host"
	}
	return host
}

// lockSessionName names one of this process's lock connections: role ("fleet
// serve", "fleet presence", "fleet account deletion"), then this store's
// random instance id — first, so truncation to 63 bytes never drops it, and
// two processes are told apart even when both run as pid 1 on hosts of the
// same name — then the host and pid, for the person reading it.
func (s *Store) lockSessionName(role string) (string, error) {
	instance, err := s.serverInstanceID()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s %s %s pid %d", role, instance, hostName(), os.Getpid()), nil
}

// truncateName keeps name within Postgres' 63-byte application_name.
func truncateName(name string) string {
	const limit = 63
	if len(name) <= limit {
		return name
	}
	cut := limit
	for cut > 0 && (name[cut]&0xC0) == 0x80 {
		cut--
	}
	return name[:cut]
}
