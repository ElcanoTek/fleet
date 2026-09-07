// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package a2a

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	wire "github.com/a2aproject/a2a-go/v2/a2a"
)

// TestCodeForMatchesTheSpecTable pins §5.4/§9.5's code assignments — the TCK
// asserts these numerically (its jsonrpc tests hardcode -32001 etc.).
func TestCodeForMatchesTheSpecTable(t *testing.T) {
	cases := []struct {
		err  error
		code int
	}{
		{wire.ErrParseError, -32700},
		{wire.ErrInvalidRequest, -32600},
		{wire.ErrMethodNotFound, -32601},
		{wire.ErrInvalidParams, -32602},
		{wire.ErrInternalError, -32603},
		{wire.ErrServerError, -32000},
		{wire.ErrTaskNotFound, -32001},
		{wire.ErrTaskNotCancelable, -32002},
		{wire.ErrPushNotificationNotSupported, -32003},
		{wire.ErrUnsupportedOperation, -32004},
		{wire.ErrUnsupportedContentType, -32005},
		{wire.ErrInvalidAgentResponse, -32006},
		{wire.ErrExtendedCardNotConfigured, -32007},
		{wire.ErrExtensionSupportRequired, -32008},
		{wire.ErrVersionNotSupported, -32009},
	}
	for _, c := range cases {
		if got := CodeFor(c.err); got != c.code {
			t.Errorf("CodeFor(%v) = %d, want %d", c.err, got, c.code)
		}
		// Wrapped errors (fmt.Errorf("%w: detail", sentinel)) must keep their code.
		if got := CodeFor(fmt.Errorf("%w: with detail", c.err)); got != c.code {
			t.Errorf("CodeFor(wrapped %v) = %d, want %d", c.err, got, c.code)
		}
	}
	if got := CodeFor(errors.New("mystery")); got != -32603 {
		t.Errorf("unknown error maps to %d, want -32603 (never a leaked class)", got)
	}
}

// TestErrorEnvelopeCarriesErrorInfo pins the breaking v1.0 error contract:
// error.data is an ARRAY of @type-tagged objects and MUST include a
// google.rpc.ErrorInfo with the UPPER_SNAKE reason and the protocol domain.
// The TCK gates this at MUST level (test_error_info.py).
func TestErrorEnvelopeCarriesErrorInfo(t *testing.T) {
	resp := NewErrorResponse(json.RawMessage(`7`), wire.ErrTaskNotFound, "no such task", map[string]string{"taskId": "abc"})
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   struct {
			Code    int              `json:"code"`
			Message string           `json:"message"`
			Data    []map[string]any `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.JSONRPC != "2.0" || string(decoded.ID) != "7" {
		t.Errorf("envelope wrong: %s", raw)
	}
	if decoded.Error.Code != -32001 || decoded.Error.Message != "no such task" {
		t.Errorf("error member wrong: %+v", decoded.Error)
	}
	if len(decoded.Error.Data) == 0 {
		t.Fatalf("error.data must be a non-empty array of typed details: %s", raw)
	}
	info := decoded.Error.Data[0]
	if info["@type"] != "type.googleapis.com/google.rpc.ErrorInfo" {
		t.Errorf("@type = %v", info["@type"])
	}
	if info["reason"] != "TASK_NOT_FOUND" || info["domain"] != "a2a-protocol.org" {
		t.Errorf("ErrorInfo reason/domain wrong: %+v", info)
	}
	meta, _ := info["metadata"].(map[string]any)
	if meta["taskId"] != "abc" {
		t.Errorf("ErrorInfo metadata wrong: %+v", info)
	}
}

func TestNormalizedNullID(t *testing.T) {
	resp := NewErrorResponse(nil, wire.ErrParseError, "", nil)
	raw, _ := json.Marshal(resp)
	if !strings.Contains(string(raw), `"id":null`) {
		t.Errorf("an absent request id must respond with id null: %s", raw)
	}
}

// TestCheckVersion pins the spec-literal §3.6.2 posture: absent means 0.3,
// and this server refuses everything but 1.0 with a message naming the fix.
func TestCheckVersion(t *testing.T) {
	if err := CheckVersion("1.0"); err != nil {
		t.Errorf("1.0 refused: %v", err)
	}
	for _, v := range []string{"", "0.3", "1.0.1", "2.0"} {
		err := CheckVersion(v)
		if !errors.Is(err, wire.ErrVersionNotSupported) {
			t.Errorf("CheckVersion(%q) = %v, want ErrVersionNotSupported", v, err)
			continue
		}
		if !strings.Contains(err.Error(), "A2A-Version: 1.0") {
			t.Errorf("CheckVersion(%q) message must name the remedy, got %q", v, err)
		}
	}
	if err := CheckVersion(""); err == nil || !strings.Contains(err.Error(), "0.3") {
		t.Errorf("an absent header must be interpreted as 0.3 per §3.6.2, got %v", err)
	}
}

// TestResponseEnvelopeCarriesExactlyOneMember pins the JSON-RPC 2.0 rule the
// package's own client enforces on receipt: a success envelope always has a
// result member (even for shapes `omitempty` used to drop, such as an empty
// struct or a nil-able value), an error envelope never has one, and a nil
// result never reaches the wire as `"result": null` — NewResponse turns it
// into an InternalError so the bug is visible instead of decoding as a
// zero-valued task at the peer.
func TestResponseEnvelopeCarriesExactlyOneMember(t *testing.T) {
	keys := func(t *testing.T, resp Response) map[string]json.RawMessage {
		t.Helper()
		raw, err := json.Marshal(resp)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("%v: %s", err, raw)
		}
		return m
	}
	id := json.RawMessage(`1`)

	m := keys(t, NewResponse(id, struct{}{}))
	if string(m["result"]) != "{}" || m["error"] != nil {
		t.Errorf("empty-struct result: %v", m)
	}
	m = keys(t, NewResponse(id, map[string]any{"ok": true}))
	if _, ok := m["result"]; !ok || m["error"] != nil {
		t.Errorf("map result: %v", m)
	}

	m = keys(t, NewErrorResponse(id, wire.ErrTaskNotFound, "", nil))
	if _, ok := m["result"]; ok || m["error"] == nil {
		t.Errorf("error envelope must carry no result member: %v", m)
	}

	var nilTask *wire.Task
	for name, result := range map[string]any{
		"untyped nil": nil,
		"nil pointer": nilTask,
		"empty raw":   json.RawMessage(nil),
		"null raw":    json.RawMessage("null"),
		"nil map":     map[string]any(nil),
		"nil slice":   []string(nil),
	} {
		m := keys(t, NewResponse(id, result))
		if _, ok := m["result"]; ok {
			t.Errorf("%s: emitted a result member: %v", name, m)
			continue
		}
		var e ErrorObject
		if err := json.Unmarshal(m["error"], &e); err != nil || e.Code != CodeFor(wire.ErrInternalError) {
			t.Errorf("%s: want -32603 error envelope, got %s (err=%v)", name, m["error"], err)
		}
	}
}
