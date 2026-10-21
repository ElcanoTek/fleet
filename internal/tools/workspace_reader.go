package tools

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/base64"
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
//
// On success it also returns release, which the caller MUST call (it is safe
// to call more than once) once nothing derived from the window is live any
// more: for an MCP argument, after the outbound call has returned. Until then
// the window — the verified file it is cut from, and WorkspaceWindowCharge of
// transient copies — stays charged to the process-wide budget (ADR-0087).
type WorkspaceFileReadFunc func(ctx context.Context, path, sha256Hex string, offset, length, maxFileBytes int64) (window []byte, release func(), err error)

// workspaceFileSource is the sandbox side of the reader. size is a cheap probe
// (no hashing) and its answer is untrusted: it only sizes the reservation and
// the cap of the read that follows, and read enforces that cap twice — the
// helper refuses a file over it, and the FileOp output sink refuses helper
// output longer than it can produce. read returns the whole file.
type workspaceFileSource struct {
	size func(ctx context.Context, root, path string) (int64, error)
	read func(ctx context.Context, root, path string, maxBytes int64) ([]byte, error)
}

// Budget (ADR-0087). Every byte Fleet holds on behalf of a workspace-file
// reference is charged to one process-wide budget from before it is
// allocated until it is released. Charged items:
//
//   - a verified file, while cached or while any window lease pins it: S;
//   - a read in flight: workspaceFileMissCost(S) = 4S + 128 KiB, covering the
//     FileOp output buffer (at most base64(S) + 64 KiB, grown no further), the
//     JSON-decoded copy of it, and the decoded S bytes;
//   - a window lease, from before the window is handed out until the caller
//     releases it: WorkspaceWindowCharge(W), covering the base64 argument and
//     every whole-request copy the dispatch path makes of it (see that
//     function).
//
// A miss reserves max(missCost(S), S + windowCharge(W)) up front and, when the
// read settles, converts that into S (the file, pinned) plus windowCharge(W)
// (the lease) without waiting again, so a caller never holds part of what it
// needs while it waits for the rest. A hit reserves windowCharge(W) and pins
// the file atomically. Only unpinned files are evicted; pinned files are what
// leases point into. A request that could never fit fails at once.
const (
	workspaceFileBudgetBytes     = 384 << 20
	workspaceFileCacheMaxBytes   = 128 << 20
	workspaceFileCacheMaxEntries = 2
	workspaceFileCacheTTL        = 5 * time.Minute
	workspaceFileMissOverhead    = 128 << 10

	// workspaceWindowChargeFactor multiplies a window's base64 length.
	// TestWorkspaceWindowDispatchAllocsWithinCharge (internal/mcpbroker)
	// measures every byte the out-of-process broker + stdio dispatch path
	// allocates for a window, growth intermediates included (an upper bound
	// on what is live at once), and fails if this charge is lower.
	workspaceWindowChargeFactor   = 12
	workspaceWindowChargeOverhead = 256 << 10
)

func workspaceFileMissCost(size int64) int64 {
	return 4*max(size, 1) + workspaceFileMissOverhead
}

// WorkspaceWindowCharge is what one leased window of n raw bytes is charged
// on top of the file it is cut from. Window-sized data on the dispatch path,
// with E = base64 length of n:
//
//	parent  base64 argument string (resolveFileReferences)          E
//	parent  broker frame encode (pooled json encodeState, growth)   up to 3E
//	child   broker frame decode buffer (growth; reset after use)    up to 3E
//	child   decoded argument string                                 E
//	child   JSON-RPC request line encode (EncodeJSONLine, growth)   up to 3E
//	child   the encoded line handed to the transport                E
//
// The in-process broker skips the frame encode/decode, so the charge covers
// both. The window itself is a sub-slice of the pinned file: no copy.
func WorkspaceWindowCharge(n int64) int64 {
	return workspaceWindowChargeFactor*int64(base64.StdEncoding.EncodedLen(int(max(n, 1)))) + workspaceWindowChargeOverhead
}

// workspaceFileStore is the verified-bytes LRU plus the admission control for
// everything that would hold workspace-file bytes. Cached entries are
// immutable. The key carries the reader's identity (one per run/turn binding
// to a sandbox), the workspace root, the path and the hash, so a run can
// never be served another run's file and a different version of the file is
// a different key.
type workspaceFileStore struct {
	mu         sync.Mutex
	budget     int64
	maxBytes   int64
	maxEntries int
	ttl        time.Duration
	now        func() time.Time

	order    *list.List // front = most recently used; values are *verifiedFile
	byKey    map[verifiedFileKey]*list.Element
	cached   int64 // bytes of all entries, pinned or not
	reserved int64 // in-flight reads plus window leases
	peak     int64 // highest cached+reserved seen; for tests
	changed  chan struct{}
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
	pins    int
}

// fileFlight is one in-progress miss. Identical concurrent misses wait on it
// instead of each reading (and reserving for) the same file.
type fileFlight struct {
	done chan struct{}
	err  error
}

func newWorkspaceFileStore(budget, maxBytes int64, maxEntries int, ttl time.Duration, now func() time.Time) *workspaceFileStore {
	return &workspaceFileStore{budget: budget, maxBytes: maxBytes, maxEntries: maxEntries, ttl: ttl, now: now,
		order: list.New(), byKey: map[verifiedFileKey]*list.Element{},
		changed: make(chan struct{}), flights: map[verifiedFileKey]*fileFlight{}}
}

// lease is one charged window. release is idempotent.
type lease struct {
	store  *workspaceFileStore
	entry  *verifiedFile
	charge int64
	once   sync.Once
}

func (l *lease) release() {
	l.once.Do(func() {
		s := l.store
		s.mu.Lock()
		defer s.mu.Unlock()
		l.entry.pins--
		s.reserved -= l.charge
		s.trimLocked()
		s.broadcastLocked()
	})
}

// acquire returns the verified file for key, pinned and with windowCharge
// reserved, reading it (once across concurrent callers) on a miss. probe and
// fill are the miss path's size probe and verified read.
func (s *workspaceFileStore) acquire(ctx context.Context, key verifiedFileKey, windowCharge int64,
	probe func(context.Context) (int64, error), fill func(context.Context, int64) ([]byte, error)) (*lease, error) {
	if windowCharge > s.budget {
		return nil, fmt.Errorf("%w: the window needs %d bytes, the process-wide budget is %d", ErrWorkspaceFileBudget, windowCharge, s.budget)
	}
	for {
		s.mu.Lock()
		if el, ok := s.byKey[key]; ok {
			entry := el.Value.(*verifiedFile)
			if entry.pins == 0 && !s.now().Before(entry.expires) {
				s.removeLocked(el)
				s.broadcastLocked()
			} else {
				s.evictForLocked(windowCharge)
				if s.cached+s.reserved+windowCharge <= s.budget {
					entry.pins++
					s.reserved += windowCharge
					s.peak = max(s.peak, s.cached+s.reserved)
					s.order.MoveToFront(el)
					s.mu.Unlock()
					return &lease{store: s, entry: entry, charge: windowCharge}, nil
				}
				if err := s.waitLocked(ctx); err != nil {
					return nil, err
				}
				continue
			}
		}
		if flight, ok := s.flights[key]; ok {
			s.mu.Unlock()
			select {
			case <-flight.done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			// The leader's own cancellation is not this caller's failure:
			// retry (as leader, or as a hit on what the leader cached).
			leaderCancelled := errors.Is(flight.err, context.Canceled) || errors.Is(flight.err, context.DeadlineExceeded)
			if flight.err != nil && (!leaderCancelled || ctx.Err() != nil) {
				return nil, flight.err
			}
			continue
		}
		flight := &fileFlight{done: make(chan struct{})}
		s.flights[key] = flight
		s.mu.Unlock()

		l, err := s.miss(ctx, key, windowCharge, probe, fill)
		s.mu.Lock()
		delete(s.flights, key)
		s.mu.Unlock()
		flight.err = err
		close(flight.done)
		return l, err
	}
}

func (s *workspaceFileStore) miss(ctx context.Context, key verifiedFileKey, windowCharge int64,
	probe func(context.Context) (int64, error), fill func(context.Context, int64) ([]byte, error)) (*lease, error) {
	size, err := probe(ctx)
	if err != nil {
		return nil, err
	}
	// The read's cap is the probed size, so the reservation bounds the read
	// however the (untrusted) probe answered.
	readCap := max(size, 1)
	cost := max(workspaceFileMissCost(readCap), readCap+windowCharge)
	if cost > s.budget {
		return nil, fmt.Errorf("%w: reading it needs %d bytes, the process-wide budget is %d", ErrWorkspaceFileBudget, cost, s.budget)
	}
	s.mu.Lock()
	for {
		s.evictForLocked(cost)
		if s.cached+s.reserved+cost <= s.budget {
			break
		}
		if err := s.waitLocked(ctx); err != nil {
			return nil, err
		}
		s.mu.Lock()
	}
	s.reserved += cost
	s.peak = max(s.peak, s.cached+s.reserved)
	s.mu.Unlock()

	data, err := fill(ctx, readCap)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reserved -= cost
	defer s.broadcastLocked()
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > readCap {
		return nil, fmt.Errorf("workspace file exceeds %d bytes", readCap)
	}
	// Convert the reservation: the file (pinned by this lease) plus the
	// lease's charge. cost >= readCap + windowCharge >= len(data) +
	// windowCharge, so this never exceeds what was just released.
	if el, ok := s.byKey[key]; ok {
		s.removeLocked(el) // an expired, unpinned twin
	}
	entry := &verifiedFile{key: key, data: data, expires: s.now().Add(s.ttl), pins: 1}
	s.byKey[key] = s.order.PushFront(entry)
	s.cached += int64(len(data))
	s.reserved += windowCharge
	s.peak = max(s.peak, s.cached+s.reserved)
	s.trimLocked()
	return &lease{store: s, entry: entry, charge: windowCharge}, nil
}

// waitLocked releases mu, waits for room to be freed (or ctx), and returns
// with mu released.
func (s *workspaceFileStore) waitLocked(ctx context.Context) error {
	wait := s.changed
	s.mu.Unlock()
	select {
	case <-wait:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// evictForLocked evicts unpinned files, least recently used first, until n
// more bytes fit or nothing is evictable.
func (s *workspaceFileStore) evictForLocked(n int64) {
	for el := s.order.Back(); el != nil && s.cached+s.reserved+n > s.budget; {
		prev := el.Prev()
		if el.Value.(*verifiedFile).pins == 0 {
			s.removeLocked(el)
		}
		el = prev
	}
}

// trimLocked applies the cache's own bounds (entries, bytes) to unpinned
// files; pinned files are bounded by the budget alone.
func (s *workspaceFileStore) trimLocked() {
	for el := s.order.Back(); el != nil && (s.order.Len() > s.maxEntries || s.cached > s.maxBytes); {
		prev := el.Prev()
		if el.Value.(*verifiedFile).pins == 0 {
			s.removeLocked(el)
		}
		el = prev
	}
}

func (s *workspaceFileStore) removeLocked(el *list.Element) {
	entry := s.order.Remove(el).(*verifiedFile)
	delete(s.byKey, entry.key)
	s.cached -= int64(len(entry.data))
}

// WorkspaceFileBudgetUsage reports the bytes currently charged to the
// process-wide workspace-file budget (cached and pinned files, reads in
// flight, window leases) and the budget itself.
func WorkspaceFileBudgetUsage() (charged, budget int64) {
	s := workspaceFiles
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cached + s.reserved, s.budget
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
	return func(ctx context.Context, path, sha256Hex string, offset, length, maxFileBytes int64) ([]byte, func(), error) {
		if maxFileBytes <= 0 {
			return nil, nil, fmt.Errorf("workspace file reader requires a positive byte limit")
		}
		if offset < 0 || length <= 0 || offset > maxFileBytes || length > maxFileBytes-offset {
			return nil, nil, fmt.Errorf("workspace file reader requires a byte range within %d bytes", maxFileBytes)
		}
		if src == nil {
			return nil, nil, fmt.Errorf("workspace file reference requires a sandbox")
		}
		if !filepath.IsLocal(path) || containsDotDotComponent(path) {
			return nil, nil, fmt.Errorf("workspace_file must be a relative path without '..'")
		}
		root := ForcedWorkingDirFromContext(ctx)
		if root == "" && ConversationIDFromContext(ctx) != "" {
			root = WorkspaceDirForConversation(ConversationIDFromContext(ctx))
		}
		if root == "" {
			return nil, nil, fmt.Errorf("workspace file reference requires an active workspace")
		}
		key := verifiedFileKey{reader: id, root: root, path: filepath.Clean(path), sha256: sha256Hex}
		l, err := store.acquire(ctx, key, WorkspaceWindowCharge(length),
			func(ctx context.Context) (int64, error) {
				size, err := src.size(ctx, root, path)
				if err == nil && size > maxFileBytes {
					err = fmt.Errorf("workspace file is %d bytes, exceeding the %d-byte cap", size, maxFileBytes)
				}
				return size, err
			},
			func(ctx context.Context, readCap int64) ([]byte, error) {
				read, err := src.read(ctx, root, path, readCap)
				if err != nil {
					return nil, err
				}
				if sum := sha256.Sum256(read); hex.EncodeToString(sum[:]) != sha256Hex {
					return nil, ErrWorkspaceFileHashMismatch
				}
				return read, nil
			})
		if err != nil {
			return nil, nil, err
		}
		data := l.entry.data
		size := int64(len(data))
		start, end := min(offset, size), min(offset+length, size)
		// Full slice expression: the caller must not be able to append into
		// the cached, shared backing array.
		return data[start:end:end], l.release, nil
	}
}
