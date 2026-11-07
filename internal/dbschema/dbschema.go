// Package dbschema creates a store's Postgres schema before its migration
// runner runs (#2049, ADR-0098).
//
// In the one-database layout (config.LayoutSchemas) each store connects with
// its schema as the connection's only search_path, set by the DSN
// (config.WithSchema). Postgres reports current_schema() as NULL while that
// schema does not exist, so the first boot against a fresh database must
// create it before either runner can create its schema_migrations table.
// Both runners (internal/store and internal/sched/db) call Ensure on their
// migration connection first; in the legacy layout — the default
// search_path, "$user", public — Ensure does nothing.
package dbschema

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Execer is what Ensure needs: *sql.Conn and *sql.DB both satisfy it.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// Configured returns the single schema the connection's search_path names,
// or "" when it does not name exactly one plain schema other than public —
// the legacy layout's default "$user", public among them.
func Configured(ctx context.Context, db Execer) (string, error) {
	var sp string
	if err := db.QueryRowContext(ctx, `SELECT current_setting('search_path')`).Scan(&sp); err != nil {
		return "", fmt.Errorf("read search_path: %w", err)
	}
	sp = strings.TrimSpace(sp)
	// The default search_path ("$user", public) is the legacy layout, whose
	// store lives in public, which always exists.
	if sp == "public" || !plainIdentifier(sp) { // LEGACY-DB-LAYOUT: remove after consolidation
		return "", nil
	}
	return sp, nil
}

// lockKeyBase namespaces the transaction-level advisory lock Ensure takes so
// two processes creating the same schema serialise rather than race into a
// duplicate-key error on pg_namespace (CREATE SCHEMA IF NOT EXISTS is not
// safe against a concurrent create).
const lockKeyBase = "fleet.dbschema."

// Ensure creates the connection's configured schema (Configured) when it
// does not exist yet, and is a no-op otherwise or in the legacy layout.
//
// It refuses one shape: creating a store schema in a database whose public
// schema already holds a fleet store (a schema_migrations table). That is a
// legacy database someone pointed DATABASE_URL at — usually one of a
// two-database install's pair — and creating empty chat/sched schemas next
// to the real data would boot an apparently empty fleet over it. The fix is
// `fleet db consolidate` (or the previous per-store DSNs), never a schema
// silently shadowing the data.
func Ensure(ctx context.Context, db Execer) error {
	schema, err := Configured(ctx, db)
	if err != nil || schema == "" {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ensure schema %s: %w", schema, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, lockKeyBase+schema); err != nil {
		return fmt.Errorf("ensure schema %s: lock: %w", schema, err)
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, schema).Scan(&exists); err != nil {
		return fmt.Errorf("ensure schema %s: %w", schema, err)
	}
	if exists {
		return nil
	}
	var legacy bool
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass('public.schema_migrations') IS NOT NULL`).Scan(&legacy); err != nil {
		return fmt.Errorf("ensure schema %s: probe public: %w", schema, err)
	}
	if legacy {
		return ErrLegacyDatabase{Schema: schema}
	}
	// schema is a plain identifier (Configured), so quoting is belt and braces.
	if _, err := tx.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS `+quoteIdent(schema)); err != nil {
		return fmt.Errorf("create schema %s: %w", schema, err)
	}
	return tx.Commit()
}

// ErrLegacyDatabase is Ensure's refusal to create Schema in a database whose
// public schema already holds a fleet store.
type ErrLegacyDatabase struct{ Schema string }

func (e ErrLegacyDatabase) Error() string {
	return fmt.Sprintf("refusing to create schema %q: this database's public schema already holds a fleet store "+
		"(a legacy two-database install's database). Point the per-store DSNs back at the two databases, "+
		"or move them into one with `fleet db consolidate` (docs/ONE-DATABASE.md)", e.Schema)
}

// IsLegacyDatabase reports whether err is (or wraps) ErrLegacyDatabase.
func IsLegacyDatabase(err error) bool {
	var e ErrLegacyDatabase
	return errors.As(err, &e)
}

func plainIdentifier(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
