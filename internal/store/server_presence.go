// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ElcanoTek/fleet/internal/logging"
)

// A fleet server marks itself as running against this database in two ways,
// and an account deletion from the CLI (ExcludeServers) refuses while either
// says one runs:
//
//   - serverPresenceLockKey, a session-level advisory lock every `fleet
//     serve` holds shared for as long as it runs (HoldServerPresence), and
//     the CLI takes exclusive for as long as it runs — a server starting
//     during a CLI deletion waits for it before it opens its listeners;
//   - a heartbeat row in core.server_presence (migration 0212), written
//     through the server's ordinary pool every serverHeartbeatEvery: it
//     covers the moments the lock's own connection has dropped and is being
//     replaced. A row older than serverHeartbeatFresh (a crashed server's)
//     blocks nobody.
//
// A server whose lock connection drops (the database restarted, say) admits
// no account work until it holds the lock again (ServerPresenceLost): a CLI
// deletion may have taken it exclusive meanwhile. It tries to take the lock
// again every serverPresenceRetry, writing its heartbeat on every try, and a
// CLI deletion that took the lock looks at the heartbeats once more after
// serverPresenceGrace, longer than a server takes to notice the drop, before
// it deletes anything (ExcludeServers): so the two never run together.
//
// Advisory locks and the row are per database, so neither depends on a unit
// name, a port or a host; the row is stamped with the database's clock.
const serverPresenceLockKey int64 = 0x464c545f53525652 // "FLT_SRVR"

// ErrServerRunning is ExcludeServers' answer while a fleet server runs
// against the database.
var ErrServerRunning = errors.New("a fleet server is running against this database")

var (
	// serverPresenceCheck is how often the held lock's connection is
	// checked; a connection found dead is replaced and the lock taken again.
	serverPresenceCheck = time.Second
	// serverPresencePing bounds one check of the lock's connection, and one
	// try to take the lock again or write the heartbeat while it is lost.
	serverPresencePing = 2 * time.Second
	// serverPresenceRetry is how often a server whose lock connection
	// dropped tries to take the lock again.
	serverPresenceRetry = 500 * time.Millisecond
	// serverPresenceGrace is how long a CLI deletion holds the lock before
	// it looks at the heartbeats a second time: longer than a running server
	// takes to notice its lock connection dropped (serverPresenceCheck plus
	// serverPresencePing) and write its heartbeat (serverPresenceRetry).
	serverPresenceGrace = 5 * time.Second
	// serverHeartbeatEvery is how often a running server writes its row.
	serverHeartbeatEvery = 15 * time.Second
	// serverHeartbeatFresh is how recent a row must be to block a CLI
	// deletion: three heartbeats.
	serverHeartbeatFresh = 45 * time.Second
	// serverPresenceWaitLog is how often a server waiting for the lock says
	// so.
	serverPresenceWaitLog = 30 * time.Second
)

// HoldServerPresence marks this process as a running fleet server: it takes
// the presence lock shared on a connection of its own — waiting, without a
// deadline, while a CLI account deletion holds it exclusive, and saying so
// every serverPresenceWaitLog — then keeps it (taking it again if its
// connection drops) and writes the heartbeat row, until release. It returns
// ctx's error, holding nothing, when ctx ends first (the server is told to
// stop while it waits). `fleet serve` calls it before it opens its
// listeners.
func (s *Store) HoldServerPresence(ctx context.Context) (release func(), err error) {
	if s == nil || s.db == nil {
		return func() {}, nil
	}
	conn, err := s.waitForPresenceLock(ctx)
	if err != nil {
		return nil, err
	}
	var idb [8]byte
	if _, err := rand.Read(idb[:]); err != nil {
		s.unlockPresence(ctx, conn)
		return nil, err
	}
	instance := hex.EncodeToString(idb[:])
	if err := s.heartbeat(ctx, instance); err != nil {
		s.unlockPresence(ctx, conn)
		return nil, fmt.Errorf("server presence heartbeat: %w", err)
	}
	life, cancel := context.WithCancel(context.WithoutCancel(ctx))
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.keepPresenceLock(life, conn, instance)
	}()
	go func() {
		defer wg.Done()
		s.keepHeartbeat(life, instance)
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			wg.Wait()
		})
	}, nil
}

// waitForPresenceLock takes the presence lock shared on a new dedicated
// connection, polling while a CLI deletion holds it exclusive, until ctx
// ends.
func (s *Store) waitForPresenceLock(ctx context.Context) (*sql.Conn, error) {
	const poll = 500 * time.Millisecond
	lastLog := time.Now()
	for {
		conn, got, err := s.tryPresenceLock(ctx)
		if err == nil && got {
			return conn, nil
		}
		if err != nil && ctx.Err() == nil {
			logging.From(ctx).Warn("server presence lock: taking it failed; retrying", "err", err)
		}
		if time.Since(lastLog) >= serverPresenceWaitLog {
			logging.From(ctx).Warn("waiting for an account deletion run from the CLI to finish before serving")
			lastLog = time.Now()
		}
		if !sleepCtx(ctx, poll) {
			return nil, ctx.Err()
		}
	}
}

// tryPresenceLock takes the lock shared without waiting, on a new dedicated
// connection; got is false (and no connection kept) while it is held
// exclusive.
func (s *Store) tryPresenceLock(ctx context.Context) (*sql.Conn, bool, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	var got bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock_shared($1)`, serverPresenceLockKey).Scan(&got); err != nil {
		discardSQLConn(conn)
		return nil, false, err
	}
	if !got {
		_ = conn.Close()
		return nil, false, nil
	}
	return conn, true, nil
}

// unlockPresence releases the shared lock and its connection.
func (s *Store) unlockPresence(ctx context.Context, conn *sql.Conn) {
	unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := conn.ExecContext(unlockCtx, `SELECT pg_advisory_unlock_shared($1)`, serverPresenceLockKey); err != nil {
		discardSQLConn(conn)
		return
	}
	_ = conn.Close()
}

// ServerPresenceLost reports whether this server's presence lock connection
// dropped and the lock has not been taken again yet (HoldServerPresence).
// Meanwhile a CLI deletion may hold the lock exclusive, so the server admits
// no account work. False on a store that holds no presence.
func (s *Store) ServerPresenceLost() bool {
	if s == nil {
		return false
	}
	return s.presenceLost.Load()
}

// keepPresenceLock holds conn (and with it the lock) until ctx ends, then
// unlocks it. Its connection is checked every serverPresenceCheck; one found
// dead is discarded, ServerPresenceLost reports true, and the lock is taken
// again on a new connection (retakePresenceLock) before it reports false.
func (s *Store) keepPresenceLock(ctx context.Context, conn *sql.Conn, instance string) {
	defer s.presenceLost.Store(false)
	t := time.NewTicker(serverPresenceCheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.unlockPresence(ctx, conn)
			return
		case <-t.C:
		}
		pingCtx, cancel := context.WithTimeout(ctx, serverPresencePing)
		err := conn.PingContext(pingCtx)
		cancel()
		if err == nil || ctx.Err() != nil {
			continue
		}
		s.presenceLost.Store(true)
		logging.From(ctx).Warn("server presence lock: its connection dropped; account work is refused until the lock is taken again", "err", err)
		discardSQLConn(conn)
		if conn = s.retakePresenceLock(ctx, instance); conn == nil {
			return
		}
		s.presenceLost.Store(false)
		logging.From(ctx).Info("server presence lock: taken again; account work is admitted again")
	}
}

// retakePresenceLock takes the lock again after its connection dropped,
// trying every serverPresenceRetry until it has it, or ctx ends (nil), and
// writes the heartbeat row once it has it. A try that does not get it writes
// the row too: a CLI deletion that took the lock exclusive in the meantime
// then finds a running server when it looks again (ExcludeServers) and lets
// go, and the next try gets it.
func (s *Store) retakePresenceLock(ctx context.Context, instance string) *sql.Conn {
	lastLog := time.Now()
	for {
		tryCtx, cancel := context.WithTimeout(ctx, serverPresencePing)
		conn, got, err := s.tryPresenceLock(tryCtx)
		hbErr := s.heartbeat(tryCtx, instance)
		cancel()
		if got {
			if hbErr != nil && ctx.Err() == nil {
				logging.From(ctx).Warn("server presence heartbeat: writing it failed; retrying", "err", hbErr)
			}
			return conn
		}
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(lastLog) >= serverPresenceWaitLog {
			logging.From(ctx).Warn("server presence lock: still taking it again; account work stays refused", "err", errors.Join(err, hbErr))
			lastLog = time.Now()
		}
		if !sleepCtx(ctx, serverPresenceRetry) {
			return nil
		}
	}
}

// keepHeartbeat writes the heartbeat row every serverHeartbeatEvery until ctx
// ends, then removes it.
func (s *Store) keepHeartbeat(ctx context.Context, instance string) {
	t := time.NewTicker(serverHeartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if _, err := s.db.ExecContext(delCtx, `DELETE FROM core.server_presence WHERE instance_id = $1`, instance); err != nil {
				logging.From(ctx).Warn("server presence heartbeat: removing it failed; it stops counting in 45s", "err", err)
			}
			return
		case <-t.C:
			if err := s.heartbeat(ctx, instance); err != nil && ctx.Err() == nil {
				logging.From(ctx).Warn("server presence heartbeat: writing it failed; retrying", "err", err)
			}
		}
	}
}

// heartbeat writes this server's row with the database's clock, and drops
// rows no server has written for an hour (a crashed server's).
func (s *Store) heartbeat(ctx context.Context, instance string) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO core.server_presence (instance_id, last_seen) VALUES ($1, now())
		 ON CONFLICT (instance_id) DO UPDATE SET last_seen = now()`, instance); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM core.server_presence WHERE last_seen < now() - interval '1 hour'`)
	return err
}

// ServerHeartbeatSeen reports whether a fleet server wrote its heartbeat row
// within serverHeartbeatFresh. It takes no lock and decides nothing —
// ExcludeServers is the check a deletion relies on — but lets a CLI deletion
// say a server is running before it asks the operator to confirm a deletion
// ExcludeServers would then refuse.
func (s *Store) ServerHeartbeatSeen(ctx context.Context) (bool, error) {
	if s == nil || s.db == nil {
		return false, nil
	}
	var fresh bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM core.server_presence WHERE last_seen > now() - make_interval(secs => $1))`,
		serverHeartbeatFresh.Seconds()).Scan(&fresh)
	return fresh, err
}

// ExcludeServers takes the server-presence lock exclusive without waiting, on
// a connection of its own, and returns the function that releases it. It
// answers ErrServerRunning, holding nothing, when a fleet server holds the
// lock or wrote its heartbeat within serverHeartbeatFresh — looked at when it
// takes the lock and again serverPresenceGrace later, so a server whose lock
// connection dropped (a database outage longer than serverHeartbeatFresh
// leaves its row stale) and that has not taken the lock back yet is seen by
// the heartbeat it writes while it tries. A CLI account deletion holds it
// while it runs, so no server runs alongside it — and a server starting
// meanwhile waits for it (HoldServerPresence).
func (s *Store) ExcludeServers(ctx context.Context) (release func(), err error) {
	if s == nil || s.db == nil {
		return func() {}, nil
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("server presence check: %w", err)
	}
	var got bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, serverPresenceLockKey).Scan(&got); err != nil {
		discardSQLConn(conn)
		return nil, fmt.Errorf("server presence check: %w", err)
	}
	if !got {
		_ = conn.Close()
		return nil, ErrServerRunning
	}
	unlock := func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(unlockCtx, `SELECT pg_advisory_unlock($1)`, serverPresenceLockKey); err != nil {
			discardSQLConn(conn)
			return
		}
		_ = conn.Close()
	}
	for look := 0; look < 2; look++ {
		if look == 1 && !sleepCtx(ctx, serverPresenceGrace) {
			unlock()
			return nil, ctx.Err()
		}
		var fresh bool
		if err := conn.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM core.server_presence WHERE last_seen > now() - make_interval(secs => $1))`,
			serverHeartbeatFresh.Seconds()).Scan(&fresh); err != nil {
			unlock()
			return nil, fmt.Errorf("server presence check: %w", err)
		}
		if fresh {
			unlock()
			return nil, ErrServerRunning
		}
	}
	var once sync.Once
	return func() { once.Do(unlock) }, nil
}

// sleepCtx sleeps for d, or until ctx ends; it reports whether it slept the
// whole time.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
