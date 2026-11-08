// legacySchedLockKey below adapts golang-migrate v4.20.1's
// database.GenerateAdvisoryLockId (the formula the in-housed sched runner
// used, so a pre-M6 binary's sched runner and this one exclude each other).
// Copyright (c) 2016 Matthias Kadenbach and contributors; see
// LICENSE.golang-migrate. SPDX-License-Identifier: MIT

package migrations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/crc32"
	"log/slog"
	"strings"
	"time"
)

// LockKey is the session-level advisory lock the runner holds on its
// connection for a whole run (Up, Down, Force). Every fleet process on a
// database serializes on it.
const LockKey int64 = 0x01A7B00B_CAFE07

// The two legacy runners' locks. The runner takes both (after LockKey)
// whenever it has work to do, so a pre-M6 binary that boots at the same time
// waits for adoption and the backlog instead of racing them.
const (
	// legacyChatLockKey is the old chat runner's key (internal/store).
	legacyChatLockKey int64 = 0x01A7B00B_CAFE01
	// golangMigrateSalt is golang-migrate's database.advisoryLockIDSalt.
	golangMigrateSalt uint32 = 1486364155
)

// legacySchedLockKey is the old sched runner's key for the database: the
// golang-migrate lock id of schema sched, table schema_migrations.
func legacySchedLockKey(databaseName string) int64 {
	return golangMigrateLockID(databaseName, StreamSched, "schema_migrations")
}

// golangMigrateLockID is golang-migrate's GenerateAdvisoryLockId: the names
// joined with NUL in the order schema, table, database, CRC-32 (IEEE), times
// the salt in uint32 arithmetic.
func golangMigrateLockID(databaseName, schemaName, tableName string) int64 {
	joined := strings.Join([]string{schemaName, tableName, databaseName}, "\x00")
	return int64(crc32.ChecksumIEEE([]byte(joined)) * golangMigrateSalt)
}

// lockPollInterval is how often acquire retries a held lock, and
// lockLogInterval how often it says it is still waiting.
const (
	lockPollInterval = 100 * time.Millisecond
	lockLogInterval  = 10 * time.Second
)

// acquire takes the session-level advisory lock key on conn within timeout.
// It polls pg_try_advisory_lock rather than blocking in pg_advisory_lock: a
// blocked call cannot report progress, and cancelling one under a deadline
// races the grant (a lock granted to a connection that then goes back to the
// pool would wedge every later boot).
func acquire(ctx context.Context, conn *sql.Conn, key int64, what string, timeout time.Duration) error {
	start := time.Now()
	deadline := start.Add(timeout)
	nextLog := start.Add(lockLogInterval)
	for {
		var got bool
		if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&got); err != nil {
			return fmt.Errorf("pg_try_advisory_lock (%s): %w", what, err)
		}
		if got {
			return nil
		}
		now := time.Now()
		if !now.Before(deadline) {
			return fmt.Errorf("timed out after %s waiting for the %s (advisory lock %d): another process holds it", timeout, what, key)
		}
		if !now.Before(nextLog) {
			slog.Warn("waiting for a migration lock", "lock", what, "key", key,
				"waited", now.Sub(start).Round(time.Second), "timeout", timeout)
			nextLog = now.Add(lockLogInterval)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for the %s: %w", what, ctx.Err())
		case <-time.After(lockPollInterval):
		}
	}
}

// release drops a session lock taken by acquire. A cancelled ctx must not
// skip it and hand a still-locked connection back to the pool.
func release(ctx context.Context, conn *sql.Conn, key int64, what string) error {
	uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	var released bool
	if err := conn.QueryRowContext(uctx, `SELECT pg_advisory_unlock($1)`, key).Scan(&released); err != nil {
		return fmt.Errorf("pg_advisory_unlock (%s): %w", what, err)
	}
	if !released {
		return fmt.Errorf("pg_advisory_unlock: the %s was not held", what)
	}
	return nil
}

// locked runs fn on a dedicated connection holding LockKey and, when legacy
// is set, both legacy keys too — always in that order, so two runners and an
// old binary (which holds one legacy key at a time) cannot deadlock.
func (r *runner) locked(ctx context.Context, db *sql.DB, legacy bool, fn func(*sql.Conn) error) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migration connection: %w", err)
	}
	defer func() { _ = conn.Close() }()

	type held struct {
		key  int64
		what string
	}
	keys := []held{{LockKey, "migration lock"}}
	if legacy {
		var dbName string
		if err := conn.QueryRowContext(ctx, `SELECT current_database()`).Scan(&dbName); err != nil {
			return fmt.Errorf("resolve database name: %w", err)
		}
		keys = append(keys,
			held{legacyChatLockKey, "legacy chat migration lock"},
			held{legacySchedLockKey(dbName), "legacy sched migration lock"})
	}
	var taken []held
	defer func() {
		for i := len(taken) - 1; i >= 0; i-- {
			if uerr := release(ctx, conn, taken[i].key, taken[i].what); uerr != nil {
				err = errors.Join(err, uerr)
			}
		}
	}()
	for _, k := range keys {
		if err := acquire(ctx, conn, k.key, k.what, r.lockTimeout); err != nil {
			return err
		}
		taken = append(taken, k)
	}
	return fn(conn)
}
