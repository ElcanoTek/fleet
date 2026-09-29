// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package mcpbroker

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

// HostedCallError marks a tools/call failure as a HOSTED (per-user remote)
// server's answer. The credential-owning child wraps the error of every call
// it routes through a remote scope in it; describeCallError lets a vendor's
// answer cross the boundary only when this marker is present. A bundle
// server's error — a stdio connector's traceback, a bundle HTTP server's
// upstream URL — is operational detail from inside the deployment and stays
// masked as it always has.
type HostedCallError struct{ Err error }

func (e *HostedCallError) Error() string { return e.Err.Error() }
func (e *HostedCallError) Unwrap() error { return e.Err }

// describeCallError decides what a failed tools/call tells the peer.
//
// The rule used to be "nothing": every backend error crossed the credential
// boundary as errBrokerCallFailed, because an operational error can embed
// connector stderr, URLs, headers or provider detail. It still does for
// everything that could — a 5xx, a transport failure, a context error, an
// unknown error type, and every error of a bundle server. But a hosted
// vendor's own ANSWER to a request is not operational detail; it is the
// same class of text as a tool result, which crosses the boundary today.
// Masking it had a real cost: Stripe refused a call with HTTP 422 "Missing
// required parameter: stripe_context", the model saw only "credential-owner
// call failed", read that as a dead credential and sent the user to
// reconnect a working connection (F8, #1006).
//
// So for a hosted server three classes cross, each bounded and scrubbed by
// sanitizeVendorText (ADR-0075):
//
//   - an HTTP 4xx other than 401 (400, 403, 404, 405, 409, 422, 429, …): the
//     status and the vendor's first line, which names the argument, the
//     permission or the limit the model can act on. A 403 is in this class
//     on purpose: it is routinely a scope or plan refusal that a reconnect
//     would not change, so it is passed as what the vendor said rather than
//     re-labelled as a credential problem;
//   - a JSON-RPC error object (the vendor's structured "invalid params",
//     "unknown tool", "rate limited"): its code and message;
//   - an HTTP 401: the status alone, worded as a credential rejection — the
//     body of a refused request is the one answer that can quote the request
//     back, so it stays host-side.
//
// The masked text keeps its exact wording so the existing tests and the
// runbook's "the real text is on this line" still hold for the masked
// classes.
func describeCallError(err error) string {
	var hosted *HostedCallError
	if !errors.As(err, &hosted) {
		return errBrokerCallFailed
	}
	var hs *mcp.HTTPStatusError
	if errors.As(err, &hs) {
		switch {
		case hs.StatusCode == http.StatusUnauthorized:
			return "mcpbroker: the server rejected the stored credential (HTTP 401) — the connection needs reconnecting under Settings → Connections"
		case hs.StatusCode >= 400 && hs.StatusCode < 500:
			body := sanitizeVendorText(hs.Body)
			if body == "" {
				return fmt.Sprintf("mcpbroker: the server answered HTTP %d %s", hs.StatusCode, http.StatusText(hs.StatusCode))
			}
			return fmt.Sprintf("mcpbroker: the server answered HTTP %d %s: %s", hs.StatusCode, http.StatusText(hs.StatusCode), body)
		}
		return errBrokerCallFailed
	}
	var rpc *mcp.RPCError
	if errors.As(err, &rpc) {
		msg := sanitizeVendorText(rpc.Message)
		if msg == "" {
			msg = "(no message)"
		}
		if rpc.Code != 0 {
			return fmt.Sprintf("mcpbroker: the server returned error %d: %s", rpc.Code, msg)
		}
		return "mcpbroker: the server returned an error: " + msg
	}
	return errBrokerCallFailed
}

// Credential carriers a vendor can echo back verbatim in an error body — a
// request URI with the key as a query parameter, an Authorization header —
// masked by shape, because the literal redactors know a credential only in
// its raw spelling and only when it is 8 bytes or longer (internal/redact).
var (
	echoedQueryCredential = regexp.MustCompile(`(?i)([?&](?:api[_-]?key|apikey|key|token|access[_-]?token|auth[_-]?token|secret|password|passwd|pwd|sig|signature)=)[^&\s"'<>]+`)
	echoedAuthScheme      = regexp.MustCompile(`(?i)\b(bearer|basic|token)\s+[A-Za-z0-9._~+/=-]{8,}`)
)

// sanitizeVendorText makes a vendor's error line safe to hand to the peer:
// credential carriers masked by shape (echoedQueryCredential,
// echoedAuthScheme), then scrubbed by both process-wide redactors (this process's, which holds the
// connector credentials it acquired, and agentcore's, for the env literals
// and the canonical token shapes), stripped of control characters so an
// escape sequence cannot repaint a terminal that echoes it, whitespace-
// collapsed, redacted once more in case a token shape only lines up after
// the collapse, and bounded on a rune boundary. The same order as
// internal/agent's connectFailureReason, for the same reasons.
func sanitizeVendorText(s string) string {
	const maxOut = 240
	if len(s) > 8<<10 {
		s = s[:8<<10]
	}
	s = echoedQueryCredential.ReplaceAllString(s, "${1}[redacted]")
	s = echoedAuthScheme.ReplaceAllString(s, "${1} [redacted]")
	s = RedactSecrets(agentcore.RedactSecrets(s))
	s = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return ' '
	}, s)
	s = RedactSecrets(agentcore.RedactSecrets(strings.Join(strings.Fields(s), " ")))
	if len(s) > maxOut {
		n := maxOut
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		s = s[:n] + "…"
	}
	return s
}
