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
		{"json-rpc unavailable", &RPCError{Code: -32603, Message: "Service unavailable"}, true, "JSON-RPC error -32603: Service unavailable"},
		{"http 401", &HTTPStatusError{StatusCode: 401, Body: "bad token"}, false, "HTTP 401 Unauthorized"},
		{"http 403", &HTTPStatusError{StatusCode: 403}, false, "HTTP 403 Forbidden"},
		{"http 404", &HTTPStatusError{StatusCode: 404}, false, "HTTP 404 Not Found"},
		{"json-rpc invalid params", &RPCError{Code: -32602, Message: "invalid params"}, false, "JSON-RPC error -32602: invalid params"},
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
	err := RetryTransientConnect(ctx, "fast_io", func() error {
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
	err := RetryTransientConnect(ctx, "fast_io", func() error {
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
	err := RetryTransientConnect(ctx, "pages", func() error {
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
	err := RetryTransientConnect(ctx, "pages", func() error {
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
	err := RetryTransientConnect(context.Background(), "pages", func() error {
		calls++
		return &HTTPStatusError{StatusCode: 503}
	})
	if err == nil || calls != 1 {
		t.Fatalf("calls = %d, err = %v; want a single attempt without the opt-in", calls, err)
	}
	if !ConnectRetryEnabled(WithConnectRetry(context.Background())) || ConnectRetryEnabled(context.Background()) {
		t.Fatal("the opt-in mark must round-trip and default off")
	}
}
