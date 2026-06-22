package tools

import (
	"context"
	"encoding/base64"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// FastIOUploadFileParams is the typed surface of the fastio_upload_file
// tool. The agent passes a workspace-relative file path plus the
// destination metadata; the chat-server handles the byte motion.
//
// Why no `content_base64` field: the entire point of this tool is to
// keep base64 OUT of the model's context. Carrying file bytes through
// `run_python.vars` → `tool_call.input` → fast.io is the failure mode
// we're closing — see mcp_fastio_guard.go for the details.
type FastIOUploadFileParams struct {
	Path         string `json:"path" description:"Workspace-relative or absolute path to the file to upload. Relative paths resolve inside the per-conversation workspace (same root the bash and run_python tools cd into). The file must exist when this is called."`
	WorkspaceID  string `json:"workspace_id" description:"19-digit fast.io workspace id. Get it once per conversation via mcp_fast_io_workspace action=list (the first row's id) and reuse the same value for every upload."`
	Filename     string `json:"filename,omitempty" description:"Filename to save under in fast.io. Defaults to the basename of path. Same-name upload to the same parent_node_id creates a new version of the existing node; pick the EXISTING name when amending a file."`
	ParentNodeID string `json:"parent_node_id,omitempty" description:"30-char fast.io folder node id to upload into. Defaults to root (the workspace root). Find subfolder ids via mcp_fast_io_storage action=search or action=list."`
	ContentType  string `json:"content_type,omitempty" description:"MIME type override (e.g. application/pdf). Auto-detected from the filename extension when omitted; sensible default for unknown extensions is application/octet-stream."`
}

// mimeOctetStream is the catch-all binary MIME type used as the
// fallback when a filename's extension isn't recognized. Pulled out
// as a constant so the multiple use sites in detectFastIOContentType
// don't trip goconst's "make it a constant" rule.
const mimeOctetStream = "application/octet-stream"

// fastIOUploadMaxBytes caps the file size this tool will forward. The
// stream-upload action carries the full base64 in a single MCP request;
// most MCP transports practically cap individual messages at ~10 MB.
// 5 MB raw → ~6.7 MB base64 leaves comfortable headroom and covers the
// 99% case (Word docs, Excel sheets, PDFs, screenshots, small CSVs).
//
// Files larger than this need fast.io's chunked blob flow, which is a
// separate ticket. The error returned at the cap points the agent at
// the lower-level `mcp_fast_io_upload` tool so it can drive the chunk
// flow manually if needed — same escape hatch the email and image
// tools use when their happy path doesn't fit.
const fastIOUploadMaxBytes = 5 * 1024 * 1024

const fastIOUploadDescription = "Uploads a file from your per-conversation workspace to fast.io WITHOUT routing the bytes through your context. " +
	"This is the right tool for `save X to fast.io`, `create a Word doc in fast.io`, `persist this report`, and similar — produce the file locally (write_file, run_python with openpyxl/reportlab/python-docx, etc.), then call this with the file path. " +
	"The chat-server reads the file from disk, base64-encodes it in Go (deterministic, no length-mangling), and forwards it to fast.io via `mcp_fast_io_upload action=stream-upload`. " +
	"You do NOT pass file bytes, base64, or `content_base64` — just the path; the bytes never enter the conversation. This sidesteps both the context bloat and the model-side base64 corruption that bit prior versions of this flow.\n\n" +
	"REQUIRED: `path` (workspace-relative or absolute) and `workspace_id` (19-digit, from `mcp_fast_io_workspace action=list`). " +
	"OPTIONAL: `filename` (defaults to basename of path), `parent_node_id` (defaults to root), `content_type` (defaults to a MIME detected from the extension).\n\n" +
	"SIZE LIMIT: 5 MB raw per call. Files bigger than that need fast.io's chunked blob flow — see protocols/fastio-mcp.md and use the raw `mcp_fast_io_upload` tool to drive create-session → chunk → finalize yourself.\n\n" +
	"VERSIONING: uploading with the same `filename` to the same `parent_node_id` creates a new version of the existing node (the node_id is preserved, the old bytes become recoverable history). That is the correct pattern when amending a file — do NOT delete-then-reupload, do NOT invent a `_v2` suffix."

// MCPCaller is the subset of *mcp.Client this package uses. Declaring
// the interface here (rather than importing the concrete type into the
// tool's signature) keeps the dependency one-way and lets tests
// substitute an in-memory fake — same shape as `mcpToolCaller` in
// httpapi/approvals.go.
type MCPCaller interface {
	CallTool(ctx context.Context, toolName string, arguments map[string]interface{}) (*mcp.ToolResult, error)
}

// NewFastIOUploadFileTool returns the fastio_upload_file native tool
// bound to the given MCP client. A nil caller surfaces a clear "fast.io
// not configured" error on invocation — same pattern used by the bash
// and run_python tools when registered without a sandbox.
func NewFastIOUploadFileTool(caller MCPCaller) fantasy.AgentTool {
	return fantasy.NewAgentTool("fastio_upload_file", fastIOUploadDescription,
		func(ctx context.Context, params FastIOUploadFileParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			payload, err := runFastIOUpload(ctx, caller, params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.NewTextResponse(payload), nil
		})
}

// runFastIOUpload is the testable core: validate, read, encode, dispatch.
// Returns the verbatim text content of fast.io's MCP response on success
// so the agent sees the same shape it would for any other fast.io call
// (web_url, node_id, _next hints, etc.).
func runFastIOUpload(ctx context.Context, caller MCPCaller, params FastIOUploadFileParams) (string, error) {
	if caller == nil {
		return "", fmt.Errorf("fastio_upload_file is unavailable: FAST_IO_MCP_TOKEN is not configured on this server")
	}

	pathArg := strings.TrimSpace(params.Path)
	if pathArg == "" {
		return "", fmt.Errorf("path is required")
	}
	workspaceID := strings.TrimSpace(params.WorkspaceID)
	if workspaceID == "" {
		return "", fmt.Errorf("workspace_id is required — call `mcp_fast_io_workspace action=list` once per conversation and reuse the 19-digit numeric id")
	}

	absPath, err := ValidatePathForRead(resolveWorkspacePath(ctx, pathArg))
	if err != nil {
		return "", fmt.Errorf("path: %w", err)
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", pathArg, err)
	}
	if info.Size() > fastIOUploadMaxBytes {
		return "", fmt.Errorf(
			"file is %d bytes; this tool caps inline uploads at %d bytes (~5 MB raw) — for larger files, use the lower-level `mcp_fast_io_upload` tool to drive the chunked blob flow (create-session → POST /blob → chunk → finalize); see protocols/fastio-mcp.md",
			info.Size(), fastIOUploadMaxBytes,
		)
	}

	raw, err := os.ReadFile(absPath) //nolint:gosec // path already validated against allowed dirs
	if err != nil {
		return "", fmt.Errorf("read %s: %w", pathArg, err)
	}

	filename := strings.TrimSpace(params.Filename)
	if filename == "" {
		filename = filepath.Base(absPath)
	}
	parentNodeID := strings.TrimSpace(params.ParentNodeID)
	if parentNodeID == "" {
		parentNodeID = "root"
	}
	contentType := strings.TrimSpace(params.ContentType)
	if contentType == "" {
		contentType = detectFastIOContentType(filename)
	}

	args := map[string]interface{}{
		"action":         "stream-upload",
		"profile_type":   "workspace",
		"profile_id":     workspaceID,
		"parent_node_id": parentNodeID,
		"filename":       filename,
		"content_type":   contentType,
		"content_base64": base64.StdEncoding.EncodeToString(raw),
	}

	result, err := caller.CallTool(ctx, "upload", args)
	if err != nil {
		return "", fmt.Errorf("fast.io upload call failed: %w", err)
	}

	text := joinMCPText(result)
	if result.IsError || text == "" {
		// Surface fast.io's own error text so the agent can react to
		// e.g. "parent_node_id not found" or "filename too long" without
		// guessing. We tag it so the model knows the call did go through
		// MCP and the error is from upstream, not from this wrapper.
		if text == "" {
			text = "(no body)"
		}
		return "", fmt.Errorf("fast.io rejected the upload: %s", text)
	}
	return text, nil
}

// detectFastIOContentType returns a MIME type for the filename using the
// stdlib mime registry, with a final fallback to application/octet-stream.
// Pure helper so the test suite can pin a few common cases without
// touching the upload flow.
func detectFastIOContentType(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	if ext == "" {
		return mimeOctetStream
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	// mime.TypeByExtension misses a few office formats on stripped-down
	// systems (it relies on /etc/mime.types). Pin the ones we care about
	// so the agent's `.docx`/`.xlsx`/`.pptx` uploads always go through
	// with the right type. The list covers the office file types agents
	// produce most often in chat — Word, Excel, PowerPoint, PDF.
	switch ext {
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case ".pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	case ".doc":
		return "application/msword"
	case ".xls":
		return "application/vnd.ms-excel"
	case ".ppt":
		return "application/vnd.ms-powerpoint"
	case ".pdf":
		return "application/pdf"
	}
	return mimeOctetStream
}

// joinMCPText flattens an MCP tool result's content blocks into a single
// string. Mirrors what mcpTool.Run does to the result before handing it
// to the agent, so the native and MCP paths surface results identically.
func joinMCPText(result *mcp.ToolResult) string {
	if result == nil {
		return ""
	}
	var sb strings.Builder
	for _, block := range result.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
			sb.WriteString("\n")
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// Compile-time check that *mcp.Client implements MCPCaller, so a future
// refactor of either side will fail at build time instead of at run.
var _ MCPCaller = (*mcp.Client)(nil)
