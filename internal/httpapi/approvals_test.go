package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/store"
)

// resolutionCallID picks the id under which the post-approval
// tool_result row is written: the original tool_call id when it was
// captured at stage time (so the chip in the UI updates), or the
// approval id as a fallback for older rows. Regression for the
// Inline-attachment-failure flow where the chip stayed stuck on
// "APPROVAL_REQUIRED..." after reload because the failure result
// was orphaned under the approval id.
func TestResolutionCallID(t *testing.T) {
	cases := []struct {
		name string
		in   *store.Approval
		want string
	}{
		{
			name: "tool_call_id present → prefer it (chip updates)",
			in:   &store.Approval{ID: "appr-1", ToolCallID: "tc_xxx"},
			want: "tc_xxx",
		},
		{
			name: "tool_call_id empty → fall back to approval id (legacy row)",
			in:   &store.Approval{ID: "appr-2", ToolCallID: ""},
			want: "appr-2",
		},
		{
			name: "nil approval → empty string (defensive)",
			in:   nil,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolutionCallID(tc.in); got != tc.want {
				t.Errorf("resolutionCallID = %q, want %q", got, tc.want)
			}
		})
	}
}

// Regression: the Kyle-email run (logs/Email-to-Kyle-Outlining-AI-
// Capabilities-*.json) staged a send with content_file pointing at a
// bare filename in the run_python workspace. The approval card
// displayed the filename string, and clicking Send hit
// "Content file not found" because the MCP server's cwd differs from
// the Go server's. materializeContentFile inlines the bytes at stage
// time so both paths see the same source of truth.
func TestMaterializeContentFileInlinesWorkspaceFile(t *testing.T) {
	convID := "conv-mat-1"

	// Point the workspace root at a temp dir so we don't pollute ./workspace.
	root := t.TempDir()
	t.Setenv("CHAT_WORKSPACE_ROOT", root)
	if err := os.MkdirAll(filepath.Join(root, convID), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "<html><body>hello kyle</body></html>"
	if err := os.WriteFile(filepath.Join(root, convID, "draft.html"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	raw := `{"to_email":"kyle@example.com","subject":"hi","content_file":"draft.html"}`
	out, err := materializeContentFile(convID, raw)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if _, ok := got["content_file"]; ok {
		t.Errorf("content_file should be removed after inlining, got %#v", got["content_file"])
	}
	if content, _ := got["content"].(string); content != body {
		t.Errorf("content mismatch: want %q, got %q", body, content)
	}
}

func TestMaterializeContentFileAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "email.html")
	body := "<p>absolute</p>"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	raw, _ := json.Marshal(map[string]any{
		"to_email":     "a@b.com",
		"subject":      "s",
		"content_file": path,
	})
	out, err := materializeContentFile("conv-abs", string(raw))
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal([]byte(out), &got)
	if content, _ := got["content"].(string); content != body {
		t.Errorf("content mismatch: want %q, got %q", body, content)
	}
}

func TestMaterializeContentFileMissingFileErrors(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CHAT_WORKSPACE_ROOT", root)

	raw := `{"to_email":"x@y.com","subject":"s","content_file":"nope.html"}`
	_, err := materializeContentFile("conv-missing", raw)
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), "nope.html") {
		t.Errorf("error should name the file, got: %v", err)
	}
}

func TestMaterializeContentFilePrefersFileOverInlineContent(t *testing.T) {
	// When both content and content_file are set, content_file takes
	// precedence — matching the tool descriptions and MCP server.
	// The file is read and replaces inline content.
	root := t.TempDir()
	t.Setenv("CHAT_WORKSPACE_ROOT", root)
	convID := "conv-pref"
	if err := os.MkdirAll(filepath.Join(root, convID), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	fileContent := "<p>file wins</p>"
	if err := os.WriteFile(filepath.Join(root, convID, "whatever.html"), []byte(fileContent), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	raw := `{"subject":"s","content":"<p>inline loses</p>","content_file":"whatever.html"}`
	out, err := materializeContentFile(convID, raw)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal([]byte(out), &got)
	if _, ok := got["content_file"]; ok {
		t.Errorf("content_file should be removed")
	}
	if got["content"].(string) != fileContent {
		t.Errorf("content should be from file, got: %v", got["content"])
	}
}

func TestMaterializeContentFileNoopWithoutContentFile(t *testing.T) {
	raw := `{"to_email":"a@b.com","subject":"s","content":"<p>hi</p>"}`
	out, err := materializeContentFile("conv-noop", raw)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if out != raw {
		t.Errorf("expected untouched input, got %s", out)
	}
}

// Regression for the John-Deere-Campaign-Performance-Report run
// (logs/John-Deere-*.json): preview_email worked, but the
// post-approval mcp_sendgrid_send_email failed with "Inline attachment
// file not found: daily_performance.png" because the relative path
// resolved against the MCP subprocess's cwd, not the conversation
// workspace. Staging now rewrites every relative attachment path to
// an absolute path under workspace/<convID>/ so the post-approval
// replay sees a path the MCP can resolve.
func TestMaterializeAttachmentPathsRewritesRelativePaths(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CHAT_WORKSPACE_ROOT", root)
	convID := "conv-attach"

	raw := `{"to_email":"brad@x.com","subject":"s","content":"<img src=\"cid:chart\">","inline_attachments":[{"path":"daily_performance.png","cid":"chart"}],"attachments":[{"path":"report.csv"}]}`
	out, err := materializeAttachmentPaths(convID, raw)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	wantInline := filepath.Join(root, convID, "daily_performance.png")
	inline := got["inline_attachments"].([]any)[0].(map[string]any)
	if inline["path"].(string) != wantInline {
		t.Errorf("inline path: want %q, got %q", wantInline, inline["path"])
	}
	if inline["cid"].(string) != "chart" {
		t.Errorf("cid lost during rewrite: %v", inline["cid"])
	}

	wantAttach := filepath.Join(root, convID, "report.csv")
	attach := got["attachments"].([]any)[0].(map[string]any)
	if attach["path"].(string) != wantAttach {
		t.Errorf("attachment path: want %q, got %q", wantAttach, attach["path"])
	}
}

func TestMaterializeAttachmentPathsLeavesAbsolutePathsAlone(t *testing.T) {
	abs := "/tmp/already-absolute.png"
	raw, _ := json.Marshal(map[string]any{
		"inline_attachments": []map[string]any{{"path": abs, "cid": "x"}},
	})
	out, err := materializeAttachmentPaths("conv-abs-attach", string(raw))
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal([]byte(out), &got)
	path := got["inline_attachments"].([]any)[0].(map[string]any)["path"].(string)
	if path != abs {
		t.Errorf("absolute path should pass through unchanged: want %q, got %q", abs, path)
	}
}

func TestMaterializeAttachmentPathsNoopWithoutAttachments(t *testing.T) {
	raw := `{"to_email":"a@b.com","subject":"s","content":"<p>hi</p>"}`
	out, err := materializeAttachmentPaths("conv-noop-attach", raw)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if out != raw {
		t.Errorf("expected untouched input, got %s", out)
	}
}

func TestMaterializeContentFileSizeCap(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CHAT_WORKSPACE_ROOT", root)
	convID := "conv-big"
	if err := os.MkdirAll(filepath.Join(root, convID), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	big := make([]byte, maxInlinedContentBytes+1)
	if err := os.WriteFile(filepath.Join(root, convID, "big.html"), big, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	raw := `{"subject":"s","content_file":"big.html"}`
	_, err := materializeContentFile(convID, raw)
	if err == nil || !strings.Contains(err.Error(), "inline cap") {
		t.Errorf("expected inline-cap error, got: %v", err)
	}
}

// fakeMCPClient implements mcpToolCaller for tests.
type fakeMCPClient struct {
	result *mcp.ToolResult
	err    error
}

func (f *fakeMCPClient) CallTool(_ context.Context, _ string, _ map[string]interface{}) (*mcp.ToolResult, error) {
	return f.result, f.err
}

func TestPrevalidateEmailRejectsBrokenHTML(t *testing.T) {
	stager := &approvalStager{
		ctx: context.Background(),
		mcpClient: &fakeMCPClient{
			result: &mcp.ToolResult{
				Content: []mcp.ContentBlock{
					{Type: "text", Text: `{"valid":false,"errors":["Unclosed tag: <td>","Unresolved token: {{cta_text}}"],"warnings":[]}`},
				},
			},
		},
	}
	err := stager.prevalidateEmail(`{"to_email":"a@b.com","subject":"s","content":"<html><body><td>broken"}`)
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "Unclosed tag") {
		t.Errorf("error should mention unclosed tag, got: %v", err)
	}
}

func TestPrevalidateEmailAcceptsValidHTML(t *testing.T) {
	stager := &approvalStager{
		ctx: context.Background(),
		mcpClient: &fakeMCPClient{
			result: &mcp.ToolResult{
				Content: []mcp.ContentBlock{
					{Type: "text", Text: `{"valid":true,"errors":[],"warnings":[]}`},
				},
			},
		},
	}
	err := stager.prevalidateEmail(`{"to_email":"a@b.com","subject":"s","content":"<html><body>hello</body></html>"}`)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestPrevalidateEmailSkippedWhenMCPDown(t *testing.T) {
	stager := &approvalStager{
		ctx: context.Background(),
		mcpClient: &fakeMCPClient{
			err: errors.New("mcp server unreachable"),
		},
	}
	err := stager.prevalidateEmail(`{"to_email":"a@b.com","subject":"s","content":"<html><body>hello</body></html>"}`)
	if err != nil {
		t.Fatalf("expected no error when MCP is down, got: %v", err)
	}
}

func TestPrevalidateEmailSkippedForNonJSON(t *testing.T) {
	stager := &approvalStager{
		ctx:       context.Background(),
		mcpClient: &fakeMCPClient{},
	}
	err := stager.prevalidateEmail(`not json`)
	if err != nil {
		t.Fatalf("expected no error for non-JSON input, got: %v", err)
	}
}

func TestPrevalidateEmailSkippedWhenNoContent(t *testing.T) {
	stager := &approvalStager{
		ctx:       context.Background(),
		mcpClient: &fakeMCPClient{},
	}
	err := stager.prevalidateEmail(`{"to_email":"a@b.com","subject":"s"}`)
	if err != nil {
		t.Fatalf("expected no error when no content, got: %v", err)
	}
}

func TestPrevalidateEmailAllowsTemplateExampleDataAsWarning(t *testing.T) {
	stager := &approvalStager{
		ctx: context.Background(),
		mcpClient: &fakeMCPClient{
			result: &mcp.ToolResult{
				Content: []mcp.ContentBlock{
					{Type: "text", Text: `{"valid":true,"errors":[],"warnings":["Template example data detected: Amazon US OLV, $589,075.45"]}`},
				},
			},
		},
	}
	err := stager.prevalidateEmail(`{"to_email":"a@b.com","subject":"s","content":"<html><body>Amazon US OLV spent $589,075.45</body></html>"}`)
	if err != nil {
		t.Fatalf("expected no blocking error for template example data (warning only), got: %v", err)
	}
}

// Regression: a preview_email or send_email call with no body field
// (just to_email + subject + inline_attachments) used to stage and
// render an empty iframe, prompting "I can't see the preview" from
// users. validateEmailHasContent rejects that pre-stage so the agent
// gets a clear retry signal instead. See logs/Email-Report-Analysis
// for the failure mode.
func TestValidateEmailHasContent_RejectsMissingBody(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"no content keys at all", `{"to_email":"a@b.com","subject":"hi"}`},
		{"empty content string", `{"to_email":"a@b.com","subject":"hi","content":""}`},
		{"whitespace only content", `{"to_email":"a@b.com","subject":"hi","content":"   \n\t "}`},
		{"only inline_attachments", `{"to_email":"a@b.com","subject":"hi","inline_attachments":[{"cid":"x","path":"y.png"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateEmailHasContent("preview_email", tc.raw)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			msg := err.Error()
			for _, want := range []string{"preview_email", "content", "content_file"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q missing substring %q", msg, want)
				}
			}
		})
	}
}

func TestValidateEmailHasContent_AcceptsRealBody(t *testing.T) {
	// content set inline (the post-materialize state for both
	// content-was-passed-inline and content_file-was-inlined cases).
	err := validateEmailHasContent("preview_email", `{"to_email":"a@b.com","subject":"hi","content":"<p>real body</p>"}`)
	if err != nil {
		t.Errorf("expected accept with real content, got: %v", err)
	}
	err = validateEmailHasContent("send_email", `{"to_email":"a@b.com","subject":"hi","content":"plain text body"}`)
	if err != nil {
		t.Errorf("expected accept on send_email path, got: %v", err)
	}
	// MCP-prefixed send tools (mcp_sendgrid_send_email) come through
	// the same code path because Stage matches `_send_email` suffix —
	// no need to test the suffix variant separately at this layer
	// since validateEmailHasContent doesn't switch on the tool name.
}

func TestValidateEmailHasContent_NoopForNonJSON(t *testing.T) {
	// If somehow we get a non-JSON payload (shouldn't in production —
	// fantasy serializes args itself), don't block on parse failure.
	// Mirrors prevalidateEmail's "let downstream handle" stance.
	err := validateEmailHasContent("preview_email", "not json")
	if err != nil {
		t.Errorf("expected no error on non-JSON, got: %v", err)
	}
}

// Regression: the lockdown PubMatic chat sent fine but its preview
// iframe rendered the email body with broken-image icons because
// `<img src="cid:spend_trend">` doesn't resolve outside an SMTP
// envelope. expandCidImagesToDataURLs reads the workspace file the
// inline_attachments entry points at and replaces cid: refs with
// data: URLs in the SUMMARY only — the underlying approval row
// keeps the cid: form so SendGrid's MIME assembler still attaches
// the images for the real send.
func TestExpandCidImagesToDataURLs_SubstitutesWorkspaceFile(t *testing.T) {
	convID := "conv-cid-1"
	root := t.TempDir()
	t.Setenv("CHAT_WORKSPACE_ROOT", root)
	convDir := filepath.Join(root, convID)
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	pngBytes := []byte("\x89PNG\r\n\x1a\nfake-bytes")
	if err := os.WriteFile(filepath.Join(convDir, "trend.png"), pngBytes, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	html := `<html><body><p>summary</p><img src="cid:spend_trend" alt="trend"></body></html>`
	args := map[string]any{
		"inline_attachments": []any{
			map[string]any{"cid": "spend_trend", "path": "trend.png"},
		},
	}
	out := expandCidImagesToDataURLs(html, args, convID)
	if strings.Contains(out, "cid:spend_trend") {
		t.Errorf("cid: ref still present: %q", out)
	}
	if !strings.Contains(out, "data:image/png;base64,") {
		t.Errorf("data URL not substituted: %q", out)
	}
}

// A cid: ref without a matching inline_attachments entry stays as-is
// (we don't synthesize a data URL from thin air).
func TestExpandCidImagesToDataURLs_LeavesUnmatchedCidsAlone(t *testing.T) {
	html := `<img src="cid:does_not_exist">`
	out := expandCidImagesToDataURLs(html, map[string]any{}, "conv-x")
	if out != html {
		t.Errorf("unmatched cid was modified: %q -> %q", html, out)
	}
}

// Path traversal in the inline_attachments entry must not leak host
// files into the preview summary. Same security shape as the
// workspace file API: relative paths resolve under the conv dir,
// EvalSymlinks + has-prefix rejects escapes, absolute paths outside
// the workspace are silently dropped.
func TestExpandCidImagesToDataURLs_RejectsPathTraversal(t *testing.T) {
	convID := "conv-cid-traversal"
	root := t.TempDir()
	t.Setenv("CHAT_WORKSPACE_ROOT", root)
	if err := os.MkdirAll(filepath.Join(root, convID), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Stage a real file outside the workspace. Under the unconfined
	// chat-server uid this is readable, so the substitution would
	// happily inline it without the EvalSymlinks check.
	hostFile := filepath.Join(t.TempDir(), "host.png")
	if err := os.WriteFile(hostFile, []byte("\x89PNGSECRET"), 0o644); err != nil {
		t.Fatalf("write host: %v", err)
	}

	html := `<img src="cid:x">`
	args := map[string]any{
		"inline_attachments": []any{
			map[string]any{"cid": "x", "path": "../" + filepath.Base(t.TempDir()) + "/host.png"},
		},
	}
	out := expandCidImagesToDataURLs(html, args, convID)
	if strings.Contains(out, "PNGSECRET") || strings.Contains(out, "data:image/png") {
		t.Errorf("traversal succeeded — host file inlined: %q", out)
	}

	// And an absolute path outside the workspace also fails.
	args = map[string]any{
		"inline_attachments": []any{
			map[string]any{"cid": "x", "path": hostFile},
		},
	}
	out = expandCidImagesToDataURLs(html, args, convID)
	if strings.Contains(out, "PNGSECRET") {
		t.Errorf("absolute outside-workspace path inlined: %q", out)
	}
}
