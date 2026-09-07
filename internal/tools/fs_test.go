//go:build fleet_host_executor

package tools

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ElcanoTek/fleet/internal/sandbox"
)

// fsTestSandbox returns a host-backed sandbox for the file tools (#784). The
// file tools now execute through the sandbox FileOp seam; the host backend runs
// the same embedded fileops.py via python3, so these tests exercise the real
// executor without podman. Skips if python3 is unavailable.
func fsTestSandbox(t *testing.T) *sandbox.Sandbox {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available; file tools execute through the sandbox fileop seam")
	}
	sb := sandbox.NewHost(nil)
	t.Cleanup(sb.Close)
	return sb
}

func TestWriteFileTool(t *testing.T) {
	sb := fsTestSandbox(t)
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")

	result, err := runWriteFile(context.Background(), sb, WriteFileParams{
		Path:    testFile,
		Content: "Hello, World!",
	})
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	if !strings.Contains(result, "Successfully wrote") {
		t.Errorf("Expected success message, got %s", result)
	}
	content, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("Failed to read created file: %v", err)
	}
	if string(content) != "Hello, World!" {
		t.Errorf("Expected 'Hello, World!', got %s", string(content))
	}
	// The seam writes 0600 (atomic temp + rename + chmod), matching the
	// pre-#784 host behavior.
	if info, _ := os.Stat(testFile); info != nil && info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestEditFileTool(t *testing.T) {
	sb := fsTestSandbox(t)
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "test.txt")
	if err := os.WriteFile(testFile, []byte("Hello, World!"), 0o644); err != nil {
		t.Fatalf("Failed to write test file: %v", err)
	}

	result, err := runEditFile(context.Background(), sb, EditFileParams{
		Path:    testFile,
		OldText: "World",
		NewText: "Go",
	})
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	if !strings.Contains(result, "Successfully replaced") {
		t.Errorf("Expected success message, got %s", result)
	}
	content, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("Failed to read edited file: %v", err)
	}
	if string(content) != "Hello, Go!" {
		t.Errorf("Expected 'Hello, Go!', got %s", string(content))
	}
}

func TestEditFileTool_OldTextAbsentLeavesFileUnchanged(t *testing.T) {
	sb := fsTestSandbox(t)
	testFile := filepath.Join(t.TempDir(), "test.txt")
	if err := os.WriteFile(testFile, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := runEditFile(context.Background(), sb, EditFileParams{Path: testFile, OldText: "absent", NewText: "x"})
	if err == nil || !strings.Contains(err.Error(), "old_text not found") {
		t.Fatalf("want old_text-not-found error, got %v", err)
	}
	if got, _ := os.ReadFile(testFile); string(got) != "original" {
		t.Errorf("file changed on failed edit: %q", got)
	}
}

func TestViewFileTool(t *testing.T) {
	sb := fsTestSandbox(t)
	testFile := filepath.Join(t.TempDir(), "test.txt")
	expected := "Test content"
	if err := os.WriteFile(testFile, []byte(expected), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := runViewFile(context.Background(), sb, ViewFileParams{Path: testFile})
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	// #787: view_file appends a sha256/size metadata trailer.
	if !strings.HasPrefix(result, expected) {
		t.Errorf("Expected content prefix %q, got %q", expected, result)
	}
	if !strings.Contains(result, "sha256=") {
		t.Errorf("expected a sha256 metadata trailer, got %q", result)
	}
}

func TestViewFileTool_OffsetLimit(t *testing.T) {
	sb := fsTestSandbox(t)
	testFile := filepath.Join(t.TempDir(), "test.txt")
	if err := os.WriteFile(testFile, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := runViewFile(context.Background(), sb, ViewFileParams{Path: testFile, Limit: 5})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if !strings.HasPrefix(res, "01234") || !strings.Contains(res, "reading limit") {
		t.Errorf("limit 5: got %q", res)
	}

	res, err = runViewFile(context.Background(), sb, ViewFileParams{Path: testFile, Offset: 5})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	// Read-to-end still appends the #787 metadata trailer.
	if !strings.HasPrefix(res, "56789") || !strings.Contains(res, "sha256=") {
		t.Errorf("offset 5: got %q", res)
	}

	res, err = runViewFile(context.Background(), sb, ViewFileParams{Path: testFile, Offset: 2, Limit: 3})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if !strings.HasPrefix(res, "234") || !strings.Contains(res, "reading limit") {
		t.Errorf("offset 2 limit 3: got %q", res)
	}
}

// TestEditFileTool_Safety pins the #787 tool-layer contract: ambiguous matches
// are rejected, a stale expected_hash fails safely, a no-op is rejected, and a
// successful edit returns a diff + old/new hashes — all without a host fallback.
func TestEditFileTool_Safety(t *testing.T) {
	sb := fsTestSandbox(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "code.txt")
	if err := os.WriteFile(path, []byte("x x x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Ambiguous single edit → rejected with the match count, file unchanged.
	_, err := runEditFile(ctx, sb, EditFileParams{Path: path, OldText: "x", NewText: "y"})
	if err == nil || !strings.Contains(err.Error(), "matches 3 locations") {
		t.Fatalf("ambiguous edit: want a 3-locations error, got %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "x x x" {
		t.Errorf("file changed on ambiguous edit: %q", got)
	}

	// No-op (old==new) rejected client-side.
	if _, err := runEditFile(ctx, sb, EditFileParams{Path: path, OldText: "x", NewText: "x"}); err == nil ||
		!strings.Contains(err.Error(), "no-op") {
		t.Errorf("no-op edit: want rejection, got %v", err)
	}

	// replace_all succeeds and returns a diff + hashes.
	out, err := runEditFile(ctx, sb, EditFileParams{Path: path, OldText: "x", NewText: "y", ReplaceAll: true})
	if err != nil {
		t.Fatalf("replace_all edit: %v", err)
	}
	for _, want := range []string{"Successfully replaced 3", "old_sha256:", "new_sha256:", "@@"} {
		if !strings.Contains(out, want) {
			t.Errorf("edit response missing %q; got %q", want, out)
		}
	}

	// Stale guard: a wrong expected_hash fails without touching the file.
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = runEditFile(ctx, sb, EditFileParams{Path: path, OldText: "hello", NewText: "world", ExpectedHash: "0000"})
	if err == nil || !strings.Contains(err.Error(), "changed since") {
		t.Fatalf("stale edit: want changed-since error, got %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "hello" {
		t.Errorf("file changed on stale edit: %q", got)
	}

	// A view_file hash round-trips as a valid expected_hash.
	view, err := runViewFile(ctx, sb, ViewFileParams{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	hash := extractSHA(t, view)
	if _, err := runEditFile(ctx, sb, EditFileParams{Path: path, OldText: "hello", NewText: "world", ExpectedHash: hash}); err != nil {
		t.Fatalf("edit with view_file's hash should succeed: %v", err)
	}
}

// extractSHA pulls the sha256=<hex> value from a view_file metadata trailer.
func extractSHA(t *testing.T, viewOutput string) string {
	t.Helper()
	i := strings.Index(viewOutput, "sha256=")
	if i < 0 {
		t.Fatalf("no sha256 in view output: %q", viewOutput)
	}
	rest := viewOutput[i+len("sha256="):]
	end := strings.IndexAny(rest, " \n")
	if end < 0 {
		end = len(rest)
	}
	return rest[:end]
}

// TestFileToolsFailClosedWithoutSandbox pins the #784 invariant: with no
// sandbox the tools error and mutate nothing — there is no host fallback.
func TestFileToolsFailClosedWithoutSandbox(t *testing.T) {
	testFile := filepath.Join(t.TempDir(), "test.txt")
	ctx := context.Background()

	if _, err := runWriteFile(ctx, nil, WriteFileParams{Path: testFile, Content: "x"}); err == nil ||
		!strings.Contains(err.Error(), "requires a sandbox") {
		t.Errorf("write_file with nil sandbox: want 'requires a sandbox', got %v", err)
	}
	if _, err := os.Stat(testFile); !os.IsNotExist(err) {
		t.Error("write_file with nil sandbox created a file on the host")
	}

	if err := os.WriteFile(testFile, []byte("orig"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runEditFile(ctx, nil, EditFileParams{Path: testFile, OldText: "orig", NewText: "x"}); err == nil ||
		!strings.Contains(err.Error(), "requires a sandbox") {
		t.Errorf("edit_file with nil sandbox: want 'requires a sandbox', got %v", err)
	}
	if got, _ := os.ReadFile(testFile); string(got) != "orig" {
		t.Error("edit_file with nil sandbox mutated the host file")
	}
	if _, err := runViewFile(ctx, nil, ViewFileParams{Path: testFile}); err == nil ||
		!strings.Contains(err.Error(), "requires a sandbox") {
		t.Errorf("view_file with nil sandbox: want 'requires a sandbox', got %v", err)
	}
}

// TestFileToolsRejectCrossConversationTraversal pins the #575 fix at the tool
// level: the file tools must refuse a relative path that escapes the caller's
// conversation workspace via "..". The path validation runs (host-side, as
// defense-in-depth input) before the operation reaches the sandbox.
func TestFileToolsRejectCrossConversationTraversal(t *testing.T) {
	sb := fsTestSandbox(t)
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
	victimConv := "conv-victim"
	victimDir := filepath.Join(root, victimConv)
	if err := os.MkdirAll(victimDir, 0o755); err != nil {
		t.Fatalf("mkdir victim workspace: %v", err)
	}
	secret := filepath.Join(victimDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("cross-tenant secret"), 0o600); err != nil {
		t.Fatalf("seed secret: %v", err)
	}

	ctx := WithConversationID(context.Background(), "conv-attacker")
	escape := "../" + victimConv + "/secret.txt"

	if out, err := runViewFile(ctx, sb, ViewFileParams{Path: escape}); err == nil {
		t.Errorf("view_file read a sibling conversation's file: %q", out)
	} else if !strings.Contains(err.Error(), "path validation failed") {
		t.Errorf("view_file: want path validation error, got: %v", err)
	}

	if _, err := runEditFile(ctx, sb, EditFileParams{Path: escape, OldText: "secret", NewText: "pwn"}); err == nil {
		t.Error("edit_file edited a sibling conversation's file")
	}

	if _, err := runWriteFile(ctx, sb, WriteFileParams{Path: "../" + victimConv + "/planted.txt", Content: "pwn"}); err == nil {
		t.Error("write_file wrote into a sibling conversation's workspace")
	}
	if _, err := os.Stat(filepath.Join(victimDir, "planted.txt")); !os.IsNotExist(err) {
		t.Errorf("planted.txt must not exist in the victim workspace (stat err = %v)", err)
	}

	got, err := os.ReadFile(secret)
	if err != nil || string(got) != "cross-tenant secret" {
		t.Fatalf("victim file changed: %q, %v", got, err)
	}

	// Legitimate in-workspace access keeps working.
	if _, err := runWriteFile(ctx, sb, WriteFileParams{Path: "sub/report.txt", Content: "mine"}); err != nil {
		t.Fatalf("in-workspace write: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "conv-attacker", "sub", "report.txt")); err != nil {
		t.Fatalf("in-workspace write landed wrong: %v", err)
	}
	out, err := runViewFile(ctx, sb, ViewFileParams{Path: "sub/report.txt"})
	if err != nil {
		t.Fatalf("in-workspace read: %v", err)
	}
	if !strings.HasPrefix(out, "mine") {
		t.Errorf("in-workspace read = %q, want prefix %q", out, "mine")
	}
}

func TestFileToolsRejectAbsoluteSiblingConversationPaths(t *testing.T) {
	sb := fsTestSandbox(t)
	root := t.TempDir()
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
	victimDir := filepath.Join(root, "conv-victim")
	if err := os.MkdirAll(victimDir, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(victimDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("victim secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := WithConversationID(context.Background(), "conv-attacker")

	if out, err := runViewFile(ctx, sb, ViewFileParams{Path: secret}); err == nil {
		t.Fatalf("absolute sibling read succeeded: %q", out)
	}
	if _, err := runEditFile(ctx, sb, EditFileParams{Path: secret, OldText: "victim", NewText: "stolen"}); err == nil {
		t.Fatal("absolute sibling edit succeeded")
	}
	if _, err := runWriteFile(ctx, sb, WriteFileParams{Path: filepath.Join(victimDir, "planted.txt"), Content: "pwn"}); err == nil {
		t.Fatal("absolute sibling write succeeded")
	}
	if got, err := os.ReadFile(secret); err != nil || string(got) != "victim secret" {
		t.Fatalf("victim changed: %q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(victimDir, "planted.txt")); !os.IsNotExist(err) {
		t.Fatalf("absolute sibling planted file: %v", err)
	}
}

func TestFileToolsSupportingDocsAreReadOnlyCapability(t *testing.T) {
	sb := fsTestSandbox(t)
	workspace := t.TempDir()
	docs, err := os.MkdirTemp("/var/tmp", "fleet-doc-capability-")
	if err != nil {
		t.Skipf("create supporting-doc fixture outside host allowlist: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(docs) })
	t.Setenv("FLEET_WORKSPACE_ROOT", workspace)
	protocol := filepath.Join(docs, "policy.md")
	if err := os.WriteFile(protocol, []byte("governed"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Preserve the process-global boot registry for other tests.
	supportingDocDirsMu.RLock()
	previous := make(map[string]string, len(supportingDocDirs))
	for k, v := range supportingDocDirs {
		previous[k] = v
	}
	supportingDocDirsMu.RUnlock()
	SetSupportingDocDirs(map[string]string{"protocols": docs})
	t.Cleanup(func() {
		supportingDocDirsMu.Lock()
		supportingDocDirs = previous
		supportingDocDirsMu.Unlock()
	})

	ctx := WithConversationID(context.Background(), "conv-docs")
	dir, err := EnsureWorkspaceDir("conv-docs")
	if err != nil {
		t.Fatal(err)
	}
	if err := sb.BindFileOpRoot(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	got, err := runViewFile(ctx, sb, ViewFileParams{Path: "protocols/policy.md"})
	if err != nil || !strings.HasPrefix(got, "governed\n\n(file metadata: sha256=") {
		t.Fatalf("supporting-doc view = %q err=%v", got, err)
	}
	if _, err := runEditFile(ctx, sb, EditFileParams{Path: "protocols/policy.md", OldText: "governed", NewText: "mutated"}); err == nil {
		t.Fatal("supporting-doc edit succeeded")
	}
	if _, err := runWriteFile(ctx, sb, WriteFileParams{Path: "protocols/policy.md", Content: "mutated"}); err == nil {
		t.Fatal("supporting-doc overwrite succeeded")
	}
	if data, err := os.ReadFile(protocol); err != nil || string(data) != "governed" {
		t.Fatalf("supporting doc changed: %q err=%v", data, err)
	}
}

// TestFileToolsPoisonedSandbox pins that a poisoned sandbox (#796) refuses file
// ops fail-closed rather than falling back to the host.
func TestFileToolsPoisonedSandbox(t *testing.T) {
	// A poisoned host sandbox is not reachable directly; assert the seam maps
	// ErrPoisoned to a retry-friendly message via fileOpError.
	err := fileOpError("view", sandbox.ErrPoisoned)
	if err == nil || !strings.Contains(err.Error(), "reset") {
		t.Errorf("poisoned view fileop: want reset message, got %v", err)
	}
	if !errors.Is(sandbox.ErrPoisoned, sandbox.ErrPoisoned) {
		t.Fatal("sanity")
	}
}

// TestFileToolsSupportingDocsReadOnlyUnderForcedWorkingDir pins the #1290 fix:
// a scheduled/one-shot run (forced working dir, no conversation id) reads a
// bundle doc through its seeded workspace symlink, while writes into the doc
// mount, non-doc symlink escapes, absolute doc-mount paths, and `..` traversal
// out of the forced root all stay refused.
func TestFileToolsSupportingDocsReadOnlyUnderForcedWorkingDir(t *testing.T) {
	sb := fsTestSandbox(t)
	parent := t.TempDir()
	forced := filepath.Join(parent, "task-run-x")
	if err := os.MkdirAll(forced, 0o755); err != nil {
		t.Fatal(err)
	}
	docs, err := os.MkdirTemp("/var/tmp", "fleet-doc-forced-")
	if err != nil {
		t.Skipf("create supporting-doc fixture outside host allowlist: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(docs) })
	protocol := filepath.Join(docs, "self-audit.md")
	if err := os.WriteFile(protocol, []byte("governed"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Preserve the process-global boot registry for other tests.
	supportingDocDirsMu.RLock()
	previous := make(map[string]string, len(supportingDocDirs))
	for k, v := range supportingDocDirs {
		previous[k] = v
	}
	supportingDocDirsMu.RUnlock()
	SetSupportingDocDirs(map[string]string{"protocols": docs})
	t.Cleanup(func() {
		supportingDocDirsMu.Lock()
		supportingDocDirs = previous
		supportingDocDirsMu.Unlock()
	})

	SeedSupportingDocSymlinks(forced)
	if target, lerr := os.Readlink(filepath.Join(forced, "protocols")); lerr != nil || target != docs {
		t.Fatalf("seeded protocols symlink = %q err=%v, want %q", target, lerr, docs)
	}

	ctx := WithForcedWorkingDir(context.Background(), forced)
	if err := sb.BindFileOpRoot(context.Background(), forced); err != nil {
		t.Fatal(err)
	}

	got, err := runViewFile(ctx, sb, ViewFileParams{Path: "protocols/self-audit.md"})
	if err != nil || !strings.HasPrefix(got, "governed\n\n(file metadata: sha256=") {
		t.Fatalf("supporting-doc view under forced dir = %q err=%v", got, err)
	}
	// Writes into the doc mount stay refused, and the doc is untouched.
	if _, err := runEditFile(ctx, sb, EditFileParams{Path: "protocols/self-audit.md", OldText: "governed", NewText: "mutated"}); err == nil {
		t.Fatal("supporting-doc edit succeeded under forced dir")
	}
	if _, err := runWriteFile(ctx, sb, WriteFileParams{Path: "protocols/self-audit.md", Content: "mutated"}); err == nil {
		t.Fatal("supporting-doc overwrite succeeded under forced dir")
	}
	if data, err := os.ReadFile(protocol); err != nil || string(data) != "governed" {
		t.Fatalf("supporting doc changed: %q err=%v", data, err)
	}

	// The exception admits ONLY registered doc roots: a symlink out of the
	// forced root to anywhere else still escapes and is refused.
	lootDir := t.TempDir()
	secret := filepath.Join(lootDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("loot"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(lootDir, filepath.Join(forced, "loot")); err != nil {
		t.Fatal(err)
	}
	if out, err := runViewFile(ctx, sb, ViewFileParams{Path: "loot/secret.txt"}); err == nil {
		t.Fatalf("non-doc symlink escape succeeded: %q", out)
	}

	// The model's UNRESOLVED path must originate beneath the forced root: the
	// doc mount's own absolute path is not admitted directly.
	if out, err := runViewFile(ctx, sb, ViewFileParams{Path: protocol}); err == nil {
		t.Fatalf("absolute doc-mount read bypassed the forced root: %q", out)
	}

	// `..` traversal out of the forced root stays refused even when the
	// target exists and is inside the host allowlist.
	escape := filepath.Join(parent, "escape.txt")
	if err := os.WriteFile(escape, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runViewFile(ctx, sb, ViewFileParams{Path: "../escape.txt"}); err == nil {
		t.Fatalf("dot-dot escape out of the forced root succeeded: %q", out)
	}
}

// TestViewFileTool_LimitInsideMultibyteRune pins the rune-safe page boundary:
// a byte limit that lands inside a multi-byte rune must not hand the model (or
// Postgres, when the turn is persisted) a split rune. The trailing partial rune
// is given back to the next page and the "read more" note names the exact
// offset to continue from, so paging with it walks the file on rune boundaries.
func TestViewFileTool_LimitInsideMultibyteRune(t *testing.T) {
	sb := fsTestSandbox(t)
	testFile := filepath.Join(t.TempDir(), "utf8.txt")
	// "héllo": 'h' (1 byte) + 'é' (2 bytes) + "llo" (3 bytes) = 6 bytes.
	const content = "héllo"
	if err := os.WriteFile(testFile, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// Limit 2 cuts 'é' in half: the window is trimmed back to "h".
	res, err := runViewFile(context.Background(), sb, ViewFileParams{Path: testFile, Limit: 2})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if !utf8.ValidString(res) {
		t.Fatalf("view_file returned invalid UTF-8: %q", res)
	}
	if !strings.HasPrefix(res, "h\n...") {
		t.Errorf("limit 2 should return only the complete rune(s): %q", res)
	}
	if !strings.Contains(res, "Use offset=1 to read more") {
		t.Errorf("read-more note should name the rune boundary offset 1: %q", res)
	}

	// Continuing from the advertised offset yields the rest, starting ON the rune.
	res, err = runViewFile(context.Background(), sb, ViewFileParams{Path: testFile, Offset: 1, Limit: 3})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if !utf8.ValidString(res) || !strings.HasPrefix(res, "él\n...") {
		t.Errorf("offset 1 limit 3: got %q", res)
	}

	// A limit that lands exactly on a boundary is untouched, and a read to
	// EOF never trims (the file's own final bytes are not "partial").
	res, err = runViewFile(context.Background(), sb, ViewFileParams{Path: testFile, Limit: 3})
	if err != nil || !strings.HasPrefix(res, "hé\n...") || !strings.Contains(res, "Use offset=3 to read more") {
		t.Errorf("limit 3: got %q err=%v", res, err)
	}
	res, err = runViewFile(context.Background(), sb, ViewFileParams{Path: testFile, Offset: 1})
	if err != nil || !strings.HasPrefix(res, "éllo") {
		t.Errorf("offset 1 to EOF: got %q err=%v", res, err)
	}
}

func TestTrimTrailingPartialRune(t *testing.T) {
	cases := []struct{ in, want string }{
		{"h\xc3", "h"},               // split 2-byte rune
		{"h\xe2\x82", "h"},           // split 3-byte rune (2 of 3 bytes)
		{"h\xf0\x9f\x98", "h"},       // split 4-byte rune (3 of 4 bytes)
		{"hé", "hé"},                 // complete rune: untouched
		{"plain", "plain"},           // ASCII: untouched
		{"\xc3", "\xc3"},             // would become empty: untouched (progress over purity)
		{"\xff\xfe", "\xff\xfe"},     // binary that was never UTF-8: untouched
		{"ok\x80\x80", "ok\x80\x80"}, // stray continuation bytes with no start: untouched
	}
	for _, c := range cases {
		if got := string(trimTrailingPartialRune([]byte(c.in))); got != c.want {
			t.Errorf("trimTrailingPartialRune(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
