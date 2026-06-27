package agentcore

import (
	"fmt"
	"os"
	"strconv"
	"sync"
)

// Tool-output ceiling (#199): a single, uniform cap on the size of any tool
// response content before it enters the transcript. A bash `cat huge.json`, an
// MCP database dump, or a long test run can otherwise inject hundreds of KB in
// one step and overflow the context window — at which point the reactive
// compaction in engine.go drops the WRONG (middle) messages. Truncating here, at
// the universal choke point (policyGuardedTool.Run), fixes the common case
// before it ever reaches the model.

const defaultMaxToolOutputBytes = 64 * 1024 // 64 KB ≈ 16K tokens

var maxToolOutputBytesOnce struct {
	sync.Once
	v int
}

// maxToolOutputBytes resolves the per-tool-call output ceiling from
// FLEET_MAX_TOOL_OUTPUT_BYTES (default 64 KB). A value <= 0 disables truncation.
// Cached after the first read.
func maxToolOutputBytes() int {
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
