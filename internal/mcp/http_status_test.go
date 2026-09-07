package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHTTPTransportNon2xxIsReportedAsStatus: a server answering a JSON-RPC
// call with a non-2xx status and a plain-text body must surface the status
// and that body, not a JSON decode error. GitHub's remote MCP server answers
// a revoked bearer exactly this way (#1006), and the transport used to report
// "invalid character 'u' looking for beginning of value".
func TestHTTPTransportNon2xxIsReportedAsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("unauthorized: AuthenticateToken authentication failed\nsecond line never shown"))
	}))
	defer srv.Close()

	c := NewClient()
	defer func() { _ = c.Close() }()
	err := c.AddHTTPServerWithOptions(context.Background(), "gh", srv.URL, HTTPServerOptions{})
	if err == nil {
		t.Fatal("AddHTTPServerWithOptions succeeded against a 401 server")
	}
	var hs *HTTPStatusError
	if !errors.As(err, &hs) {
		t.Fatalf("error %v (%T) is not an *HTTPStatusError", err, err)
	}
	if hs.StatusCode != http.StatusUnauthorized || !hs.Unauthorized() {
		t.Errorf("status = %d, want 401 / Unauthorized()", hs.StatusCode)
	}
	if hs.Body != "unauthorized: AuthenticateToken authentication failed" {
		t.Errorf("body = %q, want the first line only", hs.Body)
	}
	for _, want := range []string{"HTTP 401", "AuthenticateToken authentication failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error text %q lacks %q", err.Error(), want)
		}
	}
	if strings.Contains(err.Error(), "invalid character") {
		t.Errorf("error text still reads like a JSON decode failure: %q", err.Error())
	}
}

// A JSON-RPC error object carried on a 4xx keeps its existing parse: callers
// inspect those codes (method not found, invalid params), so the status check
// must not swallow them.
func TestHTTPTransportNon2xxWithJSONRPCErrorKeepsRPCError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`))
	}))
	defer srv.Close()

	c := NewClient()
	defer func() { _ = c.Close() }()
	err := c.AddHTTPServerWithOptions(context.Background(), "s", srv.URL, HTTPServerOptions{})
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error %v (%T) should still be the JSON-RPC error", err, err)
	}
	if rpcErr.Code != -32601 || rpcErr.Message != "method not found" {
		t.Errorf("rpc error = %+v, want -32601 method not found", rpcErr)
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine([]byte("  a   b\r\nc")); got != "a b" {
		t.Errorf("firstLine = %q, want %q", got, "a b")
	}
	if got := firstLine(nil); got != "" {
		t.Errorf("firstLine(nil) = %q, want empty", got)
	}
	long := firstLine([]byte(strings.Repeat("x", 500)))
	if len(long) > 200+len("…") || !strings.HasSuffix(long, "…") {
		t.Errorf("firstLine not bounded: len=%d", len(long))
	}
}
