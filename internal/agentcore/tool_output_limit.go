package agentcore

import (
	"fmt"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
)

// Tool-output ceiling (#199): a single, uniform cap on the size of any tool
// response content before it enters the transcript. A bash `cat huge.json`, an
// MCP database dump, or a long test run can otherwise inject hundreds of KB in
// one step and overflow the context window — at which point the reactive
// compaction in engine.go drops the WRONG (middle) messages. The cap is applied
// at BOTH result choke points — policyGuardedTool.Run (native/wrapped tools,
// including deferred MCP dispatched via tool_call) and mcpTool.Run (the
// direct-registration MCP path, #576) — so an MCP result is truncated
// identically whether or not the roster crossed the tool-disclosure threshold.

const defaultMaxToolOutputBytes = 64 * 1024 // 64 KB ≈ 16K tokens

var maxToolOutputBytesOnce struct {
	sync.Once
	v int
}

// maxToolOutputOverride is the admin-settings live override; nil = unset (env
// or built-in default serves). A pointer, not a sentinel int, because 0 ("no
// ceiling") is itself a legal override value. Read per tool call, so a change
// applies to the very next tool result with no restart.
var maxToolOutputOverride atomic.Pointer[int]

// SetMaxToolOutputBytes installs the process-wide admin override for the
// tool-output ceiling (0 disables truncation); ClearMaxToolOutputBytes is
// expressed by passing a negative value. Called by the workspace-settings
// apply hook in cmd/fleet.
func SetMaxToolOutputBytes(n int) {
	if n < 0 {
		maxToolOutputOverride.Store(nil)
		return
	}
	maxToolOutputOverride.Store(&n)
}

// EnvMaxToolOutputBytes returns the env-derived ceiling (ignoring any admin
// override) — the workspace-settings boot wiring reports it as the value a
// reset reverts to. Negative env values normalize to 0 (both mean "no
// ceiling"), so the settings registry sees only its two legal shapes.
func EnvMaxToolOutputBytes() int {
	if n := envMaxToolOutputBytes(); n > 0 {
		return n
	}
	return 0
}

// maxToolOutputBytes resolves the per-tool-call output ceiling: the admin
// override when set, else FLEET_MAX_TOOL_OUTPUT_BYTES (default 64 KB). A value
// <= 0 disables truncation. The env resolution is cached after the first read;
// the override is consulted live on every call.
func maxToolOutputBytes() int {
	if p := maxToolOutputOverride.Load(); p != nil {
		return *p
	}
	return envMaxToolOutputBytes()
}

// envMaxToolOutputBytes is the cached FLEET_MAX_TOOL_OUTPUT_BYTES resolution.
func envMaxToolOutputBytes() int {
	maxToolOutputBytesOnce.Do(func() {
		maxToolOutputBytesOnce.v = defaultMaxToolOutputBytes
		if s := os.Getenv("FLEET_MAX_TOOL_OUTPUT_BYTES"); s != "" {
			if n, err := strconv.Atoi(s); err == nil {
				maxToolOutputBytesOnce.v = n // n<=0 disables (handled by applyOutputCeiling)
			}
		}
	})
	return maxToolOutputBytesOnce.v
}

// applyOutputCeiling truncates content to at most limit bytes using a head+tail
// window so both the start and end survive (errors usually surface at the tail,
// context at the head). Returns the content and whether it was truncated. A
// non-positive limit, or content already within it, is returned unchanged.
//
// Truncation is rune-safe at the cut points so the result stays valid UTF-8
// (a mid-rune cut would corrupt the JSON the engine marshals).
func applyOutputCeiling(content string, limit int) (string, bool) {
	if limit <= 0 || len(content) <= limit {
		return content, false
	}
	headN := backToRuneBoundary(content, limit/2)
	tailStart := alignToRuneBoundary(content, len(content)-limit/4)
	omitted := tailStart - headN
	if omitted <= 0 {
		return content, false
	}
	return content[:headN] +
		fmt.Sprintf("\n\n[...truncated %d bytes of tool output — showing the first %d and last %d bytes; re-run scoped to what you need...]\n\n",
			omitted, headN, len(content)-tailStart) +
		content[tailStart:], true
}

// backToRuneBoundary returns the largest index <= i that starts a UTF-8 rune.
func backToRuneBoundary(s string, i int) int {
	if i >= len(s) {
		return len(s)
	}
	for i > 0 && !utf8RuneStart(s[i]) {
		i--
	}
	return i
}

// alignToRuneBoundary returns the smallest index >= i that starts a UTF-8 rune.
func alignToRuneBoundary(s string, i int) int {
	if i < 0 {
		return 0
	}
	for i < len(s) && !utf8RuneStart(s[i]) {
		i++
	}
	return i
}

// utf8RuneStart reports whether b is NOT a UTF-8 continuation byte (0b10xxxxxx).
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
