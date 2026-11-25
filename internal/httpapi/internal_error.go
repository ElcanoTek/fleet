package httpapi

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/httpmw"
	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/safe"
)

// internalErrorMessage is the body of every 500 the chat API answers for a
// failure the caller cannot act on: a store, driver, filesystem or network
// error. The same text as the orchestrator handlers' "internal error", so
// the browser's message catalog translates both.
const internalErrorMessage = "internal error"

// writeInternalError answers a request whose handler hit an internal error.
// The error is logged with the request's correlation ids, the method, the
// matched route and the handler line that hit it; the client gets 500 and
// internalErrorMessage. The response never depends on what the failing
// component said, so a database or filesystem message stays in the log.
//
// A 4xx whose message the handler wrote for the user (a validation failure,
// a typed refusal) is not an internal error: answer it with http.Error and
// that message. TestNoErrorTextInResponses keeps error text out of 5xx
// bodies, JSON bodies and stream frames.
func writeInternalError(w http.ResponseWriter, r *http.Request, err error) {
	logInternalError(requestContext(r), r, err, 1)
	http.Error(w, internalErrorMessage, http.StatusInternalServerError)
}

// writeInternalErrorFromHelper is writeInternalError for an error-mapping
// helper (httpErrorForSetting, writeScheduleError, …): the log's `at` names
// the handler that called the helper rather than the helper.
func writeInternalErrorFromHelper(w http.ResponseWriter, r *http.Request, err error) {
	logInternalError(requestContext(r), r, err, 2)
	http.Error(w, internalErrorMessage, http.StatusInternalServerError)
}

// logInternalError logs err as an internal error answered with a fixed
// message. r may be nil (a stream frame sent after the request returned).
// skip is the number of frames between logInternalError's caller and the code
// that hit the error: 0 when that code calls logInternalError itself.
func logInternalError(ctx context.Context, r *http.Request, err error, skip int) {
	attrs := []any{"at", safe.LogSafe(callerSite(skip + 2))}
	if r != nil {
		attrs = append(attrs, "method", safe.LogSafe(r.Method), "route", safe.LogSafe(httpmw.RouteLabel(r)))
	}
	msg := "<nil>"
	if err != nil {
		msg = err.Error()
	}
	attrs = append(attrs, "err", safe.LogSafe(msg))
	logging.From(ctx).Error("internal error answered with a fixed message", attrs...)
}

// callerSite names the function and line skip frames up the stack, counting
// callerSite itself as 0, as "(*Server).handleX (file.go:123)".
func callerSite(skip int) string {
	pc, file, line, ok := runtime.Caller(skip)
	if !ok {
		return "unknown"
	}
	name := "unknown"
	if fn := runtime.FuncForPC(pc); fn != nil {
		name = fn.Name()
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		name = strings.TrimPrefix(name, "httpapi.")
	}
	return fmt.Sprintf("%s (%s:%d)", name, filepath.Base(file), line)
}

// errTurnNotRecorded answers a direct submission whose idempotency claim could
// not be bound to its turn: the turn is dropped before it runs.
var errTurnNotRecorded = errors.New("the message could not be recorded against its turn; send it again")

// fixedServerFailures are the package's errors whose fixed text is written for
// the reader although they are answered with a 5xx: the server shutting down
// or reconnecting, a workspace that could not be prepared, a message that
// could not be bound to its turn, a transfer that failed (its cause already
// logged).
var fixedServerFailures = []error{errServerShuttingDown, errServerReconnecting, errWorkspaceUnavailable, errTurnNotRecorded, errTransferFailed}

// failureText is the response text for err answered with status, for the
// paths that choose the status at run time. A 4xx carries err's text: the
// handler's validation or refusal. Under a 5xx, an error of
// fixedServerFailures carries that error's own text, and anything else is an
// internal error: logged (see logInternalError), and answered with
// internalErrorMessage.
func failureText(ctx context.Context, r *http.Request, status int, err error) string {
	return failureTextSkip(ctx, r, status, err, 1)
}

// failureTextSkip is failureText for a helper: skip counts the frames above
// failureTextSkip's caller for the log's `at` (0: that caller).
func failureTextSkip(ctx context.Context, r *http.Request, status int, err error, skip int) string {
	if status < http.StatusInternalServerError && err != nil {
		return err.Error()
	}
	for _, fixed := range fixedServerFailures {
		if errors.Is(err, fixed) {
			return fixed.Error()
		}
	}
	logInternalError(ctx, r, err, skip+1)
	return internalErrorMessage
}

// The turn.error and summary.error texts. Each is fixed text (errors.New keeps
// it in the browser's message catalog, scripts/check_localizations_test.go);
// the ones mirroring an agent sentinel use the same words, so
// TestTurnErrorTextsMirrorAgentSentinels can pin them together.
var (
	errTurnFailed           = errors.New("the turn ended unexpectedly due to an internal error")
	errTurnAtCapacity       = errors.New("the workspace is at capacity right now — please resend your message in a moment")
	errTurnHistoryNotSaved  = errors.New("conversation history could not be saved")
	errTurnGuardrailBlocked = errors.New("content blocked by workspace guardrail")
	errSummarizeFailed      = errors.New("summarizing failed; try again")
	errSummaryNotSaved      = errors.New("the summary could not be saved; try again")
)

// turnErrorMessage is the turn.error text for a turn that failed with err. A
// refusal written for the sender (agent.TurnRefusalError: a model lockdown
// does not allow, one with no provider route, a Lockdown chat with no sandbox
// image) keeps its text; the
// workspace being at capacity, history that could not be saved and a
// guardrail block each have a fixed text; anything else is an internal error,
// answered with errTurnFailed. The caller logs err.
func turnErrorMessage(err error) string {
	var refusal *agent.TurnRefusalError
	switch {
	case errors.As(err, &refusal):
		return refusal.Error()
	case errors.Is(err, agent.ErrAtCapacity):
		return errTurnAtCapacity.Error()
	case errors.Is(err, agent.ErrHistoryCommitFailed):
		return errTurnHistoryNotSaved.Error()
	case errors.Is(err, agentcore.ErrGuardrailBlocked):
		return errTurnGuardrailBlocked.Error()
	default:
		return errTurnFailed.Error()
	}
}

// IsDatabaseError reports whether err comes from the database or its driver:
// a server error (a constraint, a missing relation), a failed connection, an
// established connection that broke (a reset, a timeout: pgx's own errors,
// which all implement SafeToRetry), or a connection, transaction or pool
// that is gone. A plain network error from elsewhere is not one. A helper
// whose other errors are written for the reader uses it to tell an internal
// error apart.
func IsDatabaseError(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	var connErr *pgconn.ConnectError
	var pgxErr interface{ SafeToRetry() bool }
	return errors.As(err, &pgErr) || errors.As(err, &connErr) || errors.As(err, &pgxErr) ||
		errors.Is(err, sql.ErrConnDone) || errors.Is(err, sql.ErrTxDone) || errors.Is(err, driver.ErrBadConn) ||
		strings.Contains(err.Error(), "sql: database is closed")
}
