// Per-conversation workspace file fetch:
// GET /conversations/{id}/workspace/<path>. Split out of server.go (#1127).

package httpapi

import (
	"errors"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/ElcanoTek/fleet/internal/tools"
)

// handleWorkspaceFile streams a single file from the per-conversation
// workspace dir so the chat UI can render images / files the agent
// produced via run_python or wrote with write_file. Used by the
// markdown img interceptor in chat-experience.tsx — when the agent
// writes `![chart](spend_chart.png)` and saves spend_chart.png to its
// workspace, the UI rewrites the relative src to
// `/api/conversations/<convID>/workspace/spend_chart.png` and the
// browser fetches it from this handler.
//
// Auth: same as every other conversation route — the caller must own
// the conversation. relPath is interpreted relative to the conv's
// workspace dir; .. traversal and absolute paths are rejected. The
// resolved file must still live under the workspace dir after symlink
// resolution (filepath.EvalSymlinks) so a maliciously-placed symlink
// can't escape.
func (s *Server) handleWorkspaceFile(w http.ResponseWriter, r *http.Request, convID, relPath string) {
	user := userFromCtx(r.Context())
	conv, err := s.store.Get(r.Context(), user, convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if conv == nil {
		http.Error(w, "conversation not found", http.StatusNotFound)
		return
	}

	if relPath == "" {
		http.Error(w, "file path required", http.StatusBadRequest)
		return
	}
	// The path arrives as a URL segment — decode percent-encoded chars
	// (spaces, parens etc. that pandas/matplotlib filenames sometimes
	// carry) before further validation.
	decoded, err := url.PathUnescape(relPath)
	if err != nil {
		http.Error(w, "bad path encoding", http.StatusBadRequest)
		return
	}
	relPath = decoded

	// Resolve the requested file against the conversation workspace root,
	// enforcing the shared path-traversal guard (tools.SafeWorkspaceJoin):
	// reject `..`/absolute/NUL paths up front, then EvalSymlinks + confirm the
	// result still lives under the workspace dir. Without the symlink check a
	// `ln -s /etc/passwd workspace/<conv>/p` written by the agent (or a
	// malicious upload) would let any user with the conversation read host
	// secrets via this endpoint.
	wsDir := tools.WorkspaceDirForConversation(convID)
	resolvedAbs, err := tools.SafeWorkspaceJoin(wsDir, relPath)
	switch {
	case errors.Is(err, tools.ErrUnsafePath):
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	case errors.Is(err, tools.ErrPathEscapesWorkspace):
		http.Error(w, "path escapes workspace", http.StatusBadRequest)
		return
	case os.IsNotExist(err):
		http.Error(w, "file not found", http.StatusNotFound)
		return
	case err != nil:
		http.Error(w, "resolve path: "+err.Error(), http.StatusInternalServerError)
		return
	}

	info, err := os.Stat(resolvedAbs) //nolint:gosec // resolvedAbs is validated by tools.SafeWorkspaceJoin to live under the workspace dir
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "file not found", http.StatusNotFound)
			return
		}
		http.Error(w, "stat: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if info.IsDir() {
		http.Error(w, "path is a directory", http.StatusBadRequest)
		return
	}

	// Content-Type from extension; mime.TypeByExtension handles the
	// common suffixes (.png/.jpg/.jpeg/.svg/.pdf/.csv/.json/.txt) and
	// returns "" for unknown — http.ServeContent's sniffing then takes
	// over. Either way the browser gets something it can render or
	// download.
	ctype := mime.TypeByExtension(filepath.Ext(resolvedAbs))
	if ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}
	// Workspace files are effectively immutable from the user's point
	// of view: each run_python that saves a chart picks a new filename
	// (the agent emits e.g. `report__8a75730b.csv` or `chart_<uuid>.png`),
	// so a cache hit on a known URL is always the right answer. A
	// generous max-age + immutable directive is what stops the
	// scroll-flicker on mobile: when the user scrolls past a chart and
	// back, the browser serves from cache without revalidating instead
	// of paint-blanking while it re-decodes a 304. 24h is more than
	// enough for an active session and the file is gone after the
	// orphan-workspace sweep anyway.
	w.Header().Set("Cache-Control", "private, max-age=86400, immutable")

	f, err := os.Open(resolvedAbs) //nolint:gosec // resolvedAbs is validated to live under the workspace dir
	if err != nil {
		http.Error(w, "open: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	http.ServeContent(w, r, filepath.Base(resolvedAbs), info.ModTime(), f)
}
