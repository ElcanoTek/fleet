// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package remotemcp

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// TestVendorOutageSeparatesOutagesFromRot pins which live-smoke failures only
// warn (the vendor is down tonight) and which fail the nightly lane (the
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
		{"unattributed rate-limit JSON-RPC error on a 2xx", handshake(&mcp.UnattributedResponseError{ResponseID: "null", WantID: 1, Raw: `{"code":-32000,"message":"Rate-limited, slow down"}`, Carried: &mcp.RPCError{Code: -32000, Message: "Rate-limited, slow down"}}), true},
		{"key-required JSON-RPC error on a 2xx (an open entry now wants a key)", handshake(&mcp.RPCError{Code: -32001, Message: "An API key is required to use this server"}), false},
		{"unattributed JSON-RPC error on a 503", handshake(&mcp.UnattributedResponseError{ResponseID: "null", WantID: 1, Raw: `{"code":-32603,"message":"Internal error"}`, HTTPStatus: 503}), true},
		{"unattributed JSON-RPC error on a 401", handshake(&mcp.UnattributedResponseError{ResponseID: "null", WantID: 1, Raw: `{"code":-32001,"message":"Unauthorized"}`, HTTPStatus: 401}), false},
		{"connection refused", dial(os.NewSyscallError("connect", syscall.ECONNREFUSED)), true},
		{"temporary DNS failure", dial(&net.DNSError{Err: "temporary failure in name resolution", Name: "mcp.vendor.example", IsTemporary: true}), true},
		{"host gone", dial(&net.DNSError{Err: "no such host", Name: "mcp.vendor.example", IsNotFound: true}), false},
		{"404", handshake(&mcp.HTTPStatusError{StatusCode: 404}), false},
		{"401 on an open entry", handshake(&mcp.HTTPStatusError{StatusCode: 401}), false},
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
// listing being wrong. Only a credential refusal qualifies; a 404, a non-MCP
// reply or a dead host still fails the fixture.
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
		{"403 at initialize", handshake(&mcp.HTTPStatusError{StatusCode: 403}), true},
		{"JSON-RPC error on a 401", handshake(&mcp.RPCError{Code: -32001, Message: "Unauthorized", HTTPStatus: 401}), true},
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
