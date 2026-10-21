package sandbox

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// fileops.go is the sandboxed file-operation seam (#784). The model-callable
// view_file / write_file / edit_file tools used to run os.ReadFile / os.WriteFile
// directly in the Fleet host process, contradicting the ADR-0002 invariant that
// every agent tool call's model-authored local execution runs inside the
// rootless-Podman sandbox. This seam routes those operations through the SAME
// per-turn backend as bash/run_python,
// so runtime selection (crun/kata/krun), seccomp, dropped caps, cgroups, PID and
// disk limits, and the lockdown network posture all apply by construction.
//
// The tool layer still does host-side path validation (resolveWorkspacePath /
// ValidatePath) as defense-in-depth INPUT, then hands an absolute path plus a
// narrow policy root to this seam. The executor enforces that root again with
// dirfd-relative no-follow traversal; execution — the actual read/write/edit —
// happens in the sandbox, not on the host.

// FileOpKind selects the operation RunFileOp performs.
type FileOpKind string

const (
	FileOpRead     FileOpKind = "read"
	FileOpWrite    FileOpKind = "write"
	FileOpEdit     FileOpKind = "edit"
	fileOpBindRoot FileOpKind = "bind_root"
)

// FileOpRequest is one file operation dispatched into the sandbox. Path and
// Root must be absolute and already validated by the tool layer; the sandbox
// opens them inside the container, where mounts use the same absolute paths.
type FileOpRequest struct {
	Op   FileOpKind
	Path string
	// Root is the narrow policy scope Path must remain beneath for the entire
	// operation. The executor opens it relative to one of the container's
	// trusted bind-mount roots and walks every descendant with dirfd-relative,
	// no-follow operations. Host-side validation alone cannot provide this
	// guarantee because an agent process in a persistent sandbox can race a
	// checked pathname by replacing an ancestor with a symlink.
	Root string

	// Read. Limit is the window length (0 = to EOF), not a size cap; the
	// helper still streams the whole file through SHA-256. MaxSize > 0 refuses
	// a file whose size exceeds it before reading a byte, so a cap the caller
	// will enforce anyway does not first cost a full hash of a huge file (and,
	// for a big enough one, a file-op timeout that retires the sandbox).
	Offset  int64
	Limit   int64
	MaxSize int64

	// Write.
	Data []byte

	// Edit.
	OldText    string
	NewText    string
	ReplaceAll bool
	// ExpectedSHA256 is an optional stale-content guard for an edit (#787) or
	// a write (#1745): when set, the op fails (ErrFileOpStale) and leaves the
	// file untouched unless the file's current content hashes to it. Accepts
	// an optional "sha256:" prefix; case-insensitive hex. A write also takes
	// ExpectAbsent: create the file only if nothing exists at Path yet.
	ExpectedSHA256 string

	// testPause/testReadyName provide a deterministic rendezvous for the
	// cancellation and symlink-swap integration tests. They are deliberately
	// unexported and never populated by the tool layer or serialized unless a
	// same-package test sets them.
	testPause     time.Duration
	testReadyName string
	expectedDev   uint64
	expectedIno   uint64
	rootBound     bool
}

// ExpectAbsent is the ExpectedSHA256 value that makes a write create-only
// (#1745): it fails with ErrFileOpStale if anything exists at the path.
const ExpectAbsent = "absent"

// FileOpResult is the outcome of a FileOpRequest.
type FileOpResult struct {
	Data          []byte // read: the bytes read
	Size          int64  // read: total file size; write: bytes written
	ReplacedCount int    // edit: number of occurrences replaced
	// SHA256 is the resulting file's content hash: the full-file hash on read,
	// the written content's hash on write/edit (#787 — the version handle for
	// a later edit's ExpectedSHA256).
	SHA256 string
	// OldSHA256 / Added / Removed / Diff describe an edit (#787), and the file
	// a write replaced (#1745): the old hash, the +/- line counts, and a
	// bounded unified diff. A write leaves OldSHA256 empty when what it
	// replaced was not a regular file.
	OldSHA256 string
	Added     int
	Removed   int
	Diff      string
	// Replaced and OldSize report that a write replaced an existing entry,
	// and that entry's size (#1745). DiffNote says why such a write has no
	// diff (binary content, a file or change over the diff caps), or no old
	// hash either (an old entry that was not a regular file, or could not be
	// read), and why an edit has no diff (a change over the line cap); Added
	// and Removed are then zero.
	Replaced     bool
	OldSize      int64
	DiffNote     string
	rootIdentity FileOpRootIdentity
}

// FileOpRootIdentity is captured inside the selected container runtime before
// the model receives any tools. Later one-shot helpers must observe the same
// directory identity. O_NOFOLLOW alone rejects symlink swaps but cannot detect
// a same-UID process exchanging an entire sibling directory into the trusted
// conversation pathname.
type FileOpRootIdentity struct {
	Dev uint64
	Ino uint64
}

// Typed sentinels so the tool layer can reproduce exact model-facing messages
// regardless of which backend executed the op.
var (
	ErrFileOpNotFound      = errors.New("file not found")
	ErrFileOpIsDirectory   = errors.New("path is a directory")
	ErrFileOpOldTextAbsent = errors.New("old_text not found in file")
	// ErrFileOpAmbiguous: edit_text matched more than one location and
	// replace_all was not set (#787). *FileOpAmbiguousError carries the count.
	ErrFileOpAmbiguous = errors.New("old_text matches multiple locations")
	// ErrFileOpStale: the file is not the one ExpectedSHA256 names (#787, #1745):
	// it changed since, is gone, or (ExpectAbsent) already exists.
	// *FileOpStaleError carries the executor's explanation.
	ErrFileOpStale = errors.New("file content has changed since it was last read")
	// ErrFileOpNoOp: old_text and new_text produce identical content (#787).
	ErrFileOpNoOp       = errors.New("edit is a no-op")
	ErrFileOpUnsafePath = errors.New("file operation path changed or contains a symlink")
)

// FileOpStaleError carries the executor's explanation of a stale-content
// refusal (#787, #1745): what was expected, what is there, and how to recover.
type FileOpStaleError struct {
	Msg string
}

func (e *FileOpStaleError) Error() string {
	if e.Msg == "" {
		return ErrFileOpStale.Error()
	}
	return e.Msg
}
func (e *FileOpStaleError) Is(target error) bool {
	return target == ErrFileOpStale
}

// FileOpAmbiguousError carries the match count + the executor's guidance for an
// ambiguous edit so the tool layer can surface both.
type FileOpAmbiguousError struct {
	Count int
	Msg   string
}

func (e *FileOpAmbiguousError) Error() string { return e.Msg }
func (e *FileOpAmbiguousError) Is(target error) bool {
	return target == ErrFileOpAmbiguous
}

//go:embed fileops.py
var fileOpsScript []byte

// RunFileOp dispatches one file operation through the active backend (#784).
// Returns ErrClosed if the sandbox was torn down, ErrPoisoned if a cancelled
// command left it in an unproven state, and one of the typed FileOp sentinels
// for semantic failures (missing file, directory, old_text absent).
func (s *Sandbox) RunFileOp(ctx context.Context, req FileOpRequest) (FileOpResult, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return FileOpResult{}, ErrClosed
	}
	boundRoot := s.fileOpRoot
	boundIdentity := s.fileOpRootIdentity
	s.mu.Unlock()
	if s.impl.poisoned() {
		return FileOpResult{}, ErrPoisoned
	}
	if len(req.Path) == 0 || req.Path[0] != '/' {
		return FileOpResult{}, errors.New("fileop path must be absolute")
	}
	if len(req.Root) == 0 || req.Root[0] != '/' {
		return FileOpResult{}, errors.New("fileop root must be absolute")
	}
	inside, err := pathWithin(req.Root, req.Path)
	if err != nil {
		return FileOpResult{}, fmt.Errorf("validate fileop scope: %w", err)
	}
	if !inside {
		return FileOpResult{}, ErrFileOpUnsafePath
	}
	if boundRoot != "" {
		// The tool layer may scope a request to a NARROWER root than the one
		// bound for this turn: a sub-agent's forced working directory is
		// <bound root>/subagents/<child> (#1043), and git-worktree isolation
		// hands out a similar sub-tree. That scope is a write de-confliction
		// default, not a capability of its own — the blessed capability is the
		// bound root, and a sub-tree of it is by construction inside it. So a
		// root that is the bound root OR lies beneath it is re-anchored at the
		// bound root and carries its identity guard; the container backends
		// then walk from the verified anchor, dirfd-relative and no-follow,
		// down through the sub-tree to the target. Matching the bound root by
		// exact equality alone made every sub-agent view_file/write_file/
		// edit_file fail closed on the podman and kubernetes backends ("writable
		// fileop root was not bound before the turn") while the host executor,
		// which has no such gate, let the tests pass. A root OUTSIDE the bound
		// root (a read-only supporting-doc mount) is left as-is: it is anchored
		// by the backend's own mount table and never writable.
		if inside, werr := pathWithin(boundRoot, req.Root); werr == nil && inside {
			req.Root = boundRoot
			req.rootBound = true
			req.expectedDev = boundIdentity.Dev
			req.expectedIno = boundIdentity.Ino
		}
	}
	return s.impl.runFileOp(ctx, req)
}

// BindFileOpRoot establishes the writable FileOp capability for this sandbox.
// It must be called immediately after a sandbox is assigned to a conversation
// or scheduled run and before any model tool executes. Persistent sandboxes
// retain the first identity; a later turn cannot bless an exchanged directory.
func (s *Sandbox) BindFileOpRoot(ctx context.Context, root string) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve fileop root: %w", err)
	}
	abs = filepath.Clean(abs)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.fileOpRoot != "" {
		if s.fileOpRoot != abs {
			return fmt.Errorf("sandbox already bound to a different fileop root: %w", ErrFileOpUnsafePath)
		}
		return nil
	}
	if s.impl.poisoned() {
		return ErrPoisoned
	}
	identity, err := s.impl.bindFileOpRoot(ctx, abs)
	if err != nil {
		return err
	}
	if identity.Dev == 0 && identity.Ino == 0 {
		return errors.New("fileop root identity was empty")
	}
	s.fileOpRoot = abs
	s.fileOpRootIdentity = identity
	return nil
}

// pathWithin performs a lexical, component-aware containment check. It is only
// an input sanity check; the security boundary is the executor's dirfd walk.
func pathWithin(root, path string) (bool, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false, err
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(filepath.Clean(rootAbs), filepath.Clean(pathAbs))
	if err != nil {
		return false, err
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel), nil
}

// encodeFileOpWire builds the JSON request fileops.py reads. Every backend
// (podman, kubernetes, host) sends it, so a field added here reaches all
// three: the podman and host backends once built their own copies, and a
// field missing from one silently dropped a guard on that backend.
func encodeFileOpWire(req FileOpRequest, anchor string) ([]byte, error) {
	wire := map[string]any{
		"op":     string(req.Op),
		"path":   req.Path,
		"root":   req.Root,
		"anchor": anchor,
	}
	switch req.Op {
	case FileOpRead:
		wire["offset"] = req.Offset
		wire["limit"] = req.Limit
		if req.MaxSize > 0 {
			wire["max_size"] = req.MaxSize
		}
	case FileOpWrite:
		wire["data_b64"] = base64.StdEncoding.EncodeToString(req.Data)
		if req.ExpectedSHA256 != "" {
			wire["expected_sha256"] = req.ExpectedSHA256
		}
	case FileOpEdit:
		wire["old_b64"] = base64.StdEncoding.EncodeToString([]byte(req.OldText))
		wire["new_b64"] = base64.StdEncoding.EncodeToString([]byte(req.NewText))
		wire["replace_all"] = req.ReplaceAll
		if req.ExpectedSHA256 != "" {
			wire["expected_sha256"] = req.ExpectedSHA256
		}
	case fileOpBindRoot:
		// Root + anchor are the complete request.
	default:
		return nil, fmt.Errorf("unknown fileop %q", req.Op)
	}
	if req.testPause > 0 {
		wire["test_pause_ms"] = req.testPause.Milliseconds()
		wire["test_ready_name"] = req.testReadyName
	}
	if req.rootBound {
		wire["expected_dev"] = req.expectedDev
		wire["expected_ino"] = req.expectedIno
	}
	out, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("marshal fileop: %w", err)
	}
	return out, nil
}

// fileOpResponse is the JSON shape fileops.py writes (shared by both backends;
// the host backend runs the same script logic in-process, see host.go).
type fileOpResponse struct {
	OK         bool   `json:"ok"`
	ErrKind    string `json:"err_kind"`
	Err        string `json:"err"`
	DataB64    string `json:"data_b64"`
	Size       int64  `json:"size"`
	Count      int    `json:"count"`
	SHA256     string `json:"sha256"`
	OldSHA256  string `json:"old_sha256"`
	MatchCount int    `json:"match_count"`
	Added      int    `json:"added"`
	Removed    int    `json:"removed"`
	Diff       string `json:"diff"`
	Hint       string `json:"hint"`
	Replaced   bool   `json:"replaced"`
	OldSize    int64  `json:"old_size"`
	DiffNote   string `json:"diff_note"`
	Dev        uint64 `json:"dev"`
	Ino        uint64 `json:"ino"`
}

// decodeFileOpResponse maps the executor's JSON envelope into a FileOpResult or
// a typed sentinel error, so every backend surfaces identical semantics.
func decodeFileOpResponse(out []byte) (FileOpResult, error) {
	var resp fileOpResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return FileOpResult{}, fmt.Errorf("decode fileop response: %w (raw: %.200s)", err, string(out))
	}
	if !resp.OK {
		switch resp.ErrKind {
		case "not_found":
			return FileOpResult{}, ErrFileOpNotFound
		case "is_dir":
			return FileOpResult{}, ErrFileOpIsDirectory
		case "old_absent":
			if resp.Hint != "" {
				return FileOpResult{}, fmt.Errorf("%w (%s)", ErrFileOpOldTextAbsent, resp.Hint)
			}
			return FileOpResult{}, ErrFileOpOldTextAbsent
		case "ambiguous":
			return FileOpResult{}, &FileOpAmbiguousError{Count: resp.MatchCount, Msg: resp.Err}
		case "stale":
			return FileOpResult{}, &FileOpStaleError{Msg: resp.Err}
		case "noop":
			return FileOpResult{}, ErrFileOpNoOp
		case "unsafe_path":
			return FileOpResult{}, ErrFileOpUnsafePath
		default:
			return FileOpResult{}, errors.New(resp.Err)
		}
	}
	var data []byte
	if resp.DataB64 != "" {
		var derr error
		data, derr = base64.StdEncoding.DecodeString(resp.DataB64)
		if derr != nil {
			return FileOpResult{}, fmt.Errorf("decode fileop data: %w", derr)
		}
	}
	return FileOpResult{
		Data: data, Size: resp.Size, ReplacedCount: resp.Count, SHA256: resp.SHA256,
		OldSHA256: resp.OldSHA256, Added: resp.Added, Removed: resp.Removed, Diff: resp.Diff,
		Replaced: resp.Replaced, OldSize: resp.OldSize, DiffNote: resp.DiffNote,
		rootIdentity: FileOpRootIdentity{Dev: resp.Dev, Ino: resp.Ino},
	}, nil
}
