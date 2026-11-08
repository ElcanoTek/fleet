package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// These exercise the email-arg materialization used by the governed staging
// surface (the web approval stager). They need no DB and no model — just a
// workspace root.

func TestMaterializeContentFileInlinesWorkspaceFile(t *testing.T) {
	convID := "conv-mat-1"

	// Point the workspace root at a temp dir so we don't pollute ./workspace.
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
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
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
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
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
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
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
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
	t.Setenv("FLEET_WORKSPACE_ROOT", root)

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
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
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
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
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
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
	// Absolute and already inside the workspace root (a shared-library file
	// staged directly under the root, say): passes through unchanged.
	abs := filepath.Join(root, "shared", "already-absolute.png")
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
		t.Errorf("absolute in-root path should pass through unchanged: want %q, got %q", abs, path)
	}
}

// TestMaterializeAttachmentPathsRefusesEscapes: the connector opens attachment
// paths host-side, so a model-authored path must stay under the workspace
// root. Three ways out used to be open: an absolute host path, a `..` walk,
// and $VAR expansion — os.ExpandEnv substituted the VALUE of any fleet env var
// (every connector secret the host holds) into the persisted approval args.
func TestMaterializeAttachmentPathsRefusesEscapes(t *testing.T) {
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
	t.Setenv("FLEET_TEST_FAKE_SECRET", "leaked-host-env-value")

	cases := map[string]string{
		"absolute host file":    "/etc/fleet/fleet.env",
		"dot-dot walk":          "../../etc/passwd",
		"env var stays literal": "$FLEET_TEST_FAKE_SECRET/report.csv",
		"tilde stays literal":   "~/report.csv",
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"attachments": []map[string]any{{"path": p}}})
			out, err := MaterializeAttachmentPaths("conv-escape", string(raw))
			switch name {
			case "absolute host file", "dot-dot walk":
				if err == nil {
					t.Fatalf("expected a containment error for %q, got %s", p, out)
				}
				if strings.Contains(err.Error(), "leaked-host-env-value") {
					t.Fatalf("error leaked a secret: %v", err)
				}
			default:
				// `$VAR` and `~` are just characters in a filename now: the
				// path is anchored under the conversation workspace verbatim,
				// and the env var's VALUE appears nowhere.
				if err != nil {
					t.Fatalf("literal path %q should be accepted, got %v", p, err)
				}
				if strings.Contains(out, "leaked-host-env-value") {
					t.Fatalf("env var value leaked into the args: %s", out)
				}
				var got map[string]any
				_ = json.Unmarshal([]byte(out), &got)
				path := got["attachments"].([]any)[0].(map[string]any)["path"].(string)
				if want := filepath.Join(root, "conv-escape", p); path != want {
					t.Fatalf("path = %q, want %q (literal, anchored in the workspace)", path, want)
				}
			}
		})
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
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
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

// attachmentWorkspace builds a workspace root with two conversations and a
// file outside the root, for the attachment containment tests.
func attachmentWorkspace(t *testing.T) (root, outside string) {
	t.Helper()
	root = t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
	for _, conv := range []string{"conv-a", "conv-b"} {
		if err := os.MkdirAll(filepath.Join(root, conv), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "conv-a", "report.csv"), []byte("a,b\n1,2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "conv-b", "notes.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside = filepath.Join(t.TempDir(), "host.txt")
	if err := os.WriteFile(outside, []byte("host"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, outside
}

func attachArgs(paths ...string) string {
	items := make([]map[string]any, 0, len(paths))
	for _, p := range paths {
		items = append(items, map[string]any{"path": p})
	}
	raw, _ := json.Marshal(map[string]any{"subject": "s", "content": "c", "attachments": items})
	return string(raw)
}

func TestMaterializeAttachmentPathsResolvesThroughConversationWorkspace(t *testing.T) {
	root, outside := attachmentWorkspace(t)
	if err := os.Symlink(outside, filepath.Join(root, "conv-a", "linked.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "conv-b", "notes.txt"), filepath.Join(root, "conv-a", "sibling.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "conv-a", "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := MaterializeAttachmentPaths("conv-a", attachArgs("report.csv")); err != nil {
		t.Fatalf("a file in the conversation workspace should be accepted: %v", err)
	}
	for name, p := range map[string]string{
		"symlink leaving the root":         "linked.txt",
		"symlink into another workspace":   "sibling.txt",
		"another conversation by dot-dot":  "../conv-b/notes.txt",
		"another conversation by absolute": filepath.Join(root, "conv-b", "notes.txt"),
		"a directory":                      "subdir",
	} {
		t.Run(name, func(t *testing.T) {
			if out, err := MaterializeAttachmentPaths("conv-a", attachArgs(p)); err == nil {
				t.Fatalf("expected %q to be refused, got %s", p, out)
			}
		})
	}
}

func TestMaterializeAttachmentPathsAcceptsSharedLibraryFiles(t *testing.T) {
	root, _ := attachmentWorkspace(t)
	shared := filepath.Join(root, SharedFilesDirName)
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shared, "deck.pdf"), []byte("%PDF"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The conversation reaches the library through its `shared` link.
	if err := os.Symlink(shared, filepath.Join(root, "conv-a", SharedFilesDirName)); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeAttachmentPaths("conv-a", attachArgs("shared/deck.pdf")); err != nil {
		t.Fatalf("a shared-library file reached through the conversation link should be accepted: %v", err)
	}
	if _, err := MaterializeAttachmentPaths("conv-a", attachArgs(filepath.Join(shared, "deck.pdf"))); err != nil {
		t.Fatalf("an absolute shared-library path should be accepted: %v", err)
	}
}

func TestSnapshotEmailAttachmentsCopiesTheResolvedFile(t *testing.T) {
	root, _ := attachmentWorkspace(t)
	staged, err := MaterializeAttachmentPaths("conv-a", attachArgs("report.csv"))
	if err != nil {
		t.Fatal(err)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(staged), &args); err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	cleanup, err := SnapshotEmailAttachments("conv-a", args, staging)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	got := args["attachments"].([]any)[0].(map[string]any)["path"].(string)
	if !strings.HasPrefix(got, staging+string(filepath.Separator)) || filepath.Base(got) != "report.csv" {
		t.Fatalf("path should name a copy under the staging dir keeping its base name, got %q", got)
	}
	if strings.HasPrefix(got, root) {
		t.Fatalf("copy must not live under the workspace root: %q", got)
	}
	if b, err := os.ReadFile(got); err != nil || string(b) != "a,b\n1,2\n" {
		t.Fatalf("copy content = %q, %v", b, err)
	}
	cleanup()
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Fatalf("cleanup should remove the copy, stat err = %v", err)
	}
}

func TestSnapshotEmailAttachmentsRechecksBeforeSending(t *testing.T) {
	root, outside := attachmentWorkspace(t)
	staged, err := MaterializeAttachmentPaths("conv-a", attachArgs("report.csv"))
	if err != nil {
		t.Fatal(err)
	}
	// The file is replaced by a link after the card was staged.
	p := filepath.Join(root, "conv-a", "report.csv")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, p); err != nil {
		t.Fatal(err)
	}
	var args map[string]any
	_ = json.Unmarshal([]byte(staged), &args)
	if _, err := SnapshotEmailAttachments("conv-a", args, t.TempDir()); err == nil {
		t.Fatal("a path that now resolves outside the workspace must be refused at send time")
	}
}

// An attachment directory that is a symlink into a workspace is compared by
// where it resolves, not how it is written: the copies would otherwise land
// somewhere a sandbox mounts.
func TestSnapshotEmailAttachmentsRefusesStagingLinkedIntoWorkspace(t *testing.T) {
	root, _ := attachmentWorkspace(t)
	for name, target := range map[string]string{
		"workspace root":         root,
		"conversation workspace": filepath.Join(root, "conv-a"),
		"other conversation":     filepath.Join(root, "conv-b"),
	} {
		t.Run(name, func(t *testing.T) {
			link := filepath.Join(t.TempDir(), "attachments")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			args := map[string]any{"attachments": []any{map[string]any{"path": "report.csv"}}}
			cleanup, err := SnapshotEmailAttachments("conv-a", args, OutboundAttachmentStagingDir(link))
			if err == nil {
				cleanup()
				t.Fatal("a staging directory that resolves into the workspace must be refused")
			}
			if !strings.Contains(err.Error(), "outside the workspace root") {
				t.Fatalf("error = %v, want the staging refusal", err)
			}
			entries, _ := os.ReadDir(filepath.Join(target, "outbound"))
			if len(entries) != 0 {
				t.Fatalf("refused staging left %d entries behind", len(entries))
			}
		})
	}
	// A linked attachment directory that resolves outside every workspace
	// is still accepted.
	link := filepath.Join(t.TempDir(), "attachments")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	args := map[string]any{"attachments": []any{map[string]any{"path": "report.csv"}}}
	cleanup, err := SnapshotEmailAttachments("conv-a", args, OutboundAttachmentStagingDir(link))
	if err != nil {
		t.Fatalf("a linked staging directory outside the workspace was refused: %v", err)
	}
	cleanup()
}

func TestSnapshotEmailAttachmentsRefusesNonRegularFiles(t *testing.T) {
	root, _ := attachmentWorkspace(t)
	if err := syscall.Mkfifo(filepath.Join(root, "conv-a", "pipe"), 0o644); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	args := map[string]any{"attachments": []any{map[string]any{"path": filepath.Join(root, "conv-a", "pipe")}}}
	if _, err := SnapshotEmailAttachments("conv-a", args, t.TempDir()); err == nil {
		t.Fatal("a FIFO must be refused")
	}
	if _, err := MaterializeAttachmentPaths("conv-a", attachArgs("pipe")); err == nil {
		t.Fatal("a FIFO must be refused at staging too")
	}
}

func TestSnapshotEmailAttachmentsSizeBound(t *testing.T) {
	root, _ := attachmentWorkspace(t)
	big := filepath.Join(root, "conv-a", "big.bin")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxEmailAttachmentBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	args := map[string]any{"attachments": []any{map[string]any{"path": big}}}
	if _, err := SnapshotEmailAttachments("conv-a", args, t.TempDir()); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("an over-limit attachment must be refused, got %v", err)
	}
}

// A scheduled run's attachments resolve through its own working directory
// and the shared-file library (reached through the job directory's `shared`
// link, as the scheduled sandbox mounts it): another job's or a
// conversation's file, a link out of the job directory, and an absolute path
// elsewhere are refused; a job file and a shared file are copied.
func TestSnapshotEmailAttachmentsInDir(t *testing.T) {
	wsRoot := t.TempDir()
	job := filepath.Join(wsRoot, "tasks", "job-a")
	for _, d := range []string{job, filepath.Join(wsRoot, "tasks", "job-b"), filepath.Join(wsRoot, "conv-1"), filepath.Join(wsRoot, "shared")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(job, "out.csv"), "job")
	write(filepath.Join(wsRoot, "tasks", "job-b", "b.csv"), "b")
	write(filepath.Join(wsRoot, "conv-1", "c.txt"), "c")
	write(filepath.Join(wsRoot, "shared", "lib.pdf"), "lib")
	if err := os.Symlink(filepath.Join(wsRoot, "shared"), filepath.Join(job, "shared")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(wsRoot, "conv-1", "c.txt"), filepath.Join(job, "c-link.txt")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "o.txt")
	write(outside, "o")
	staging := filepath.Join(t.TempDir(), "outbound")
	args := func(path string) map[string]any {
		return map[string]any{"attachments": []any{map[string]any{"path": path}}}
	}

	for _, p := range []string{"../job-b/b.csv", filepath.Join(wsRoot, "conv-1", "c.txt"), "c-link.txt", outside} {
		if _, err := SnapshotEmailAttachmentsInDir(wsRoot, job, args(p), staging); err == nil {
			t.Fatalf("%s: accepted", p)
		}
	}
	for p, want := range map[string]string{"out.csv": "job", "shared/lib.pdf": "lib"} {
		a := args(p)
		cleanup, err := SnapshotEmailAttachmentsInDir(wsRoot, job, a, staging)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		got := a["attachments"].([]any)[0].(map[string]any)["path"].(string)
		if data, err := os.ReadFile(got); err != nil || string(data) != want || !strings.HasPrefix(got, staging) {
			t.Fatalf("%s: copy %q content %q err %v", p, got, data, err)
		}
		cleanup()
		if _, err := os.Stat(got); !os.IsNotExist(err) {
			t.Fatalf("%s: copy not removed", p)
		}
	}
	// A staging directory inside the workspace root is refused.
	if _, err := SnapshotEmailAttachmentsInDir(wsRoot, job, args("out.csv"), filepath.Join(wsRoot, "outbound")); err == nil {
		t.Fatal("staging inside the workspace root was accepted")
	}
	// No working directory: attachments are refused, a plain email passes.
	if _, err := SnapshotEmailAttachmentsInDir(wsRoot, "", args("out.csv"), staging); err == nil {
		t.Fatal("attachments accepted with no working directory")
	}
	if _, err := SnapshotEmailAttachmentsInDir(wsRoot, "", map[string]any{"subject": "s"}, staging); err != nil {
		t.Fatalf("plain email: %v", err)
	}
}
