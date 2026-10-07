package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// noConnectRetryDelay makes RetryTransientConnect retry without sleeping.
func noConnectRetryDelay(t *testing.T) {
	t.Helper()
	saved := ConnectRetryDelays
	ConnectRetryDelays = []time.Duration{0, 0}
	t.Cleanup(func() { ConnectRetryDelays = saved })
}

// wrapAsInitialize nests err the way AddHTTPServerWithOptions reports a
// failed handshake, so the classifier is exercised through the real chain.
func wrapAsInitialize(err error) error {
	return fmt.Errorf("failed to initialize server: %w", fmt.Errorf("initialize call failed: %w", err))
}

func TestIsTransientConnectError(t *testing.T) {
	dial := func(err error) error {
		return &url.Error{Op: "Post", URL: "https://pages.example/mcp", Err: &net.OpError{Op: "dial", Net: "tcp", Err: err}}
	}
	unattributed := newUnattributedResponseError("null", 1, []byte(`{"code":-32000,"message":"Auth validation temporarily unavailable. Retry."}`))
	cases := []struct {
		name    string
		err     error
		want    bool
		summary string
	}{
		{"dns no such host", dial(&net.DNSError{Err: "no such host", Name: "pages.example", IsNotFound: true}), true, "DNS lookup failed (no such host)"},
		{"dns temporary failure", dial(&net.DNSError{Err: "temporary failure in name resolution", Name: "pages.example", IsTemporary: true}), true, "DNS lookup failed (temporary failure in name resolution)"},
		{"connection refused", dial(os.NewSyscallError("connect", syscall.ECONNREFUSED)), true, "connection refused"},
		{"connection reset", dial(os.NewSyscallError("read", syscall.ECONNRESET)), true, "connection reset"},
		{"request deadline", &url.Error{Op: "Post", URL: "https://x/mcp", Err: context.DeadlineExceeded}, true, "timed out"},
		{"http 503", &HTTPStatusError{StatusCode: 503, Body: "upstream down"}, true, "HTTP 503 Service Unavailable"},
		{"http 429", &HTTPStatusError{StatusCode: 429}, true, "HTTP 429 Too Many Requests"},
		{"unattributed temporary json-rpc error", unattributed, true, "JSON-RPC error -32000: Auth validation temporarily unavailable. Retry."},
		{"json-rpc service unavailable", &RPCError{Code: -32603, Message: "Service unavailable"}, true, "JSON-RPC error -32603: Service unavailable"},
		{"json-rpc try again", &RPCError{Code: -32000, Message: "Upstream busy, please try again later"}, true, "JSON-RPC error -32000: Upstream busy, please try again later"},
		{"http 502", &HTTPStatusError{StatusCode: 502}, true, "HTTP 502 Bad Gateway"},
		{"http 504", &HTTPStatusError{StatusCode: 504}, true, "HTTP 504 Gateway Timeout"},
		{"http 401", &HTTPStatusError{StatusCode: 401, Body: "bad token"}, false, "HTTP 401 Unauthorized"},
		{"http 403", &HTTPStatusError{StatusCode: 403}, false, "HTTP 403 Forbidden"},
		{"http 404", &HTTPStatusError{StatusCode: 404}, false, "HTTP 404 Not Found"},
		{"json-rpc invalid params", &RPCError{Code: -32602, Message: "invalid params"}, false, "JSON-RPC error -32602: invalid params"},
		// Bare "retry" / "unavailable" are not a transient signal.
		{"json-rpc do not retry", &RPCError{Code: -32001, Message: "Invalid API key. Do not retry."}, false, "JSON-RPC error -32001: Invalid API key. Do not retry."},
		{"json-rpc unavailable on plan", &RPCError{Code: -32001, Message: "This tool is unavailable on your plan"}, false, "JSON-RPC error -32001: This tool is unavailable on your plan"},
		{"json-rpc temporary but do not try again", &RPCError{Code: -32001, Message: "Account temporarily locked; do not try again"}, false, "JSON-RPC error -32001: Account temporarily locked; do not try again"},
		{"http 501", &HTTPStatusError{StatusCode: 501}, false, "HTTP 501 Not Implemented"},
		{"bad url", errors.New(`parse "::": missing protocol scheme`), false, "failed to connect"},
		{"caller cancelled", &url.Error{Op: "Post", URL: "https://x/mcp", Err: context.Canceled}, false, "failed to connect"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := wrapAsInitialize(tc.err)
			if got := IsTransientConnectError(err); got != tc.want {
				t.Errorf("IsTransientConnectError(%v) = %t, want %t", err, got, tc.want)
			}
			if got := ConnectErrorSummary(err); got != tc.summary {
				t.Errorf("ConnectErrorSummary = %q, want %q", got, tc.summary)
			}
			if strings.Contains(ConnectErrorSummary(err), "pages.example") {
				t.Error("the summary must not quote the server's host or URL")
			}
		})
	}
	if IsTransientConnectError(nil) || ConnectErrorSummary(nil) != "" {
		t.Error("nil is neither transient nor summarized")
	}
}

// TestUnattributedResponseErrorKeepsTextAndIsNotAnRPCError: the id:null
// handshake error keeps its historical message and is not an *RPCError to
// callers that branch on a response to their own request.
func TestUnattributedResponseErrorKeepsTextAndIsNotAnRPCError(t *testing.T) {
	err := error(newUnattributedResponseError("null", 1, []byte(`{"code":-32000,"message":"Retry."}`)))
	if want := `MCP http: response id null does not match request id 1 (response carried error: {"code":-32000,"message":"Retry."})`; err.Error() != want {
		t.Fatalf("Error() = %q, want %q", err.Error(), want)
	}
	var rpcErr *RPCError
	if errors.As(err, &rpcErr) {
		t.Fatal("an unattributed response must not unwrap to an *RPCError")
	}
}

// flakyMCPServer answers initialize with the fast.io outage reply (id:null,
// -32000 "temporarily unavailable") for the first `failures` handshakes, then
// like a healthy server.
func flakyMCPServer(t *testing.T, failures int32, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var initializes atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "initialize" {
			if n := initializes.Add(1); n <= failures {
				if status != 0 {
					w.WriteHeader(status)
					_, _ = w.Write([]byte("refused"))
					return
				}
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32000,"message":"Auth validation temporarily unavailable. Retry."}}`))
				return
			}
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "initialize":
			resp["result"] = map[string]any{"protocolVersion": "2024-11-05"}
		case "tools/list":
			resp["result"] = map[string]any{"tools": []Tool{{Name: "upload", Description: "upload"}}}
		default:
			resp["result"] = map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, &initializes
}

func TestRetryTransientConnectRegistersAfterAVendorBlip(t *testing.T) {
	noConnectRetryDelay(t)
	srv, initializes := flakyMCPServer(t, 2, 0)
	client := NewClient()
	defer func() { _ = client.Close() }()
	ctx := WithConnectRetry(context.Background())
	err := RetryTransientConnect(ctx, "fast_io", func(ctx context.Context) error {
		return client.AddHTTPServerWithOptions(ctx, "fast_io", srv.URL, HTTPServerOptions{})
	})
	if err != nil {
		t.Fatalf("registration failed after a two-handshake blip: %v", err)
	}
	if got := initializes.Load(); got != 3 {
		t.Fatalf("initialize attempts = %d, want 3", got)
	}
	if !client.HasServer("fast_io") {
		t.Fatal("server not registered after the retried handshake")
	}
}

func TestRetryTransientConnectGivesUpAfterThreeAttempts(t *testing.T) {
	noConnectRetryDelay(t)
	srv, initializes := flakyMCPServer(t, 10, 0)
	client := NewClient()
	defer func() { _ = client.Close() }()
	ctx := WithConnectRetry(context.Background())
	err := RetryTransientConnect(ctx, "fast_io", func(ctx context.Context) error {
		return client.AddHTTPServerWithOptions(ctx, "fast_io", srv.URL, HTTPServerOptions{})
	})
	if err == nil || !IsTransientConnectError(err) {
		t.Fatalf("err = %v, want the last transient handshake error", err)
	}
	if got := initializes.Load(); got != 3 {
		t.Fatalf("initialize attempts = %d, want 3 (bounded)", got)
	}
	if client.HasServer("fast_io") {
		t.Fatal("a server that never initialized must not be registered")
	}
}

func TestRetryTransientConnectDoesNotRetryARefusedCredential(t *testing.T) {
	noConnectRetryDelay(t)
	srv, initializes := flakyMCPServer(t, 10, http.StatusUnauthorized)
	client := NewClient()
	defer func() { _ = client.Close() }()
	ctx := WithConnectRetry(context.Background())
	err := RetryTransientConnect(ctx, "pages", func(ctx context.Context) error {
		return client.AddHTTPServerWithOptions(ctx, "pages", srv.URL, HTTPServerOptions{})
	})
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("err = %v, want the 401", err)
	}
	if got := initializes.Load(); got != 1 {
		t.Fatalf("initialize attempts = %d, want 1 — a refused credential fails the same way every time", got)
	}
}

func TestRetryTransientConnectStopsWhenTheContextEnds(t *testing.T) {
	saved := ConnectRetryDelays
	ConnectRetryDelays = []time.Duration{time.Hour, time.Hour}
	t.Cleanup(func() { ConnectRetryDelays = saved })
	ctx, cancel := context.WithTimeout(WithConnectRetry(context.Background()), 50*time.Millisecond)
	defer cancel()
	calls := 0
	err := RetryTransientConnect(ctx, "pages", func(context.Context) error {
		calls++
		return &HTTPStatusError{StatusCode: 503}
	})
	if err == nil || calls != 1 {
		t.Fatalf("calls = %d, err = %v; want one attempt, then return on ctx end", calls, err)
	}
}

// TestRetryTransientConnectIsOptIn: without WithConnectRetry (an interactive
// turn) a transient failure is attempted once, as before.
func TestRetryTransientConnectIsOptIn(t *testing.T) {
	noConnectRetryDelay(t)
	calls := 0
	err := RetryTransientConnect(context.Background(), "pages", func(context.Context) error {
		calls++
		return &HTTPStatusError{StatusCode: 503}
	})
	if err == nil || calls != 1 {
		t.Fatalf("calls = %d, err = %v; want a single attempt without the opt-in", calls, err)
	}
	if ConnectRetryBudgetLeft(WithConnectRetry(context.Background())) != MaxConnectRetryBudget || ConnectRetryBudgetLeft(context.Background()) != 0 {
		t.Fatal("the opt-in allowance must round-trip and default off")
	}
}

// TestRetryTransientConnectDoesNotRetryATimeout: a server that accepts the
// connection and never answers already cost the whole request timeout; the
// retry must not triple that on every run.
func TestRetryTransientConnectDoesNotRetryATimeout(t *testing.T) {
	noConnectRetryDelay(t)
	for _, timeout := range []error{
		&url.Error{Op: "Post", URL: "https://x/mcp", Err: context.DeadlineExceeded},
		&url.Error{Op: "Post", URL: "https://x/mcp", Err: &net.OpError{Op: "read", Net: "tcp", Err: timeoutError{}}},
	} {
		calls := 0
		err := RetryTransientConnect(WithConnectRetry(context.Background()), "pages", func(context.Context) error {
			calls++
			return wrapAsInitialize(timeout)
		})
		if calls != 1 || !IsTransientConnectError(err) {
			t.Fatalf("%v: calls = %d, transient = %t; want one attempt, still classified transient for the occurrence re-run", timeout, calls, IsTransientConnectError(err))
		}
	}
}

// timeoutError is a net.Error whose Timeout() is true (an i/o timeout).
type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

// TestRetryTransientConnectSharesOneRunBudget: every registration under one
// WithConnectRetry ctx draws on one allowance, so many failing servers add at
// most MaxConnectRetryBudget to the run — a pause that does not fit stops the
// retry — and a retried attempt runs under a deadline of what is left.
func TestRetryTransientConnectSharesOneRunBudget(t *testing.T) {
	saved := ConnectRetryDelays
	ConnectRetryDelays = []time.Duration{50 * time.Millisecond, 50 * time.Millisecond}
	t.Cleanup(func() { ConnectRetryDelays = saved })
	// 140ms: both of the first server's 50ms pauses fit even if its attempts
	// are slow (up to 40ms), and what is left after them (< 50ms) never fits
	// another pause.
	ctx := WithConnectRetryBudget(context.Background(), 140*time.Millisecond)
	attempts := 0
	var deadlines []time.Duration
	for _, server := range []string{"a", "b", "c"} {
		_ = RetryTransientConnect(ctx, server, func(ctx context.Context) error {
			attempts++
			if dl, ok := ctx.Deadline(); ok {
				deadlines = append(deadlines, time.Until(dl))
			}
			return &HTTPStatusError{StatusCode: 503}
		})
	}
	// a: first attempt, pause, retry, pause, retry (>= 100ms spent).
	// b and c: first attempt only — the next pause no longer fits.
	if attempts != 5 {
		t.Fatalf("attempts = %d, want 5 (3 for the first server, then the allowance is spent)", attempts)
	}
	if spent := ConnectRetrySpent(ctx); spent < 100*time.Millisecond || spent > 140*time.Millisecond {
		t.Fatalf("spent = %s, want within the 140ms allowance", spent)
	}
	if len(deadlines) != 2 || deadlines[0] > 90*time.Millisecond || deadlines[1] > 40*time.Millisecond {
		t.Fatalf("retried attempts' deadlines = %v, want bounded by what was left of the allowance", deadlines)
	}
	if WithConnectRetryBudget(context.Background(), time.Hour) == nil || ConnectRetryBudgetLeft(WithConnectRetryBudget(context.Background(), time.Hour)) != MaxConnectRetryBudget {
		t.Fatal("an allowance is capped at MaxConnectRetryBudget")
	}
	SpendConnectRetryBudget(ctx, time.Hour)
	if ConnectRetryBudgetLeft(ctx) != 0 {
		t.Fatal("a charge past the allowance leaves nothing")
	}
}

// notifyFlakyMCPServer answers initialize and tools/list normally but refuses
// the first failNotifies notifications/initialized POSTs with status — the
// shape that lost kiwi-flights its nightly smoke on 2026-10-06 (#1683).
func notifyFlakyMCPServer(t *testing.T, failNotifies int32, status int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var notifies atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Method == "notifications/initialized" {
			if notifies.Add(1) <= failNotifies {
				http.Error(w, "upstream unavailable", status)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "initialize":
			resp["result"] = map[string]any{"protocolVersion": "2024-11-05"}
		case "tools/list":
			resp["result"] = map[string]any{"tools": []Tool{{Name: "search", Description: "search"}}}
		default:
			resp["result"] = map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, &notifies
}

// A refused notifications/initialized is an HTTPStatusError like a refused
// request, so the classifier sees its status: 503 transient, 401 Unauthorized
// and not transient. It used to be a plain error that both missed.
func TestNotifyRefusalCarriesItsHTTPStatus(t *testing.T) {
	for _, c := range []struct {
		status        int
		transient     bool
		unauthorized  bool
		wantBodyQuote string
	}{
		{http.StatusServiceUnavailable, true, false, "upstream unavailable"},
		{http.StatusUnauthorized, false, true, "upstream unavailable"},
		{http.StatusNotFound, false, false, "upstream unavailable"},
	} {
		t.Run(http.StatusText(c.status), func(t *testing.T) {
			srv, _ := notifyFlakyMCPServer(t, 1, c.status)
			err := NewHTTPTransport(srv.URL).Notify(context.Background(), "notifications/initialized", map[string]any{})
			var statusErr *HTTPStatusError
			if !errors.As(err, &statusErr) || statusErr.StatusCode != c.status {
				t.Fatalf("err = %v, want an HTTPStatusError %d", err, c.status)
			}
			if statusErr.Body != c.wantBodyQuote {
				t.Fatalf("body = %q, want %q", statusErr.Body, c.wantBodyQuote)
			}
			if !strings.HasPrefix(err.Error(), "notification notifications/initialized: ") {
				t.Fatalf("err = %q, want it to name the notification", err)
			}
			if got := IsTransientConnectError(err); got != c.transient {
				t.Fatalf("IsTransientConnectError = %v, want %v", got, c.transient)
			}
			if got := statusErr.Unauthorized(); got != c.unauthorized {
				t.Fatalf("Unauthorized = %v, want %v", got, c.unauthorized)
			}
		})
	}
}

func TestRetryTransientConnectRegistersAfterANotifyBlip(t *testing.T) {
	noConnectRetryDelay(t)
	srv, notifies := notifyFlakyMCPServer(t, 1, http.StatusServiceUnavailable)
	client := NewClient()
	defer func() { _ = client.Close() }()
	ctx := WithConnectRetry(context.Background())
	err := RetryTransientConnect(ctx, "kiwi", func(ctx context.Context) error {
		return client.AddHTTPServerWithOptions(ctx, "kiwi", srv.URL, HTTPServerOptions{})
	})
	if err != nil {
		t.Fatalf("registration failed after one 503 on notifications/initialized: %v", err)
	}
	if got := notifies.Load(); got != 2 {
		t.Fatalf("notifications/initialized attempts = %d, want 2", got)
	}
	if !client.HasServer("kiwi") {
		t.Fatal("server not registered after the retried handshake")
	}
}

// A JSON-RPC error that arrives on a non-2xx keeps its status, so a classifier
// can tell a 503 carrying {"error":{"message":"Internal error"}} from the
// server refusing the request; the RPCError is still what errors.As finds.
func TestHTTPStatusRPCErrorKeepsItsStatus(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"Internal error"}}`))
		}))
		_, err := NewHTTPTransport(srv.URL).Call(context.Background(), "initialize", map[string]any{})
		srv.Close()
		var rpcErr *RPCError
		if !errors.As(err, &rpcErr) || rpcErr.Code != -32603 {
			t.Fatalf("status %d: err = %v, want the JSON-RPC error", status, err)
		}
		want := status
		if status == http.StatusOK {
			want = 0
		}
		if rpcErr.HTTPStatus != want {
			t.Fatalf("status %d: RPCError.HTTPStatus = %d, want %d", status, rpcErr.HTTPStatus, want)
		}
	}
}
