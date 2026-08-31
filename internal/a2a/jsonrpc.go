// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package a2a

// The JSON-RPC 2.0 envelope for the A2A binding (spec §9): request/response
// framing, the A2A error-code table, and the google.rpc.ErrorInfo detail the
// spec REQUIRES on every A2A-specific error (error.data is an ARRAY of
// @type-tagged objects — a breaking v1.0 change the TCK gates at MUST level).
// Method dispatch lives in internal/sched/handlers; this file only shapes
// bytes.

import (
	"encoding/json"
	"errors"
	"fmt"

	wire "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/errordetails"
)

// JSON-RPC method names (spec §9.1: PascalCase, matching the gRPC methods —
// the slash-delimited v0.3 names are gone).
const (
	MethodSendMessage          = "SendMessage"
	MethodSendStreamingMessage = "SendStreamingMessage"
	MethodGetTask              = "GetTask"
	MethodListTasks            = "ListTasks"
	MethodCancelTask           = "CancelTask"
	MethodSubscribeToTask      = "SubscribeToTask"
	MethodGetExtendedAgentCard = "GetExtendedAgentCard"
	MethodCreatePushConfig     = "CreateTaskPushNotificationConfig"
	MethodGetPushConfig        = "GetTaskPushNotificationConfig"
	MethodListPushConfigs      = "ListTaskPushNotificationConfigs"
	MethodDeletePushConfig     = "DeleteTaskPushNotificationConfig"
)

// Request is an incoming JSON-RPC 2.0 request envelope.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is an outgoing JSON-RPC 2.0 response envelope. Exactly one of
// Result / Error is set; ID mirrors the request id (JSON null when the
// request's id was absent or unparseable).
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *ErrorObject    `json:"error,omitempty"`
}

// ErrorObject is the JSON-RPC error member. Data is the spec's array of
// @type-tagged detail objects; NewErrorResponse always includes the
// google.rpc.ErrorInfo entry (reason + domain "a2a-protocol.org").
type ErrorObject struct {
	Code    int                   `json:"code"`
	Message string                `json:"message"`
	Data    []*errordetails.Typed `json:"data,omitempty"`
}

// NewResponse builds a success envelope.
func NewResponse(id json.RawMessage, result any) Response {
	return Response{JSONRPC: "2.0", ID: normalizeID(id), Result: result}
}

// NewErrorResponse builds an error envelope for one of the wire error
// sentinels. message overrides the sentinel's generic text when non-empty;
// meta lands in the ErrorInfo detail's metadata map (taskId, remedies, …).
func NewErrorResponse(id json.RawMessage, sentinel error, message string, meta map[string]string) Response {
	if message == "" {
		message = sentinel.Error()
	}
	return Response{
		JSONRPC: "2.0",
		ID:      normalizeID(id),
		Error: &ErrorObject{
			Code:    CodeFor(sentinel),
			Message: message,
			Data: []*errordetails.Typed{
				errordetails.NewErrorInfo(wire.ErrorReason(sentinel), wire.ProtocolDomain, meta),
			},
		},
	}
}

// normalizeID keeps the response id JSON-valid: an absent request id becomes
// JSON null, per JSON-RPC 2.0's rule for responses to broken requests.
func normalizeID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

// CodeFor maps a wire error sentinel to its JSON-RPC code (spec §5.4/§9.5).
// Unknown errors report as -32603 InternalError — never a leaked Go error
// class.
func CodeFor(err error) int {
	for _, row := range errorCodes {
		if errors.Is(err, row.sentinel) {
			return row.code
		}
	}
	return -32603
}

var errorCodes = []struct {
	sentinel error
	code     int
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

// CheckVersion enforces the A2A-Version service parameter spec-literally
// (§3.6.2): clients MUST send it, a server MUST interpret an absent/empty
// value as protocol 0.3, and this server implements 1.0 only — so anything
// but "1.0" is refused with a message naming the fix. Deliberately strict;
// recorded in docs/A2A.md's Honest scope.
func CheckVersion(headerValue string) error {
	if headerValue == string(wire.Version) {
		return nil
	}
	got := headerValue
	if got == "" {
		got = "0.3 (the spec-mandated default when the A2A-Version header is absent)"
	}
	return fmt.Errorf("%w: this server implements A2A %s only, request declared %s — send the header A2A-Version: %s",
		wire.ErrVersionNotSupported, wire.Version, got, wire.Version)
}
