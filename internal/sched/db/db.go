// Package db provides the PostgreSQL database layer for the fleet orchestrator
// (sched). Ported from moc's internal/db and converged from lib/pq onto
// jackc/pgx/v5 (registered through the stdlib database/sql adapter). The one
// schema change vs moc: per-task target_node_* routing is replaced by an
// mcp_selection JSONB column (plan §6.2).
package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/ElcanoTek/fleet/internal/dbschema"
	"github.com/ElcanoTek/fleet/internal/identity"
	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/secretbox"
)

// Database is the PostgreSQL database wrapper for the orchestrator.
type Database struct {
	conn *sql.DB

	// archiveKey is the optional 32-byte AES-256-GCM key for log archival (#272).
	// Held host-side and NEVER logged. nil = archives are gzip-only (no
	// encryption). Set once via SetLogArchiveKey before the archival sweep runs.
	archiveKey []byte

	// pushCipher seals A2A push-config secrets at rest (#1279 Phase 2). Set
	// once via SetA2APushCipher before serving; nil fails those stores closed
	// (see a2a_push.go). Held in memory only, never logged.
	pushCipher *secretbox.Cipher

	// merge is the identity merge's outcome at Init (IdentityMerge).
	merge identity.MergeResult
}

// SetLogArchiveKey configures the host-side AES-256-GCM key used to encrypt
// archived log payloads (#272). A nil/empty key disables encryption (archives
// are gzip-only). The key is held in memory only and never logged or persisted.
// It must be exactly 32 bytes; a wrong length surfaces only when the archival
// sweep or a read of an encrypted archive runs.
func (db *Database) SetLogArchiveKey(key []byte) { db.archiveKey = key }

// New creates a new Database instance.
func New() *Database {
	return &Database{}
}

// Init initializes the database connection and schema. Accepts a connection
// string or reads from DATABASE_URL. A legacy file-path argument (leading '.'
// or '/', or empty) is ignored in favor of DATABASE_URL / DB_* env vars.
// PoolConfig tunes the sched DB connection pool (#276). Local to this package
// (the cmd layer maps env-derived config into it) to keep it decoupled from
// internal/config. DefaultPoolConfig reproduces the historical hard-coded values.
type PoolConfig struct {
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxIdleTime time.Duration
	ConnMaxLifetime time.Duration
	ConnectTimeout  time.Duration
}

// DefaultPoolConfig is the behavior-preserving baseline: 25 open / 5 idle, 5m
// lifetime, 10s connect ping (sched historically pinged at 10s).
func DefaultPoolConfig() PoolConfig {
	return PoolConfig{
		MaxOpenConns:    25,
		MaxIdleConns:    5,
		ConnMaxLifetime: 5 * time.Minute,
		ConnectTimeout:  10 * time.Second,
	}
}

// Stats returns the connection-pool snapshot for metrics (#276).
func (db *Database) Stats() sql.DBStats { return db.conn.Stats() }

// Ping verifies the sched DB is reachable (readiness probe, #215).
func (db *Database) Ping(ctx context.Context) error { return db.conn.PingContext(ctx) }

// ErrEmptyDSN is Init's refusal of an empty DSN. Init no longer reads the
// environment itself (#2018): every caller resolves the DSN through
// config.ResolveDSNs (or a test helper) and passes it in, so an empty one is a
// caller bug, not a cue to guess.
var ErrEmptyDSN = errors.New("sched DB DSN is empty")

// Init opens the sched database at connStr, pings it within
// pool.ConnectTimeout and runs the migrations.
func (db *Database) Init(connStr string, pool PoolConfig) error {
	if strings.TrimSpace(connStr) == "" {
		return ErrEmptyDSN
	}

	conn, err := sql.Open("pgx", connStr)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	db.conn = conn

	db.conn.SetMaxOpenConns(pool.MaxOpenConns)
	db.conn.SetMaxIdleConns(pool.MaxIdleConns)
	db.conn.SetConnMaxIdleTime(pool.ConnMaxIdleTime)
	db.conn.SetConnMaxLifetime(pool.ConnMaxLifetime)

	connectTimeout := pool.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = 10 * time.Second
	}
	pingCtx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	if err := db.conn.PingContext(pingCtx); err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}

	if err := RunMigrations(db.conn); err != nil {
		return fmt.Errorf("failed to run migrations: %w", err)
	}
	if err := db.mergeIdentities(context.Background()); err != nil {
		return err
	}

	return nil
}

// mergeIdentities folds the scheduler's legacy users table into the one
// identity table once both stores are migrated (identity.MergeLegacySchedUsers,
// #2049 M4). Every open tries; after the first success it is one read. When the
// chat store has not created core.users yet (a scheduler-only verb on a fresh
// database) it does nothing and a later open merges. A failed merge fails the
// open: the Operations Center must not run on half an identity table.
func (db *Database) mergeIdentities(ctx context.Context) error {
	schema, err := dbschema.Configured(ctx, db.conn)
	if err != nil {
		return err
	}
	if schema == "" {
		// A DSN naming no store schema (the removed two-database layout is
		// refused before any store opens): nothing to merge into here.
		return nil
	}
	res, err := identity.MergeLegacySchedUsers(ctx, db.conn, schema)
	if err != nil {
		return fmt.Errorf("merge Operations Center accounts into the identity table: %w", err)
	}
	db.merge = res
	if res.Ran {
		for _, line := range res.Report.Lines() {
			logging.From(ctx).Info("identity merge: " + line)
		}
	}
	return nil
}

// IdentityMerge is what this open's identity merge found or did.
func (db *Database) IdentityMerge() identity.MergeResult { return db.merge }

// Close closes the database connection.
func (db *Database) Close() error {
	if db.conn != nil {
		return db.conn.Close()
	}
	return nil
}

// Conn returns the underlying database connection for transaction support.
func (db *Database) Conn() *sql.DB {
	return db.conn
}

func marshalJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		slog.Warn("Warning: failed to marshal JSON", "err", err, "value_type", fmt.Sprintf("%T", v))
		return "[]"
	}
	return string(b)
}

func unmarshalStringSlice(ctx context.Context, s string) []string {
	if s == "" {
		return []string{}
	}
	var result []string
	if err := json.Unmarshal([]byte(s), &result); err != nil {
		logging.From(ctx).Warn("Warning: failed to unmarshal string slice", "err", err, "input", fmt.Sprintf("%.100s", s))
		return []string{}
	}
	if result == nil {
		return []string{}
	}
	return result
}

func unmarshalMCPSelection(ctx context.Context, s string) models.MCPSelection {
	if s == "" {
		return models.MCPSelection{}
	}
	var result models.MCPSelection
	if err := json.Unmarshal([]byte(s), &result); err != nil {
		logging.From(ctx).Warn("Warning: failed to unmarshal mcp_selection", "err", err, "input", fmt.Sprintf("%.100s", s))
		return models.MCPSelection{}
	}
	if result == nil {
		return models.MCPSelection{}
	}
	return result
}

// uuidStrings converts a slice of UUIDs to their string forms for array params.
func uuidStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

// Transaction support for atomic operations

// BeginTx starts a new transaction.
func (db *Database) BeginTx(ctx context.Context) (*sql.Tx, error) {
	return db.conn.BeginTx(ctx, nil)
}
