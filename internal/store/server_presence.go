// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ElcanoTek/fleet/internal/logging"
)

// serverPresenceLockKey is the session-level advisory lock that marks a fleet
// server as running against this database: every `fleet serve` holds it
// shared for as long as it runs (HoldServerPresence), and an account deletion
// from the CLI takes it exclusive for as long as it runs (ExcludeServers), so
// the two never run against one database at once. Advisory locks are per
// database, so the check does not depend on a unit name, a port or a host.
const serverPresenceLockKey int64 = 0x464c545f53525652 // "FLT_SRVR"

// ErrServerRunning is ExcludeServers' answer while a fleet server runs
// against the database.
var ErrServerRunning = errors.New("a fleet server is running against this database")

// serverPresenceCheck is how often the held lock's connection is checked; a
// connection found dead is replaced and the lock taken again.
var serverPresenceCheck = 15 * time.Second

// HoldServerPresence takes the server-presence lock shared, on a connection
// of its own, and keeps it until release is called: a CLI account deletion
// refuses to run meanwhile (ExcludeServers). It waits up to wait for the lock
// (a CLI deletion in progress holds it exclusive) before it returns; past
// that it goes on trying in the background, logging the delay. If the
// connection drops, the lock is taken again on a new one.
func (s *Store) HoldServerPresence(wait time.Duration) (release func()) {
	if s == nil || s.db == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	held := make(chan struct{})
	var heldOnce sync.Once
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			conn, err := s.lockPresence(ctx)
			if err != nil {
				if ctx.Err() == nil {
					logging.From(ctx).Error("server presence lock: taking it failed; retrying", "err", err)
					sleepCtx(ctx, serverPresenceCheck)
				}
				continue
			}
			heldOnce.Do(func() { close(held) })
			s.keepPresence(ctx, conn)
		}
	}()
	select {
	case <-held:
	case <-time.After(wait):
		logging.From(ctx).Warn("server presence lock: not taken yet (a CLI account deletion may be running); taking it in the background")
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}

// lockPresence takes the lock shared on a new dedicated connection, waiting
// while a CLI deletion holds it exclusive.
func (s *Store) lockPresence(ctx context.Context) (*sql.Conn, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock_shared($1)`, serverPresenceLockKey); err != nil {
		discardSQLConn(conn)
		return nil, err
	}
	return conn, nil
}

// keepPresence holds conn (and with it the lock) until ctx ends, which
// unlocks and closes it, or until the connection is found dead, which
// discards it so the caller takes the lock again.
func (s *Store) keepPresence(ctx context.Context, conn *sql.Conn) {
	t := time.NewTicker(serverPresenceCheck)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if _, err := conn.ExecContext(unlockCtx, `SELECT pg_advisory_unlock_shared($1)`, serverPresenceLockKey); err != nil {
				discardSQLConn(conn)
				return
			}
			_ = conn.Close()
			return
		case <-t.C:
			pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := conn.PingContext(pingCtx)
			cancel()
			if err != nil && ctx.Err() == nil {
				logging.From(ctx).Warn("server presence lock: its connection dropped; taking the lock again", "err", err)
				discardSQLConn(conn)
				return
			}
		}
	}
}

// ExcludeServers takes the server-presence lock exclusive without waiting, on
// a connection of its own, and returns the function that releases it:
// ErrServerRunning when a fleet server holds it. A CLI account deletion holds
// it while it runs, so no server runs alongside it — and a server starting
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
	var once sync.Once
	return func() {
		once.Do(func() {
			unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if _, err := conn.ExecContext(unlockCtx, `SELECT pg_advisory_unlock($1)`, serverPresenceLockKey); err != nil {
				discardSQLConn(conn)
				return
			}
			_ = conn.Close()
		})
	}, nil
}

// sleepCtx sleeps for d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
