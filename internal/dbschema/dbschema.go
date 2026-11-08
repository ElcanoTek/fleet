// Package dbschema creates fleet's Postgres schemas before the migration
// runner runs (#2049, ADR-0098, ADR-0107).
//
// Every fleet store lives in one database, in three schemas: chat and sched
// (the two stores, each connecting with its schema as the connection's only
// search_path, set by the DSN — config.WithSchema) and core (what both read:
// the identity table and the one migration ledger, core.schema_versions).
// The one migration runner (internal/migrations) calls Ensure on its
// connection first, whatever that connection's search_path: it creates the
// schemas that do not exist yet, after the one readiness check.
package dbschema

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
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
// the default "$user", public among them.
func Configured(ctx context.Context, db Execer) (string, error) {
	var sp string
	if err := db.QueryRowContext(ctx, `SELECT current_setting('search_path')`).Scan(&sp); err != nil {
		return "", fmt.Errorf("read search_path: %w", err)
	}
	sp = strings.TrimSpace(sp)
	// public (or the default "$user", public) always exists, so there is
	// nothing to create; fleet's own DSNs always name a store schema.
	if sp == "public" || !plainIdentifier(sp) {
		return "", nil
	}
	return sp, nil
}

// PublicStoreSQL answers whether the database's public schema holds a fleet
// store (a schema_migrations table) — a database of the two-database layout
// removed in #2049. It reads the catalogs, which every role may: to_regclass on
// 'public.…' fails outright for a role without USAGE on public (a hardened
// database), which would stop every store from opening.
const PublicStoreSQL = `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_class c
	JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
	WHERE n.nspname = 'public' AND c.relname = 'schema_migrations' AND c.relkind IN ('r', 'p'))`

// Schemas are fleet's schemas in the one database, in byte order: the chat
// store, the identity schema both stores read (and the one migration ledger,
// core.schema_versions), and the scheduler. internal/config names the same
// three (ChatSchema, IdentitySchema, SchedSchema); a test pins them equal, and
// scripts/lib/pgprovision.sh's fleet_db_store_readiness lists them too.
var Schemas = []string{"chat", "core", "sched"}

// lockKeyBase namespaces the transaction-level advisory lock Ensure takes so
// two processes creating the same schema serialise rather than race into a
// duplicate-key error on pg_namespace (CREATE SCHEMA IF NOT EXISTS is not
// safe against a concurrent create).
const lockKeyBase = "fleet.dbschema."

// Ensure creates each of fleet's schemas (Schemas) that does not exist yet,
// whatever the connection's search_path, and is a no-op when they all exist.
//
// It refuses first, on every call, what CheckReadiness refuses: above all a
// database whose public schema holds a fleet store (a schema_migrations
// table). That is a database of the removed two-database layout someone
// pointed DATABASE_URL at, and running the stores in empty (or stray) schemas
// next to that data would boot an apparently empty fleet over it. The probe
// runs even when the schemas already exist: an earlier boot, or a hand-made
// schema, must not turn the refusal into a one-time check.
func Ensure(ctx context.Context, db Execer) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ensure schemas: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// The one readiness check — the same that `fleet validate-config` runs and
	// bootstrap's shell gate mirrors (TestExternalSchemaPrivilegeCheck pins
	// the two).
	r, err := CheckReadiness(ctx, tx, Schemas)
	if err != nil {
		return fmt.Errorf("ensure schemas: %w", err)
	}
	if r.LegacyPublicStore {
		return LegacyDatabaseError{Schema: strings.Join(Schemas, "/")}
	}
	if !r.OK() {
		return fmt.Errorf("ensure schemas: %s", r.Problem())
	}
	created := false
	for _, schema := range Schemas {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, lockKeyBase+schema); err != nil {
			return fmt.Errorf("ensure schema %s: lock: %w", schema, err)
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)`, schema).Scan(&exists); err != nil {
			return fmt.Errorf("ensure schema %s: %w", schema, err)
		}
		if exists {
			continue
		}
		// schema is a constant plain identifier, so quoting is belt and braces.
		if _, err := tx.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS `+quoteIdent(schema)); err != nil {
			return fmt.Errorf("create schema %s: %w", schema, err)
		}
		created = true
	}
	if !created {
		return nil
	}
	return tx.Commit()
}

// QueryRower runs one query returning rows: *sql.DB, *sql.Conn and *sql.Tx.
type QueryRower interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Readiness is whether fleet's stores can run in a database as the
// connection's role — the ONE definition, read-only:
//
//   - LegacyPublicStore: the public schema holds a fleet store (a database
//     of the removed two-database layout) — refused, whatever else holds;
//   - MissingCreate: store schemas that do not exist yet while the role has
//     no CREATE on the database (the store could not create them);
//   - NotOwned: for store schemas that DO exist, the schema itself or a
//     table, sequence or view in it that the role does not own ("x is owned
//     by y") — the migrations ALTER them, which needs ownership.
//
// scripts/lib/pgprovision.sh's fleet_db_store_readiness applies the same
// rules for bootstrap, and `fleet validate-config` calls CheckReadiness itself;
// TestExternalSchemaPrivilegeCheck (internal/admincli) pins the shell and Go
// verdicts together at every step. Both order the findings byte-wise (COLLATE
// "C" in SQL, sort.Strings here) so the five shown are the same five.
type Readiness struct {
	LegacyPublicStore bool
	MissingCreate     []string
	NotOwned          []string
}

// OK reports whether the stores can run.
func (r Readiness) OK() bool {
	return !r.LegacyPublicStore && len(r.MissingCreate) == 0 && len(r.NotOwned) == 0
}

// Problem says what is wrong, for an operator ("" when OK).
func (r Readiness) Problem() string {
	switch {
	case r.LegacyPublicStore:
		return LegacyDatabaseError{Schema: strings.Join(Schemas, "/")}.Error()
	case len(r.NotOwned) > 0:
		shown := r.NotOwned
		if len(shown) > 5 {
			shown = shown[:5]
		}
		return "the role must own the existing store schemas and everything in them, but found: " + strings.Join(shown, "; ")
	case len(r.MissingCreate) > 0:
		return "the role cannot create schema(s) " + strings.Join(r.MissingCreate, ", ") + ": it needs CREATE on the database"
	}
	return ""
}

// CheckReadiness evaluates Readiness for the given store schemas over db.
func CheckReadiness(ctx context.Context, db QueryRower, schemas []string) (Readiness, error) {
	var r Readiness
	if err := db.QueryRowContext(ctx, PublicStoreSQL).Scan(&r.LegacyPublicStore); err != nil {
		return r, fmt.Errorf("probe public: %w", err)
	}
	for _, s := range schemas {
		var exists, canCreate bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname = $1),
			has_database_privilege(current_database(), 'CREATE')`, s).Scan(&exists, &canCreate); err != nil {
			return r, fmt.Errorf("probe schema %s: %w", s, err)
		}
		if !exists {
			if !canCreate {
				r.MissingCreate = append(r.MissingCreate, s)
			}
			continue
		}
		rows, err := db.QueryContext(ctx, `SELECT x FROM (
			SELECT format('schema %s is owned by %s', n.nspname, pg_get_userbyid(n.nspowner)) AS x
			  FROM pg_catalog.pg_namespace n WHERE n.nspname = $1 AND pg_get_userbyid(n.nspowner) <> current_user
			UNION ALL
			SELECT format('%s.%s is owned by %s', n.nspname, c.relname, pg_get_userbyid(c.relowner))
			  FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = $1 AND c.relkind IN ('r', 'p', 'S', 'v', 'm') AND pg_get_userbyid(c.relowner) <> current_user
		) f ORDER BY x COLLATE "C"`, s)
		if err != nil {
			return r, fmt.Errorf("probe ownership in %s: %w", s, err)
		}
		for rows.Next() {
			var x string
			if err := rows.Scan(&x); err != nil {
				_ = rows.Close()
				return r, err
			}
			r.NotOwned = append(r.NotOwned, x)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return r, err
		}
	}
	sort.Strings(r.NotOwned)
	return r, nil
}

// LegacyDatabaseError is Ensure's refusal to create Schema in a database whose
// public schema already holds a fleet store.
type LegacyDatabaseError struct{ Schema string }

func (e LegacyDatabaseError) Error() string {
	return fmt.Sprintf("refusing to run a store in schema %q: this database's public schema holds a fleet store "+
		"(a database of the removed two-database layout). Point DATABASE_URL at the one database whose "+
		"schemas chat, core and sched hold the stores, or at a new database (docs/ONE-DATABASE.md)", e.Schema)
}

// IsLegacyDatabase reports whether err is (or wraps) LegacyDatabaseError.
func IsLegacyDatabase(err error) bool {
	var e LegacyDatabaseError
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
