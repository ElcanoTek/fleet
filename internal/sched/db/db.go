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
	"fmt"
	"log"
	"os"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// Database is the PostgreSQL database wrapper for the orchestrator.
type Database struct {
	conn *sql.DB

	// archiveKey is the optional 32-byte AES-256-GCM key for log archival (#272).
	// Held host-side and NEVER logged. nil = archives are gzip-only (no
	// encryption). Set once via SetLogArchiveKey before the archival sweep runs.
	archiveKey []byte
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

func (db *Database) Init(connStr string, pool PoolConfig) error {
	if connStr == "" || connStr[0] == '.' || connStr[0] == '/' {
		connStr = os.Getenv("DATABASE_URL")
		if connStr == "" {
			host := getEnvOrDefault("DB_HOST", "localhost")
			port := getEnvOrDefault("DB_PORT", "5432")
			user := getEnvOrDefault("DB_USER", "fleet")
			password := getEnvOrDefault("DB_PASSWORD", "")
			dbname := getEnvOrDefault("DB_NAME", "sched")
			sslmode := getEnvOrDefault("DB_SSLMODE", "disable")
			connStr = fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
				host, port, user, password, dbname, sslmode)
		}
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

	return nil
}

func getEnvOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

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
		log.Printf("Warning: failed to marshal JSON: %v (value type: %T)", err, v)
		return "[]"
	}
	return string(b)
}

func unmarshalStringSlice(s string) []string {
	if s == "" {
		return []string{}
	}
	var result []string
	if err := json.Unmarshal([]byte(s), &result); err != nil {
		log.Printf("Warning: failed to unmarshal string slice: %v (input: %.100s)", err, s)
		return []string{}
	}
	if result == nil {
		return []string{}
	}
	return result
}

func unmarshalMCPSelection(s string) models.MCPSelection {
	if s == "" {
		return models.MCPSelection{}
	}
	var result models.MCPSelection
	if err := json.Unmarshal([]byte(s), &result); err != nil {
		log.Printf("Warning: failed to unmarshal mcp_selection: %v (input: %.100s)", err, s)
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
