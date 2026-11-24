package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5/pgconn"
)

// IsTransientDBError reports whether a failed write is worth trying again
// unchanged: the database or the path to it was briefly unavailable (a
// restart, a failover — including a primary demoted to read-only during a
// switchover —, a dropped or refused connection, a timeout, an exhausted
// connection limit) or the transaction lost a concurrency race
// (serialization failure, deadlock, lock timeout). The same write can land a
// moment later. sql.ErrTxDone counts too: it is what Commit returns once the
// attempt's context expired and database/sql has already rolled back.
//
// Everything else is false: a constraint or data error, a refused lease
// (ErrTaskLeaseNotHeld), a contract violation, an unknown error. Those fail
// the same way however often they are tried, so the runner records a
// fallback outcome instead of retrying them (runner.recordOutcome). An error
// wrapping ErrTaskCommitOutcomeUnknown is classified by its cause: a broken
// connection at COMMIT is transient, a deferred constraint that raised at
// COMMIT is not.
func IsTransientDBError(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return transientSQLState(pgErr.Code)
	}
	if errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, driver.ErrBadConn) ||
		errors.Is(err, sql.ErrConnDone) ||
		errors.Is(err, sql.ErrTxDone) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EPIPE) {
		return true
	}
	if pgconn.Timeout(err) || pgconn.SafeToRetry(err) {
		return true
	}
	var connectErr *pgconn.ConnectError
	if errors.As(err, &connectErr) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// transientSQLState is the server-reported half of IsTransientDBError.
func transientSQLState(code string) bool {
	switch {
	case strings.HasPrefix(code, "08"): // connection_exception
		return true
	case strings.HasPrefix(code, "53"): // insufficient_resources (too_many_connections, out of memory or disk)
		return true
	}
	switch code {
	case "57P01", // admin_shutdown (a restart)
		"57P02", // crash_shutdown
		"57P03", // cannot_connect_now (starting up, or in recovery)
		"57P05", // idle_session_timeout
		"25006", // read_only_sql_transaction (a primary demoted mid-switchover)
		"40001", // serialization_failure
		"40P01", // deadlock_detected
		"55P03", // lock_not_available
		"57014", // query_canceled (a statement timeout)
		"58000", // system_error
		"58030": // io_error
		return true
	}
	return false
}
