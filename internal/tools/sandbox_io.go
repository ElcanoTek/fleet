package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/ElcanoTek/fleet/internal/sandbox"
)

// sandbox_io.go — whole-file read/write helpers over the sandbox FileOp seam
// (#784) for the native tools whose file I/O used to run host-side
// (download_url, generate_image, xlsx — #1083). Host-side path validation
// (resolveWorkspacePath / ValidatePath) stays as defense-in-depth INPUT in
// each tool; the byte transfer itself executes inside the per-turn sandbox,
// so it inherits the runtime, seccomp, caps, cgroups, disk/PID limits, and
// lockdown network posture exactly like view_file/write_file/edit_file. A nil
// sandbox fails closed — there is no host-execution fallback.

// sandboxReadFile reads the whole validated file through the FileOp seam.
// maxBytes > 0 rejects files larger than the cap (checked against the file's
// true size, not the transferred window). resolved is the pre-symlink path
// (for supporting-doc root selection); valid is the validated absolute path.
func sandboxReadFile(ctx context.Context, sb *sandbox.Sandbox, resolved, valid string, maxBytes int64) ([]byte, error) {
	if sb == nil {
		return nil, fmt.Errorf("file access requires a sandbox; pool.Take returned nil or was bypassed")
	}
	root, err := fileOpRoot(ctx, resolved, valid, false)
	if err != nil {
		return nil, err
	}
	res, err := sb.RunFileOp(ctx, sandbox.FileOpRequest{
		Op:    sandbox.FileOpRead,
		Path:  valid,
		Root:  root,
		Limit: maxBytes, // 0 = whole file
	})
	if err != nil {
		return nil, err
	}
	if maxBytes > 0 && res.Size > maxBytes {
		return nil, fmt.Errorf("file %s is %d bytes, exceeding the %d-byte cap", valid, res.Size, maxBytes)
	}
	return res.Data, nil
}

// sandboxWriteFile writes data to the validated path through the FileOp seam.
// Parent directories are created inside the sandbox by the executor; the
// replacement is atomic and fsynced.
func sandboxWriteFile(ctx context.Context, sb *sandbox.Sandbox, resolved, valid string, data []byte) error {
	if sb == nil {
		return fmt.Errorf("file access requires a sandbox; pool.Take returned nil or was bypassed")
	}
	root, err := fileOpRoot(ctx, resolved, valid, true)
	if err != nil {
		return err
	}
	_, err = sb.RunFileOp(ctx, sandbox.FileOpRequest{
		Op:   sandbox.FileOpWrite,
		Path: valid,
		Root: root,
		Data: data,
	})
	return err
}

// sandboxFileExists probes a validated path via a 1-byte sandboxed read.
// Used by download_url's collision-suffix loop in place of host os.Stat.
func sandboxFileExists(ctx context.Context, sb *sandbox.Sandbox, resolved, valid string) (bool, error) {
	if sb == nil {
		return false, fmt.Errorf("file access requires a sandbox; pool.Take returned nil or was bypassed")
	}
	root, err := fileOpRoot(ctx, resolved, valid, false)
	if err != nil {
		return false, err
	}
	_, err = sb.RunFileOp(ctx, sandbox.FileOpRequest{
		Op:    sandbox.FileOpRead,
		Path:  valid,
		Root:  root,
		Limit: 1,
	})
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, sandbox.ErrFileOpNotFound):
		return false, nil
	case errors.Is(err, sandbox.ErrFileOpIsDirectory):
		return true, nil
	default:
		return false, err
	}
}

// errStreamTooLarge: the source held more than sandboxStreamWrite's cap.
var errStreamTooLarge = errors.New("stream exceeded its size cap")

// sandboxStreamWrite copies src to the validated path through the FileOp seam
// without holding it whole: chunkBytes at a time into a hidden partial file
// beside the destination (sandbox.FileOpStream), renamed to the destination
// with the last chunk, never replacing an existing file. More than maxBytes
// fails with errStreamTooLarge. On any failure the partial file is removed,
// so a destination either holds the whole body or does not exist.
func sandboxStreamWrite(ctx context.Context, sb *sandbox.Sandbox, resolved, valid string, src io.Reader, maxBytes int64, chunkBytes int) (int64, error) {
	if sb == nil {
		return 0, fmt.Errorf("file access requires a sandbox; pool.Take returned nil or was bypassed")
	}
	root, err := fileOpRoot(ctx, resolved, valid, true)
	if err != nil {
		return 0, err
	}
	partialName, err := sandbox.StreamPartialName()
	if err != nil {
		return 0, err
	}
	partial := filepath.Join(filepath.Dir(valid), partialName)
	final := filepath.Base(valid)
	started := false
	abort := func() {
		if started {
			// Best effort on a fresh context: the caller's may be what failed.
			_, _ = sb.RunFileOp(context.WithoutCancel(ctx), sandbox.FileOpRequest{Op: sandbox.FileOpStream, Path: partial, Root: root, Abort: true})
		}
	}
	limited := io.LimitReader(src, maxBytes+1)
	buf := make([]byte, chunkBytes)
	var offset int64
	for {
		n, rerr := io.ReadFull(limited, buf)
		last := errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF)
		if rerr != nil && !last {
			abort()
			return 0, fmt.Errorf("read response body: %w", rerr)
		}
		if offset+int64(n) > maxBytes {
			abort()
			return 0, errStreamTooLarge
		}
		req := sandbox.FileOpRequest{Op: sandbox.FileOpStream, Path: partial, Root: root, Offset: offset, Data: buf[:n]}
		if last {
			req.Final = final
		}
		started = true
		if _, err := sb.RunFileOp(ctx, req); err != nil {
			abort()
			return 0, err
		}
		offset += int64(n)
		if last {
			return offset, nil
		}
	}
}
