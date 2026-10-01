// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package remotemcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

func boolp(b bool) *bool { return &b }

// keyProbeVendor is a streamable-HTTP MCP server shaped like the vendors F14
// found: initialize and tools/list answer ANY key, and only tools/call
// checks it. What tools/call answers is scripted per test; every call is
// recorded so a test can assert which tool the probe chose (and that none
// was chosen).
type keyProbeVendor struct {
	t        *testing.T
	tools    []map[string]any
	validKey string
	// onCall answers a tools/call for the named tool with the given key
	// already accepted; nil answers an empty successful result.
	onCall func(tool string) (result map[string]any, rpcErr map[string]any)
	// refuse is the reply to a wrong key at tools/call.
	refuse func(w http.ResponseWriter, id int)
	// onAnyCall, when set, answers every tools/call the same way whatever the
	// key — the shape of a scope denial that hits valid and invalid keys alike.
	onAnyCall func(w http.ResponseWriter, id int)

	mu    sync.Mutex
	calls []string
}

func (v *keyProbeVendor) called() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.calls...)
}

func (v *keyProbeVendor) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     *int           `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": *req.ID}
		switch req.Method {
		case "initialize":
			resp["result"] = map[string]any{"protocolVersion": "2025-06-18"}
		case "tools/list":
			resp["result"] = map[string]any{"tools": v.tools}
		case "tools/call":
			name, _ := req.Params["name"].(string)
			v.mu.Lock()
			v.calls = append(v.calls, name)
			v.mu.Unlock()
			if v.onAnyCall != nil {
				v.onAnyCall(w, *req.ID)
				return
			}
			if r.Header.Get("X-API-Key") != v.validKey {
				v.refuse(w, *req.ID)
				return
			}
			if v.onCall != nil {
				res, rpcErr := v.onCall(name)
				if rpcErr != nil {
					resp["error"] = rpcErr
				} else {
					resp["result"] = res
				}
			} else {
				resp["result"] = map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}}
			}
		default:
			resp["error"] = map[string]any{"code": -32601, "message": "method not found"}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func refuseWith401(w http.ResponseWriter, _ int) {
	http.Error(w, `{"error":"invalid_api_key"}`, http.StatusUnauthorized)
}

func refuseWithRPCError(w http.ResponseWriter, id int) {
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": -32001, "message": "Authentication required. Include 'Authorization: Bearer <token>' header."}})
}

func refuseWithIsError(w http.ResponseWriter, id int) {
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id,
		"result": map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": `{"error":{"code":1001,"message":"This API Key is invalid."}}`}}}})
}

func tool(name string, required []string, ann *mcp.ToolAnnotations) map[string]any {
	m := map[string]any{"name": name, "description": "t", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}}
	if required != nil {
		m["inputSchema"].(map[string]any)["required"] = required
	}
	if ann != nil {
		a := map[string]any{}
		if ann.ReadOnlyHint != nil {
			a["readOnlyHint"] = *ann.ReadOnlyHint
		}
		if ann.DestructiveHint != nil {
			a["destructiveHint"] = *ann.DestructiveHint
		}
		m["annotations"] = a
	}
	return m
}

// TestAddAPIKeyRejectedAtFirstToolCall is F14 itself: a vendor that answers
// the handshake to any bearer and refuses the key only at tools/call used to
// get a wrong key stored as "connected, N tools". Each refusal shape a real
// vendor uses — an HTTP 401, a JSON-RPC -32001, an isError result with the
// vendor's wording — must fail the add, store nothing, and name the tool.
func TestAddAPIKeyRejectedAtFirstToolCall(t *testing.T) {
	for name, refuse := range map[string]func(http.ResponseWriter, int){
		"http 401": refuseWith401, "jsonrpc -32001": refuseWithRPCError, "isError result": refuseWithIsError,
	} {
		t.Run(name, func(t *testing.T) {
			v := &keyProbeVendor{t: t, validKey: "sk-good", refuse: refuse, tools: []map[string]any{
				tool("create_thing", []string{"name"}, nil),
				tool("list_things", nil, nil),
			}}
			srv := v.server()
			defer srv.Close()
			fs := newFakeStore()
			svc := newTestService(t, fs, srv)
			_, _, err := svc.AddServer(context.Background(), AddServerInput{
				Email: "u@x.com", Name: "vendor", URL: srv.URL, AuthMode: "api_key", APIKey: "sk-wrong", APIKeyHeader: "X-API-Key",
			})
			if err == nil || !strings.Contains(err.Error(), "rejected the key on its first tool call (list_things)") {
				t.Fatalf("AddServer(wrong key, checked at call) = %v; want the refusal naming list_things", err)
			}
			var kr *keyRejectedError
			if !errors.As(err, &kr) {
				t.Errorf("error is not a keyRejectedError: %T", err)
			}
			if len(fs.servers) != 0 {
				t.Fatalf("a key refused at tools/call stored %d server(s)", len(fs.servers))
			}
			if got := v.called(); len(got) != 1 || got[0] != "list_things" {
				t.Errorf("probe called %v; want exactly [list_things] (the read tool, never create_thing)", got)
			}
		})
	}
}

// TestAddAPIKeyVerifiedByToolCall: with the right key the same vendor's add
// succeeds and the key is reported verified — whatever the blind call
// answers (success, an argument-validation error) — because the CONTROL
// probe with an invalid key is refused at tools/call, which proves this
// vendor checks keys where the probe can see. The invalid key must be sent
// under the same header the real one was.
func TestAddAPIKeyVerifiedByToolCall(t *testing.T) {
	for name, onCall := range map[string]func(string) (map[string]any, map[string]any){
		"call succeeds": nil,
		"invalid params": func(string) (map[string]any, map[string]any) {
			return nil, map[string]any{"code": -32602, "message": "Invalid params: 'query' is required"}
		},
		"isError validation": func(string) (map[string]any, map[string]any) {
			return map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": "query must not be empty"}}}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			v := &keyProbeVendor{t: t, validKey: "sk-good", refuse: refuseWith401, onCall: onCall, tools: []map[string]any{
				tool("search", []string{"query"}, nil),
				tool("create_search", nil, nil),
			}}
			srv := v.server()
			defer srv.Close()
			fs := newFakeStore()
			svc := newTestService(t, fs, srv)
			server, report, err := svc.AddServer(context.Background(), AddServerInput{
				Email: "u@x.com", Name: "vendor", URL: srv.URL, AuthMode: "api_key", APIKey: "sk-good", APIKeyHeader: "X-API-Key",
			})
			if err != nil {
				t.Fatalf("AddServer: %v", err)
			}
			if !report.KeyVerified || report.ToolCount != 2 || report.CheckedWith != "search" {
				t.Errorf("report = %+v; want verified via search with 2 tools", report)
			}
			// The real call, then the control call with the invalid key —
			// both on the read tool; create_search has a write verb and must
			// never be called.
			if got := v.called(); len(got) != 2 || got[0] != "search" || got[1] != "search" {
				t.Errorf("probe called %v; want [search search]", got)
			}
			if server == nil || len(fs.servers) != 1 {
				t.Errorf("server not stored")
			}
			// Rotation goes through the same verification.
			if rep, err := svc.SetAPIKey(context.Background(), "u@x.com", server.ID, "sk-wrong"); err == nil || !strings.Contains(err.Error(), "previous key is unchanged") {
				t.Errorf("SetAPIKey(wrong) = %+v, %v; want refusal", rep, err)
			}
			if rep, err := svc.SetAPIKey(context.Background(), "u@x.com", server.ID, "sk-good"); err != nil || !rep.KeyVerified {
				t.Errorf("SetAPIKey(good) = %+v, %v; want verified", rep, err)
			}
		})
	}
}

// TestAddAPIKeyUnverifiableWhenNoSafeTool: a vendor whose tools all look like
// writes (or are marked destructive) gives the probe nothing it may call
// blind, and this one answers the handshake to any key, so the control probe
// is not refused either. The add succeeds — refusing every such vendor would
// be worse than the old behaviour — but the report says the key is
// unverified, no tools/call is made, and the store keeps the row.
func TestAddAPIKeyUnverifiableWhenNoSafeTool(t *testing.T) {
	v := &keyProbeVendor{t: t, validKey: "sk-good", refuse: refuseWith401, tools: []map[string]any{
		tool("send_message", []string{"to"}, nil),
		tool("get_thing", nil, &mcp.ToolAnnotations{DestructiveHint: boolp(true)}),
		tool("transfer_funds", nil, &mcp.ToolAnnotations{ReadOnlyHint: boolp(false)}),
	}}
	srv := v.server()
	defer srv.Close()
	fs := newFakeStore()
	svc := newTestService(t, fs, srv)
	_, report, err := svc.AddServer(context.Background(), AddServerInput{
		Email: "u@x.com", Name: "vendor", URL: srv.URL, AuthMode: "api_key", APIKey: "sk-wrong", APIKeyHeader: "X-API-Key",
	})
	if err != nil {
		t.Fatalf("AddServer: %v (a vendor with no safe tool must still add)", err)
	}
	if report.KeyVerified || report.ToolCount != 3 {
		t.Errorf("report = %+v; want unverified with 3 tools", report)
	}
	if got := v.called(); len(got) != 0 {
		t.Errorf("probe called %v; want no call — nothing here is safe to call blind", got)
	}
	if len(fs.servers) != 1 {
		t.Errorf("stored %d servers, want 1", len(fs.servers))
	}
}

// TestAddOpenServerMakesNoToolCall: an open connection carries no key, so
// there is nothing to verify and the probe stays a handshake.
func TestAddOpenServerMakesNoToolCall(t *testing.T) {
	v := &keyProbeVendor{t: t, validKey: "", refuse: refuseWith401, tools: []map[string]any{tool("list_docs", nil, nil)}}
	srv := v.server()
	defer srv.Close()
	svc := newTestService(t, newFakeStore(), srv)
	_, report, err := svc.AddServer(context.Background(), AddServerInput{Email: "u@x.com", Name: "docs", URL: srv.URL, AuthMode: "open"})
	if err != nil {
		t.Fatalf("AddServer(open): %v", err)
	}
	if report.KeyVerified || report.ToolCount != 1 {
		t.Errorf("report = %+v", report)
	}
	if got := v.called(); len(got) != 0 {
		t.Errorf("open add called %v; want none", got)
	}
}

func TestPickProbeTool(t *testing.T) {
	cases := []struct {
		name  string
		tools []mcp.Tool
		want  string
	}{
		{"nothing safe", []mcp.Tool{{Name: "create_x"}, {Name: "send_email"}}, ""},
		{"read verb wins over nothing", []mcp.Tool{{Name: "create_x"}, {Name: "list_x"}}, "list_x"},
		{"token match, not substring", []mcp.Tool{{Name: "get_payment"}, {Name: "search_posts"}, {Name: "create_search"}}, "get_payment"},
		{"vendor prefixes and dashes", []mcp.Tool{{Name: "tavily-search"}, {Name: "tavily-extract"}}, "tavily-search"},
		{"read-only annotation beats a write-looking name", []mcp.Tool{{Name: "list_x"}, {Name: "run_report", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: boolp(true)}}}, "run_report"},
		{"destructive annotation is never chosen", []mcp.Tool{{Name: "list_x", Annotations: &mcp.ToolAnnotations{DestructiveHint: boolp(true)}}, {Name: "get_y"}}, "get_y"},
		{"no required args preferred", []mcp.Tool{{Name: "get_a", InputSchema: map[string]any{"required": []any{"id"}}}, {Name: "list_b"}}, "list_b"},
		{"alphabetical tie-break", []mcp.Tool{{Name: "list_z"}, {Name: "get_a"}}, "get_a"},
		{"conjunction hides a write", []mcp.Tool{{Name: "search_and_replace"}, {Name: "read_then_ack"}}, ""},
		{"check_in is not a read", []mcp.Tool{{Name: "check_in"}, {Name: "check-out"}}, ""},
		{"explicit readOnlyHint false is never chosen", []mcp.Tool{{Name: "fetch_and_process"}, {Name: "fetch_report", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: boolp(false)}}}, ""},
		{"scrape-style verbs are not reads", []mcp.Tool{{Name: "firecrawl_scrape"}, {Name: "firecrawl_search"}}, "firecrawl_search"},
	}
	for _, c := range cases {
		if got := pickProbeTool(c.tools); got != c.want {
			t.Errorf("%s: pickProbeTool = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestIsDefiniteKeyRejection(t *testing.T) {
	cases := []struct {
		name string
		out  callOutcome
		want bool
	}{
		{"http 401", callOutcome{kind: callHTTPError, status: 401}, true},
		{"http 403 plain (scope, not the key)", callOutcome{kind: callHTTPError, status: 403, text: "insufficient permissions"}, false},
		{"http 403 naming the key", callOutcome{kind: callHTTPError, status: 403, text: `{"error":"Invalid API key"}`}, true},
		{"http 500", callOutcome{kind: callHTTPError, status: 500}, false},
		{"rpc -32001 with auth wording", callOutcome{kind: callRPCError, code: -32001, text: "Authentication required. Include 'Authorization: Bearer <token>' header."}, true},
		{"rpc -32001 SDK timeout is not a rejection", callOutcome{kind: callRPCError, code: -32001, text: "Request timed out"}, false},
		{"rpc invalid params", callOutcome{kind: callRPCError, code: -32602, text: "Invalid params: query is required"}, false},
		{"pagination token is not the key", callOutcome{kind: callResultError, text: "page_token is required"}, false},
		{"query parser token is not the key", callOutcome{kind: callResultError, text: "Invalid token at position 3"}, false},
		{"echoed upstream status is not the key", callOutcome{kind: callResultError, text: "upstream returned status 401 for /v2/things"}, false},
		{"forbidden alone is not the key", callOutcome{kind: callResultError, text: "forbidden: this tool is not enabled for your plan"}, false},
		{"isError invalid key", callOutcome{kind: callResultError, text: `{"error":{"code":1001,"message":"This API Key is invalid."}}`}, true},
		{"isError key format", callOutcome{kind: callResultError, text: "API key must have 40+ characters, has 22."}, true},
		{"isError token exchange", callOutcome{kind: callResultError, text: "Token exchange failed"}, true},
		{"isError invalid_authorization", callOutcome{kind: callResultError, text: "Got error reason: 'invalid_authorization'"}, true},
		{"isError access token", callOutcome{kind: callResultError, text: "Vultr API returned 401: Invalid API token."}, true},
		{"isError validation", callOutcome{kind: callResultError, text: "query must not be empty"}, false},
		{"success mentioning unauthorized users", callOutcome{kind: callOK, text: "unauthorized users: 0"}, false},
		{"transport", callOutcome{kind: callTransport, text: "context deadline exceeded"}, false},
	}
	for _, c := range cases {
		if got := isDefiniteKeyRejection(c.out); got != c.want {
			t.Errorf("%s: isDefiniteKeyRejection = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestOutcomesDiffer(t *testing.T) {
	ok := callOutcome{kind: callOK, text: `["eu","us"]`}
	validation := callOutcome{kind: callResultError, text: "url is required"}
	scope := callOutcome{kind: callHTTPError, status: 403, text: "insufficient permissions"}
	refused := callOutcome{kind: callHTTPError, status: 401, text: `{"error":"invalid_api_key"}`}
	cases := []struct {
		name          string
		real, control callOutcome
		want          bool
	}{
		{"real ok, invalid refused", ok, refused, true},
		{"real ok, invalid ok — keyless tool", ok, ok, false},
		{"both the same validation error — arguments checked first", validation, validation, false},
		{"validation for the real key, refusal for the invalid one", validation, refused, true},
		{"scope denial for both — the key is valid but not allowed this tool", scope, scope, false},
		{"control did not answer", ok, callOutcome{kind: callTransport, text: "timeout"}, false},
		{"same kind, different wording", validation, callOutcome{kind: callResultError, text: "Invalid API key"}, true},
	}
	for _, c := range cases {
		if got := outcomesDiffer(c.real, c.control); got != c.want {
			t.Errorf("%s: outcomesDiffer = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestHandshakeRefused(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"http 401", fmt.Errorf("initialize: %w", &mcp.HTTPStatusError{StatusCode: 401}), true},
		{"http 403", &mcp.HTTPStatusError{StatusCode: 403}, true},
		{"rpc error, any wording", &mcp.RPCError{Code: -32000, Message: "Missing x-access-key or x-secret-key headers"}, true},
		{"id-mismatch reply carrying an error", errors.New("MCP http: response id 0 does not match request id 1 (response carried error: {...})"), true},
		{"deadline", context.DeadlineExceeded, false},
		{"unknown transport wording", errors.New("dial tcp: connection refused"), false},
	}
	for _, c := range cases {
		if got := handshakeRefused(c.err); got != c.want {
			t.Errorf("%s: handshakeRefused = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestAddAPIKeyRealCallErrorsThatAreNotRejections: answers to the real key's
// blind call that MENTION credentials-adjacent words but are not the vendor
// refusing the key must not fail the add — a pagination argument, the SDK's
// timeout code, a scope denial. The control probe still decides "verified".
func TestAddAPIKeyRealCallErrorsThatAreNotRejections(t *testing.T) {
	for name, tc := range map[string]struct {
		onCall       func(string) (map[string]any, map[string]any)
		wantVerified bool
	}{
		"pagination token required": {func(string) (map[string]any, map[string]any) {
			return map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": "page_token is required"}}}, nil
		}, true},
		"sdk timeout code": {func(string) (map[string]any, map[string]any) {
			return nil, map[string]any{"code": -32001, "message": "Request timed out"}
		}, true},
	} {
		t.Run(name, func(t *testing.T) {
			v := &keyProbeVendor{t: t, validKey: "sk-good", refuse: refuseWith401, onCall: tc.onCall, tools: []map[string]any{tool("list_pages", nil, nil)}}
			srv := v.server()
			defer srv.Close()
			_, report, err := newTestService(t, newFakeStore(), srv).AddServer(context.Background(), AddServerInput{
				Email: "u@x.com", Name: "vendor", URL: srv.URL, AuthMode: "api_key", APIKey: "sk-good", APIKeyHeader: "X-API-Key",
			})
			if err != nil {
				t.Fatalf("AddServer refused a valid key over a non-rejection answer: %v", err)
			}
			if report.KeyVerified != tc.wantVerified {
				t.Errorf("report = %+v; want verified=%v (the control key was refused with 401)", report, tc.wantVerified)
			}
		})
	}
}

// TestAddAPIKeyScopeDeniedIsNotRejected: a valid key that is not allowed the
// probe's tool gets a 403 — and so does the invalid key. The add succeeds
// (the key may well be right) and the report is honest: not verified, since
// both keys got the same answer.
func TestAddAPIKeyScopeDeniedIsNotRejected(t *testing.T) {
	deny := func(w http.ResponseWriter, _ int) {
		http.Error(w, `{"error":"insufficient permissions for this tool"}`, http.StatusForbidden)
	}
	v := &keyProbeVendor{t: t, validKey: "sk-good", refuse: deny, onAnyCall: deny, tools: []map[string]any{tool("list_accounts", nil, nil)}}
	srv := v.server()
	defer srv.Close()
	_, report, err := newTestService(t, newFakeStore(), srv).AddServer(context.Background(), AddServerInput{
		Email: "u@x.com", Name: "vendor", URL: srv.URL, AuthMode: "api_key", APIKey: "sk-good", APIKeyHeader: "X-API-Key",
	})
	if err != nil {
		t.Fatalf("AddServer refused a scope-limited key: %v", err)
	}
	if report.KeyVerified {
		t.Errorf("report = %+v; a 403 for both keys proves nothing, want unverified", report)
	}
	if got := v.called(); len(got) != 2 {
		t.Errorf("calls = %v; want the real call and the control call", got)
	}
}

// TestProbeReportsToolSchemaIssues: the add-time probe runs the model
// boundary's schema check on every listed tool and reports findings without
// failing the add (the connection is sound). The nightly catalog smoke reads
// the same field to flag a vendor that ships an invalid schema.
func TestProbeReportsToolSchemaIssues(t *testing.T) {
	tuple := tool("fetch_range", nil, nil)
	tuple["inputSchema"] = map[string]any{"type": "object", "properties": map[string]any{
		"range": map[string]any{"type": "array", "items": []any{map[string]any{"type": "string"}, map[string]any{"type": "string"}}},
	}}
	broken := tool("broken", nil, nil)
	broken["inputSchema"] = map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": 42}}}
	v := &keyProbeVendor{t: t, refuse: refuseWith401, tools: []map[string]any{tool("list_docs", nil, nil), tuple, broken}}
	srv := v.server()
	defer srv.Close()
	svc := newTestService(t, newFakeStore(), srv)
	_, report, err := svc.AddServer(context.Background(), AddServerInput{Email: "u@x.com", Name: "docs", URL: srv.URL, AuthMode: "open"})
	if err != nil {
		t.Fatalf("AddServer(open): %v", err)
	}
	if report.ToolCount != 3 {
		t.Fatalf("report = %+v, want 3 tools", report)
	}
	statuses := map[string]string{}
	for _, issue := range report.SchemaIssues {
		statuses[issue.Tool] = issue.Status
	}
	if len(statuses) != 2 || statuses["fetch_range"] != agentcore.ToolSchemaRewritten || statuses["broken"] != agentcore.ToolSchemaInvalid {
		t.Fatalf("schema issues = %+v, want fetch_range=rewritten, broken=invalid", report.SchemaIssues)
	}
}

// TestAddServerSendsTheEntrySchemePrefix is the api_key_prefix seam: a vendor
// that wants "Authorization: Token token=<key>" (PagerDuty) gets exactly that
// from the add-time probe when the entry declares the prefix — the user
// pasted only the key — and the control probe carries the same scheme, so
// the vendor's refusal of the invalid key is a refusal of the key, not of a
// missing scheme. Without the prefix the same key is refused.
func TestAddServerSendsTheEntrySchemePrefix(t *testing.T) {
	var seen []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Token token=u+good" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized","error_description":"No permission -- see authorization schemes"}`))
			return
		}
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		result := map[string]any{}
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "list_incidents", "description": "List incidents", "inputSchema": map[string]any{"type": "object"}, "annotations": map[string]any{"readOnlyHint": true}}}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "[]"}}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer srv.Close()

	svc := newTestService(t, newFakeStore(), srv)
	server, report, err := svc.AddServer(context.Background(), AddServerInput{
		Email: "u@x.com", Name: "pagerduty", URL: srv.URL, AuthMode: "api_key",
		APIKey: "u+good", APIKeyHeader: "Authorization", APIKeyPrefix: "Token token=",
	})
	if err != nil {
		t.Fatalf("AddServer with the prefix: %v", err)
	}
	if server.APIKeyPrefix != "Token token=" || server.APIKeyHeader != "Authorization" {
		t.Fatalf("stored row = header %q prefix %q", server.APIKeyHeader, server.APIKeyPrefix)
	}
	if !report.KeyVerified {
		t.Fatalf("key not verified: the vendor refuses the control probe's invalid key, so the real key's pass is a verification (report %+v)", report)
	}
	mu.Lock()
	defer mu.Unlock()
	var sawInvalid bool
	for _, v := range seen {
		if v != "Token token=u+good" && v != "Token token="+invalidProbeKey {
			t.Fatalf("a request carried %q, want the scheme in front of either key", v)
		}
		if v == "Token token="+invalidProbeKey {
			sawInvalid = true
		}
	}
	if !sawInvalid {
		t.Fatal("the control probe did not carry the scheme prefix")
	}

	// The same key without the entry's prefix is what the old hint produced:
	// the vendor refuses it, and the add fails as a refused key.
	if _, _, err := svc.AddServer(context.Background(), AddServerInput{
		Email: "u@x.com", Name: "pagerduty-raw", URL: srv.URL, AuthMode: "api_key",
		APIKey: "u+good", APIKeyHeader: "Authorization",
	}); err == nil {
		t.Fatal("the raw key without the scheme was accepted")
	}

	// A prefix needs a named header; the default bearer shape takes none.
	if _, _, err := svc.AddServer(context.Background(), AddServerInput{
		Email: "u@x.com", Name: "bad", URL: srv.URL, AuthMode: "api_key", APIKey: "u+good", APIKeyPrefix: "Token token=",
	}); err == nil || !strings.Contains(err.Error(), "needs a header name") {
		t.Fatalf("prefix without a header: err = %v", err)
	}
}
