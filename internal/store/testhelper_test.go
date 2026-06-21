package store

import (
	"context"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// testDSN returns the Postgres DSN for store tests. It reads the canonical
// FLEET_TEST_DATABASE_URL first, falling back to the legacy
// CHAT_TEST_DATABASE_URL so existing .env files keep working during the
// fleet monorepo migration. Empty means no test database is configured.
func testDSN() string {
	if v := os.Getenv("FLEET_TEST_DATABASE_URL"); v != "" {
		return v
	}
	return os.Getenv("CHAT_TEST_DATABASE_URL")
}

// newTestStore opens a Store against the test DSN (see testDSN) and wipes
// every data row before returning. Tests skip when the env var isn't
// set so `go test ./...` on a laptop without a running Postgres still
// passes.
//
// Isolation strategy: TRUNCATE every app table (CASCADE picks up the
// FK-linked messages/turn_metrics/approvals rows) before each test.
// Tests within a package run serially by default, so there's no risk
// of cross-test races — and this avoids the per-test CREATE DATABASE
// overhead that would dominate a small suite.
func newTestStore(t testing.TB) *Store {
	t.Helper()
	dsn := testDSN()
	if dsn == "" {
		t.Skip("FLEET_TEST_DATABASE_URL / CHAT_TEST_DATABASE_URL is not set — skipping Postgres-backed test")
	}
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.TruncateAllForTest(context.Background()); err != nil {
		_ = s.Close()
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
