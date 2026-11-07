package store

import (
	"context"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/ElcanoTek/fleet/internal/testdb"
)

// newTestStore opens a Store against the test DSN (testdb.ChatDSN) and wipes
// every data row before returning. Tests skip when the env var isn't
// set so `go test ./...` on a laptop without a running Postgres still
// passes.
//
// Isolation strategy: TRUNCATE every app table (CASCADE picks up the
// FK-linked messages/turn_metrics/approvals rows) before each test.
// This avoids the per-test CREATE DATABASE overhead that would dominate
// a small suite.
//
// Serial test execution is NOT the same as an idle database, which this
// comment used to assume. `go test` starts the next test as soon as the
// previous one returns, and a goroutine that outlived its test — a turn
// driver still persisting events, say — keeps writing straight through the
// next fixture's wipe. That is what deadlocked a bare TRUNCATE in CI and
// failed PRs with nothing near the database in their diff, so
// TruncateAllForTest now takes its locks defensively; see its comment.
func newTestStore(t testing.TB) *Store {
	t.Helper()
	dsn := testdb.ChatDSN(t)
	s, err := Open(dsn, DefaultPoolConfig())
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
