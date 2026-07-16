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
}

// FileOpResult is the outcome of a FileOpRequest.
type FileOpResult struct {
	Data          []byte // read: the bytes read
	Size          int64  // read: total file size; write: bytes written
	ReplacedCount int    // edit: number of occurrences replaced
}

// Typed sentinels so the tool layer can reproduce today's exact model-facing
// messages regardless of which backend executed the op.
var (
	ErrFileOpNotFound      = errors.New("file not found")
	ErrFileOpIsDirectory   = errors.New("path is a directory")
	ErrFileOpOldTextAbsent = errors.New("old_text not found in file")
)

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
	OK      bool   `json:"ok"`
	ErrKind string `json:"err_kind"`
	Err     string `json:"err"`
	DataB64 string `json:"data_b64"`
	Size    int64  `json:"size"`
	Count   int    `json:"count"`
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
			return FileOpResult{}, ErrFileOpOldTextAbsent
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
	return FileOpResult{Data: data, Size: resp.Size, ReplacedCount: resp.Count}, nil
}
