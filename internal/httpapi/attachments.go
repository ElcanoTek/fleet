package httpapi

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// Per-file size cap. Generous on purpose — the default SweepAttachments TTL
// will reclaim unused uploads, and the only real constraint is disk pressure
// on the host. (Compile-time constant — there is no env override.)
const defaultMaxUploadBytes int64 = 256 << 20 // 256 MiB

// uploadedAttachment is the per-file metadata we return to the caller
// (Next.js), which echoes it back in the next /chat request.
type uploadedAttachment struct {
	Name string `json:"name"` // display name, sanitized
	Path string `json:"path"` // server-relative path we trust later
	Size int64  `json:"size"`
	MIME string `json:"mime,omitempty"`
}

// postAttachments accepts one-or-more files via multipart/form-data under
// the "files" field and stashes them under <EmailAttachmentDir>/uploads/<token>/.
// A fresh random token per upload keeps paths unguessable and prevents
// collisions across users. The existing SweepAttachments loop covers the
// uploads subtree without any extra wiring.
func (s *Server) postAttachments(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	maxBytes := defaultMaxUploadBytes
	// multipart has both per-part and total request caps; we set the
	// request cap to (maxBytes * 8) to allow a handful of big files per
	// request while still refusing truly abusive uploads.
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes*8)

	// 32 MiB in-memory threshold — anything larger spills to a temp file,
	// which we then stream into the final destination. The overall request
	// size is capped by http.MaxBytesReader above.
	if err := r.ParseMultipartForm(32 << 20); err != nil { //nolint:gosec // bounded by MaxBytesReader above
		http.Error(w, "parse multipart: "+err.Error(), http.StatusBadRequest)
		return
	}

	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		http.Error(w, "no files provided (use field name 'files')", http.StatusBadRequest)
		return
	}

	baseDir := filepath.Join(s.cfg.EmailAttachmentDir, "uploads")
	if err := os.MkdirAll(baseDir, 0o755); err != nil { //nolint:gosec // mounted into the sandbox container, needs to be readable by that user
		http.Error(w, "mkdir uploads: "+err.Error(), http.StatusInternalServerError)
		return
	}

	out := make([]uploadedAttachment, 0, len(files))
	for _, fh := range files {
		if fh.Size > maxBytes {
			http.Error(w, fmt.Sprintf("%q exceeds per-file limit of %d bytes", fh.Filename, maxBytes), http.StatusRequestEntityTooLarge)
			return
		}
		att, err := saveUpload(baseDir, fh)
		if err != nil {
			log.Printf("saveUpload %q: %v", fh.Filename, err) //nolint:gosec // filename is %q-quoted
			http.Error(w, "save upload: "+err.Error(), http.StatusInternalServerError)
			return
		}
		out = append(out, att)
	}

	writeJSON(w, map[string]any{"attachments": out})
}

// saveUpload copies one multipart file into its own random subdirectory
// under baseDir and returns the metadata the client should attach to the
// follow-up /chat call.
func saveUpload(baseDir string, fh *multipart.FileHeader) (uploadedAttachment, error) {
	token, err := randomToken()
	if err != nil {
		return uploadedAttachment{}, fmt.Errorf("token: %w", err)
	}
	dir := filepath.Join(baseDir, token)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // mounted into the sandbox container, needs to be readable by that user
		return uploadedAttachment{}, fmt.Errorf("mkdir: %w", err)
	}

	name := sanitizeFilename(fh.Filename)
	dst := filepath.Join(dir, name)

	src, err := fh.Open()
	if err != nil {
		return uploadedAttachment{}, fmt.Errorf("open upload: %w", err)
	}
	defer src.Close()

	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // dst is filepath.Join(dir, sanitized name) — both controlled by us
	if err != nil {
		return uploadedAttachment{}, fmt.Errorf("create: %w", err)
	}
	if _, err := io.Copy(f, src); err != nil {
		_ = f.Close()
		_ = os.Remove(dst)
		return uploadedAttachment{}, fmt.Errorf("copy: %w", err)
	}
	if err := f.Close(); err != nil {
		return uploadedAttachment{}, fmt.Errorf("close: %w", err)
	}
	if strings.EqualFold(filepath.Ext(name), ".xlsx") {
		if err := sanitizeXLSX(dst); err != nil {
			_ = os.Remove(dst)
			return uploadedAttachment{}, fmt.Errorf("sanitize xlsx: %w", err)
		}
	}

	mime := ""
	if ct := fh.Header.Get("Content-Type"); ct != "" {
		mime = ct
	}

	return uploadedAttachment{
		Name: name,
		Path: filepath.ToSlash(dst),
		Size: fh.Size,
		MIME: mime,
	}, nil
}

// sanitizeFilename strips any directory components the client might have
// included, drops control characters, and falls back to a timestamped
// default if nothing usable remains. The file is scoped by a random dir
// token so collisions are already impossible — this is purely cosmetic.
func sanitizeFilename(name string) string {
	// Strip any path the client included (Windows and POSIX).
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSpace(name)
	// Drop leading dots so we don't create hidden files.
	name = strings.TrimLeft(name, ".")

	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20, r == 0x7f:
			// control chars → skip
		case r == ':', r == '*', r == '?', r == '"', r == '<', r == '>', r == '|':
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		out = fmt.Sprintf("upload-%d", time.Now().UnixNano())
	}
	// Keep filenames short enough for most filesystems.
	if len(out) > 200 {
		ext := filepath.Ext(out)
		if len(ext) > 20 {
			ext = ""
		}
		out = out[:200-len(ext)] + ext
	}
	return out
}

// randomToken returns 16 bytes of randomness as a lowercase base32 string
// (no padding). Short enough to fit in a path, long enough that two
// concurrent uploads never collide.
func randomToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	enc := base32.StdEncoding.WithPadding(base32.NoPadding)
	return strings.ToLower(enc.EncodeToString(buf)), nil
}

// ── chat-request side: validation + prompt augmentation ─────────────────

// chatAttachment is the metadata the browser echoes back to /chat after a
// successful /attachments upload. We re-validate every path against the
// uploads root so a compromised client can't point the agent at arbitrary
// files on disk.
type chatAttachment struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	MIME string `json:"mime,omitempty"`
}

// toAgentImageAttachments adapts validated chatAttachment metadata into the
// shape RunTurn expects. Both types are intentionally different so changes
// to the wire-level struct (HTTP layer) don't ripple into the agent package.
func toAgentImageAttachments(atts []chatAttachment) []agent.ImageAttachment {
	if len(atts) == 0 {
		return nil
	}
	out := make([]agent.ImageAttachment, 0, len(atts))
	for _, a := range atts {
		mime := strings.TrimSpace(strings.ToLower(a.MIME))
		if mime == "" {
			if mt := tools.ImageMIMEFromName(a.Name); mt != "" {
				mime = mt
			}
		}
		out = append(out, agent.ImageAttachment{
			Path:      a.Path,
			MediaType: mime,
			Name:      a.Name,
		})
	}
	return out
}

// splitAttachmentsByKind partitions the validated set into images (those
// the agent should see as multimodal vision input) and others (which still
// flow through the legacy markdown reference path so view_file etc. can
// reach them). MIME comes from the upload Content-Type when present, with
// extension fallback so a curl client that omits the header doesn't lose
// vision routing.
func splitAttachmentsByKind(atts []chatAttachment) (images []chatAttachment, others []chatAttachment) {
	for _, a := range atts {
		mime := strings.TrimSpace(strings.ToLower(a.MIME))
		if mime == "" {
			if mt := tools.ImageMIMEFromName(a.Name); mt != "" {
				mime = mt
			}
		}
		if tools.IsImageMIME(mime) {
			a.MIME = mime
			images = append(images, a)
		} else {
			others = append(others, a)
		}
	}
	return images, others
}

// validateAttachments drops any entries whose path isn't a regular file
// sitting under <EmailAttachmentDir>/uploads/. Returns the accepted subset.
// Silent on rejections — logging is enough; the agent just won't see them.
func (s *Server) validateAttachments(atts []chatAttachment) []chatAttachment {
	if len(atts) == 0 {
		return nil
	}
	root, err := filepath.Abs(filepath.Join(s.cfg.EmailAttachmentDir, "uploads"))
	if err != nil {
		return nil
	}
	root = filepath.Clean(root) + string(filepath.Separator)

	accepted := make([]chatAttachment, 0, len(atts))
	for _, a := range atts {
		if a.Path == "" {
			continue
		}
		abs, err := filepath.Abs(a.Path)
		if err != nil {
			continue
		}
		abs = filepath.Clean(abs)
		if !strings.HasPrefix(abs+string(filepath.Separator), root) && abs+string(filepath.Separator) != root {
			log.Printf("attachment rejected (outside uploads root): %s", a.Path)
			continue
		}
		info, err := os.Stat(abs)
		if err != nil || !info.Mode().IsRegular() {
			log.Printf("attachment rejected (stat): %s: %v", a.Path, err)
			continue
		}
		a.Path = filepath.ToSlash(abs)
		if a.Size <= 0 {
			a.Size = info.Size()
		}
		accepted = append(accepted, a)
	}
	return accepted
}

// appendAttachmentsBlock tacks a short, LLM-facing markdown section onto
// the user message describing each attachment. Image attachments are flagged
// as already-attached vision input so the agent doesn't waste a view_file
// call on raw image bytes; non-image files keep absolute paths so view_file
// or downstream tools can reach them. The agent's system prompt tells it
// what to do with this section.
func appendAttachmentsBlock(message string, images, others []chatAttachment) string {
	if len(images) == 0 && len(others) == 0 {
		return message
	}
	var b strings.Builder
	b.WriteString(strings.TrimRight(message, "\n"))
	if len(images) > 0 {
		b.WriteString("\n\n---\n**User attached images** (already provided to you as vision input — examine them directly; do NOT call view_file on these):\n")
		for _, a := range images {
			fmt.Fprintf(&b, "- `%s` (%s)\n", a.Name, humanSize(a.Size))
		}
	}
	if len(others) > 0 {
		b.WriteString("\n\n---\n**User attached files:**\n")
		for _, a := range others {
			fmt.Fprintf(&b, "- `%s` (%s, %s)\n", a.Name, humanSize(a.Size), a.Path)
		}
		b.WriteString("\nThese files are saved to the conversation's temporary uploads area and will be swept after the normal TTL. If the user wants to keep a file for future sessions, offer to persist it via `mcp_fast_io_upload`.\n")
	}
	return b.String()
}

// maxWorkspaceInventoryEntries caps how many persisted-file rows we surface
// at the top of a turn. The point is to remind the agent which downloads /
// generated artifacts from previous turns are still on disk; in pathological
// cases (an agent that wrote hundreds of intermediate CSVs) we don't want
// to flood every subsequent user message. 50 covers normal usage and keeps
// the block well under a kilobyte of text.
const maxWorkspaceInventoryEntries = 50

// appendWorkspaceInventoryBlock lists files currently sitting in the
// per-conversation workspace and appends them to the user message. Surfaces
// state the agent would otherwise have to remember turn-to-turn — the
// run_python kernel resets each turn, so a report downloaded on turn 1 is
// often forgotten by turn 4 even though the file is still on disk. Naming
// these files in-context turns "what did I download earlier?" into a
// look-don't-recall question.
//
// Only lists top-level regular files. Skips symlinks (the protocols/,
// personas/, system_prompts/ symlinks installed by EnsureWorkspaceDir are
// structural, not state), dotfiles, and zero-byte files. Newest first by
// modtime so a long-running chat surfaces the most relevant files; tail
// with a "+N more" marker when the cap is hit.
func appendWorkspaceInventoryBlock(message, workspaceDir string) string {
	if workspaceDir == "" {
		return message
	}
	entries, err := os.ReadDir(workspaceDir)
	if err != nil {
		// First turn of a brand-new conversation: the dir doesn't exist
		// yet. Stay silent — there's nothing to surface.
		return message
	}

	files := make([]workspaceFile, 0, len(entries))
	for _, e := range entries {
		// Type() returns the dirent type without a stat() syscall. We
		// only want regular files: directories (none expected at the
		// top level), symlinks (structural — see above), pipes, etc.
		// all skip.
		if !e.Type().IsRegular() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Size() == 0 {
			continue
		}
		files = append(files, workspaceFile{name: name, size: info.Size(), modTime: info.ModTime()})
	}
	if len(files) == 0 {
		return message
	}

	// Newest first — a long chat's recent downloads matter more to the
	// next turn than its earliest scratch CSVs.
	sort.SliceStable(files, func(i, j int) bool {
		return files[i].modTime.After(files[j].modTime)
	})

	var b strings.Builder
	b.WriteString(strings.TrimRight(message, "\n"))
	b.WriteString("\n\n---\n**Workspace files persisted from earlier turns** (still on disk in this chat's scratch dir; reference them by name in `bash`/`run_python` without re-downloading; to give the user a download link, write a markdown link to the bare filename — `[name](name)` — never a `sandbox:` or absolute path):\n")
	overflow := 0
	if len(files) > maxWorkspaceInventoryEntries {
		overflow = len(files) - maxWorkspaceInventoryEntries
		files = files[:maxWorkspaceInventoryEntries]
	}
	for _, f := range files {
		fmt.Fprintf(&b, "- `%s` (%s)\n", f.name, humanSize(f.size))
	}
	if overflow > 0 {
		fmt.Fprintf(&b, "- …and %d more — use `bash ls` to enumerate the full list.\n", overflow)
	}
	return b.String()
}

type workspaceFile struct {
	name    string
	size    int64
	modTime time.Time
}

func humanSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	default:
		return fmt.Sprintf("%.1f GB", float64(n)/(1024*1024*1024))
	}
}
