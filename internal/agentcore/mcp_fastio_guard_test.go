package agentcore

// Merged from chat + cutlass mcp_fastio_guard_test.go. The rejection hint names
// the blob flow alone — fleet ships no upload tool of its own since the fast.io
// tooling moved to the client bundle.

import (
	"strings"
	"testing"
)

// TestRejectFastIOInlineBase64Upload_DefaultHintIsBlobFlowOnly pins the hint:
// the blob flow, and no native-tool line. fleet used to hardcode
// `fastio_upload_file` here, which became a pointer to a tool that no longer
// exists in this repo — a rejection hint that names an unreachable tool is worse
// than one that names none, because the model burns a turn trying it.
func TestRejectFastIOInlineBase64Upload_DefaultHintIsBlobFlowOnly(t *testing.T) {
	args := map[string]any{
		"action":         "stream-upload",
		"filename":       "doc.docx",
		"profile_type":   "workspace",
		"profile_id":     "1234567890000000000",
		"content_base64": strings.Repeat("A", fastIOInlineUploadByteCap+1),
	}
	ok, hint := rejectFastIOInlineBase64Upload(fastIOUploadToolName, args)
	if ok {
		t.Fatal("expected rejection for oversized content_base64")
	}
	for _, want := range []string{"blob", "create-session", "chunk", "finalize", "stream-upload", "content_base64"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint missing %q\n--- hint ---\n%s", want, hint)
		}
	}
	if strings.Contains(hint, "fastio_upload_file") {
		t.Errorf("default hint names a tool fleet no longer ships:\n%s", hint)
	}
}

func TestRejectFastIOInlineBase64Upload_AcceptsSmallInlineContent(t *testing.T) {
	args := map[string]any{
		"action":         "stream-upload",
		"content_base64": strings.Repeat("A", 1024),
	}
	ok, hint := rejectFastIOInlineBase64Upload(fastIOUploadToolName, args)
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
		ok, hint := rejectFastIOInlineBase64Upload(fastIOUploadToolName, args)
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
	ok, hint := rejectFastIOInlineBase64Upload(fastIOUploadToolName, args)
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
	ok, _ := rejectFastIOInlineBase64Upload("mcp_gamma_upload_asset", args)
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
	ok, _ := rejectFastIOInlineBase64Upload(fastIOUploadToolName, nil)
	if !ok {
		t.Fatal("nil args should be a no-op")
	}
}

func TestRejectFastIOInlineBase64Upload_DefaultsActionInHint(t *testing.T) {
	args := map[string]any{
		"content_base64": strings.Repeat("A", fastIOInlineUploadByteCap+1),
	}
	ok, hint := rejectFastIOInlineBase64Upload(fastIOUploadToolName, args)
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
