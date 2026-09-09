package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// statusMismatchServer answers initialize with 200 and every other call with
// the given status — the body being a complete, valid JSON-RPC response in
// both cases. It models Google's Drive MCP server (Developer Preview), which
// answers tools/list with HTTP 403 and a full result when the project has not
// enabled drivemcp.googleapis.com (#1006).
func statusMismatchServer(t *testing.T, status int, tools []map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     *int   `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted) // notifications/initialized
			return
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": *req.ID}
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			resp["result"] = map[string]any{"protocolVersion": "2024-11-05"}
		case "tools/list":
			resp["result"] = map[string]any{"tools": tools}
			w.WriteHeader(status)
		default:
			resp["error"] = map[string]any{"code": -32601, "message": "method not found"}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

// TestHTTPTransportNon2xxWithLargeJSONRPCResultHonorsBody: a JSON-RPC body
// wins over the HTTP status at any size the 2xx path would accept. The first
// status-aware transport re-parsed such bodies only up to 4 KiB, so a 43-tool
// list on a 403 was "unparsable" and the server could not be mounted at all —
// worse than before the status check existed.
func TestHTTPTransportNon2xxWithLargeJSONRPCResultHonorsBody(t *testing.T) {
	tools := make([]map[string]any, 40)
	for i := range tools {
		tools[i] = map[string]any{
			"name":        fmt.Sprintf("tool_%02d", i),
			"description": strings.Repeat("d", 200), // ~10 KiB total: well past httpStatusBodyCap
			"inputSchema": map[string]any{"type": "object"},
		}
	}
	srv := statusMismatchServer(t, http.StatusForbidden, tools)
	defer srv.Close()

	c := NewClient()
	defer func() { _ = c.Close() }()
	if err := c.AddHTTPServerWithOptions(context.Background(), "drive", srv.URL, HTTPServerOptions{}); err != nil {
		t.Fatalf("a 403 carrying a complete JSON-RPC tools/list result must still mount: %v", err)
	}
	got := 0
	for _, st := range c.GetAllTools() {
		if st.ServerName == "drive" {
			got++
		}
	}
	if got != len(tools) {
		t.Errorf("mounted %d tools, want %d", got, len(tools))
	}
}

// A large NON-JSON body on a non-2xx stays an HTTPStatusError quoting the
// first line: the head kept for the message is bounded, the stream is not.
func TestHTTPTransportNon2xxLargeTextBodyStaysStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("\n  forbidden: the caller does not have permission\n" + strings.Repeat("x", 20000)))
	}))
	defer srv.Close()

	c := NewClient()
	defer func() { _ = c.Close() }()
	err := c.AddHTTPServerWithOptions(context.Background(), "s", srv.URL, HTTPServerOptions{})
	var hs *HTTPStatusError
	if !errors.As(err, &hs) {
		t.Fatalf("error %v (%T) is not an *HTTPStatusError", err, err)
	}
	if hs.StatusCode != http.StatusForbidden || hs.Body != "forbidden: the caller does not have permission" {
		t.Errorf("got status=%d body=%q", hs.StatusCode, hs.Body)
	}
}

// A plain-text reason delivered in two flushed chunks must still be quoted
// whole. The head is filled through a tee by whatever the peek pulls — one
// Read — so without an explicit drain the error would carry only the first
// chunk ("forbidden: the caller") and lose exactly the diagnostic #1454 exists
// to surface.
func TestHTTPTransportNon2xxChunkedTextBodyQuotesWholeLine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("forbidden: the caller "))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		_, _ = w.Write([]byte("does not have permission\n" + strings.Repeat("x", 20000)))
	}))
	defer srv.Close()

	c := NewClient()
	defer func() { _ = c.Close() }()
	err := c.AddHTTPServerWithOptions(context.Background(), "s", srv.URL, HTTPServerOptions{})
	var hs *HTTPStatusError
	if !errors.As(err, &hs) {
		t.Fatalf("error %v (%T) is not an *HTTPStatusError", err, err)
	}
	if hs.Body != "forbidden: the caller does not have permission" {
		t.Errorf("chunked body quoted as %q, want the whole first line", hs.Body)
	}
}

// A Google-style REST error object is JSON but not JSON-RPC: it has an
// "error" key yet neither a jsonrpc member nor our request id, so it must be
// reported as the status plus its text — not as a phantom id mismatch.
func TestHTTPTransportNon2xxRESTErrorObjectIsStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":403,"message":"Drive MCP API has not been used in project 123 before or it is disabled.","status":"PERMISSION_DENIED"}}`))
	}))
	defer srv.Close()

	c := NewClient()
	defer func() { _ = c.Close() }()
	err := c.AddHTTPServerWithOptions(context.Background(), "s", srv.URL, HTTPServerOptions{})
	var hs *HTTPStatusError
	if !errors.As(err, &hs) {
		t.Fatalf("error %v (%T) is not an *HTTPStatusError", err, err)
	}
	if !strings.Contains(hs.Body, "PERMISSION_DENIED") || strings.Contains(err.Error(), "does not match request id") {
		t.Errorf("got %q, want the REST error text without an id-mismatch complaint", err.Error())
	}
}
