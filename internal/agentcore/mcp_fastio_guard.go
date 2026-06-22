package agentcore

import (
	"fmt"
	"strings"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// fast.io inline-upload guard (merged from chat + cutlass mcp_fastio_guard.go).
//
// The two repos' guards were identical except for the remediation hint text:
// chat pointed the agent at the blob upload flow (create-session → POST /blob →
// chunk → finalize); cutlass pointed at its native `fastio_upload_file` tool
// (and the chunked blob flow for files past that tool's cap). Per the migration
// ledger this merge parameterizes the hint via RemediationHints so BOTH
// remediation paths are exposed; the produced hint names the native tool AND
// walks the blob flow, so either front-end's expectations are satisfied.

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

// RemediationHints parameterizes the remediation guidance the guard emits when
// it rejects an oversized inline upload. Both fields are optional; when both are
// set the produced hint exposes both paths (native tool first, then the chunked
// blob flow). DefaultRemediationHints reproduces the union of the chat blob-flow
// hint and the cutlass native-tool hint.
type RemediationHints struct {
	// NativeUploadTool, when non-empty, advertises a host-side native tool that
	// reads the file from disk and base64-encodes it deterministically (cutlass:
	// "fastio_upload_file path=<file> workspace_id=<id> ..."). Empty omits the
	// native-tool line.
	NativeUploadTool string
	// IncludeBlobFlow, when true, includes the chunked blob upload flow
	// (create-session → POST /blob → chunk → finalize) that both products use
	// for files past the native tool's cap (chat: the only remediation path).
	IncludeBlobFlow bool
}

// DefaultRemediationHints exposes BOTH remediation paths — the cutlass native
// `fastio_upload_file` tool AND the chat blob upload flow — so the merged guard
// satisfies both front-ends' parity tests.
var DefaultRemediationHints = RemediationHints{
	NativeUploadTool: "fastio_upload_file path=<file> workspace_id=<19-digit id> [filename=...] [parent_node_id=...]",
	IncludeBlobFlow:  true,
}

// fastIOServerEnabled reports whether the fast_io MCP server is wired up for
// this run. Probed by name (not token presence) so it works in tests.
func fastIOServerEnabled(serverTools []mcp.ServerTool) bool {
	for _, st := range serverTools {
		if st.ServerName == mcpServerFastIO {
			return true
		}
	}
	return false
}

// rejectFastIOInlineBase64Upload returns ok=false plus a hint string when args
// describe an `mcp_fast_io_upload` call carrying an inline content_base64 (or
// content) payload over the cap. Callers short-circuit the dispatch with the
// hint as the tool-result text — they must NOT echo the inline payload back into
// the conversation.
//
// Returns ok=true (no rejection) for: any other tool name; calls routing bytes
// through blob_id/blob_ref; calls under the cap; nil args.
//
// The hints parameter exposes the remediation path(s); DefaultRemediationHints
// exposes both the native tool and the blob flow.
// RejectFastIOInlineBase64Upload is the exported form used by the native-acp host
// broker so a delegated `mcp_fast_io_upload` call gets the SAME inline-base64
// pre-flight rejection the in-process mcpTool applies (parity). Pass the
// fully-prefixed tool name (mcp_<server>_<tool>) and DefaultRemediationHints.
func RejectFastIOInlineBase64Upload(toolName string, args map[string]any, hints RemediationHints) (ok bool, hint string) {
	return rejectFastIOInlineBase64Upload(toolName, args, hints)
}

func rejectFastIOInlineBase64Upload(toolName string, args map[string]any, hints RemediationHints) (ok bool, hint string) {
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

	if hints.NativeUploadTool != "" {
		fmt.Fprintf(&b,
			" Use the native tool instead — it reads the file from disk, base64-encodes it in Go, and keeps the bytes "+
				"out of your context entirely:\n  %s\n",
			hints.NativeUploadTool)
	}

	if hints.IncludeBlobFlow {
		if hints.NativeUploadTool != "" {
			b.WriteString("For files past the native tool's 5 MB cap, drive the chunked blob upload flow yourself:\n")
		} else {
			b.WriteString(" Use the blob upload flow instead — it keeps the bytes out of the model's context entirely:\n")
		}
		fmt.Fprintf(&b,
			"  1. mcp_fast_io_upload action=create-session profile_type=workspace profile_id=<id> "+
				"parent_node_id=<folder_id> filename=<name>\n"+
				"  2. POST the file bytes to the blob endpoint in the response, capture the returned `blob_id`.\n"+
				"  3. mcp_fast_io_upload action=chunk session_id=<id> blob_id=<blob_id>\n"+
				"  4. mcp_fast_io_upload action=finalize session_id=<id>\n")
	}

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
