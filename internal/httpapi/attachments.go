package httpapi

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
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

// Per-file size cap fallback, used only if the config carries no value.
// The real limit is cfg.UploadMaxBytes (FLEET_UPLOAD_MAX_BYTES, default
// 1 GiB) — generous on purpose, since the SweepAttachments TTL reclaims
// unused uploads and the only real constraint is disk pressure on the host.
const defaultMaxUploadBytes int64 = 1 << 30 // 1 GiB

// uploadedAttachment is the per-file metadata we return to the caller
// (Next.js), which echoes it back in the next /chat request.
type uploadedAttachment struct {
	Name string `json:"name"` // display name, sanitized
	Path string `json:"path"` // server-relative path we trust later
	Size int64  `json:"size"`
	MIME string `json:"mime,omitempty"`
}

// userUploadsRoot is the uploads subtree belonging to one user:
// <EmailAttachmentDir>/uploads/<sha256(email) truncated>/.
//
// The per-user segment is what makes containment double as an OWNERSHIP check
// (ADR-0058). Before it, uploads/ was one flat tree of random tokens and
// validateAttachments confined a claimed path only to that root — so any
// authenticated caller who learned a path (a copied message, an export, a
// branched transcript) could name another user's upload in their OWN /chat
// request and have fleet stage it, or read an image's bytes straight into
// their model context. Confining to the caller's own subtree instead makes
// that a rejection rather than a read.
//
// The segment is a hash, not the address: the uploads tree is world-readable
// to the host user and shows up in operator `du` output and backups, and
// there is no reason for it to enumerate who uses the box. Truncated to 32 hex
// chars — this is a namespace separator, not a secret, and the containment
// check never trusts it as one (the caller never supplies the segment; the
// server derives it from the authenticated identity).
//
// An empty identity hashes to its own bucket rather than to the uploads root,
// so a caller with no email still cannot reach anyone else's subtree. That is
// a test-only shape: every route reaching here sits behind the auth + member
// middleware, which refuses a request with no X-User-Email.
func userUploadsRoot(baseDir, userEmail string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(userEmail))))
	return filepath.Join(baseDir, "uploads", hex.EncodeToString(sum[:])[:32])
}

// postAttachments accepts one-or-more files via multipart/form-data under the
// "files" field and stashes them under
// <EmailAttachmentDir>/uploads/<user>/<token>/. A fresh random token per
// upload keeps paths unguessable and prevents collisions; the per-user segment
// above is what makes a path from one user unusable by another. The existing
// SweepAttachments loop walks the whole tree, so the extra level needs no
// extra wiring.
func (s *Server) postAttachments(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	maxBytes := s.cfg.UploadMaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxUploadBytes
	}
	// multipart has both per-part and total request caps; we set the
	// request cap to (maxBytes * 2) to allow a couple of big files per
	// request while still refusing truly abusive uploads. Kept aligned
	// with Next.js's experimental.proxyClientMaxBodySize ("2gb"), which
	// caps the same request one hop earlier.
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes*2)

	// 32 MiB in-memory threshold — anything larger spills to a temp file,
	// which we then stream into the final destination. The overall request
	// size is capped by http.MaxBytesReader above.
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		// Tripping the request cap mid-parse surfaces as a generic parse
		// error; report it as the size problem it actually is instead of
		// an opaque 400 the composer can't explain to the user.
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			http.Error(w, fmt.Sprintf("upload is over this server's %s combined request limit — attach fewer files at once", humanSize(mbe.Limit)), http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "parse multipart: "+err.Error(), http.StatusBadRequest)
		return
	}

	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		http.Error(w, "no files provided (use field name 'files')", http.StatusBadRequest)
		return
	}

	// Per-caller subtree, derived from the authenticated identity (never from
	// anything in the request body) — see userUploadsRoot.
	baseDir := userUploadsRoot(s.cfg.EmailAttachmentDir, userFromCtx(r.Context()))
	if err := os.MkdirAll(baseDir, 0o755); err != nil { //nolint:gosec // host-side upload landing area, readable by the host user only
		http.Error(w, "mkdir uploads: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Validate every size up front so an oversize file mid-batch doesn't
	// leave earlier files orphaned on disk (they'd sit until the TTL
	// sweep) while the client is told the whole request failed.
	for _, fh := range files {
		if fh.Size > maxBytes {
			http.Error(w, fmt.Sprintf("%q is %s — over this server's %s per-file upload limit", fh.Filename, humanSize(fh.Size), humanSize(maxBytes)), http.StatusRequestEntityTooLarge)
			return
		}
	}

	out := make([]uploadedAttachment, 0, len(files))
	for _, fh := range files {
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
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // host-side landing dir; the staged copy in the conversation workspace is what a sandbox reads
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
// sitting under THIS CALLER's uploads subtree
// (<EmailAttachmentDir>/uploads/<user>/, see userUploadsRoot). Returns the
// accepted subset. Silent on rejections — logging is enough; the agent just
// won't see them.
//
// The per-user root is the ownership gate (ADR-0058): a path is accepted only
// if the caller is the user who uploaded it, so learning another user's upload
// path — from a copied message, an export, a branched transcript — no longer
// lets a request pull that file into the caller's turn (neither staged for the
// sandbox nor read host-side as vision input).
//
// Uploads written before the per-user segment existed sit at
// uploads/<token>/… and therefore no longer validate for anyone. They are
// TTL-swept ephemera whose only lifetime is between POST /attachments and the
// next /chat call, so the blast radius is a message in flight across a
// restart: the composer's file is dropped exactly as any other unvalidatable
// entry is, and re-attaching it works.
func (s *Server) validateAttachments(userEmail string, atts []chatAttachment) []chatAttachment {
	if len(atts) == 0 {
		return nil
	}
	root, err := filepath.Abs(userUploadsRoot(s.cfg.EmailAttachmentDir, userEmail))
	if err != nil {
		return nil
	}
	root = filepath.Clean(root)

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
		// Confine the path to the caller's uploads root: filepath.Rel splits off the
		// remainder below root, and filepath.IsLocal rejects a "../" escape
		// that Rel would otherwise hand back with a nil error. Everything
		// after the guard uses ONLY the vetted remainder rejoined to the
		// trusted root — never the raw client value — so the path handed to
		// os.Stat is derived from sanitized data (this is also the shape
		// CodeQL's path-injection query recognizes; Join(root, rel) is
		// byte-identical to abs whenever the guard passes). rel == "." —
		// the uploads dir itself — passes IsLocal but is dropped below by
		// !IsRegular.
		rel, relErr := filepath.Rel(root, abs)
		if relErr != nil || !filepath.IsLocal(rel) {
			// %q, not %s: a.Path here is the RAW client-supplied string on the
			// branch where containment just FAILED, so it is hostile by
			// construction. %q escapes CR/LF and cannot forge a log entry.
			log.Printf("attachment rejected (outside the caller's own uploads root): %q", a.Path)
			continue
		}
		abs = filepath.Join(root, rel)
		info, err := os.Stat(abs)
		if err != nil || !info.Mode().IsRegular() {
			// %q for the same reason as above: still the pre-validation client string.
			log.Printf("attachment rejected (stat): %q: %v", a.Path, err)
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

// appendAttachmentsBlock tacks a short, LLM-facing markdown section onto the
// turn's injected context describing each attachment. Image attachments are
// flagged as already-attached vision input so the agent doesn't waste a
// view_file call on raw image bytes; non-image files keep absolute paths so
// view_file or downstream tools can reach them. The agent's system prompt
// tells it what to do with this section.
//
// The non-image paths are the STAGED copies in this conversation's workspace
// (stageAttachmentsIntoWorkspace) on both sandbox backends, so the trailer
// describes the workspace lifetime rather than the uploads-area TTL: the
// uploads tree is control-plane state no sandbox can resolve (ADR-0058).
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
		b.WriteString("\nThese files are saved in this conversation's workspace (the `attachments/` subdirectory) and live as long as the conversation does. If the user wants to keep a file beyond that, offer to persist it via `mcp_fast_io_upload`.\n")
	}
	return b.String()
}

// ── per-conversation attachment staging ──────────────────────────────────

// stageAttachmentsIntoWorkspace copies validated non-image attachments into
// <conversation workspace>/attachments/ — the one tree every sandbox sees on
// both backends (the podman workspace bind mount; the kubernetes workspace
// claim) — and rewrites each entry's Path to the staged copy.
//
// This is the reachability mechanism AND the scoping boundary (ADR-0058,
// docs/ATTACHMENT-SCOPING.md). The uploads root is mounted into no sandbox at
// all, so an uploads path resolves nowhere inside one: knowing another user's
// upload path is no longer enough to read it, and each turn reads through a
// path in the conversation it belongs to.
//
// Inputs are expected from validateAttachments, but the stager does not TRUST
// its caller: each entry's Path is re-confined to uploadsRoot with the same
// Rel + IsLocal + rejoin barrier validateAttachments uses (also the shape
// CodeQL's path-injection query recognizes), so a future call site that skips
// validation cannot turn the copy source into an arbitrary host read.
//
// Idempotent for the queue-drain case: a drained row echoes the same
// attachment metadata a live submit already staged, and a same-name
// same-size staged copy is reused rather than duplicated. A same-name
// different-size file (a genuinely new attachment reusing a filename) gets a
// numbered variant instead — names are cheap, clobbering a file the agent may
// have already read mid-conversation is not.
//
// Per-entry failures degrade rather than fail the turn: the entry keeps its
// uploads path and the error is logged. That degradation now FAILS CLOSED
// rather than falling back to the original bytes — nothing mounts the uploads
// root, so the agent gets a not-found on that path and says so, instead of
// reading a file through a tree no conversation owns.
func stageAttachmentsIntoWorkspace(uploadsRoot, convID string, atts []chatAttachment) []chatAttachment {
	if len(atts) == 0 {
		return atts
	}
	root, err := filepath.Abs(uploadsRoot)
	if err != nil {
		log.Printf("attachment staging: resolve uploads root: %v (attachments keep uploads paths)", err)
		return atts
	}
	root = filepath.Clean(root)
	convDir, err := tools.EnsureWorkspaceDir(convID)
	if err != nil {
		log.Printf("attachment staging: workspace dir for %q: %v (attachments keep uploads paths)", logSafeSlug(convID), err)
		return atts
	}
	dir := filepath.Join(convDir, "attachments")
	// 0o755 like every workspace dir: the sandbox uid (1000) must read it.
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // see comment above
		log.Printf("attachment staging: mkdir %s: %v (attachments keep uploads paths)", dir, err)
		return atts
	}
	out := make([]chatAttachment, 0, len(atts))
	for _, a := range atts {
		src, ok := confineToRoot(root, a.Path)
		if !ok {
			// %q + CR/LF strip: on this branch Path is by definition a value
			// that failed containment, so treat it as hostile in the log too.
			log.Printf("attachment staging: %q is outside the uploads root (kept unstaged)", logSafeSlug(a.Path))
			out = append(out, a)
			continue
		}
		dst, err := stageOneAttachment(dir, src, a)
		if err != nil {
			log.Printf("attachment staging: %q: %v (this attachment keeps its uploads path)", logSafeSlug(a.Name), err)
			out = append(out, a)
			continue
		}
		a.Path = filepath.ToSlash(dst)
		out = append(out, a)
	}
	return out
}

// confineToRoot re-derives path from trusted parts: the remainder below root
// (rejected unless local) rejoined to root — byte-identical to the input
// whenever the guard passes, never derived from the raw value when it fails.
func confineToRoot(root, path string) (string, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false
	}
	rel, err := filepath.Rel(root, filepath.Clean(abs))
	if err != nil || !filepath.IsLocal(rel) {
		return "", false
	}
	return filepath.Join(root, rel), true
}

// stageOneAttachment places one attachment (src already confined to the
// uploads root by the caller) under dir and returns the staged path. See
// stageAttachmentsIntoWorkspace for the naming rules.
func stageOneAttachment(dir, src string, a chatAttachment) (string, error) {
	base := sanitizeFilename(a.Name)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 1; i <= 100; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		// sanitizeFilename already stripped separators and leading dots; the
		// IsLocal check restates that as the barrier CodeQL's path-injection
		// query recognizes, so the Join below is provably confined to dir.
		if !filepath.IsLocal(name) {
			return "", fmt.Errorf("attachment name is unusable as a staged filename")
		}
		dst := filepath.Join(dir, name)
		if info, err := os.Stat(dst); err == nil {
			if info.Mode().IsRegular() && info.Size() == a.Size {
				return dst, nil // already staged (queue drain, or a re-send)
			}
			continue // occupied by something else — try the next variant
		}
		if err := copyAttachment(src, dst); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue // lost a race for this name — try the next variant
			}
			return "", err
		}
		return dst, nil
	}
	return "", fmt.Errorf("no free staged name after 100 variants")
}

// copyAttachment copies src (confined to the uploads root by the caller) to a
// fresh dst. O_EXCL so a concurrent stager can never interleave writes into
// one file; 0o644 so the sandbox uid can read it through the claim.
func copyAttachment(src, dst string) error {
	in, err := os.Open(src) //nolint:gosec // src re-derived by confineToRoot (Rel + IsLocal + rejoin against the uploads root)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) //nolint:gosec // dst is Join(workspace attachments dir, IsLocal-checked sanitized name); world-readable for the sandbox uid
	if err != nil {
		return err // undecorated: the caller inspects os.ErrExist
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("copy: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("close: %w", err)
	}
	return nil
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
