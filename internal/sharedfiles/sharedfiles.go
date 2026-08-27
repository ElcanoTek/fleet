// Package sharedfiles maintains the cross-chat shared file library's two
// on-disk trees (docs/SHARED-FILES.md).
//
// The CANONICAL tree, <DataDir>/shared_files/<id>, is control-plane state: it
// is never mounted into a sandbox, so an agent can never corrupt the authentic
// bytes. The STAGED tree, <WorkspaceRoot>/shared/[folder/]name, is the copy
// agents actually read — it lives under the workspace root because that is the
// one directory visible inside sandboxes on BOTH backends (the podman bind
// mount and the kubernetes workspace claim), and it is additionally mounted
// read-only over the read-write workspace mount so a turn cannot tamper with
// what every other chat reads.
//
// The store's shared_files table is the manifest; this package makes the
// staged tree match it. Mutating endpoints stage/unstage eagerly, and
// Sync reconciles the whole tree — at boot and on every hourly maintenance
// pass — so a wiped workspace volume, a crashed mid-mutation process, or
// host-side drift self-heals without operator action.
package sharedfiles

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// Library locates the two trees. Both paths are absolute after New.
type Library struct {
	// CanonicalDir holds the authentic bytes, one file per row id.
	CanonicalDir string
	// StagedRoot is the sandbox-visible tree the manifest is reconciled into.
	StagedRoot string
}

// New derives the trees from the deployment's data dir and workspace root —
// the same values cmd/fleet hands the store and the sandbox pool.
func New(dataDir, workspaceRoot string) Library {
	canonical := filepath.Join(dataDir, "shared_files")
	if abs, err := filepath.Abs(canonical); err == nil {
		canonical = abs
	}
	staged := StagedRootFor(workspaceRoot)
	return Library{CanonicalDir: canonical, StagedRoot: staged}
}

// StagedRootFor returns the staged tree's path for a workspace root —
// tools.SharedFilesDir, re-exported so this package's callers read naturally.
// The constant itself lives in internal/tools (below both the agent manager
// and the store in the import graph) so mounts, symlinks, staging, and prompt
// text can never disagree about where the library is.
func StagedRootFor(workspaceRoot string) string {
	return tools.SharedFilesDir(workspaceRoot)
}

// maxNameLen keeps staged names comfortably under every filesystem's limit
// while leaving room for the folder segment in the same path.
const maxNameLen = 200

// maxFolderLen bounds the single folder segment.
const maxFolderLen = 64

// SanitizeName validates and normalizes a display filename into the staged
// basename. Unlike the chat-upload sanitizer (which can fall back to a
// generated name because a random token dir isolates it), the shared library
// stages files under their display name, so an unusable name is an error the
// admin fixes rather than a silent rename.
func SanitizeName(name string) (string, error) {
	// Strip any path the client included (Windows and POSIX).
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSpace(name)
	name = strings.TrimLeft(name, ".") // no hidden files in the staged tree
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
	if out == "" || out == "." || out == ".." {
		return "", fmt.Errorf("filename %q has no usable characters", name)
	}
	if len(out) > maxNameLen {
		ext := filepath.Ext(out)
		if len(ext) > 20 {
			ext = ""
		}
		out = out[:maxNameLen-len(ext)] + ext
	}
	if !filepath.IsLocal(out) {
		return "", fmt.Errorf("invalid filename %q", name)
	}
	return out, nil
}

// SanitizeFolder validates the optional single-level folder. "" means the
// library root. Rejection over repair for separators: a folder that LOOKS
// nested but silently flattens would mislead the admin about the staged path.
func SanitizeFolder(folder string) (string, error) {
	folder = strings.TrimSpace(folder)
	if folder == "" {
		return "", nil
	}
	if strings.ContainsAny(folder, `/\`) {
		return "", fmt.Errorf("folder must be a single name, not a path: %q", folder)
	}
	out, err := SanitizeName(folder)
	if err != nil {
		return "", fmt.Errorf("folder %q has no usable characters", folder)
	}
	if len(out) > maxFolderLen {
		return "", fmt.Errorf("folder name is too long (max %d characters)", maxFolderLen)
	}
	return out, nil
}

// stagedRel is the file's path relative to StagedRoot. name and folder were
// validated on write, but re-check locality so a hand-edited DB row can never
// path-traverse the staging ops that trust this function.
func stagedRel(f store.SharedFile) (string, error) {
	rel := f.Name
	if f.Folder != "" {
		rel = filepath.Join(f.Folder, f.Name)
	}
	if rel == "" || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("shared file %s has an unsafe staged path %q/%q", f.ID, f.Folder, f.Name)
	}
	return rel, nil
}

// StagedPath returns the absolute sandbox-visible path for a row.
func (l Library) StagedPath(f store.SharedFile) (string, error) {
	rel, err := stagedRel(f)
	if err != nil {
		return "", err
	}
	return filepath.Join(l.StagedRoot, rel), nil
}

// PromptPath is the path agents are told to read: relative to the chat's cwd,
// through the per-conversation "shared" symlink, so it resolves identically in
// bash/run_python and the file tools on both sandbox backends.
func PromptPath(f store.SharedFile) string {
	if f.Folder != "" {
		return tools.SharedFilesDirName + "/" + f.Folder + "/" + f.Name
	}
	return tools.SharedFilesDirName + "/" + f.Name
}

// MaxPromptEntries caps the announcement block, mirroring the workspace
// inventory's cap: enough to make the library discoverable, bounded so a
// large library cannot bloat every prompt.
const MaxPromptEntries = 50

// PromptBlock renders the announcement that tells the model the library
// exists: paths it can read RIGHT NOW (through the "shared" symlink both the
// chat and scheduled workspace seeding plant), instead of state it must
// remember or rediscover. Returns "" for an empty library. Both drivers use
// this one renderer — chat appends it to each turn's user message
// (httpapi.appendSharedFilesBlock), scheduled runs to the run's system prompt
// (#1301) — so what the model is told about the library can never differ by
// entrypoint.
func PromptBlock(files []store.SharedFile) string {
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("---\n**Shared file library** (files your administrator published to every conversation; read them at these paths with `bash`/`run_python`/`view_file`. They are READ-ONLY — to modify one, copy it into your workspace first):\n")
	overflow := 0
	if len(files) > MaxPromptEntries {
		overflow = len(files) - MaxPromptEntries
		files = files[:MaxPromptEntries]
	}
	for _, f := range files {
		fmt.Fprintf(&b, "- `%s` (%s)", PromptPath(f), humanSize(f.SizeBytes))
		if f.Description != "" {
			fmt.Fprintf(&b, " — %s", f.Description)
		}
		b.WriteString("\n")
	}
	if overflow > 0 {
		fmt.Fprintf(&b, "- …and %d more — use `bash ls -R shared/` to enumerate the full library.\n", overflow)
	}
	return b.String()
}

// humanSize matches the httpapi attachment formatter so the same file reads
// the same size everywhere it is announced.
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

// canonicalPath locates a row's authentic bytes. The id is server-minted, but
// verify locality anyway so no DB state can turn this into a traversal.
func (l Library) canonicalPath(id string) (string, error) {
	if id == "" || !filepath.IsLocal(id) || strings.ContainsAny(id, `/\`) {
		return "", fmt.Errorf("unsafe shared file id %q", id)
	}
	return filepath.Join(l.CanonicalDir, id), nil
}

// SaveCanonical streams the upload into the canonical tree (temp file +
// rename, so a crashed upload never leaves a half-written canonical file) and
// returns the byte count and hex SHA-256 for the manifest row.
func (l Library) SaveCanonical(id string, r io.Reader) (int64, string, error) {
	dst, err := l.canonicalPath(id)
	if err != nil {
		return 0, "", err
	}
	// 0o700: canonical bytes are control-plane state; nothing but this process
	// reads them.
	if err := os.MkdirAll(l.CanonicalDir, 0o700); err != nil {
		return 0, "", fmt.Errorf("mkdir canonical dir: %w", err)
	}
	tmp, err := os.CreateTemp(l.CanonicalDir, ".upload-*")
	if err != nil {
		return 0, "", fmt.Errorf("create temp: %w", err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		return 0, "", fmt.Errorf("write canonical: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return 0, "", fmt.Errorf("close canonical: %w", err)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return 0, "", fmt.Errorf("finalize canonical: %w", err)
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// RemoveCanonical deletes a row's authentic bytes. Absent is success — the end
// state is what the caller asked for.
func (l Library) RemoveCanonical(id string) error {
	p, err := l.canonicalPath(id)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Stage copies a row's canonical bytes to its sandbox-visible path (temp +
// rename inside the same directory, so readers never observe a half-copied
// file). 0o644 with 0o755 dirs: the rootless sandbox user (uid 1000) must be
// able to read them through the mount; write protection is the mount's job
// (read-only bind / subPath mount), not the file mode's.
func (l Library) Stage(f store.SharedFile) error {
	dst, err := l.StagedPath(f)
	if err != nil {
		return err
	}
	src, err := l.canonicalPath(f.ID)
	if err != nil {
		return err
	}
	in, err := os.Open(src) //nolint:gosec // canonicalPath confines to CanonicalDir
	if err != nil {
		return fmt.Errorf("open canonical %s: %w", f.ID, err)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { //nolint:gosec // staged tree is read by the sandbox uid — see the function comment
		return fmt.Errorf("mkdir staged folder: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".staging-*")
	if err != nil {
		return fmt.Errorf("create staging temp: %w", err)
	}
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
	}()
	if _, err := io.Copy(tmp, in); err != nil {
		return fmt.Errorf("stage %s: %w", f.ID, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close staged: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil { //nolint:gosec // see the function comment
		return fmt.Errorf("chmod staged: %w", err)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return fmt.Errorf("finalize staged: %w", err)
	}
	return nil
}

// Unstage removes a row's sandbox-visible copy and prunes its folder if that
// left it empty. Absent is success.
func (l Library) Unstage(f store.SharedFile) error {
	p, err := l.StagedPath(f)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if f.Folder != "" {
		// Best-effort: fails (and is ignored) when the folder still has files.
		_ = os.Remove(filepath.Join(l.StagedRoot, f.Folder))
	}
	return nil
}

// EnsureStagedRoot creates the staged root. Called at boot BEFORE the sandbox
// pool spawns anything: on kubernetes the pod spec mounts the claim's "shared"
// subPath, and if the kubelet creates that directory first it is root-owned
// and the control plane can no longer write into it.
func (l Library) EnsureStagedRoot() error {
	return os.MkdirAll(l.StagedRoot, 0o755) //nolint:gosec // read by the sandbox uid through the mount
}

// Sync reconciles the staged tree against the manifest: it stages every row
// that is missing or has the wrong size, and removes every regular file the
// manifest doesn't claim (an agent cannot write here — the mount is read-only —
// but deleted rows, crashed mutations, and host-side edits all leave strays).
// Empty folders left behind are pruned. Best-effort per entry: one bad row
// must not stop the rest of the library from healing; the first error is
// returned after the full pass so callers can log it.
func (l Library) Sync(files []store.SharedFile) error {
	if err := l.EnsureStagedRoot(); err != nil {
		return err
	}
	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	want := make(map[string]store.SharedFile, len(files))
	for _, f := range files {
		rel, err := stagedRel(f)
		if err != nil {
			note(err)
			continue
		}
		want[rel] = f
	}

	// Remove strays (and collect dirs for the empty-prune below).
	var dirs []string
	note(filepath.WalkDir(l.StagedRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == l.StagedRoot {
			return nil
		}
		rel, relErr := filepath.Rel(l.StagedRoot, path)
		if relErr != nil {
			return relErr
		}
		if d.IsDir() {
			dirs = append(dirs, path)
			return nil
		}
		if _, ok := want[rel]; !ok {
			//nolint:gosec // G122: this walks the server's own staged tree (a root the control plane created), removing files the manifest does not claim; paths come from WalkDir over that server-controlled root, not from request input, and handler staging is serialized against this pass by the caller's mutex.
			note(os.Remove(path))
		}
		return nil
	}))

	// Stage what's missing or wrong-sized. Size is the cheap integrity check —
	// the mount makes in-sandbox tampering impossible, so this only has to
	// catch incomplete copies and out-of-band edits, and hashing multi-GB
	// files on every hourly pass would be real I/O for no added guarantee.
	for rel, f := range want {
		p := filepath.Join(l.StagedRoot, rel)
		info, err := os.Stat(p)
		if err == nil && info.Mode().IsRegular() && info.Size() == f.SizeBytes {
			continue
		}
		note(l.Stage(f))
	}

	// Prune empty leftover folders, deepest first.
	for i := len(dirs) - 1; i >= 0; i-- {
		if _, ok := usedDir(want, l.StagedRoot, dirs[i]); !ok {
			_ = os.Remove(dirs[i]) // fails harmlessly when non-empty
		}
	}
	return firstErr
}

// usedDir reports whether any manifest entry lives under dir.
func usedDir(want map[string]store.SharedFile, root, dir string) (string, bool) {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return "", true // can't tell — leave it alone
	}
	prefix := rel + string(filepath.Separator)
	for k := range want {
		if strings.HasPrefix(k, prefix) {
			return k, true
		}
	}
	return "", false
}

// TotalBytes sums the manifest — the number the quota check and the admin UI
// meter report.
func TotalBytes(files []store.SharedFile) int64 {
	var total int64
	for _, f := range files {
		total += f.SizeBytes
	}
	return total
}
