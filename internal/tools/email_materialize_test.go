package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These exercise the email-arg materialization used by the governed staging
// surface (the web approval stager). They need no DB and no model — just a
// workspace root.

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
	out, err := MaterializeContentFile(convID, raw)
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

func TestMaterializeContentFileAbsolutePathInsideWorkspace(t *testing.T) {
	// An absolute path is fine as long as it lives INSIDE the conversation
	// workspace — agents often echo back the absolute path a previous tool
	// returned. Absolute paths outside it are rejected (see the traversal test).
	root := t.TempDir()
	t.Setenv("CHAT_WORKSPACE_ROOT", root)
	convID := "conv-abs"
	if err := os.MkdirAll(filepath.Join(root, convID), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(root, convID, "email.html")
	body := "<p>absolute</p>"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	raw, _ := json.Marshal(map[string]any{
		"to_email":     "a@b.com",
		"subject":      "s",
		"content_file": path,
	})
	out, err := MaterializeContentFile(convID, string(raw))
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal([]byte(out), &got)
	if content, _ := got["content"].(string); content != body {
		t.Errorf("content mismatch: want %q, got %q", body, content)
	}
}

// TestMaterializeContentFileRejectsEscapes pins the #573 fix: content_file is
// model-controlled and its bytes become an OUTBOUND email body, so it must not
// be able to name anything outside the conversation workspace — neither via an
// absolute path (/proc/self/environ holds the process env, i.e. host secrets)
// nor via a '..'-escaping relative path.
func TestMaterializeContentFileRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CHAT_WORKSPACE_ROOT", root)
	convID := "conv-escape"
	if err := os.MkdirAll(filepath.Join(root, convID), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A sibling conversation's secret, reachable pre-fix via "../other/…".
	if err := os.MkdirAll(filepath.Join(root, "other-conv"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "other-conv", "secret.html"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	for _, file := range []string{
		"/proc/self/environ",               // absolute: the fleet process env = host secrets
		"/etc/passwd",                      // absolute, outside the workspace
		"../other-conv/secret.html",        // relative '..' escape into a sibling conversation
		"../../../../etc/passwd",           // relative '..' escape off the workspace root
		"sub/../../other-conv/secret.html", // '..' hidden mid-path
	} {
		raw, _ := json.Marshal(map[string]any{"subject": "s", "content_file": file})
		out, err := MaterializeContentFile(convID, string(raw))
		if err == nil {
			t.Errorf("content_file %q: want rejection, got success: %s", file, out)
		}
	}
}

// TestMaterializeContentFileNoEnvOrTildeExpansion proves $VAR and ~/ in a
// model-supplied content_file are treated as LITERAL filename bytes, never
// expanded through the fleet process environment (pre-#573 the path went
// through os.ExpandEnv + UserHomeDir).
func TestMaterializeContentFileNoEnvOrTildeExpansion(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CHAT_WORKSPACE_ROOT", root)
	convID := "conv-noexpand"
	if err := os.MkdirAll(filepath.Join(root, convID), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// A file literally named "$SECRET_DIR.html" in the workspace: with the old
	// os.ExpandEnv behavior (and SECRET_DIR set) the lookup would miss it.
	t.Setenv("SECRET_DIR", "/etc")
	body := "<p>literal</p>"
	if err := os.WriteFile(filepath.Join(root, convID, "$SECRET_DIR.html"), []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	raw := `{"subject":"s","content_file":"$SECRET_DIR.html"}`
	out, err := MaterializeContentFile(convID, raw)
	if err != nil {
		t.Fatalf("materialize literal $VAR filename: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal([]byte(out), &got)
	if got["content"].(string) != body {
		t.Errorf("content should be the literal file's bytes, got: %v", got["content"])
	}

	// "~/..." must not resolve to the real home dir — it stays a literal
	// relative path inside the workspace, which here does not exist.
	raw = `{"subject":"s","content_file":"~/.aws/credentials"}`
	if out, err := MaterializeContentFile(convID, raw); err == nil {
		t.Errorf("~/ path: want rejection (no home expansion), got success: %s", out)
	}
}

func TestMaterializeContentFileMissingFileErrors(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CHAT_WORKSPACE_ROOT", root)

	raw := `{"to_email":"x@y.com","subject":"s","content_file":"nope.html"}`
	_, err := MaterializeContentFile("conv-missing", raw)
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !strings.Contains(err.Error(), "nope.html") {
		t.Errorf("error should name the file, got: %v", err)
	}
}

func TestMaterializeContentFilePrefersFileOverInlineContent(t *testing.T) {
	// When both content and content_file are set, content_file takes precedence —
	// matching the tool descriptions and MCP server. The file is read and replaces
	// inline content.
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
	out, err := MaterializeContentFile(convID, raw)
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
	out, err := MaterializeContentFile("conv-noop", raw)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if out != raw {
		t.Errorf("expected untouched input, got %s", out)
	}
}

func TestMaterializeAttachmentPathsRewritesRelativePaths(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CHAT_WORKSPACE_ROOT", root)
	convID := "conv-attach"

	raw := `{"to_email":"brad@x.com","subject":"s","content":"<img src=\"cid:chart\">","inline_attachments":[{"path":"daily_performance.png","cid":"chart"}],"attachments":[{"path":"report.csv"}]}`
	out, err := MaterializeAttachmentPaths(convID, raw)
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
	out, err := MaterializeAttachmentPaths("conv-abs-attach", string(raw))
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
	out, err := MaterializeAttachmentPaths("conv-noop-attach", raw)
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
	big := make([]byte, MaxInlinedContentBytes+1)
	if err := os.WriteFile(filepath.Join(root, convID, "big.html"), big, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	raw := `{"subject":"s","content_file":"big.html"}`
	_, err := MaterializeContentFile(convID, raw)
	if err == nil || !strings.Contains(err.Error(), "inline cap") {
		t.Errorf("expected inline-cap error, got: %v", err)
	}
}

func TestIsEmailToolName(t *testing.T) {
	for _, name := range []string{"send_email", "preview_email", "mcp_sendgrid_send_email"} {
		if !IsEmailToolName(name) {
			t.Errorf("IsEmailToolName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"bash", "run_python", "mcp_acme_lookup", "send_emailer"} {
		if IsEmailToolName(name) {
			t.Errorf("IsEmailToolName(%q) = true, want false", name)
		}
	}
}
