package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

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

// errStreamTooLarge: the source held more than sandboxStreamWrite's cap.
var errStreamTooLarge = errors.New("stream exceeded its size cap")

// sandboxFileSHA256 probes a validated path through the FileOp seam: whether
// an entry exists there and, for a regular file of at most maxSize bytes, its
// SHA-256 (the executor hashes the whole file whatever window it returns). A
// larger file reports no hash without being read; the op's time limit scales
// with maxSize, so hashing a large file on slow storage does not time out
// (a timed-out file op retires the sandbox).
func sandboxFileSHA256(ctx context.Context, sb *sandbox.Sandbox, resolved, valid string, maxSize int64) (exists bool, sum string, err error) {
	root, err := fileOpRoot(ctx, resolved, valid, false)
	if err != nil {
		return false, "", err
	}
	res, err := sb.RunFileOp(ctx, sandbox.FileOpRequest{
		Op: sandbox.FileOpRead, Path: valid, Root: root, Limit: 1,
		MaxSize: max(maxSize, 1), Timeout: hashTimeout(maxSize),
	})
	switch {
	case err == nil:
		return true, strings.TrimPrefix(strings.ToLower(res.SHA256), "sha256:"), nil
	case errors.Is(err, sandbox.ErrFileOpNotFound):
		return false, "", nil
	case errors.Is(err, sandbox.ErrFileOpIsDirectory), errors.Is(err, sandbox.ErrFileOpTooLarge):
		return true, "", nil
	default:
		return false, "", err
	}
}

// hashTimeout is the time limit for a file op that reads size bytes: a
// minute plus a second per 4 MiB, so even slow network storage finishes.
func hashTimeout(size int64) time.Duration {
	return time.Minute + time.Duration(max(size, 0)>>22)*time.Second
}

// streamTarget is where a streamed body ends up: Path, validated, in the
// streamed directory; Reuse when an identical file is already there, so the
// new copy is discarded instead of committed.
type streamTarget struct {
	Path  string
	Reuse bool
}

// sandboxStreamWrite copies src into dir through the FileOp seam without
// holding it whole: chunkBytes at a time into a hidden partial file
// (sandbox.FileOpStream). Before each chunk room(next) may refuse it (a disk
// headroom check); the release it returns runs once the chunk is on disk.
// Each step passes the partial file's ctime from the step before, so the
// executor refuses to go on if anything else changed the file in between.
// Once the whole body landed, choose picks the destination from the body's
// SHA-256 and size: a new name, committed with a no-replace rename, or an
// identical existing file, in which case the partial file is removed. More
// than maxBytes fails with errStreamTooLarge. On any failure the partial file
// is removed (or, when the abort cannot run, swept by a later streamed
// write), so nothing half-written is left behind.
func sandboxStreamWrite(ctx context.Context, sb *sandbox.Sandbox, resolvedDir, validDir string, src io.Reader,
	maxBytes int64, chunkBytes int, room func(next int64) (release func(), err error), choose func(sum string, size int64) (streamTarget, error),
) (target streamTarget, size int64, sha256Hex string, err error) {
	if sb == nil {
		return streamTarget{}, 0, "", fmt.Errorf("file access requires a sandbox; pool.Take returned nil or was bypassed")
	}
	partialName, err := sandbox.StreamPartialName()
	if err != nil {
		return streamTarget{}, 0, "", err
	}
	partial := filepath.Join(validDir, partialName)
	root, err := fileOpRoot(ctx, filepath.Join(resolvedDir, partialName), partial, true)
	if err != nil {
		return streamTarget{}, 0, "", err
	}
	started := false
	fail := func(err error) (streamTarget, int64, string, error) {
		if started {
			// Best effort on a fresh context: the caller's may be what failed.
			_, _ = sb.RunFileOp(context.WithoutCancel(ctx), sandbox.FileOpRequest{Op: sandbox.FileOpStream, Path: partial, Root: root, Abort: true})
		}
		return streamTarget{}, 0, "", err
	}
	sum := sha256.New()
	limited := io.LimitReader(src, maxBytes+1)
	buf := make([]byte, chunkBytes)
	var offset, ctime int64
	for {
		n, rerr := io.ReadFull(limited, buf)
		last := errors.Is(rerr, io.EOF) || errors.Is(rerr, io.ErrUnexpectedEOF)
		if rerr != nil && !last {
			return fail(fmt.Errorf("read response body: %w", rerr))
		}
		if offset+int64(n) > maxBytes {
			return fail(errStreamTooLarge)
		}
		release := func() {}
		if room != nil {
			if release, err = room(int64(n)); err != nil {
				return fail(err)
			}
		}
		sum.Write(buf[:n])
		started = true
		res, err := sb.RunFileOp(ctx, sandbox.FileOpRequest{
			Op: sandbox.FileOpStream, Path: partial, Root: root, Offset: offset, Data: buf[:n], ExpectedCTimeNS: ctime,
		})
		release()
		if err != nil {
			return fail(err)
		}
		if res.Size != offset+int64(n) {
			// The executor checks this too; a mismatch means the partial file
			// was written by something else, so its bytes are not the body's.
			return fail(fmt.Errorf("streamed file holds %d bytes, expected %d", res.Size, offset+int64(n)))
		}
		offset += int64(n)
		ctime = res.CTimeNS
		if last {
			break
		}
	}
	digest := hex.EncodeToString(sum.Sum(nil))
	target, err = choose(digest, offset)
	if err != nil {
		return fail(err)
	}
	if target.Reuse {
		_, _, _, _ = fail(nil)
		return target, offset, digest, nil
	}
	if filepath.Dir(target.Path) != validDir {
		return fail(fmt.Errorf("destination %s is outside the streamed directory", target.Path))
	}
	if _, err := sb.RunFileOp(ctx, sandbox.FileOpRequest{
		Op: sandbox.FileOpStream, Path: partial, Root: root, Offset: offset,
		Final: filepath.Base(target.Path), ExpectedCTimeNS: ctime, CommitOnly: true,
	}); err != nil {
		return fail(err)
	}
	return target, offset, digest, nil
}
