package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// IsTransientDBError decides whether the runner retries an outcome write or
// records a fallback, so both directions matter: an outage retried as
// permanent writes a misleading fallback, and a refusal retried as transient
// holds the run until shutdown.
func TestIsTransientDBError(t *testing.T) {
	t.Parallel()

	pg := func(code string) error { return &pgconn.PgError{Severity: "ERROR", Code: code, Message: "x"} }
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"restart (admin_shutdown)", pg("57P01"), true},
		{"crash_shutdown", pg("57P02"), true},
		{"starting up / in recovery", pg("57P03"), true},
		{"connection failure", pg("08006"), true},
		{"connection refused by server", pg("08001"), true},
		{"too many connections", pg("53300"), true},
		{"serialization failure", pg("40001"), true},
		{"deadlock", pg("40P01"), true},
		{"lock timeout", pg("55P03"), true},
		{"statement timeout", pg("57014"), true},
		{"I/O error", pg("58030"), true},
		{"read-only transaction (demoted primary)", pg("25006"), true},
		{"check violation", pg("23514"), false},
		{"unique violation", pg("23505"), false},
		{"raise_exception", pg("P0001"), false},
		{"invalid text", pg("22P02"), false},
		{"database dropped", pg("57P04"), false},
		{"commit unknown, connection lost", fmt.Errorf("%w: %w", ErrTaskCommitOutcomeUnknown, pg("08006")), true},
		{"commit unknown, deferred constraint", fmt.Errorf("%w: %w", ErrTaskCommitOutcomeUnknown, pg("23514")), false},
		{"commit unknown, bare", ErrTaskCommitOutcomeUnknown, false},
		{"attempt timeout", fmt.Errorf("update: %w", context.DeadlineExceeded), true},
		{"bad connection", driver.ErrBadConn, true},
		{"connection done", sql.ErrConnDone, true},
		{"tx already rolled back", sql.ErrTxDone, true},
		{"commit after the attempt timed out", fmt.Errorf("%w: %w", ErrTaskCommitOutcomeUnknown, sql.ErrTxDone), true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"connection reset", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, true},
		{"connection refused", fmt.Errorf("dial: %w", syscall.ECONNREFUSED), true},
		{"lease not held", ErrTaskLeaseNotHeld, false},
		{"contract", fmt.Errorf("%w: output_json", ErrStructuredOutputContract), false},
		{"unknown", errors.New("something else"), false},
	}
	for _, tc := range cases {
		if got := IsTransientDBError(tc.err); got != tc.want {
			t.Errorf("%s: IsTransientDBError(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}
