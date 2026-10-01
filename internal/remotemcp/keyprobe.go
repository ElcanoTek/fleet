// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package remotemcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

// ProbeReport is what the add-time validation of an open or api_key
// connection learned. ToolCount is the size of the server's tool list.
// KeyVerified says whether the credential was PROVEN, not merely carried
// through a handshake the vendor may never have checked. Roughly half of
// the hosted api_key vendors in the built-in directory answer initialize
// and tools/list to any bearer and check the key only at the first
// tools/call (F14 in docs/MCP-CATALOG-STATUS.md), so before this report a
// mistyped key was stored as "connected, N tools" and failed in the user's
// first turn, far from the field they could fix.
//
// The probe therefore does two more things after the handshake. It makes
// one read-only tool call with no arguments using the real key: a DEFINITE
// credential rejection there (an HTTP 401, or an error that names the key
// or credential as invalid or missing) fails the add. Then it repeats the
// handshake and the same call with a deliberately invalid key and compares
// outcomes: if the vendor answered the invalid key differently — refused
// the handshake it had just accepted, or answered the call with an error
// where the real key got something else — it checks keys somewhere fleet
// can see, so the real key's pass meant something and KeyVerified is true.
// If both keys got the same answer, nothing observed proves the real key
// (the vendor validates arguments before it authenticates, the tool needs
// no key, or the key is valid but not allowed that tool), and KeyVerified
// is false so the caller can say the key could not be verified now instead
// of implying a check that did not happen. A live measurement of both is
// TestAPIKeyProbeRefusesBogusKeyLive. Open connections carry no credential;
// KeyVerified is always false for them and means nothing.
type ProbeReport struct {
	ToolCount   int
	KeyVerified bool
	// CheckedWith names the read-only tool the verification call used, or
	// "" when the server offered none the probe could call blind.
	CheckedWith string
	// SchemaIssues lists tools whose input schema the model boundary would
	// translate or withhold (agentcore.CheckMCPToolSchema). Informational: it
	// never fails a probe, since the connection itself is sound.
	SchemaIssues []agentcore.ToolSchemaIssue
}

// noProbe is the report for a connection that was not probed (an OAuth add,
// which proves itself at the consent screen). ToolCount -1 is the sentinel
// the HTTP layer keys on.
var noProbe = ProbeReport{ToolCount: -1}

// keyRejectedError is the vendor refusing the credential at the verification
// call. It wraps the vendor's own wording so the user sees why.
type keyRejectedError struct {
	tool   string
	reason string
}

func (e *keyRejectedError) Error() string {
	return fmt.Sprintf("the server accepted the handshake but rejected the key on its first tool call (%s): %s", e.tool, e.reason)
}

// Verbs that, as a whole token of the tool name, mark a call as reading
// (candidate) or as something the probe must never do blind. Matched on
// tokens, not substrings, so `get_payment` is a read and `search_posts` is
// not a write. The unsafe list is deliberately long and includes the
// conjunctions (`search_and_replace`, `read_then_ack`) and the particles of
// `check_in`/`check_out`: a false "read" here is a write on the user's real
// account, a false "write" only costs a verification.
var (
	probeReadVerbs   = tokenSet("list", "get", "search", "find", "describe", "read", "fetch", "lookup", "query", "show", "status", "ping", "whoami", "me", "retrieve", "browse", "view", "check", "count", "info", "health")
	probeUnsafeVerbs = tokenSet(
		"and", "then", "in", "out",
		"delete", "remove", "create", "update", "send", "write", "post", "put", "set", "add", "execute", "run", "start", "stop",
		"purchase", "pay", "transfer", "refund", "cancel", "deploy", "trigger", "upload", "insert", "drop", "reset", "revoke",
		"restart", "kill", "terminate", "modify", "patch", "change", "assign", "invite", "publish", "schedule", "submit", "edit",
		"move", "archive", "import", "approve", "reject", "charge", "book", "order", "buy", "sell",
		"replace", "clear", "mark", "purge", "destroy", "rename", "merge", "rotate", "generate", "sync", "push", "apply", "restore",
		"close", "enable", "disable", "block", "unblock", "login", "logout", "ack", "acknowledge", "resolve", "snooze", "dismiss",
		"complete", "finish", "mutate", "toggle", "flag", "unflag", "star", "unstar", "follow", "unfollow", "subscribe", "unsubscribe",
		"like", "unlike", "vote", "claim", "release", "lock", "unlock", "grant", "deny", "ban", "kick", "mute", "unmute", "pin", "unpin",
		"react", "reply", "comment", "forward", "share", "tag", "untag", "label", "attach", "detach", "link", "unlink", "save", "store",
		"record", "log", "track", "register", "unregister", "provision", "deprovision", "scale", "resize", "migrate", "backup", "redeploy",
		"checkin", "checkout", "process", "convert", "transform", "compute", "calculate", "index", "crawl", "scrape", "extract", "parse",
	)
)

func tokenSet(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

// pickProbeTool chooses the one tool the verification call may use, or ""
// when none is safe to call blind. Preference: a tool the server itself
// marks read-only (and not destructive), then a tool whose name has a read
// verb and no unsafe verb; within a tier, a tool that requires no arguments
// first, then alphabetical, so the choice is stable across runs. A tool the
// server marks destructive, or explicitly NOT read-only, is never chosen
// whatever its name.
func pickProbeTool(tools []mcp.Tool) string {
	type cand struct {
		name     string
		tier     int
		required int
	}
	var cands []cand
	for _, t := range tools {
		if t.Annotations != nil {
			if t.Annotations.DestructiveHint != nil && *t.Annotations.DestructiveHint {
				continue
			}
			if t.Annotations.ReadOnlyHint != nil && !*t.Annotations.ReadOnlyHint {
				continue
			}
		}
		tier := 0
		switch {
		case t.Annotations != nil && t.Annotations.ReadOnlyHint != nil && *t.Annotations.ReadOnlyHint:
			tier = 1
		case nameReadsOnly(t.Name):
			tier = 2
		default:
			continue
		}
		cands = append(cands, cand{name: t.Name, tier: tier, required: requiredArgCount(t)})
	}
	if len(cands) == 0 {
		return ""
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].tier != cands[j].tier {
			return cands[i].tier < cands[j].tier
		}
		if (cands[i].required == 0) != (cands[j].required == 0) {
			return cands[i].required == 0
		}
		return cands[i].name < cands[j].name
	})
	return cands[0].name
}

// nameReadsOnly reports whether a tool name reads as a read: at least one
// read verb among its tokens and no unsafe verb.
func nameReadsOnly(name string) bool {
	tokens := strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return r == '_' || r == '-' || r == '.' || r == ':' || r == '/'
	})
	read := false
	for _, tok := range tokens {
		if probeUnsafeVerbs[tok] {
			return false
		}
		if probeReadVerbs[tok] {
			read = true
		}
	}
	return read
}

func requiredArgCount(t mcp.Tool) int {
	switch req := t.InputSchema["required"].(type) {
	case []any:
		return len(req)
	case []string:
		return len(req)
	}
	return 0
}

// callKind classifies what a tools/call came back as.
type callKind int

const (
	callOK          callKind = iota // a result without isError
	callResultError                 // a result with isError set
	callRPCError                    // a JSON-RPC error object
	callHTTPError                   // a non-2xx HTTP status
	callTransport                   // no answer: timeout, connection, context
)

// callOutcome is one tools/call reduced to what the comparison needs.
type callOutcome struct {
	kind   callKind
	status int    // HTTP status for callHTTPError
	code   int    // JSON-RPC code for callRPCError
	text   string // the message or the result text, whitespace-collapsed
}

func classifyCall(result *mcp.ToolResult, err error) callOutcome {
	if err != nil {
		var hs *mcp.HTTPStatusError
		if errors.As(err, &hs) {
			return callOutcome{kind: callHTTPError, status: hs.StatusCode, text: collapse(hs.Body)}
		}
		var rpc *mcp.RPCError
		if errors.As(err, &rpc) {
			return callOutcome{kind: callRPCError, code: rpc.Code, text: collapse(rpc.Message)}
		}
		return callOutcome{kind: callTransport, text: collapse(err.Error())}
	}
	var text strings.Builder
	if result != nil {
		for _, c := range result.Content {
			text.WriteString(c.Text)
			text.WriteString(" ")
		}
	}
	if result != nil && result.IsError {
		return callOutcome{kind: callResultError, text: collapse(text.String())}
	}
	return callOutcome{kind: callOK, text: collapse(text.String())}
}

func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// keyRejectWording is what a vendor says when it means THE KEY, as opposed to
// the argument-validation errors a blind call with no arguments provokes.
// The nouns are deliberately specific (an "api key", an "access token", a
// "bearer token", "credentials") — a bare "token" would match a pagination
// argument ("page_token is required"), and a bare "invalid token" a query
// parser — because on the real key's call a match is fatal to the add.
var keyRejectWording = regexp.MustCompile(`(?i)(` +
	`\bunauthori[sz]ed\b|not authenticated|authentication (required|failed|error)|invalid_authori[sz]ation|token exchange failed` +
	`|(invalid|incorrect|bad|malformed|unknown|wrong|expired|revoked|missing|no) (api[ _-]?key|access[ _-]?token|auth(entication|orization)?[ _-]?token|bearer[ _-]?token|api[ _-]?token|credentials?)\b` +
	`|(api[ _-]?key|access[ _-]?token|bearer[ _-]?token|api[ _-]?token|credentials?) (is |was |are )?(invalid|missing|required|not found|expired|revoked|incorrect|unknown|wrong|not valid)` +
	`|api[ _-]?key (must|should)\b|api[ _-]?key not valid` +
	`)`)

// isDefiniteKeyRejection is the rule for the REAL key's verification call:
// fatal only on evidence that names the credential. An HTTP 401 is that by
// definition. An HTTP 403, "forbidden" or "access denied" is not — a valid
// key can be denied one tool by scope, and refusing the whole connection
// over it would strand a user who was fine before this check existed. A
// JSON-RPC code on its own is not either (-32001, which several vendors use
// for "authenticate first", is also the SDK's request-timeout code); the
// wording decides.
func isDefiniteKeyRejection(o callOutcome) bool {
	switch o.kind {
	case callHTTPError:
		return o.status == http.StatusUnauthorized || keyRejectWording.MatchString(o.text)
	case callRPCError, callResultError:
		return keyRejectWording.MatchString(o.text)
	case callOK, callTransport:
		return false
	}
	return false
}

// outcomesDiffer reports whether the vendor answered the invalid key
// differently from the real one at the same call. Same kind, same status or
// code and the same text is "the same answer" — the vendor did not
// distinguish the keys (arguments validated first, a keyless tool, a scope
// denial both keys share). A transport failure on the control says nothing.
func outcomesDiffer(actual, control callOutcome) bool {
	if control.kind == callTransport {
		return false
	}
	if control.kind == callOK {
		// The invalid key got a plain success: the tool needs no key.
		return false
	}
	if actual.kind != control.kind || actual.status != control.status || actual.code != control.code {
		return true
	}
	return actual.text != control.text
}

// handshakeRefused reports whether a handshake error is the server answering
// (an HTTP status, a JSON-RPC error) rather than not being reachable. The
// real key passed this same handshake moments earlier, so any answer that
// turns the invalid key away is the vendor distinguishing them.
func handshakeRefused(err error) bool {
	if err == nil {
		return false
	}
	var hs *mcp.HTTPStatusError
	var rpc *mcp.RPCError
	if errors.As(err, &hs) || errors.As(err, &rpc) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return false
	}
	// The transport reports a JSON-RPC error reply that arrived under the
	// wrong id as a plain error mentioning it; that is still the server
	// answering. Anything else unknown is treated as not reachable.
	return strings.Contains(err.Error(), "response carried error")
}

// probeTool picks the verification tool from a connected client, or "".
func probeTool(client *mcp.Client) string {
	all := client.GetAllTools()
	tools := make([]mcp.Tool, 0, len(all))
	for _, st := range all {
		tools = append(tools, st.Tool)
	}
	return pickProbeTool(tools)
}

// invalidProbeKey is the deliberately wrong credential the control probe
// sends. It must never collide with a real key; the shape is chosen so no
// vendor's format check could accept it as one of theirs.
const invalidProbeKey = "fleet-invalid-key-probe-0000" // gitleaks:allow — a deliberately INVALID credential the control probe sends; not a secret (generic-api-key false positive)

// keyClientOptions attaches a credential the way probeServer does: under the
// named header with the entry's scheme prefix in front ("Token token=<key>"),
// under the default Authorization: Bearer when no header is named, or as a
// query parameter — the same three shapes the per-run overlay mounts.
func (s *Service) keyClientOptions(headerName, queryName, prefix, credential string) mcp.HTTPServerOptions {
	opts := mcp.HTTPServerOptions{HTTPClient: s.httpClient}
	switch {
	case credential != "" && queryName != "":
		opts.HTTPClient = mcp.WithQueryParam(s.httpClient, queryName, credential)
	case credential != "":
		header, value := "Authorization", "Bearer "+credential
		if headerName != "" {
			header, value = headerName, prefix+credential
		}
		opts.Headers = map[string]string{header: value}
	}
	return opts
}

// blindCall makes the one read-only call with no arguments under its own
// timeout and classifies the answer.
func (s *Service) blindCall(ctx context.Context, client *mcp.Client, serverName, tool string) callOutcome {
	cctx, cancel := context.WithTimeout(ctx, s.cfg.HTTPTimeout)
	defer cancel()
	result, err := client.CallToolOn(cctx, serverName, tool, map[string]any{})
	return classifyCall(result, err)
}

// controlProbe repeats the handshake and, if the vendor lets it through, the
// blind call, with invalidProbeKey attached exactly as the real key was, and
// reports whether the vendor told the two keys apart. It runs under its own
// timeout so a slow vendor cannot eat the real probe's budget, and one
// invalid attempt per add or rotation is all it ever sends.
func (s *Service) controlProbe(ctx context.Context, url, headerName, queryName, prefix, tool string, actual callOutcome) bool {
	cctx, cancel := context.WithTimeout(ctx, s.cfg.HTTPTimeout)
	defer cancel()
	client := mcp.NewClient()
	defer func() { _ = client.Close() }()
	if err := client.AddHTTPServerWithOptions(cctx, "control", url, s.keyClientOptions(headerName, queryName, prefix, invalidProbeKey)); err != nil {
		return handshakeRefused(err)
	}
	if tool == "" {
		return false
	}
	return outcomesDiffer(actual, s.blindCall(cctx, client, "control", tool))
}

func truncateReason(s string) string {
	s = collapse(s)
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// probeTimeout is the budget for the real handshake alone; the verification
// call and the control probe each get their own s.cfg.HTTPTimeout.
func probeTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}
