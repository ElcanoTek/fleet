package mcp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
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
// resolution — production saw a one-minute resolver blip fail a whole run), a
// timeout or deadline on the dial or the request, a refused, reset or
// unreachable connection, an HTTP 5xx or 429, and a JSON-RPC error whose
// message says it is temporary (fast.io answers "Auth validation temporarily
// unavailable. Retry." with code -32000 while its auth backend is down).
//
// Not transient: an HTTP 401/403 or any other 4xx (a credential or
// permission the operator must fix), a malformed URL, a TLS or protocol
// failure, and the caller's own cancellation.
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
		return statusErr.StatusCode >= http.StatusInternalServerError || statusErr.StatusCode == http.StatusTooManyRequests
	}
	if rpcErr := connectRPCError(err); rpcErr != nil {
		return rpcMessageSaysTransient(rpcErr.Message)
	}
	return false
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
// would: a server that says it is temporarily unavailable, or asks to be
// retried, is.
func rpcMessageSaysTransient(message string) bool {
	m := strings.ToLower(message)
	for _, word := range []string{"temporar", "unavailable", "retry", "try again"} {
		if strings.Contains(m, word) {
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
// to register a server whose previous attempt failed transiently: three
// attempts in all, about seven seconds of waiting, so a short DNS or vendor
// blip no longer takes a server out of a whole run. A variable so tests do
// not sleep.
var ConnectRetryDelays = []time.Duration{2 * time.Second, 5 * time.Second}

type connectRetryKey struct{}

// WithConnectRetry marks ctx so that server registrations made under it retry
// a transient failure (RetryTransientConnect). Unattended scheduled runs opt
// in: one skipped server can dead-letter a run nobody is watching. An
// interactive chat turn does not, so a turn never waits seconds on a server
// that is down — or whose host does not resolve at all — before it starts.
// Across the broker boundary the mark travels as mcpbroker.ScopeSpec's
// RetryTransientConnect.
func WithConnectRetry(ctx context.Context) context.Context {
	return context.WithValue(ctx, connectRetryKey{}, true)
}

// ConnectRetryEnabled reports whether ctx carries WithConnectRetry.
func ConnectRetryEnabled(ctx context.Context) bool {
	on, _ := ctx.Value(connectRetryKey{}).(bool)
	return on
}

// RetryTransientConnect runs connect and, when ctx carries WithConnectRetry,
// runs it again after each ConnectRetryDelays pause for as long as it fails
// with a transient error (IsTransientConnectError) and ctx is live. A
// non-transient failure returns at once — a refused credential or a bad URL
// fails the same way on every attempt. Each retry is logged with the server
// name and the error class; the returned error is the last attempt's.
func RetryTransientConnect(ctx context.Context, server string, connect func() error) error {
	err := connect()
	if !ConnectRetryEnabled(ctx) {
		return err
	}
	for i, delay := range ConnectRetryDelays {
		if err == nil || !IsTransientConnectError(err) || ctx.Err() != nil {
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
		err = connect()
	}
	return err
}
