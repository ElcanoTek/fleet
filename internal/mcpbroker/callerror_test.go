// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package mcpbroker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// TestDescribeCallError pins which failed tools/call answers cross the
// credential boundary and how (F8): a vendor's 4xx or JSON-RPC answer,
// bounded and scrubbed; a 401 as a value-free credential rejection; and
// everything operational still as the masked text.
func TestDescribeCallError(t *testing.T) {
	// Scoped so the literal retires after the test instead of living in the
	// process-wide redactor for every later test.
	RegisterSecretLiterals("test:describe-call-error", true, "registered-fake-secret-SECRETVALUE-8f3a1c")
	t.Cleanup(func() { RegisterSecretLiterals("test:describe-call-error", true, "retired-placeholder-value") })
	hosted := func(err error) error { return &HostedCallError{Err: err} }
	cases := []struct {
		name string
		err  error
		want string // substring the peer must see
		bad  string // substring the peer must never see
	}{
		{"stripe 422 names the argument", hosted(&mcp.HTTPStatusError{StatusCode: 422, Body: `{"error":{"message":"Missing required parameter: stripe_context"}}`}),
			"the server answered HTTP 422 Unprocessable Entity: {\"error\":{\"message\":\"Missing required parameter: stripe_context\"}}", ""},
		{"wrapped 404", hosted(fmt.Errorf("call: %w", &mcp.HTTPStatusError{StatusCode: 404, Body: "no such route"})), "HTTP 404 Not Found: no such route", "credential-owner"},
		{"429 with no body", hosted(&mcp.HTTPStatusError{StatusCode: 429}), "the server answered HTTP 429 Too Many Requests", ""},
		{"401 stays value-free", hosted(&mcp.HTTPStatusError{StatusCode: 401, Body: "unauthorized: token registered-fake-secret-SECRETVALUE-8f3a1c revoked"}), "rejected the stored credential (HTTP 401)", "SECRETVALUE"},
		{"403 is passed as what the vendor said, not as a credential problem", hosted(&mcp.HTTPStatusError{StatusCode: 403, Body: "The caller does not have permission"}), "HTTP 403 Forbidden: The caller does not have permission", "reconnecting"},
		{"5xx stays masked", hosted(&mcp.HTTPStatusError{StatusCode: 502, Body: "upstream at https://internal.vendor:8443 down"}), errBrokerCallFailed, "internal.vendor"},
		{"json-rpc invalid params", hosted(&mcp.RPCError{Code: -32602, Message: "Invalid params: 'query' is required"}), "the server returned error -32602: Invalid params: 'query' is required", "credential-owner"},
		{"json-rpc without a code", hosted(&mcp.RPCError{Message: "tool not found"}), "the server returned an error: tool not found", ""},
		{"registered secret scrubbed from a 4xx body", hosted(&mcp.HTTPStatusError{StatusCode: 400, Body: "bad request for registered-fake-secret-SECRETVALUE-8f3a1c"}), "HTTP 400 Bad Request: bad request for ", "SECRETVALUE"},
		{"echoed query credential masked by shape", hosted(&mcp.HTTPStatusError{StatusCode: 404, Body: "no route for /mcp?user_api_key=ab%2Bcd7&x=1"}), "HTTP 404 Not Found: no route for /mcp?user_api_key=[", "ab%2Bcd7"},
		{"echoed bearer masked by shape", hosted(&mcp.HTTPStatusError{StatusCode: 400, Body: "rejected Authorization: Bearer shortkey12 for this route"}), "Bearer [", "shortkey12"},
		{"control characters and a long body are tamed", hosted(&mcp.HTTPStatusError{StatusCode: 422, Body: "\x1b[31mred\x1b[0m " + strings.Repeat("x", 600)}), "HTTP 422 Unprocessable Entity: [31mred [0m " + strings.Repeat("x", 100), "\x1b"},
		{"a bundle server's 4xx stays masked (no hosted marker)", &mcp.HTTPStatusError{StatusCode: 422, Body: "internal detail https://10.0.0.9/"}, errBrokerCallFailed, "10.0.0.9"},
		{"a bundle server's json-rpc traceback stays masked", &mcp.RPCError{Code: -32603, Message: "Internal error: Traceback … /srv/bundle/mcp/server.py"}, errBrokerCallFailed, "Traceback"},
		{"transport error stays masked", errors.New("dial tcp 10.0.0.5:443: i/o timeout"), errBrokerCallFailed, "10.0.0.5"},
		{"context error stays masked", context.DeadlineExceeded, errBrokerCallFailed, "deadline"},
	}
	for _, c := range cases {
		got := describeCallError(c.err)
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: describeCallError = %q, want it to contain %q", c.name, got, c.want)
		}
		if c.bad != "" && strings.Contains(got, c.bad) {
			t.Errorf("%s: describeCallError = %q leaked %q", c.name, got, c.bad)
		}
		if len(got) > 400 {
			t.Errorf("%s: description is %d bytes; want bounded", c.name, len(got))
		}
	}
}

// TestClientServer_VendorAnswerCrossesTheWire: end to end through the
// loopback pair, a Stripe-shaped 422 reaches the peer as the vendor's own
// words while a 5xx and a transport error still arrive masked. The host log
// keeps the full detail either way.
func TestClientServer_VendorAnswerCrossesTheWire(t *testing.T) {
	logged := captureLog(t)
	fake := &fakeBroker{err: &HostedCallError{Err: &mcp.HTTPStatusError{StatusCode: 422, Body: `{"error":{"message":"Missing required parameter: livemode"}}`}}}
	client := loopback(t, fake)

	_, _, err := client.CallMCP(context.Background(), "stripe", "stripe_api_read", nil)
	if err == nil || !strings.Contains(err.Error(), "HTTP 422") || !strings.Contains(err.Error(), "livemode") {
		t.Fatalf("CallMCP err = %v; want the vendor's 422 and its argument name", err)
	}
	if strings.Contains(err.Error(), errBrokerCallFailed) {
		t.Errorf("a 4xx answer must not be masked: %v", err)
	}
	if out := logged(); !strings.Contains(out, "stripe.stripe_api_read") || !strings.Contains(out, "livemode") || !strings.Contains(out, "passed to the caller as") {
		t.Errorf("host log should still carry the full detail and say it was passed:\n%s", out)
	}

	fake = &fakeBroker{err: &HostedCallError{Err: &mcp.HTTPStatusError{StatusCode: 503, Body: "internal upstream https://10.1.2.3/ unavailable"}}}
	client = loopback(t, fake)
	if _, _, err := client.CallMCP(context.Background(), "s", "t", nil); err == nil || err.Error() != errBrokerCallFailed {
		t.Fatalf("5xx err = %v, want the masked %q", err, errBrokerCallFailed)
	}
}
