// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package remotemcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

// TestVendorOutageSeparatesOutagesFromRot pins which live-smoke failures only
// warn (the vendor is down tonight) and which fail the weekly lane (the
// listing is wrong). It runs in the PR gate — no network — so a change to the
// transient classifier cannot quietly turn catalog rot into a warning.
func TestVendorOutageSeparatesOutagesFromRot(t *testing.T) {
	dial := func(err error) error {
		return &url.Error{Op: "Post", URL: "https://mcp.vendor.example", Err: &net.OpError{Op: "dial", Net: "tcp", Err: err}}
	}
	handshake := func(err error) error {
		return fmt.Errorf("failed to initialize server: %w", err)
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"503 on notifications/initialized", handshake(fmt.Errorf("notification notifications/initialized: %w", &mcp.HTTPStatusError{StatusCode: 503})), true},
		{"502 at initialize", handshake(&mcp.HTTPStatusError{StatusCode: 502}), true},
		{"408 Request Timeout", handshake(&mcp.HTTPStatusError{StatusCode: 408}), true},
		{"429", handshake(&mcp.HTTPStatusError{StatusCode: 429}), true},
		{"Cloudflare 522 (origin timed out)", handshake(&mcp.HTTPStatusError{StatusCode: 522}), true},
		{"Cloudflare 530", handshake(&mcp.HTTPStatusError{StatusCode: 530}), true},
		{"507", handshake(&mcp.HTTPStatusError{StatusCode: 507}), true},
		{"501 Not Implemented", handshake(&mcp.HTTPStatusError{StatusCode: 501}), false},
		{"503 with a JSON-RPC error body", handshake(&mcp.RPCError{Code: -32603, Message: "Internal error", HTTPStatus: 503}), true},
		{"JSON-RPC error on a 2xx", handshake(&mcp.RPCError{Code: -32603, Message: "Internal error"}), false},
		{"JSON-RPC error on a 501", handshake(&mcp.RPCError{Code: -32601, Message: "Method not found", HTTPStatus: 501}), false},
		{"429 with a JSON-RPC error body", handshake(&mcp.RPCError{Code: -32000, Message: "rate limit exceeded", HTTPStatus: 429}), true},
		{"rate limit as a JSON-RPC error on a 2xx (Parallel Search, #1844)", handshake(fmt.Errorf("tools/list call failed: %w", &mcp.RPCError{Code: -32000, Message: "You've hit the free-tier rate limit for Parallel Search MCP. To continue with higher limits, add your own API key — set header `x-api-key: YOUR_KEY`. Get a key at https://platform.parallel.ai."})), true},
		{"Too Many Requests as a JSON-RPC error on a 2xx", handshake(&mcp.RPCError{Code: -32000, Message: "Too Many Requests"}), true},
		{"403 gateway rate-limit body on shared runner IP", handshake(&mcp.HTTPStatusError{StatusCode: 403, Body: `{"message":"API rate limit exceeded for shared IP"}`}), true},
		{"403 gateway throttling body", handshake(&mcp.HTTPStatusError{StatusCode: 403, Body: "Request throttled by edge gateway"}), true},
		{"unattributed rate-limit JSON-RPC error on a 2xx", handshake(&mcp.UnattributedResponseError{ResponseID: "null", WantID: 1, Raw: `{"code":-32000,"message":"Rate-limited, slow down"}`, Carried: &mcp.RPCError{Code: -32000, Message: "Rate-limited, slow down"}}), true},
		{"key-required JSON-RPC error on a 2xx (an open entry now wants a key)", handshake(&mcp.RPCError{Code: -32001, Message: "An API key is required to use this server"}), false},
		{"unattributed JSON-RPC error on a 503", handshake(&mcp.UnattributedResponseError{ResponseID: "null", WantID: 1, Raw: `{"code":-32603,"message":"Internal error"}`, HTTPStatus: 503}), true},
		{"unattributed JSON-RPC error on a 401", handshake(&mcp.UnattributedResponseError{ResponseID: "null", WantID: 1, Raw: `{"code":-32001,"message":"Unauthorized"}`, HTTPStatus: 401}), false},
		{"connection refused", dial(os.NewSyscallError("connect", syscall.ECONNREFUSED)), true},
		{"broken pipe mid-request", handshake(&url.Error{Op: "Post", URL: "https://mcp.vendor.example", Err: syscall.EPIPE}), true},
		{"pre-header connection close (url.Error wrapping EOF)", handshake(&url.Error{Op: "Post", URL: "https://mcp.vendor.example", Err: io.EOF}), true},
		{"truncated response body (unexpected EOF)", handshake(fmt.Errorf("SSE read error: %w", io.ErrUnexpectedEOF)), true},
		{"HTTP/2 stream reset", handshake(errors.New("stream error: stream ID 1; INTERNAL_ERROR; received from peer")), true},
		{"HTTP/2 GOAWAY", handshake(errors.New("http2: server sent GOAWAY and closed the connection; LastStreamID=0")), true},
		{"clean HTTP 200 with empty body (not an MCP server)", handshake(io.EOF), false},
		{"temporary DNS failure", dial(&net.DNSError{Err: "temporary failure in name resolution", Name: "mcp.vendor.example", IsTemporary: true}), true},
		{"host gone", dial(&net.DNSError{Err: "no such host", Name: "mcp.vendor.example", IsNotFound: true}), false},
		{"404", handshake(&mcp.HTTPStatusError{StatusCode: 404}), false},
		{"401 on an open entry", handshake(&mcp.HTTPStatusError{StatusCode: 401}), false},
		{"403 auth refusal without rate-limit wording", handshake(&mcp.HTTPStatusError{StatusCode: 403, Body: "Forbidden"}), false},
		{"not an MCP server", handshake(errors.New("MCP http: unparsable response")), false},
	}
	for _, c := range cases {
		if got := vendorOutage(c.err); got != c.want {
			t.Errorf("%s: vendorOutage = %v, want %v (err: %v)", c.name, got, c.want, c.err)
		}
	}
}

// TestFixtureKeyRefused pins which failures of an api_key fixture's real-key
// probe read as the fixture's secret going bad (a warning) rather than the
// listing being wrong. Only a credential refusal or exhausted account plan
// qualifies; a 404, a non-MCP reply or a dead host still fails the fixture.
func TestFixtureKeyRefused(t *testing.T) {
	handshake := func(err error) error {
		return fmt.Errorf("failed to initialize server: initialize call failed: %w", err)
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"401 at initialize (PagerDuty EU, #1776)", handshake(&mcp.HTTPStatusError{StatusCode: 401, Body: `{"error":"unauthorized","error_description":"No permission -- see authorization schemes"}`}), true},
		{"402 free-tier credits exhausted", handshake(&mcp.HTTPStatusError{StatusCode: 402, Body: `{"error":"Payment Required: monthly credit limit reached"}`}), true},
		{"403 at initialize", handshake(&mcp.HTTPStatusError{StatusCode: 403}), true},
		{"JSON-RPC error on a 401", handshake(&mcp.RPCError{Code: -32001, Message: "Unauthorized", HTTPStatus: 401}), true},
		{"JSON-RPC insufficient credits on a 2xx", handshake(&mcp.RPCError{Code: -32000, Message: "Insufficient credits remaining on your plan"}), true},
		{"unattributed JSON-RPC error on a 403", handshake(&mcp.UnattributedResponseError{ResponseID: "null", WantID: 1, Raw: `{"code":-32001,"message":"Forbidden"}`, HTTPStatus: 403}), true},
		{"key rejected at the verification call", &keyRejectedError{tool: "list_things", reason: "invalid api key"}, true},
		{"404", handshake(&mcp.HTTPStatusError{StatusCode: 404}), false},
		{"503", handshake(&mcp.HTTPStatusError{StatusCode: 503}), false},
		{"JSON-RPC error on a 2xx", handshake(&mcp.RPCError{Code: -32603, Message: "Internal error"}), false},
		{"not an MCP server", handshake(errors.New("MCP http: unparsable response")), false},
	}
	for _, c := range cases {
		if got := fixtureKeyRefused(c.err); got != c.want {
			t.Errorf("%s: fixtureKeyRefused = %v, want %v (err: %v)", c.name, got, c.want, c.err)
		}
	}
}

func TestProbeTimeoutBudgetCoversMultiStepHandshake(t *testing.T) {
	const perReq = 30 * time.Second
	openBudget := probeTimeoutBudget(perReq, "")
	if want := 2*perReq + mcp.MaxConnectRetryBudget; openBudget != want {
		t.Errorf("unauthenticated probeTimeoutBudget = %s, want %s", openBudget, want)
	}
	keyBudget := probeTimeoutBudget(perReq, "secret-key")
	if want := 6*perReq + mcp.MaxConnectRetryBudget; keyBudget != want {
		t.Errorf("credentialed probeTimeoutBudget = %s, want %s", keyBudget, want)
	}

	// Regression test: when initialize and tools/list each finish within
	// HTTPTimeout (80ms < 120ms) while their sum exceeds one HTTPTimeout
	// (160ms > 120ms), both probeServer's inner handshake budget and
	// probeForTest's outer budget must let the multi-step handshake finish.
	const reqTimeout = 120 * time.Millisecond
	const stepDelay = 80 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			time.Sleep(stepDelay)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"protocolVersion": "2024-11-05",
					"capabilities":    map[string]any{},
					"serverInfo":      map[string]any{"name": "slow-open", "version": "1.0"},
				},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			time.Sleep(stepDelay)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"tools": []map[string]any{{
						"name":        "get_status",
						"description": "status",
						"inputSchema": map[string]any{"type": "object"},
					}},
				},
			})
		default:
			time.Sleep(stepDelay)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": "ok"}},
				},
			})
		}
	}))
	defer srv.Close()

	hc := srv.Client()
	hc.Timeout = reqTimeout
	svc := &Service{cfg: Config{HTTPTimeout: reqTimeout}, httpClient: hc}

	report, err := svc.probeForTest(srv.URL, "", "", "", "")
	if err != nil {
		t.Fatalf("open probeForTest failed when each request stayed within HTTPTimeout: %v", err)
	}
	if report.ToolCount != 1 {
		t.Fatalf("open probeForTest ToolCount = %d, want 1", report.ToolCount)
	}

	keyReport, err := svc.probeForTest(srv.URL, "", "", "", "real-key")
	if err != nil {
		t.Fatalf("credentialed probeForTest failed when each request stayed within HTTPTimeout: %v", err)
	}
	if keyReport.ToolCount != 1 {
		t.Fatalf("credentialed probeForTest ToolCount = %d, want 1", keyReport.ToolCount)
	}
}

// TestProbeTrouble pins which probe results earn a confirmation probe: every
// result the live smoke would fail an entry on, so a transient one cannot
// file the nightly alarm on its first try. A rewritten (older-draft) schema
// only logs, so it is not trouble.
func TestProbeTrouble(t *testing.T) {
	cases := []struct {
		name   string
		report ProbeReport
		err    error
		want   bool
	}{
		{"healthy", ProbeReport{ToolCount: 3}, nil, false},
		{"rewritten schema", ProbeReport{ToolCount: 1, SchemaIssues: []agentcore.ToolSchemaIssue{{Tool: "search", Status: agentcore.ToolSchemaRewritten}}}, nil, false},
		{"error", ProbeReport{}, errors.New("MCP http: unparsable response"), true},
		{"no tools", ProbeReport{}, nil, true},
		{"invalid schema", ProbeReport{ToolCount: 1, SchemaIssues: []agentcore.ToolSchemaIssue{{Tool: "search", Status: agentcore.ToolSchemaInvalid}}}, nil, true},
	}
	for _, c := range cases {
		if got := probeTrouble(c.report, c.err) != ""; got != c.want {
			t.Errorf("%s: probeTrouble != \"\" is %v, want %v", c.name, got, c.want)
		}
	}
}
