package tools

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// fakeMCPCaller is an in-memory stub of *mcp.Client for testing. It
// records every call and returns a configurable response — keeps the
// test surface to (params in, args out, response back) so we can pin
// the exact arguments fastio_upload_file forwards to fast.io.
type fakeMCPCaller struct {
	calls    []fakeMCPCall
	response *mcp.ToolResult
	err      error
}

type fakeMCPCall struct {
	toolName string
	args     map[string]interface{}
}

func (f *fakeMCPCaller) CallTool(_ context.Context, toolName string, args map[string]interface{}) (*mcp.ToolResult, error) {
	// Deep enough copy to detect post-call mutation by the tool.
	argsCopy := make(map[string]interface{}, len(args))
	for k, v := range args {
		argsCopy[k] = v
	}
	f.calls = append(f.calls, fakeMCPCall{toolName: toolName, args: argsCopy})
	if f.err != nil {
		return nil, f.err
	}
	return f.response, nil
}

// withWorkspace writes a file under a fresh per-conversation workspace
// dir (using CHAT_WORKSPACE_ROOT so we don't pollute the repo's
// workspace/ tree) and returns a context carrying the conversation id
// the agent harness would have threaded.
func withWorkspace(t *testing.T, filename string, body []byte) (context.Context, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
	const convID = "conv-fastio-upload-test"
	dir, err := EnsureWorkspaceDir(convID)
	if err != nil {
		t.Fatalf("EnsureWorkspaceDir: %v", err)
	}
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return WithConversationID(context.Background(), convID), path
}

func okResult(text string) *mcp.ToolResult {
	return &mcp.ToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: text}}}
}

// TestFastIOUploadFile_ForwardsBytesAsBase64 is the headline behavior:
// the agent passes a workspace-relative path; the tool reads the file,
// base64-encodes it server-side, and forwards via stream-upload with
// the right metadata. The bytes match the file verbatim — no mangling.
func TestFastIOUploadFile_ForwardsBytesAsBase64(t *testing.T) {
	body := []byte("Hello, fast.io! This is a tiny .docx-shaped payload.\x00\x01\x02")
	ctx, _ := withWorkspace(t, "report.docx", body)

	caller := &fakeMCPCaller{response: okResult(`{"node_id":"abc","web_url":"https://elcano.fast.io/file/abc"}`)}

	out, err := runFastIOUpload(ctx, caller, FastIOUploadFileParams{
		Path:        "report.docx",
		WorkspaceID: "***REMOVED***",
	})
	if err != nil {
		t.Fatalf("upload returned error: %v", err)
	}
	if !strings.Contains(out, "https://elcano.fast.io/file/abc") {
		t.Errorf("expected fast.io response forwarded verbatim, got: %s", out)
	}

	if len(caller.calls) != 1 {
		t.Fatalf("expected 1 MCP call, got %d", len(caller.calls))
	}
	call := caller.calls[0]
	if call.toolName != "upload" {
		t.Errorf("expected toolName=upload, got %q", call.toolName)
	}
	gotB64, _ := call.args["content_base64"].(string)
	wantB64 := base64.StdEncoding.EncodeToString(body)
	if gotB64 != wantB64 {
		t.Errorf("content_base64 mismatch\n got %q\nwant %q", gotB64, wantB64)
	}
	for k, want := range map[string]string{
		"action":         "stream-upload",
		"profile_type":   "workspace",
		"profile_id":     "***REMOVED***",
		"parent_node_id": "root",
		"filename":       "report.docx",
		"content_type":   "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	} {
		if got, _ := call.args[k].(string); got != want {
			t.Errorf("args[%q] = %q, want %q", k, got, want)
		}
	}
}

// TestFastIOUploadFile_FilenameAndParentDefault confirms the two
// defaults the agent depends on: a missing `filename` falls back to the
// basename of `path`, and a missing `parent_node_id` falls back to
// `root`. Same-name re-uploads (the versioning case) need both to be
// stable.
func TestFastIOUploadFile_FilenameAndParentDefault(t *testing.T) {
	ctx, _ := withWorkspace(t, "Davis Elen Log.docx", []byte("tiny"))
	caller := &fakeMCPCaller{response: okResult(`{"node_id":"x"}`)}

	if _, err := runFastIOUpload(ctx, caller, FastIOUploadFileParams{
		Path:        "Davis Elen Log.docx",
		WorkspaceID: "wid",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, _ := caller.calls[0].args["filename"].(string)
	if got != "Davis Elen Log.docx" {
		t.Errorf("filename default = %q, want %q", got, "Davis Elen Log.docx")
	}
	parent, _ := caller.calls[0].args["parent_node_id"].(string)
	if parent != "root" {
		t.Errorf("parent_node_id default = %q, want root", parent)
	}
}

// TestFastIOUploadFile_RejectsOversizedFile guards the 5 MB cap. We
// don't want a 100 MB file silently base64-bloating into a 134 MB MCP
// message and timing out — give the agent a clear pointer at the blob
// flow up front. Verifies the error mentions the cap AND the escape
// hatch.
func TestFastIOUploadFile_RejectsOversizedFile(t *testing.T) {
	ctx, path := withWorkspace(t, "big.bin", []byte("seed"))
	if err := os.Truncate(path, fastIOUploadMaxBytes+1); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	caller := &fakeMCPCaller{response: okResult("ignored")}

	_, err := runFastIOUpload(ctx, caller, FastIOUploadFileParams{
		Path:        "big.bin",
		WorkspaceID: "wid",
	})
	if err == nil {
		t.Fatal("expected size-cap rejection")
	}
	for _, want := range []string{"5 MB", "blob flow", "mcp_fast_io_upload"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
	if len(caller.calls) != 0 {
		t.Errorf("oversized file should not reach the MCP layer; got %d calls", len(caller.calls))
	}
}

// TestFastIOUploadFile_RejectsMissingWorkspaceID: a missing
// workspace_id is the one thing the agent typically forgets to thread
// through. The error needs to point at `mcp_fast_io_workspace
// action=list` so the agent knows where to get it without re-reading
// the protocol.
func TestFastIOUploadFile_RejectsMissingWorkspaceID(t *testing.T) {
	ctx, _ := withWorkspace(t, "x.txt", []byte("hi"))
	caller := &fakeMCPCaller{response: okResult("ok")}
	_, err := runFastIOUpload(ctx, caller, FastIOUploadFileParams{Path: "x.txt"})
	if err == nil {
		t.Fatal("expected missing workspace_id error")
	}
	if !strings.Contains(err.Error(), "mcp_fast_io_workspace") {
		t.Errorf("error should point at workspace list call, got: %v", err)
	}
	if len(caller.calls) != 0 {
		t.Error("missing workspace_id should short-circuit before MCP call")
	}
}

// TestFastIOUploadFile_RejectsMissingPath covers the simplest input
// gate. Without a path there's nothing to upload — fail fast rather
// than letting the call hit the wire and error out generically.
func TestFastIOUploadFile_RejectsMissingPath(t *testing.T) {
	caller := &fakeMCPCaller{}
	_, err := runFastIOUpload(context.Background(), caller, FastIOUploadFileParams{
		WorkspaceID: "wid",
	})
	if err == nil || !strings.Contains(err.Error(), "path is required") {
		t.Fatalf("expected path-required error, got: %v", err)
	}
}

// TestFastIOUploadFile_RejectsNonexistentFile: ValidatePathForRead
// should fire if the agent points at a file that doesn't exist. The
// error needs to surface the path the agent passed so the agent
// notices a typo rather than re-running the whole produce-the-file
// step blindly.
func TestFastIOUploadFile_RejectsNonexistentFile(t *testing.T) {
	ctx, _ := withWorkspace(t, "exists.txt", []byte("hi"))
	caller := &fakeMCPCaller{}
	_, err := runFastIOUpload(ctx, caller, FastIOUploadFileParams{
		Path:        "ghost.docx",
		WorkspaceID: "wid",
	})
	if err == nil {
		t.Fatal("expected file-not-found error")
	}
	if !strings.Contains(err.Error(), "ghost.docx") {
		t.Errorf("error should name the missing file, got: %v", err)
	}
}

// TestFastIOUploadFile_SurfacesUpstreamErrorVerbatim: when fast.io
// rejects an otherwise-well-formed upload (auth expired,
// parent_node_id not found, etc.), the agent needs to see fast.io's
// own error text — not a wrapper string that hides it. Otherwise the
// `_recovery` block in fast.io's error never reaches the model.
func TestFastIOUploadFile_SurfacesUpstreamErrorVerbatim(t *testing.T) {
	ctx, _ := withWorkspace(t, "x.txt", []byte("hi"))
	caller := &fakeMCPCaller{
		response: &mcp.ToolResult{
			IsError: true,
			Content: []mcp.ContentBlock{{Type: "text", Text: "parent_node_id 'bogus' not found"}},
		},
	}
	_, err := runFastIOUpload(ctx, caller, FastIOUploadFileParams{
		Path:         "x.txt",
		WorkspaceID:  "wid",
		ParentNodeID: "bogus",
	})
	if err == nil {
		t.Fatal("expected upstream error")
	}
	if !strings.Contains(err.Error(), "parent_node_id 'bogus' not found") {
		t.Errorf("upstream error text not surfaced, got: %v", err)
	}
}

// TestFastIOUploadFile_SurfacesTransportError: a transport failure
// (network drop, MCP subprocess crashed) needs to be distinguishable
// from a fast.io validation error so the agent can decide whether to
// retry. The wrapper string makes that distinction explicit.
func TestFastIOUploadFile_SurfacesTransportError(t *testing.T) {
	ctx, _ := withWorkspace(t, "x.txt", []byte("hi"))
	caller := &fakeMCPCaller{err: errors.New("transport: broken pipe")}
	_, err := runFastIOUpload(ctx, caller, FastIOUploadFileParams{
		Path:        "x.txt",
		WorkspaceID: "wid",
	})
	if err == nil {
		t.Fatal("expected transport error")
	}
	if !strings.Contains(err.Error(), "transport: broken pipe") {
		t.Errorf("transport error not surfaced, got: %v", err)
	}
}

// TestFastIOUploadFile_NilCallerSurfacesConfigError: when the
// fast_io MCP server isn't configured (no FAST_IO_MCP_TOKEN), the
// registration path supplies a nil caller. The tool needs to fail with
// a config-level error rather than a nil-pointer panic, so an operator
// reading the chat-server logs can fix the env without guessing.
func TestFastIOUploadFile_NilCallerSurfacesConfigError(t *testing.T) {
	_, err := runFastIOUpload(context.Background(), nil, FastIOUploadFileParams{
		Path:        "x.txt",
		WorkspaceID: "wid",
	})
	if err == nil {
		t.Fatal("expected config error for nil caller")
	}
	if !strings.Contains(err.Error(), "FAST_IO_MCP_TOKEN") {
		t.Errorf("error should mention the env var, got: %v", err)
	}
}

// TestFastIOUploadFile_HonorsExplicitFilenameAndParent: the agent
// passing `filename` and `parent_node_id` explicitly (the
// download → edit → upload pattern needs both pinned for versioning)
// must not be overridden by the defaults.
func TestFastIOUploadFile_HonorsExplicitFilenameAndParent(t *testing.T) {
	ctx, _ := withWorkspace(t, "local-name.xlsx", []byte("tiny"))
	caller := &fakeMCPCaller{response: okResult("ok")}
	if _, err := runFastIOUpload(ctx, caller, FastIOUploadFileParams{
		Path:         "local-name.xlsx",
		Filename:     "Master Tracking.xlsx",
		ParentNodeID: "node-1234567890-abc",
		WorkspaceID:  "wid",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	gotName, _ := caller.calls[0].args["filename"].(string)
	if gotName != "Master Tracking.xlsx" {
		t.Errorf("filename = %q, want %q", gotName, "Master Tracking.xlsx")
	}
	gotParent, _ := caller.calls[0].args["parent_node_id"].(string)
	if gotParent != "node-1234567890-abc" {
		t.Errorf("parent_node_id = %q, want %q", gotParent, "node-1234567890-abc")
	}
}

// TestFastIOUploadFile_HonorsExplicitContentType: agents that produce
// uncommon binary formats (a .parquet, a custom .bin, etc.) can
// override the auto-detected MIME so fast.io stores the right type.
// Pin the override path so a future refactor of detectFastIOContentType
// doesn't accidentally clobber it.
func TestFastIOUploadFile_HonorsExplicitContentType(t *testing.T) {
	ctx, _ := withWorkspace(t, "data.bin", []byte("tiny"))
	caller := &fakeMCPCaller{response: okResult("ok")}
	if _, err := runFastIOUpload(ctx, caller, FastIOUploadFileParams{
		Path:        "data.bin",
		WorkspaceID: "wid",
		ContentType: "application/vnd.apache.parquet",
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, _ := caller.calls[0].args["content_type"].(string)
	if got != "application/vnd.apache.parquet" {
		t.Errorf("content_type = %q, want override applied", got)
	}
}

// TestDetectFastIOContentType pins the Office-format mappings we ship
// even on a stripped-down container where /etc/mime.types is missing.
// Without these the agent's most common uploads — .docx and .xlsx —
// would land in fast.io as application/octet-stream.
func TestDetectFastIOContentType(t *testing.T) {
	cases := map[string]string{
		"report.docx":  "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		"sheet.xlsx":   "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"deck.pptx":    "application/vnd.openxmlformats-officedocument.presentationml.presentation",
		"legacy.doc":   "application/msword",
		"legacy.xls":   "application/vnd.ms-excel",
		"legacy.ppt":   "application/vnd.ms-powerpoint",
		"manual.pdf":   "application/pdf",
		"snapshot.bin": "application/octet-stream",
		"NO_EXTENSION": "application/octet-stream",
	}
	for name, want := range cases {
		if got := detectFastIOContentType(name); got != want {
			// Allow stdlib to win where it ships a mapping (e.g.
			// .pdf may be on /etc/mime.types as application/pdf
			// already) — accept any prefix match on the type.
			if !strings.HasPrefix(got, want) {
				t.Errorf("detectFastIOContentType(%q) = %q, want %q", name, got, want)
			}
		}
	}
}
