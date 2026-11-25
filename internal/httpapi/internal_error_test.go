package httpapi

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/settings"
	"github.com/ElcanoTek/fleet/internal/store"
)

// driverText is what a database driver says about a failed write: the
// constraint, the table and the server it ran on.
const driverText = `ERROR: duplicate key value violates unique constraint "chat_conversations_pkey" (SQLSTATE 23505) host=db.internal user=fleet_chat`

// captureLogs swaps the default logger for a JSON one writing to the returned
// buffer. Not for parallel tests.
func captureLogs(t *testing.T) *lockedLogBuf {
	t.Helper()
	out := &lockedLogBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(logging.NewHandler("json", out, slog.LevelDebug)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return out
}

// internalErrorRecord is the one "internal error" log record whose err
// carries marker.
func internalErrorRecord(t *testing.T, out *lockedLogBuf, marker string) map[string]any {
	t.Helper()
	var found map[string]any
	for _, m := range out.records(t) {
		if m["msg"] == "internal error answered with a fixed message" && strings.Contains(fmt.Sprint(m["err"]), marker) {
			if found != nil {
				t.Fatalf("two records for %q: %v and %v", marker, found, m)
			}
			found = m
		}
	}
	if found == nil {
		t.Fatalf("no internal-error record carries %q: %v", marker, out.records(t))
	}
	return found
}

// writeInternalError answers 500 with the fixed message, and the log gets the
// real error with the request's correlation, the route and the handler line.
func TestWriteInternalErrorLogsTheErrorAndAnswersAFixedMessage(t *testing.T) {
	out := captureLogs(t)
	req := httptest.NewRequest(http.MethodPost, "/conversations/c1/title", nil)
	req = req.WithContext(logging.WithRequestID(req.Context(), "req-ie"))
	rec := httptest.NewRecorder()
	writeInternalError(rec, req, fmt.Errorf("update title: %w", errors.New(driverText)))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != internalErrorMessage {
		t.Fatalf("body = %q, want %q", got, internalErrorMessage)
	}
	m := internalErrorRecord(t, out, "chat_conversations_pkey")
	if m["request_id"] != "req-ie" || m["method"] != http.MethodPost || m["err"] != "update title: "+driverText {
		t.Fatalf("record lacks the request or the error: %v", m)
	}
	if at := fmt.Sprint(m["at"]); !strings.Contains(at, "TestWriteInternalErrorLogsTheErrorAndAnswersAFixedMessage") || !strings.Contains(at, "internal_error_test.go:") {
		t.Fatalf("at = %q, want the caller of writeInternalError", at)
	}
}

// A store failure on a real route: the chat submission's idempotency lookup
// fails, the client gets 500 and the fixed message, and the log names the
// handler and carries the store's error.
func TestStoreFailureOnAChatRouteIsLoggedAndAnsweredFixed(t *testing.T) {
	out := captureLogs(t)
	st := &failClosedStore{fakeChatStore: newFakeChatStore(), lookupErr: errors.New(driverText)}
	srv := newDefaultChatServer(t, &fakeEngine{}, st)
	conv, err := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "m", false)
	if err != nil {
		t.Fatal(err)
	}
	w := postChatRequest(t, srv, map[string]any{"message": "hello", "conversation_id": conv.ID, "input_id": "idem-ie"})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", w.Code, w.Body.String())
	}
	if body := w.Body.String(); strings.TrimSpace(body) != internalErrorMessage || strings.Contains(body, "SQLSTATE") {
		t.Fatalf("body = %q, want only %q", body, internalErrorMessage)
	}
	m := internalErrorRecord(t, out, "SQLSTATE 23505")
	if at := fmt.Sprint(m["at"]); !strings.Contains(at, "replayAcceptedInput") {
		t.Fatalf("at = %q, want the handler that hit the error", at)
	}
}

// failingTurnEngine fails every turn with err.
type failingTurnEngine struct {
	*fakeEngine
	err error
}

func (e *failingTurnEngine) RunTurn(context.Context, TurnInput, agent.EventSink) (*TurnResult, error) {
	return nil, e.err
}

// turn.error carries a fixed text: a store error in the turn's failure
// (history that could not be committed, a sandbox that would not start) is
// logged by runTurnAsync and never reaches the frame.
func TestTurnErrorFrameCarriesAFixedMessage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"an internal failure", fmt.Errorf("take turn sandbox: %w", errors.New(driverText)), errTurnFailed.Error()},
		{"history that could not be saved", fmt.Errorf("%w: %w", agent.ErrHistoryCommitFailed, errors.New(driverText)), "conversation history could not be saved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := newFakeChatStore()
			srv := newDefaultChatServer(t, &failingTurnEngine{fakeEngine: &fakeEngine{}, err: tc.err}, st)
			conv, err := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "", false)
			if err != nil {
				t.Fatal(err)
			}
			w := postChatRequest(t, srv, map[string]any{"message": "go", "conversation_id": conv.ID})
			body := w.Body.String()
			if !strings.Contains(body, "event: turn.error") {
				t.Fatalf("no turn.error frame:\n%s", body)
			}
			if !strings.Contains(body, `"message":"`+tc.want+`"`) || strings.Contains(body, "SQLSTATE") {
				t.Fatalf("turn.error should carry only %q:\n%s", tc.want, body)
			}
		})
	}
}

// Each turn failure class maps to its text; anything unclassified is the
// fixed internal-error text.
func TestTurnErrorMessage(t *testing.T) {
	t.Parallel()
	refusal := &agent.TurnRefusalError{Err: errors.New(`model "x/y" not allowed in lockdown mode`)}
	for _, tc := range []struct {
		err  error
		want string
	}{
		{errors.New(driverText), errTurnFailed.Error()},
		{fmt.Errorf("open MCP turn scope: %w", errors.New(driverText)), errTurnFailed.Error()},
		{refusal, `model "x/y" not allowed in lockdown mode`},
		{agent.ErrAtCapacity, errTurnAtCapacity.Error()},
		{fmt.Errorf("%w: %w", agent.ErrHistoryCommitFailed, errors.New(driverText)), errTurnHistoryNotSaved.Error()},
		{fmt.Errorf("%w: profile strict", agentcore.ErrGuardrailBlocked), errTurnGuardrailBlocked.Error()},
	} {
		if got := turnErrorMessage(tc.err); got != tc.want {
			t.Errorf("turnErrorMessage(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// The turn.error texts that mirror an agent sentinel use its exact words, so
// either spelling finds the browser's translation.
func TestTurnErrorTextsMirrorAgentSentinels(t *testing.T) {
	t.Parallel()
	for _, pair := range [][2]error{
		{errTurnAtCapacity, agent.ErrAtCapacity},
		{errTurnHistoryNotSaved, agent.ErrHistoryCommitFailed},
		{errTurnGuardrailBlocked, agentcore.ErrGuardrailBlocked},
	} {
		if pair[0].Error() != pair[1].Error() {
			t.Errorf("%q differs from the agent's %q", pair[0], pair[1])
		}
	}
}

// failingSummaryEngine fails every summary with err.
type failingSummaryEngine struct {
	*fakeEngine
	err error
}

func (e *failingSummaryEngine) Summarize(context.Context, SummarizeInput) (*SummarizeResult, error) {
	return nil, e.err
}

// summary.error carries a fixed text for a summary that failed for an
// internal reason; the credits message and a lockdown refusal keep theirs.
func TestSummaryErrorFrameCarriesAFixedMessage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"an internal failure", fmt.Errorf("replay history for summarize: %w", errors.New(driverText)), errSummarizeFailed.Error()},
		{"a lockdown refusal", &agent.TurnRefusalError{Err: errors.New("model not allowed in lockdown mode")}, "model not allowed in lockdown mode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st := newFakeChatStore()
			srv := newDefaultChatServer(t, &failingSummaryEngine{fakeEngine: &fakeEngine{}, err: tc.err}, st)
			conv, err := st.CreateConversation(context.Background(), "u@x.com", "t", "generic", "", false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.AppendHistory(context.Background(), conv.ID, []agent.HistoryEntry{
				histEntry("user", "text", agent.TextContent{Text: "question"}),
				histEntry("assistant", "text", agent.TextContent{Text: "answer"}),
			}); err != nil {
				t.Fatal(err)
			}
			w := do(t, srv.Routes(), http.MethodPost, "/conversations/"+conv.ID+"/summarize", map[string]any{"model": "test/model"}, "u@x.com")
			body := w.Body.String()
			if !strings.Contains(body, "event: summary.error") || !strings.Contains(body, `"message":"`+tc.want+`"`) || strings.Contains(body, "SQLSTATE") {
				t.Fatalf("summary.error should carry only %q:\n%s", tc.want, body)
			}
		})
	}
}

// failureText keeps a 4xx's text and an error of fixedServerFailures; any
// other 5xx is logged and answered with the fixed message.
func TestFailureText(t *testing.T) {
	out := captureLogs(t)
	ctx := context.Background()
	if got := failureText(ctx, nil, http.StatusBadRequest, errors.New("persona name is invalid")); got != "persona name is invalid" {
		t.Errorf("4xx: %q", got)
	}
	if got := failureText(ctx, nil, http.StatusInternalServerError, fmt.Errorf("isolate conversation workspace: %w", errWorkspaceUnavailable)); got != errWorkspaceUnavailable.Error() {
		t.Errorf("fixed 5xx: %q", got)
	}
	if got := failureText(ctx, nil, http.StatusServiceUnavailable, errServerShuttingDown); got != errServerShuttingDown.Error() {
		t.Errorf("shutting down: %q", got)
	}
	if got := failureText(ctx, nil, http.StatusInternalServerError, errors.New("load history: "+driverText)); got != internalErrorMessage {
		t.Errorf("internal 5xx: %q", got)
	}
	internalErrorRecord(t, out, "load history: ")
}

// An administrator's request that failed records a refusal's own text, and
// a store failure as the fixed message (logged), for the admin chats page.
func TestAdminRequestFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("%w: this chat is not in the recycle bin", store.ErrAdminChatNotAllowed), store.ErrAdminChatNotAllowed.Error() + ": this chat is not in the recycle bin"},
		{store.ErrConversationNotFound, "the chat no longer exists"},
		{errTransferBusy, errTransferBusy.Error()},
		{errTransferFailed, errTransferFailed.Error()},
		{errors.New(driverText), internalErrorMessage},
	} {
		if got := adminRequestFailure(ctx, tc.err); got != tc.want {
			t.Errorf("adminRequestFailure(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// A setting the apply hook refused answers with the hook's text: 409 when the
// change was undone, 500 with what to do when the undo failed too; a store
// failure is an internal error.
func TestHTTPErrorForSetting(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodPut, "/admin/settings/guardrail_mode", nil)
	hookErr := errors.New("guardrail mode block requires guardrail_url: empty URL")
	for _, tc := range []struct {
		err    error
		status int
		body   string
	}{
		{&settings.ApplyError{Key: "guardrail_mode", Err: hookErr}, http.StatusConflict, "apply guardrail_mode: " + hookErr.Error()},
		{&settings.ApplyError{Key: "guardrail_mode", Err: hookErr, Compensation: "rolling back the persisted row failed — run Reset"}, http.StatusInternalServerError, "apply guardrail_mode: " + hookErr.Error() + " (and rolling back the persisted row failed — run Reset)"},
		{fmt.Errorf("persist guardrail_mode: %w", errors.New(driverText)), http.StatusInternalServerError, internalErrorMessage},
	} {
		rec := httptest.NewRecorder()
		httpErrorForSetting(rec, req, tc.err)
		if rec.Code != tc.status || strings.TrimSpace(rec.Body.String()) != tc.body {
			t.Errorf("%v: %d %q, want %d %q", tc.err, rec.Code, rec.Body.String(), tc.status, tc.body)
		}
	}
}

// A partly applied admin change says what changed and to reload; the failed
// step is logged.
func TestAdminChatErrorPartialChange(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodPatch, "/admin/chats/c1", nil)
	want := `the change was partly applied: renamed the chat to "X"; reload`
	// A failed step that reads as not found (the chat deleted mid-change)
	// still reports the partial apply.
	for _, stepErr := range []error{errors.New(driverText), store.ErrConversationNotFound} {
		rec := httptest.NewRecorder()
		adminChatError(rec, req, &store.AdminPartialChangeError{Did: []string{`renamed the chat to "X"`}, Err: stepErr})
		if rec.Code != http.StatusInternalServerError || strings.TrimSpace(rec.Body.String()) != want {
			t.Fatalf("%v: %d %q, want 500 %q", stepErr, rec.Code, rec.Body.String(), want)
		}
	}
}

// Stored notification secrets the current key cannot decrypt answer 409 with
// what to do, not the decryption error.
func TestHTTPErrorForNotifySettingsUndecryptable(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodPost, "/admin/notify-settings/test", nil)
	rec := httptest.NewRecorder()
	httpErrorForNotifySettings(rec, req, fmt.Errorf("%w: smtp password: %w", store.ErrNotifySecretsUndecryptable, errors.New("cipher: message authentication failed")))
	if rec.Code != http.StatusConflict || strings.TrimSpace(rec.Body.String()) != errNotifySecretsUnreadable.Error() {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
}

// retryable stands in for pgx's unexported connection errors ("receive
// message failed: read tcp …: connection reset by peer"), which all implement
// SafeToRetry.
type retryable struct{}

func (retryable) Error() string {
	return "receive message failed: read tcp 10.0.0.5:5432: connection reset by peer"
}
func (retryable) SafeToRetry() bool { return false }

func TestIsDatabaseError(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"a wrapped server error", fmt.Errorf("list servers: %w", &pgconn.PgError{Code: "42P01"}), true},
		{"a failed connect", &pgconn.ConnectError{}, true},
		{"a broken connection", fmt.Errorf("list servers: %w", retryable{}), true},
		{"a bad driver connection", driver.ErrBadConn, true},
		{"a closed pool", errors.New("sql: database is closed"), true},
		{"a remote server's network error", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, false},
		{"a refusal", errors.New("nothing to change"), false},
		{"nil", nil, false},
	} {
		if got := IsDatabaseError(tc.err); got != tc.want {
			t.Errorf("%s: IsDatabaseError = %v, want %v", tc.name, got, tc.want)
		}
	}
}
