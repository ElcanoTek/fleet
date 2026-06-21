package agentcore

// Merged from chat + cutlass mcp_fastio_guard_test.go. The headline assertion
// is the parameterization win: with DefaultRemediationHints the rejection hint
// exposes BOTH remediation paths — the cutlass native `fastio_upload_file` tool
// AND the chat blob upload flow (create-session → chunk → finalize) — so both
// front-ends' expectations are satisfied by one guard.

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/mcp"
)

// TestRejectFastIOInlineBase64Upload_ExposesBothRemediationHints is the merge
// gate: the rejection hint must name BOTH the native tool (cutlass) and the
// blob upload flow (chat).
func TestRejectFastIOInlineBase64Upload_ExposesBothRemediationHints(t *testing.T) {
	args := map[string]any{
		"action":         "stream-upload",
		"filename":       "doc.docx",
		"profile_type":   "workspace",
		"profile_id":     "4817763504744262145",
		"content_base64": strings.Repeat("A", fastIOInlineUploadByteCap+1),
	}
	ok, hint := rejectFastIOInlineBase64Upload(fastIOUploadToolName, args, DefaultRemediationHints)
	if ok {
		t.Fatal("expected rejection for oversized content_base64")
	}
	// cutlass native-tool hint AND chat blob-flow hint, plus the shared markers.
	for _, want := range []string{
		"fastio_upload_file", // cutlass native tool
		"blob",               // chat blob upload flow
		"create-session",
		"chunk",
		"finalize",
		"stream-upload",
		"content_base64",
	} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint missing %q\n--- hint ---\n%s", want, hint)
		}
	}
}

// TestRejectFastIOInlineBase64Upload_BlobFlowOnlyHint asserts a chat-style
// hints config (no native tool, blob flow only) still produces the chat hint.
func TestRejectFastIOInlineBase64Upload_BlobFlowOnlyHint(t *testing.T) {
	args := map[string]any{
		"action":         "stream-upload",
		"content_base64": strings.Repeat("A", fastIOInlineUploadByteCap+1),
	}
	ok, hint := rejectFastIOInlineBase64Upload(fastIOUploadToolName, args, RemediationHints{IncludeBlobFlow: true})
	if ok {
		t.Fatal("expected rejection")
	}
	if strings.Contains(hint, "fastio_upload_file") {
		t.Errorf("blob-flow-only hint should not mention the native tool, got: %s", hint)
	}
	for _, want := range []string{"blob", "create-session", "chunk", "finalize"} {
		if !strings.Contains(hint, want) {
			t.Errorf("blob-flow hint missing %q\n%s", want, hint)
		}
	}
}

func TestRejectFastIOInlineBase64Upload_AcceptsSmallInlineContent(t *testing.T) {
	args := map[string]any{
		"action":         "stream-upload",
		"content_base64": strings.Repeat("A", 1024),
	}
	ok, hint := rejectFastIOInlineBase64Upload(fastIOUploadToolName, args, DefaultRemediationHints)
	if !ok {
		t.Fatalf("small inline upload should pass; rejected with: %s", hint)
	}
}

func TestRejectFastIOInlineBase64Upload_AcceptsBlobFlow(t *testing.T) {
	for _, refField := range []string{"blob_id", "blob_ref"} {
		args := map[string]any{
			"action":         "chunk",
			"session_id":     "sess-1",
			refField:         "blob-abc-123",
			"content_base64": strings.Repeat("A", fastIOInlineUploadByteCap+1),
		}
		ok, hint := rejectFastIOInlineBase64Upload(fastIOUploadToolName, args, DefaultRemediationHints)
		if !ok {
			t.Fatalf("upload via %s should pass even with stray content_base64; rejected with: %s", refField, hint)
		}
	}
}

func TestRejectFastIOInlineBase64Upload_RejectsOversizedPlainContent(t *testing.T) {
	args := map[string]any{
		"action":  "stream-upload",
		"content": strings.Repeat("A", fastIOInlineUploadByteCap+1),
	}
	ok, hint := rejectFastIOInlineBase64Upload(fastIOUploadToolName, args, DefaultRemediationHints)
	if ok {
		t.Fatal("expected rejection for oversized inline content")
	}
	if !strings.Contains(hint, "`content`") {
		t.Errorf("hint should name the offending field 'content', got: %s", hint)
	}
}

func TestRejectFastIOInlineBase64Upload_IgnoresOtherTools(t *testing.T) {
	args := map[string]any{
		"content_base64": strings.Repeat("A", fastIOInlineUploadByteCap+1),
	}
	ok, _ := rejectFastIOInlineBase64Upload("mcp_gamma_upload_asset", args, DefaultRemediationHints)
	if !ok {
		t.Fatal("guard leaked outside mcp_fast_io_upload")
	}
}

func TestRejectFastIOInlineBase64Upload_SafeOnNilArgs(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("guard panicked on nil args: %v", r)
		}
	}()
	ok, _ := rejectFastIOInlineBase64Upload(fastIOUploadToolName, nil, DefaultRemediationHints)
	if !ok {
		t.Fatal("nil args should be a no-op")
	}
}

func TestRejectFastIOInlineBase64Upload_DefaultsActionInHint(t *testing.T) {
	args := map[string]any{
		"content_base64": strings.Repeat("A", fastIOInlineUploadByteCap+1),
	}
	ok, hint := rejectFastIOInlineBase64Upload(fastIOUploadToolName, args, DefaultRemediationHints)
	if ok {
		t.Fatal("expected rejection")
	}
	if strings.Contains(hint, "action=`") || strings.Contains(hint, "action= ") {
		t.Errorf("hint should fill in a default action name, got: %s", hint)
	}
	if !strings.Contains(hint, "action=stream-upload") {
		t.Errorf("hint should default action to stream-upload, got: %s", hint)
	}
}

func TestFastIOServerEnabled_DetectsByServerName(t *testing.T) {
	cases := []struct {
		name string
		in   []mcp.ServerTool
		want bool
	}{
		{"empty", nil, false},
		{"no fast_io", []mcp.ServerTool{
			{ServerName: "sendgrid", Tool: mcp.Tool{Name: "send_email"}},
			{ServerName: "openx_mcp", Tool: mcp.Tool{Name: "openx_auth_status"}},
		}, false},
		{"fast_io present", []mcp.ServerTool{
			{ServerName: "sendgrid", Tool: mcp.Tool{Name: "send_email"}},
			{ServerName: "fast_io", Tool: mcp.Tool{Name: "upload"}},
		}, true},
		{"fast_io only", []mcp.ServerTool{
			{ServerName: "fast_io", Tool: mcp.Tool{Name: "workspace"}},
		}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := fastIOServerEnabled(c.in); got != c.want {
				t.Errorf("fastIOServerEnabled(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}
