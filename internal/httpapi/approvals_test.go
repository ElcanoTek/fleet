package httpapi

import (
	"context"
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
