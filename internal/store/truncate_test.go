package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsRetryableLockError(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("boom"), false},
		{"deadlock_detected", &pgconn.PgError{Code: "40P01"}, true},
		{"lock_not_available", &pgconn.PgError{Code: "55P03"}, true},
		{"serialization_failure", &pgconn.PgError{Code: "40001"}, true},
		// Retrying these would loop forever on a fault that will not clear.
		{"undefined_table", &pgconn.PgError{Code: "42P01"}, false},
		{"insufficient_privilege", &pgconn.PgError{Code: "42501"}, false},
		{"unique_violation", &pgconn.PgError{Code: "23505"}, false},
		// The classification has to survive wrapping, since the retry loop sees
		// whatever the tx helpers hand back.
		{"wrapped deadlock", fmt.Errorf("truncate: %w", &pgconn.PgError{Code: "40P01"}), true},
		{"wrapped non-lock", fmt.Errorf("truncate: %w", &pgconn.PgError{Code: "42P01"}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryableLockError(tc.err); got != tc.want {
				t.Errorf("isRetryableLockError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// A bare TRUNCATE here deadlocked in CI, and the victim was as likely to be the
// ordinary writer as the fixture. TRUNCATE locks its tables one at a time in list
// order — conversations early, users later — so a transaction that writes users
// and then conversations holds the same two locks in the opposite order and the
// pair cycles.
//
// This reconstructs that inversion deliberately: hold users, let the fixture start
// wiping, then reach for conversations. The fixture must not have parked itself on
// conversations while waiting for users, so the writer's second statement has to
// succeed and its transaction has to commit.
//
// One-sided by design. If the timing slips and the fixture finishes before the
// writer's second statement, everything simply succeeds and the test passes — it
// can only fail if a deadlock genuinely happens, which keeps it from becoming the
// next flake in this very file's history.
func TestTruncateAllForTestDoesNotDeadlockAContendingWriter(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	writer, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin writer: %v", err)
	}
	defer func() { _ = writer.Rollback() }()

	// Lock #1, taken by the writer: users.
	if _, err := writer.ExecContext(ctx,
		`INSERT INTO users (email, password_hash, created_at, updated_at) VALUES ($1,'x',0,0)`,
		"contender@example.com"); err != nil {
		t.Fatalf("writer insert users: %v", err)
	}

	truncated := make(chan error, 1)
	go func() { truncated <- s.TruncateAllForTest(ctx) }()

	// Give the fixture time to be in flight and holding whatever it holds.
	time.Sleep(150 * time.Millisecond)

	// Lock #2, the inversion: the fixture wants users, the writer now wants
	// conversations. Under the old bare TRUNCATE this is where one of them died.
	if _, err := writer.ExecContext(ctx,
		`INSERT INTO conversations (id, user_email, title, persona, model, pinned, lockdown, created_at, updated_at)
		 VALUES (gen_random_uuid(), $1, 't', 'generic', 'm', FALSE, FALSE, 0, 0)`,
		"contender@example.com"); err != nil {
		t.Fatalf("writer insert conversations: the fixture made an ordinary writer a lock victim: %v", err)
	}
	if err := writer.Commit(); err != nil {
		t.Fatalf("writer commit: the fixture made an ordinary writer a lock victim: %v", err)
	}

	select {
	case err := <-truncated:
		if err != nil {
			t.Fatalf("TruncateAllForTest: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("TruncateAllForTest did not return: the retry/escalation loop is not converging")
	}
}
