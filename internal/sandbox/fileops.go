package sandbox

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// fileops.go is the sandboxed file-operation seam (#784). The model-callable
// view_file / write_file / edit_file tools used to run os.ReadFile / os.WriteFile
// directly in the Fleet host process, contradicting the ADR-0002 invariant that
// EVERY agent tool call executes inside the rootless-Podman sandbox. This seam
// routes those operations through the SAME per-turn backend as bash/run_python,
// so runtime selection (crun/kata/krun), seccomp, dropped caps, cgroups, PID and
// disk limits, and the lockdown network posture all apply by construction.
//
// The tool layer still does host-side path validation (resolveWorkspacePath /
// ValidatePath) as defense-in-depth INPUT, then hands an absolute, pre-validated
// path to this seam; execution — the actual read/write/edit — happens in the
// sandbox, not on the host.

// FileOpKind selects the operation RunFileOp performs.
type FileOpKind string

const (
	FileOpRead  FileOpKind = "read"
	FileOpWrite FileOpKind = "write"
	FileOpEdit  FileOpKind = "edit"
)

// FileOpRequest is one file operation dispatched into the sandbox. Path must be
// absolute and already validated by the tool layer; the sandbox re-resolves it
// inside the container, where the workspace is bind-mounted at the same path.
type FileOpRequest struct {
	Op   FileOpKind
	Path string

	// Read.
	Offset int64
	Limit  int64

	// Write.
	Data []byte

	// Edit.
	OldText    string
	NewText    string
	ReplaceAll bool
	// ExpectedSHA256 is an optional stale-content guard (#787): when set, the
	// edit fails (ErrFileOpStale) unless the file's current content hashes to
	// it. Accepts an optional "sha256:" prefix; case-insensitive hex.
	ExpectedSHA256 string
}

// FileOpResult is the outcome of a FileOpRequest.
type FileOpResult struct {
	Data          []byte // read: the bytes read
	Size          int64  // read: total file size; write: bytes written
	ReplacedCount int    // edit: number of occurrences replaced
	// SHA256 is the resulting file's content hash: the full-file hash on read,
	// the written content's hash on write/edit (#787 — the version handle for
	// a later edit's ExpectedSHA256).
	SHA256 string
	// OldSHA256 / Added / Removed / Diff describe an edit (#787): the
	// pre-edit hash, the +/- line counts, and a bounded unified diff.
	OldSHA256 string
	Added     int
	Removed   int
	Diff      string
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
	// ErrFileOpStale: the file changed since ExpectedSHA256 was captured (#787).
	ErrFileOpStale = errors.New("file content has changed since it was last read")
	// ErrFileOpNoOp: old_text and new_text produce identical content (#787).
	ErrFileOpNoOp = errors.New("edit is a no-op")
)

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
	s.mu.Unlock()
	if s.impl.poisoned() {
		return FileOpResult{}, ErrPoisoned
	}
	if len(req.Path) == 0 || req.Path[0] != '/' {
		return FileOpResult{}, errors.New("fileop path must be absolute")
	}
	return s.impl.runFileOp(ctx, req)
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
			return FileOpResult{}, fmt.Errorf("%w: %s", ErrFileOpStale, resp.Err)
		case "noop":
			return FileOpResult{}, ErrFileOpNoOp
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
	}, nil
}
