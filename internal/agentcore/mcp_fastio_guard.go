package agentcore

import (
	"fmt"
	"strings"
)

// fast.io inline-upload guard (merged from chat + cutlass mcp_fastio_guard.go).
//
// The guard's remediation hint walks the agent through the chunked blob upload
// flow (create-session → POST /blob → chunk → finalize), the one remediation
// fleet can name without knowing what a bundle installs. (cutlass also pointed
// at its native path-taking upload tool; that tooling moved to the client
// bundle, and no deployment ever configured a replacement, so the hint names
// only the blob flow.)

const fastIOUploadToolName = "mcp_fast_io_upload"
const fastIOStreamUploadAction = "stream-upload"
const mcpServerFastIO = "fast_io"

const (
	argFieldContent       = "content"
	argFieldContentBase64 = "content_base64"
)

// fastIOInlineUploadByteCap is the soft ceiling on inline payload size before
// the guard rejects locally. 10 KB matches protocols/fastio-mcp.md's "<10 KB
// text" boundary; a typical .docx/.xlsx/PDF lands at 15–50 KB.
const fastIOInlineUploadByteCap = 10 * 1024

// rejectFastIOInlineBase64Upload returns ok=false plus a hint string when args
// describe an `mcp_fast_io_upload` call carrying an inline content_base64 (or
// content) payload over the cap. Callers short-circuit the dispatch with the
// hint as the tool-result text — they must NOT echo the inline payload back into
// the conversation.
//
// Returns ok=true (no rejection) for: any other tool name; calls routing bytes
// through blob_id/blob_ref; calls under the cap; nil args.
//
// Every caller reaches this through the localMCPBroker (issue #167): the
// in-process loop and an out-of-process broker share one implementation, so the
// guard runs identically without a separate exported entry point.
func rejectFastIOInlineBase64Upload(toolName string, args map[string]any) (ok bool, hint string) {
	if toolName != fastIOUploadToolName {
		return true, ""
	}
	if args == nil {
		return true, ""
	}
	if hasNonEmptyString(args, "blob_id") || hasNonEmptyString(args, "blob_ref") {
		return true, ""
	}

	field, size := largestInlineField(args)
	if size <= fastIOInlineUploadByteCap {
		return true, ""
	}

	action, _ := args["action"].(string)
	if action == "" {
		action = fastIOStreamUploadAction
	}

	var b strings.Builder
	fmt.Fprintf(&b,
		"Rejected locally before forwarding to fast.io: `mcp_fast_io_upload action=%s` carries %d bytes of inline `%s`, "+
			"which is over the %d-byte cap. Inline base64 over this size routinely bloats conversation context and "+
			"trips fast.io's length validator (the agent encodes N bytes but the server sees N±k after JSON "+
			"round-trips).",
		action, size, field, fastIOInlineUploadByteCap)

	b.WriteString(" Use the blob upload flow instead — it keeps the bytes out of the model's context entirely:\n")
	fmt.Fprintf(&b,
		"  1. mcp_fast_io_upload action=create-session profile_type=workspace profile_id=<id> "+
			"parent_node_id=<folder_id> filename=<name>\n"+
			"  2. POST the file bytes to the blob endpoint in the response, capture the returned `blob_id`.\n"+
			"  3. mcp_fast_io_upload action=chunk session_id=<id> blob_id=<blob_id>\n"+
			"  4. mcp_fast_io_upload action=finalize session_id=<id>\n")

	fmt.Fprintf(&b,
		"See protocols/fastio-mcp.md for the full pattern. Do NOT retry the same `%s` call with the same inline "+
			"payload — it will be rejected here again.",
		action)

	return false, b.String()
}

// largestInlineField returns the inline payload field with the largest byte
// length, plus that length. Returns "", 0 when neither is present or both empty.
func largestInlineField(args map[string]any) (string, int) {
	var bestField string
	var bestSize int
	for _, name := range []string{argFieldContentBase64, argFieldContent} {
		s, _ := args[name].(string)
		if len(s) > bestSize {
			bestField = name
			bestSize = len(s)
		}
	}
	return bestField, bestSize
}

// hasNonEmptyString reports whether args[key] is a non-empty string.
func hasNonEmptyString(args map[string]any, key string) bool {
	s, _ := args[key].(string)
	return s != ""
}
