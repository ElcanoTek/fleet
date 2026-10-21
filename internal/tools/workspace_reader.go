package tools

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ElcanoTek/fleet/internal/sandbox"
)

// ErrWorkspaceFileHashMismatch: the workspace file's bytes, hashed by Fleet,
// are not the version the reference's sha256 names.
var ErrWorkspaceFileHashMismatch = errors.New("workspace file SHA-256 mismatch; no MCP call was made")

// ErrWorkspaceFileBudget: reading the file would need more transfer memory
// than the whole process-wide workspace-file budget, so it can never run.
var ErrWorkspaceFileBudget = errors.New("workspace file is too large for the workspace-file transfer budget")

// WorkspaceFileReadFunc returns [offset, offset+length) of the workspace file
// at path, provided the WHOLE file hashes (in Fleet, not in the sandbox) to
// sha256Hex and is at most maxFileBytes. The window may be shorter than length
// when the range runs past the end of the file.
type WorkspaceFileReadFunc func(ctx context.Context, path, sha256Hex string, offset, length, maxFileBytes int64) ([]byte, error)

// workspaceFileSource is the sandbox side of the reader. size is a cheap probe
// (no hashing) and its answer is untrusted: it only sizes the reservation and
// the cap of the read that follows, and read enforces that cap twice — the
// helper refuses a file over it, and the FileOp output sink refuses helper
// output longer than it can produce. read returns the whole file.
type workspaceFileSource struct {
	size func(ctx context.Context, root, path string) (int64, error)
	read func(ctx context.Context, root, path string, maxBytes int64) ([]byte, error)
}

// Budget (ADR-0087). Everything Fleet holds for workspace-file references —
// verified files in the cache plus the reservations of reads in flight — stays
// within workspaceFileBudgetBytes, process wide.
//
// A miss for a file of S bytes reserves workspaceFileMissCost(S) = 4S + 128 KiB
// before its read starts. That covers the read's peak: the FileOp output
// buffer (base64, at most 4S/3 + 64 KiB, grown no further than that), the
// string the JSON decoder copies it into (the same again), and the decoded
// S bytes. When the read finishes the reservation is released and, if the
// file verified, its S bytes are charged to the cache instead.
//
// One 64 MiB file therefore reserves 256 MiB + 128 KiB; the budget of 320 MiB
// admits that alongside up to 64 MiB of cached files, and evicts cached files
// (least recently used first) to make room before a miss waits.
const (
	workspaceFileBudgetBytes     = 320 << 20
	workspaceFileCacheMaxBytes   = 128 << 20
	workspaceFileCacheMaxEntries = 2
	workspaceFileCacheTTL        = 5 * time.Minute
	workspaceFileMissOverhead    = 128 << 10
)

func workspaceFileMissCost(size int64) int64 {
	return 4*max(size, 1) + workspaceFileMissOverhead
}

// workspaceFileStore is the verified-bytes LRU plus the admission control for
// reads that would fill it. Cached entries are immutable. The key carries the
// reader's identity (one per run/turn binding to a sandbox), the workspace
// root, the path and the hash, so a run can never be served another run's
// file and a different version of the file is a different key.
type workspaceFileStore struct {
	mu         sync.Mutex
	budget     int64
	maxBytes   int64
	maxEntries int
	ttl        time.Duration
	now        func() time.Time

	order    *list.List // front = most recently used; values are *verifiedFile
	byKey    map[verifiedFileKey]*list.Element
	cached   int64
	reserved int64
	peak     int64         // highest cached+reserved seen; for tests
	changed  chan struct{} // closed and replaced whenever room is freed
	flights  map[verifiedFileKey]*fileFlight
}

type verifiedFileKey struct {
	reader uint64
	root   string
	path   string
	sha256 string
}

type verifiedFile struct {
	key     verifiedFileKey
	data    []byte
	expires time.Time
}

// fileFlight is one in-progress miss. Identical concurrent misses wait on it
// instead of each reading (and reserving for) the same file.
type fileFlight struct {
	done chan struct{}
	data []byte
	err  error
}

func newWorkspaceFileStore(budget, maxBytes int64, maxEntries int, ttl time.Duration, now func() time.Time) *workspaceFileStore {
	return &workspaceFileStore{budget: budget, maxBytes: maxBytes, maxEntries: maxEntries, ttl: ttl, now: now,
		order: list.New(), byKey: map[verifiedFileKey]*list.Element{},
		changed: make(chan struct{}), flights: map[verifiedFileKey]*fileFlight{}}
}

// load returns the verified bytes for key, running fill at most once across
// concurrent callers of the same key.
func (s *workspaceFileStore) load(ctx context.Context, key verifiedFileKey, fill func(context.Context) ([]byte, error)) ([]byte, error) {
	for {
		s.mu.Lock()
		if data, ok := s.getLocked(key); ok {
			s.mu.Unlock()
			return data, nil
		}
		if flight, ok := s.flights[key]; ok {
			s.mu.Unlock()
			select {
			case <-flight.done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			// The leader's own cancellation is not this caller's failure.
			if flight.err != nil && (errors.Is(flight.err, context.Canceled) || errors.Is(flight.err, context.DeadlineExceeded)) && ctx.Err() == nil {
				continue
			}
			return flight.data, flight.err
		}
		flight := &fileFlight{done: make(chan struct{})}
		s.flights[key] = flight
		s.mu.Unlock()

		flight.data, flight.err = fill(ctx)
		s.mu.Lock()
		delete(s.flights, key)
		s.mu.Unlock()
		close(flight.done)
		return flight.data, flight.err
	}
}

// reserve admits a read costing n bytes, evicting cached files to make room
// and otherwise waiting for reads in flight to finish. A cost over the whole
// budget fails at once: no amount of waiting could admit it.
func (s *workspaceFileStore) reserve(ctx context.Context, n int64) error {
	if n > s.budget {
		return fmt.Errorf("%w: reading it needs %d bytes, the process-wide budget is %d", ErrWorkspaceFileBudget, n, s.budget)
	}
	for {
		s.mu.Lock()
		for s.order.Len() > 0 && s.cached+s.reserved+n > s.budget {
			s.removeLocked(s.order.Back())
		}
		if s.cached+s.reserved+n <= s.budget {
			s.reserved += n
			s.peak = max(s.peak, s.cached+s.reserved)
			s.mu.Unlock()
			return nil
		}
		wait := s.changed
		s.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// settle ends a reservation of n bytes. data, when non-nil, is a verified file
// to cache under key; its bytes come out of the reservation being released.
func (s *workspaceFileStore) settle(n int64, key verifiedFileKey, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reserved -= n
	if data != nil {
		s.putLocked(key, data)
	}
	s.broadcastLocked()
}

func (s *workspaceFileStore) getLocked(key verifiedFileKey) ([]byte, bool) {
	el, ok := s.byKey[key]
	if !ok {
		return nil, false
	}
	entry := el.Value.(*verifiedFile)
	if !s.now().Before(entry.expires) {
		s.removeLocked(el)
		s.broadcastLocked()
		return nil, false
	}
	s.order.MoveToFront(el)
	return entry.data, true
}

func (s *workspaceFileStore) putLocked(key verifiedFileKey, data []byte) {
	size := int64(len(data))
	if size > s.maxBytes {
		return
	}
	if el, ok := s.byKey[key]; ok {
		s.removeLocked(el)
	}
	for s.order.Len() > 0 && (s.order.Len() >= s.maxEntries || s.cached+size > s.maxBytes || s.cached+s.reserved+size > s.budget) {
		s.removeLocked(s.order.Back())
	}
	if s.cached+s.reserved+size > s.budget {
		return
	}
	s.byKey[key] = s.order.PushFront(&verifiedFile{key: key, data: data, expires: s.now().Add(s.ttl)})
	s.cached += size
	s.peak = max(s.peak, s.cached+s.reserved)
}

func (s *workspaceFileStore) removeLocked(el *list.Element) {
	entry := s.order.Remove(el).(*verifiedFile)
	delete(s.byKey, entry.key)
	s.cached -= int64(len(entry.data))
}

func (s *workspaceFileStore) broadcastLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

var (
	workspaceFiles = newWorkspaceFileStore(workspaceFileBudgetBytes, workspaceFileCacheMaxBytes,
		workspaceFileCacheMaxEntries, workspaceFileCacheTTL, time.Now)
	workspaceReaderIDs atomic.Uint64
)

// WorkspaceFileReader is a bounded, sandbox-only input seam for brokered MCP
// binary arguments. References cannot select supporting-doc mounts or sibling
// conversations. The sandbox pins the root and rejects symlink traversal.
//
// The sandbox is model-writable, so nothing it reports about a file is taken
// on trust: the whole file is read out once (refused by size first if it is
// over the cap) and Fleet hashes the bytes itself. The verified bytes are kept
// in a small LRU so the next window of the same reference is sliced from them
// instead of re-reading the file, and every read is admitted against one
// process-wide memory budget before it starts (ADR-0087).
func WorkspaceFileReader(sb *sandbox.Sandbox) WorkspaceFileReadFunc {
	var src *workspaceFileSource
	if sb != nil {
		src = &workspaceFileSource{
			size: func(ctx context.Context, root, path string) (int64, error) {
				// MaxSize 1: the helper answers from fstat alone (a too_large
				// refusal carrying the size) without hashing the file.
				result, err := sb.RunFileOp(ctx, sandbox.FileOpRequest{
					Op: sandbox.FileOpRead, Root: root, Path: filepath.Join(root, path), Limit: 1, MaxSize: 1,
				})
				var tooLarge *sandbox.FileOpTooLargeError
				switch {
				case err == nil:
					return result.Size, nil
				case errors.As(err, &tooLarge) && tooLarge.Size > 0:
					return tooLarge.Size, nil
				default:
					return 0, fmt.Errorf("cannot read workspace file: %w", err)
				}
			},
			read: func(ctx context.Context, root, path string, maxBytes int64) ([]byte, error) {
				result, err := sb.RunFileOp(ctx, sandbox.FileOpRequest{
					Op: sandbox.FileOpRead, Root: root, Path: filepath.Join(root, path), MaxSize: maxBytes,
				})
				if err != nil {
					if errors.Is(err, sandbox.ErrFileOpTooLarge) {
						return nil, fmt.Errorf("workspace file changed size while it was read; retry: %w", err)
					}
					return nil, fmt.Errorf("cannot read workspace file: %w", err)
				}
				return result.Data, nil
			},
		}
	}
	return newVerifiedWorkspaceReader(src, workspaceFiles)
}

func newVerifiedWorkspaceReader(src *workspaceFileSource, store *workspaceFileStore) WorkspaceFileReadFunc {
	id := workspaceReaderIDs.Add(1)
	return func(ctx context.Context, path, sha256Hex string, offset, length, maxFileBytes int64) ([]byte, error) {
		if maxFileBytes <= 0 {
			return nil, fmt.Errorf("workspace file reader requires a positive byte limit")
		}
		if offset < 0 || length <= 0 || offset > maxFileBytes || length > maxFileBytes-offset {
			return nil, fmt.Errorf("workspace file reader requires a byte range within %d bytes", maxFileBytes)
		}
		if src == nil {
			return nil, fmt.Errorf("workspace file reference requires a sandbox")
		}
		if !filepath.IsLocal(path) || containsDotDotComponent(path) {
			return nil, fmt.Errorf("workspace_file must be a relative path without '..'")
		}
		root := ForcedWorkingDirFromContext(ctx)
		if root == "" && ConversationIDFromContext(ctx) != "" {
			root = WorkspaceDirForConversation(ConversationIDFromContext(ctx))
		}
		if root == "" {
			return nil, fmt.Errorf("workspace file reference requires an active workspace")
		}
		key := verifiedFileKey{reader: id, root: root, path: filepath.Clean(path), sha256: sha256Hex}
		data, err := store.load(ctx, key, func(ctx context.Context) ([]byte, error) {
			size, err := src.size(ctx, root, path)
			if err != nil {
				return nil, err
			}
			if size > maxFileBytes {
				return nil, fmt.Errorf("workspace file is %d bytes, exceeding the %d-byte cap", size, maxFileBytes)
			}
			// The read's cap is the probed size, so the reservation below
			// bounds it however the (untrusted) probe answered.
			readCap := max(size, 1)
			cost := workspaceFileMissCost(readCap)
			if err := store.reserve(ctx, cost); err != nil {
				return nil, err
			}
			read, err := src.read(ctx, root, path, readCap)
			if err == nil && int64(len(read)) > readCap {
				err = fmt.Errorf("workspace file exceeds %d bytes", readCap)
			}
			if err == nil {
				if sum := sha256.Sum256(read); hex.EncodeToString(sum[:]) != sha256Hex {
					err = ErrWorkspaceFileHashMismatch
				}
			}
			if err != nil {
				store.settle(cost, key, nil)
				return nil, err
			}
			store.settle(cost, key, read)
			return read, nil
		})
		if err != nil {
			return nil, err
		}
		size := int64(len(data))
		start, end := min(offset, size), min(offset+length, size)
		// Full slice expression: the caller must not be able to append into
		// the cached, shared backing array.
		return data[start:end:end], nil
	}
}
