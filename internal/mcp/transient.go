package mcp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

// UnattributedResponseError is a JSON-RPC response that does not answer the
// request's id but carries an error member — what a server that cannot
// attribute a request (an auth gateway in front of it, typically) answers
// with, id:null. The call still fails: a response that does not match the
// request id is never attributed as its result. Error() keeps the historical
// text; Carried is the error member parsed as an RPCError (nil when it does not
// parse), so a caller can read the server's real complaint without matching
// the text.
//
// It deliberately does NOT unwrap to the RPCError: callers that branch on an
// *RPCError's code (method-not-found, invalid params, the key probe) mean a
// response to THEIR request, and this is not one.
type UnattributedResponseError struct {
	ResponseID string
	WantID     int
	Raw        string
	Carried    *RPCError
}

func newUnattributedResponseError(responseID string, wantID int, raw []byte) *UnattributedResponseError {
	e := &UnattributedResponseError{ResponseID: responseID, WantID: wantID, Raw: string(raw)}
	carried := &RPCError{}
	if err := carried.UnmarshalJSON(raw); err == nil {
		e.Carried = carried
	}
	return e
}

func (e *UnattributedResponseError) Error() string {
	return fmt.Sprintf("MCP http: response id %s does not match request id %d (response carried error: %s)", e.ResponseID, e.WantID, e.Raw)
}

// IsTransientConnectError reports whether err — the failure to register an MCP
// server, i.e. to connect to it and complete the initialize handshake — is
// weather that a later attempt can clear, rather than a configuration or
// protocol problem that will fail the same way every time.
//
// Transient: a DNS lookup failure (no such host, temporary failure in name
// resolution — production saw a one-minute "lookup pages.elcanotek.com: no
// such host" blip fail a whole run), a timeout or deadline on the dial or the
// request, a refused, reset or unreachable connection, an HTTP 500, 502, 503,
// 504 or 429, and a JSON-RPC error whose message says the condition is
// temporary (fast.io answers "Auth validation temporarily unavailable.
// Retry." with code -32000 while its auth backend is down).
//
// Not transient: an HTTP 401/403 or any other 4xx, a 501 (the server does not
// implement the request) and any other 5xx, a malformed URL, a TLS or
// protocol failure, a JSON-RPC error that does not say it is temporary
// ("Invalid API key. Do not retry.", "This tool is unavailable on your
// plan"), and the caller's own cancellation.
//
// A DNS failure stays transient although a typo'd or decommissioned host
// fails the same way forever: the recurrence park breaker bounds that case
// (ADR-0077), not this classifier.
func IsTransientConnectError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		switch statusErr.StatusCode {
		case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable,
			http.StatusGatewayTimeout, http.StatusTooManyRequests:
			return true
		}
		return false
	}
	if rpcErr := connectRPCError(err); rpcErr != nil {
		return rpcMessageSaysTransient(rpcErr.Message)
	}
	return false
}

// isRetryableConnectError reports whether a failed registration is worth
// another attempt within the same run: transient AND fast-failing. A timeout
// or deadline is transient, but it already spent the whole request timeout
// (up to DefaultMCPHTTPTimeout); retrying a server that accepts the
// connection and never answers would triple that cost on every scheduled
// run. Such a failure is left to the occurrence-level re-run instead.
func isRetryableConnectError(err error) bool {
	if !IsTransientConnectError(err) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var netErr net.Error
	return !errors.As(err, &netErr) || !netErr.Timeout()
}

// connectRPCError returns the JSON-RPC error a failed handshake carried —
// answered to the request or unattributed (id:null) — or nil.
func connectRPCError(err error) *RPCError {
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		return rpcErr
	}
	var unattributed *UnattributedResponseError
	if errors.As(err, &unattributed) {
		return unattributed.Carried
	}
	return nil
}

// rpcMessageSaysTransient reads a JSON-RPC error message the way an operator
// would: a server that says the condition is temporary ("temporarily
// unavailable", "temporary failure"), that its service is unavailable, or
// to try again, is. Bare "retry" or "unavailable" are not enough — "Invalid
// API key. Do not retry." and "This tool is unavailable on your plan" are
// permanent — and an explicit "do not retry" / "do not try again" always
// wins.
func rpcMessageSaysTransient(message string) bool {
	m := strings.Join(strings.Fields(strings.ToLower(message)), " ")
	for _, negation := range []string{"do not retry", "don't retry", "do not try again", "don't try again"} {
		if strings.Contains(m, negation) {
			return false
		}
	}
	for _, phrase := range []string{"temporar", "try again", "service unavailable", "server unavailable", "service is unavailable", "server is unavailable"} {
		if strings.Contains(m, phrase) {
			return true
		}
	}
	return false
}

// maxConnectErrorSummaryRunes bounds the vendor text a summary quotes.
const maxConnectErrorSummaryRunes = 160

// ConnectErrorSummary is a short, credential-free description of why a server
// failed to register: the class of failure and, for a JSON-RPC error, the
// server's own code and (bounded) message. It never quotes the URL, a header
// or a response body, so it may cross the broker boundary and land in a task's
// error message, where the full error (which can quote a resolved URL) may
// not. "" when err is nil; "failed to connect" when nothing more specific is
// known.
func ConnectErrorSummary(err error) string {
	if err == nil {
		return ""
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		switch {
		case dnsErr.IsNotFound:
			return "DNS lookup failed (no such host)"
		case dnsErr.IsTimeout:
			return "DNS lookup timed out"
		default:
			return "DNS lookup failed (temporary failure in name resolution)"
		}
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.ECONNABORTED):
		return "connection reset"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "host unreachable"
	}
	var statusErr *HTTPStatusError
	if errors.As(err, &statusErr) {
		return fmt.Sprintf("HTTP %d %s", statusErr.StatusCode, http.StatusText(statusErr.StatusCode))
	}
	if rpcErr := connectRPCError(err); rpcErr != nil {
		message := strings.Join(strings.Fields(rpcErr.Message), " ")
		if utf8.RuneCountInString(message) > maxConnectErrorSummaryRunes {
			message = string([]rune(message)[:maxConnectErrorSummaryRunes]) + "…"
		}
		if rpcErr.Code != 0 {
			return fmt.Sprintf("JSON-RPC error %d: %s", rpcErr.Code, message)
		}
		return "JSON-RPC error: " + message
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timed out"
	}
	return "failed to connect"
}

// ConnectRetryDelays are the pauses before the second and the third attempt
// to register a server whose previous attempt failed with a retryable error:
// three attempts in all, about seven seconds of waiting, so a short DNS or
// vendor blip no longer takes a server out of a whole run. A variable so
// tests do not sleep.
var ConnectRetryDelays = []time.Duration{2 * time.Second, 5 * time.Second}

// MaxConnectRetryBudget bounds the time the connect retry may add to one run,
// across every server it registers: the pauses plus the retried attempts.
// Servers bind one after another, so without a run-wide cap a run whose
// owner has many connections could wait the per-server pauses for each.
const MaxConnectRetryBudget = 15 * time.Second

// connectRetryBudget is the run's remaining retry allowance (see
// WithConnectRetry). Shared by every registration under one ctx.
type connectRetryBudget struct {
	mu        sync.Mutex
	remaining time.Duration
	spent     time.Duration
}

func (b *connectRetryBudget) left() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.remaining
}

func (b *connectRetryBudget) spend(d time.Duration) {
	if d <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.spent += d
	b.remaining = max(b.remaining-d, 0)
}

type connectRetryKey struct{}

// WithConnectRetry marks ctx so that server registrations made under it retry
// a retryable failure (RetryTransientConnect), sharing one
// MaxConnectRetryBudget. Unattended scheduled runs opt in, once per run: one
// skipped server can dead-letter a run nobody is watching. An interactive
// chat turn does not, so a turn never waits on a server that is down — or
// whose host does not resolve at all — before it starts. Across the broker
// boundary the remaining allowance travels as mcpbroker.ScopeSpec's
// ConnectRetryBudgetMs and the child reports back what it spent.
func WithConnectRetry(ctx context.Context) context.Context {
	return WithConnectRetryBudget(ctx, MaxConnectRetryBudget)
}

// WithConnectRetryBudget is WithConnectRetry with an explicit allowance (the
// broker child receives what is left of the parent's). A non-positive
// allowance leaves ctx unmarked: no retry.
func WithConnectRetryBudget(ctx context.Context, budget time.Duration) context.Context {
	if budget <= 0 {
		return ctx
	}
	return context.WithValue(ctx, connectRetryKey{}, &connectRetryBudget{remaining: min(budget, MaxConnectRetryBudget)})
}

func connectRetryBudgetFrom(ctx context.Context) *connectRetryBudget {
	b, _ := ctx.Value(connectRetryKey{}).(*connectRetryBudget)
	return b
}

// ConnectRetryBudgetLeft is the retry allowance ctx still carries; 0 when it
// carries none (no WithConnectRetry, or spent).
func ConnectRetryBudgetLeft(ctx context.Context) time.Duration {
	if b := connectRetryBudgetFrom(ctx); b != nil {
		return b.left()
	}
	return 0
}

// ConnectRetrySpent is how much of ctx's allowance the retry has used.
func ConnectRetrySpent(ctx context.Context) time.Duration {
	b := connectRetryBudgetFrom(ctx)
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent
}

// SpendConnectRetryBudget charges d against ctx's allowance — what a broker
// child reported it spent on the run's behalf. No-op without one.
func SpendConnectRetryBudget(ctx context.Context, d time.Duration) {
	if b := connectRetryBudgetFrom(ctx); b != nil {
		b.spend(d)
	}
}

// RetryTransientConnect runs connect and, when ctx carries WithConnectRetry,
// runs it again after each ConnectRetryDelays pause for as long as it fails
// with a retryable error (transient and fast-failing: not a timeout, see
// isRetryableConnectError) and ctx is live. Every pause and every retried
// attempt is charged to the run's allowance: a pause that does not fit stops
// the retry, and a retried attempt runs under a deadline of what is left, so
// the retry adds at most MaxConnectRetryBudget to a run however many servers
// fail. A non-retryable failure returns at once — a refused credential or a
// bad URL fails the same way on every attempt. Each retry is logged with the
// server name and the error class; the returned error is the last attempt's.
func RetryTransientConnect(ctx context.Context, server string, connect func(context.Context) error) error {
	err := connect(ctx)
	budget := connectRetryBudgetFrom(ctx)
	if budget == nil {
		return err
	}
	for i, delay := range ConnectRetryDelays {
		if err == nil || !isRetryableConnectError(err) || ctx.Err() != nil {
			return err
		}
		if delay >= budget.left() {
			log.Printf("mcp: server %q failed to connect (%s); not retrying — the run's %s connect-retry budget is spent",
				server, ConnectErrorSummary(err), MaxConnectRetryBudget)
			return err
		}
		log.Printf("mcp: server %q failed to connect (%s); attempt %d of %d, retrying in %s",
			server, ConnectErrorSummary(err), i+1, len(ConnectRetryDelays)+1, delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return err
		case <-timer.C:
		}
		budget.spend(delay)
		attemptCtx, cancel := context.WithTimeout(ctx, budget.left())
		started := time.Now()
		err = connect(attemptCtx)
		cancel()
		budget.spend(time.Since(started))
	}
	return err
}
