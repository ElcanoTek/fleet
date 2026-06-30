package httpapi

// Client capability negotiation for the SSE stream (#194).
//
// By default the server fans every event type to every subscriber. A client
// that only handles a subset (e.g. a CLI that wants text.delta + turn.* and
// nothing else) can declare what it supports in the X-Fleet-Capabilities request
// header — a JSON array of capability tokens. The server then suppresses the
// governed events the client did NOT declare, and advertises the full supported
// set back via the X-Fleet-Supported-Capabilities response header AND a synthetic
// fleet.capabilities event sent as the first SSE frame (so a browser EventSource,
// which can't read response headers, can still discover support).
//
// Backward compatibility is the design's spine: NO header (or a malformed one) =
// nil filter = emit everything, exactly as before. Unknown tokens are ignored.
// Only events listed in capabilityForEvent are gated; every lifecycle/control
// frame (turn.*, conversation*, user.message, status, reconnect, heartbeat,
// buffer_expired, fleet.capabilities) always flows so a filtering client can't
// accidentally suppress the frames that drive turn state.

import (
	"encoding/json"
	"net/http"
	"strings"
)

// SSECapability is a token a client advertises to declare it can handle a class
// of SSE event. The wire form is the lowercase string constant.
type SSECapability string

const (
	CapText          SSECapability = "text"           // text.delta
	CapReasoning     SSECapability = "reasoning"      // reasoning.start/delta/end
	CapToolCalls     SSECapability = "tool_calls"     // tool.call
	CapToolResults   SSECapability = "tool_results"   // tool.result
	CapApprovalCards SSECapability = "approval_cards" // tool.approval_*/auto_resolved, memory.proposed
)

// allSSECapabilities is the full set the server can emit — advertised in the
// X-Fleet-Supported-Capabilities header and the fleet.capabilities event.
//
// Deliberately scoped to events this codebase ACTUALLY emits. The issue also
// listed enforcement_nudges / usage_snapshots / permissions, but those event
// names have no emit site here (they came from an external runtime that does not
// exist in this repo), so shipping tokens for them would advertise a filter that
// can never do anything — omitted for honesty.
var allSSECapabilities = []SSECapability{
	CapText, CapReasoning, CapToolCalls, CapToolResults, CapApprovalCards,
}

// capabilityForEvent maps a GOVERNED SSE event name to the capability that gates
// it. Any event NOT in this map is lifecycle/control (turn.*, conversation*,
// user.message, status, and the synthetic reconnect/heartbeat/buffer_expired/
// fleet.capabilities frames) and is ALWAYS emitted regardless of the client's
// declared set.
var capabilityForEvent = map[string]SSECapability{
	"text.delta":               CapText,
	"reasoning.start":          CapReasoning,
	"reasoning.delta":          CapReasoning,
	"reasoning.end":            CapReasoning,
	"tool.call":                CapToolCalls,
	"tool.result":              CapToolResults,
	"tool.approval_required":   CapApprovalCards,
	"tool.approval_superseded": CapApprovalCards,
	"tool.auto_resolved":       CapApprovalCards,
	"memory.proposed":          CapApprovalCards,
}

const (
	clientCapabilitiesHeaderName    = "X-Fleet-Capabilities"
	supportedCapabilitiesHeaderName = "X-Fleet-Supported-Capabilities"
	capabilitiesEventName           = "fleet.capabilities"
)

// parseClientCapabilities decodes the JSON-array X-Fleet-Capabilities header
// into a lookup set. Returns nil — meaning "no filter, emit everything" — when
// the header is absent or malformed, preserving the pre-#194 behaviour for any
// client that doesn't negotiate.
func parseClientCapabilities(hdr string) map[SSECapability]bool {
	if strings.TrimSpace(hdr) == "" {
		return nil
	}
	var tokens []string
	if err := json.Unmarshal([]byte(hdr), &tokens); err != nil {
		return nil // malformed → safe default: no filter
	}
	set := make(map[SSECapability]bool, len(tokens))
	for _, t := range tokens {
		set[SSECapability(t)] = true
	}
	return set
}

// shouldEmit reports whether an event flows to a subscriber with the given
// declared capability set. A nil set (no/invalid header) emits everything; an
// event governed by no capability always flows; otherwise the client must have
// declared the governing capability.
func shouldEmit(caps map[SSECapability]bool, eventName string) bool {
	if caps == nil {
		return true
	}
	capability, governed := capabilityForEvent[eventName]
	if !governed {
		return true
	}
	return caps[capability]
}

// supportedCapabilitiesJSON is the JSON-array value advertised in the
// X-Fleet-Supported-Capabilities response header and the fleet.capabilities
// event payload.
func supportedCapabilitiesJSON() string {
	b, _ := json.Marshal(allSSECapabilities)
	return string(b)
}

// setSupportedCapabilitiesHeader advertises the server's full capability set.
// Must be called before the response WriteHeader.
func setSupportedCapabilitiesHeader(w http.ResponseWriter) {
	w.Header().Set(supportedCapabilitiesHeaderName, supportedCapabilitiesJSON())
}
