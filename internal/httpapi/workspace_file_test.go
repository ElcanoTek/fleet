package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWorkspaceFile_HappyPath: a file written into the per-conversation
// workspace dir (the way run_python writes a chart) is reachable via
// GET /conversations/{id}/workspace/<file> with the right Content-Type.
func TestWorkspaceFile_HappyPath(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()

	// Create a conversation owned by the user so the auth check passes.
	w := do(t, h, http.MethodPost, "/conversations",
		map[string]any{"persona": "victoria"}, "owner@example.com")
	if w.Code != http.StatusOK {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var createResp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &createResp); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	convID := createResp.ID
	if convID == "" {
		t.Fatalf("conversation id missing in %q", w.Body.String())
	}

	// Drop a file into the conv's workspace, the way run_python would.
	wsRoot := t.TempDir()
	t.Setenv("CHAT_WORKSPACE_ROOT", wsRoot)
	convDir := filepath.Join(wsRoot, convID)
	if err := os.MkdirAll(convDir, 0o755); err != nil {
		t.Fatalf("mkdir convDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(convDir, "chart.png"), []byte("\x89PNG\r\n\x1a\nfake"), 0o644); err != nil {
		t.Fatalf("write chart: %v", err)
	}

	w = do(t, h, http.MethodGet, "/conversations/"+convID+"/workspace/chart.png", nil, "owner@example.com")
	if w.Code != http.StatusOK {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "image/png") {
		t.Errorf("Content-Type = %q, want image/png", got)
	}
	if !strings.HasPrefix(w.Body.String(), "\x89PNG") {
		t.Errorf("body did not start with PNG signature: %q", w.Body.String()[:min(20, w.Body.Len())])
	}
}

// TestWorkspaceFile_PathTraversal: every flavor of `..` and absolute
// path is rejected before the file system is touched. Mirrors the
// security contract the chat-experience img rewriter relies on (a
// hostile agent message that emits `![](../../etc/passwd)` should not
// be able to read host files).
func TestWorkspaceFile_PathTraversal(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()

	w := do(t, h, http.MethodPost, "/conversations",
		map[string]any{"persona": "victoria"}, "owner@example.com")
	if w.Code != http.StatusOK {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var createResp struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &createResp)
	convID := createResp.ID
	if convID == "" {
		t.Fatalf("conversation id missing in %q", w.Body.String())
	}

	// Workspace exists and is empty — but a real file at /etc/passwd is
	// readable by the chat-server process. The handler must NOT serve it.
	wsRoot := t.TempDir()
	t.Setenv("CHAT_WORKSPACE_ROOT", wsRoot)
	if err := os.MkdirAll(filepath.Join(wsRoot, convID), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	cases := []string{
		"../../../etc/passwd",
		"foo/../../../etc/passwd",
		"/etc/passwd",
		"%2e%2e/%2e%2e/etc/passwd", // url-encoded traversal
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			w := do(t, h, http.MethodGet,
				"/conversations/"+convID+"/workspace/"+p, nil, "owner@example.com")
			// Either Bad Request (rejected by the explicit check) or Not
			// Found (rejected by EvalSymlinks because the resolved path
			// doesn't exist under the workspace) is acceptable. What's
			// NOT acceptable is 200 + body of /etc/passwd.
			if w.Code == http.StatusOK {
				t.Fatalf("traversal succeeded with body=%q", w.Body.String())
			}
		})
	}
}

// TestWorkspaceFile_AuthRequired: another user can't read this user's
// workspace files. Conversations are user-scoped on Get(), so a wrong
// user id resolves to 404 the same as a non-existent conv.
func TestWorkspaceFile_AuthRequired(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()

	w := do(t, h, http.MethodPost, "/conversations",
		map[string]any{"persona": "victoria"}, "alice@example.com")
	if w.Code != http.StatusOK {
		t.Fatalf("create: %d", w.Code)
	}
	var createResp struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &createResp)
	convID := createResp.ID
	if convID == "" {
		t.Fatalf("conversation id missing in %q", w.Body.String())
	}

	wsRoot := t.TempDir()
	t.Setenv("CHAT_WORKSPACE_ROOT", wsRoot)
	convDir := filepath.Join(wsRoot, convID)
	_ = os.MkdirAll(convDir, 0o755)
	_ = os.WriteFile(filepath.Join(convDir, "secret.png"), []byte("\x89PNG"), 0o644)

	// Bob can't read Alice's workspace.
	w = do(t, h, http.MethodGet,
		"/conversations/"+convID+"/workspace/secret.png", nil, "bob@example.com")
	if w.Code == http.StatusOK {
		t.Fatalf("cross-user read succeeded: %d %q", w.Code, w.Body.String())
	}
}

// TestWorkspaceFile_SymlinkEscape: a symlink pointing outside the
// workspace must not be followed. Even though chat-server itself can
// read the target, exposing it via this endpoint would let any agent
// turn that wrote a symlink later read host files.
func TestWorkspaceFile_SymlinkEscape(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()

	w := do(t, h, http.MethodPost, "/conversations",
		map[string]any{"persona": "victoria"}, "owner@example.com")
	if w.Code != http.StatusOK {
		t.Fatalf("create: %d", w.Code)
	}
	var createResp struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &createResp)
	convID := createResp.ID
	if convID == "" {
		t.Fatalf("conversation id missing in %q", w.Body.String())
	}

	wsRoot := t.TempDir()
	t.Setenv("CHAT_WORKSPACE_ROOT", wsRoot)
	convDir := filepath.Join(wsRoot, convID)
	_ = os.MkdirAll(convDir, 0o755)

	// Stage a real file outside the workspace and a symlink to it inside.
	outside := filepath.Join(t.TempDir(), "host-secret.txt")
	if err := os.WriteFile(outside, []byte("HOST SECRET"), 0o644); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(convDir, "escape.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	w = do(t, h, http.MethodGet,
		"/conversations/"+convID+"/workspace/escape.txt", nil, "owner@example.com")
	if w.Code == http.StatusOK && strings.Contains(w.Body.String(), "HOST SECRET") {
		t.Fatalf("symlink escape succeeded: %d %q", w.Code, w.Body.String())
	}
}

// TestWorkspaceFile_NotFound: missing files return 404, not a 500.
func TestWorkspaceFile_NotFound(t *testing.T) {
	s := serverFixture(t)
	h := s.Routes()

	w := do(t, h, http.MethodPost, "/conversations",
		map[string]any{"persona": "victoria"}, "owner@example.com")
	if w.Code != http.StatusOK {
		t.Fatalf("create: %d", w.Code)
	}
	var createResp struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &createResp)
	convID := createResp.ID
	if convID == "" {
		t.Fatalf("conversation id missing in %q", w.Body.String())
	}

	wsRoot := t.TempDir()
	t.Setenv("CHAT_WORKSPACE_ROOT", wsRoot)
	_ = os.MkdirAll(filepath.Join(wsRoot, convID), 0o755)

	w = do(t, h, http.MethodGet,
		"/conversations/"+convID+"/workspace/does-not-exist.png", nil, "owner@example.com")
	if w.Code != http.StatusNotFound {
		t.Errorf("missing file: %d (want 404), body=%q", w.Code, w.Body.String())
	}
}
