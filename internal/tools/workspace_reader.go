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

// WorkspaceFileRef names one window of a workspace file: the whole file must
// hash to SHA256 (lowercase hex), and [Offset, Offset+Length) is returned.
type WorkspaceFileRef struct {
	Path   string
	SHA256 string
	Offset int64
	Length int64
}

// WorkspaceFileReadFunc returns the windows for every ref of ONE call, each
// cut from a whole file that Fleet (not the sandbox) hashed to its SHA256 and
// that is at most maxFileBytes. A window may be shorter than its Length when
// the range runs past the end of the file.
//
// All refs are admitted against the process-wide budget together, in one
// step, so a call never holds some of its windows while it waits for the
// rest (ADR-0088). On success the caller MUST call release (idempotent) once
// nothing derived from the windows is live any more: for an MCP call, after
// the outbound dispatch has let go of its arguments. Until then the files the
// windows are cut from stay pinned and WorkspaceWindowCharge of each window
// stays charged.
type WorkspaceFileReadFunc func(ctx context.Context, refs []WorkspaceFileRef, maxFileBytes int64) (windows [][]byte, release func(), err error)

// workspaceFileSource is the sandbox side of the reader. size is a cheap probe
// (no hashing) and its answer is untrusted: it only sizes the reservation and
// the cap of the read that follows, and read enforces that cap twice — the
// helper refuses a file over it, and the FileOp output sink refuses helper
// output longer than it can produce. read returns the whole file.
type workspaceFileSource struct {
	size func(ctx context.Context, root, path string) (int64, error)
	read func(ctx context.Context, root, path string, maxBytes int64) ([]byte, error)
}

// Budget (ADR-0088). Every byte Fleet holds on behalf of workspace-file
// references is charged to one process-wide budget from before it is
// allocated until it is released:
//
//   - a file being read: workspaceFileMissCost(S) = 2.5·S + 192 KiB. The
//     FileOp output buffer is allocated once at its exact bound, base64(S) +
//     64 KiB, and the payload is decoded straight out of it into S bytes; the
//     rest of the response is a few hundred bytes.
//   - a verified file, cached or pinned by a lease: S.
//   - a leased window: WorkspaceWindowCharge(W), from before the window is
//     handed out until the caller releases it.
//
// A call's refs are admitted together: hits reserve their window charges and
// pin their files; misses reserve their final cost (S + window charges) plus,
// once for the whole call, the largest transient excess of a read over its
// final cost, because the call reads its misses one at a time. Only unpinned
// files are evicted. A call that could never fit fails at once.
const (
	workspaceFileBudgetBytes     = 384 << 20
	workspaceFileCacheMaxBytes   = 128 << 20
	workspaceFileCacheMaxEntries = 2
	workspaceFileCacheTTL        = 5 * time.Minute
	workspaceFileMissOverhead    = 192 << 10

	// workspaceWindowChargeFactor multiplies a window's base64 length.
	// TestWorkspaceWindowDispatchAllocsWithinCharge (internal/mcpbroker)
	// measures every byte the out-of-process broker + stdio dispatch path
	// allocates for a window, growth intermediates included (an upper bound
	// on what is live at once: about 9×), and fails if this charge is lower.
	workspaceWindowChargeFactor   = 10
	workspaceWindowChargeOverhead = 256 << 10
)

func workspaceFileMissCost(size int64) int64 {
	size = max(size, 1)
	return size*5/2 + 1 + workspaceFileMissOverhead
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
// Not every copy is live at once (measured: about 9E allocated in total). The
// in-process broker skips the frame encode/decode. The window itself is a
// sub-slice of the pinned file: no copy. n is clamped to [1, 1 GiB] so the
// arithmetic cannot overflow; callers cap windows far below that.
func WorkspaceWindowCharge(n int64) int64 {
	n = min(max(n, 1), 1<<30)
	return workspaceWindowChargeFactor*((n+2)/3*4) + workspaceWindowChargeOverhead
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

// fileFlight is a key some call is reading. Others wanting the same key wait
// for it (holding nothing) instead of reading it again.
type fileFlight struct {
	done chan struct{}
	err  error
}

func newWorkspaceFileStore(budget, maxBytes int64, maxEntries int, ttl time.Duration, now func() time.Time) *workspaceFileStore {
	return &workspaceFileStore{budget: budget, maxBytes: maxBytes, maxEntries: maxEntries, ttl: ttl, now: now,
		order: list.New(), byKey: map[verifiedFileKey]*list.Element{},
		changed: make(chan struct{}), flights: map[verifiedFileKey]*fileFlight{}}
}

// fileRequest is one key a call needs, with every window it wants from it.
type fileRequest struct {
	key          verifiedFileKey
	windowCharge int64 // all of this call's windows of this key
	windows      int
	probe        func(context.Context) (int64, error)
	fill         func(context.Context, int64) ([]byte, error)

	// planning state
	size  int64 // probed size; -1 until probed
	entry *verifiedFile
}

// lease is one call's admitted windows. release is idempotent.
type lease struct {
	store  *workspaceFileStore
	pinned []*verifiedFile // with multiplicity
	charge int64
	once   sync.Once
}

func (l *lease) release() {
	l.once.Do(func() {
		s := l.store
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, entry := range l.pinned {
			entry.pins--
		}
		s.reserved -= l.charge
		s.trimLocked()
		s.broadcastLocked()
	})
}

// acquire admits every request of one call in a single step and returns its
// lease, with each request's entry set. It never holds a charge or a pin
// while it waits: on any wait it holds nothing, then plans again.
func (s *workspaceFileStore) acquire(ctx context.Context, reqs []*fileRequest) (*lease, error) {
	for _, r := range reqs {
		r.size = -1
	}
	for {
		s.mu.Lock()
		var hits, misses []*fileRequest
		var wait *fileFlight
		var probe []*fileRequest
		for _, r := range reqs {
			r.entry = nil
			if el, ok := s.byKey[r.key]; ok {
				entry := el.Value.(*verifiedFile)
				if entry.pins > 0 || s.now().Before(entry.expires) {
					r.entry = entry
					hits = append(hits, r)
					continue
				}
				s.removeLocked(el)
				s.broadcastLocked()
			}
			if flight, ok := s.flights[r.key]; ok {
				wait = flight
				break
			}
			if r.size < 0 {
				probe = append(probe, r)
			}
			misses = append(misses, r)
		}
		if wait != nil {
			s.mu.Unlock()
			select {
			case <-wait.done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			continue // the key is cached now, or failed: plan again
		}
		if len(probe) > 0 {
			s.mu.Unlock()
			for _, r := range probe {
				size, err := r.probe(ctx)
				if err != nil {
					return nil, err
				}
				r.size = max(size, 1)
			}
			continue
		}
		var need, extra int64
		for _, r := range hits {
			need += r.windowCharge
		}
		for _, r := range misses {
			final := r.size + r.windowCharge
			need += final
			extra = max(extra, workspaceFileMissCost(r.size)-final)
		}
		need += extra
		if need > s.budget {
			s.mu.Unlock()
			return nil, fmt.Errorf("%w: this call's files and windows need %d bytes, the process-wide budget is %d", ErrWorkspaceFileBudget, need, s.budget)
		}
		// Pin the hits before making room, so eviction cannot take them.
		for _, r := range hits {
			r.entry.pins++
		}
		s.evictForLocked(need)
		if s.cached+s.reserved+need > s.budget {
			for _, r := range hits {
				r.entry.pins--
			}
			if err := s.waitLocked(ctx); err != nil {
				return nil, err
			}
			continue
		}
		s.reserved += need
		s.peak = max(s.peak, s.cached+s.reserved)
		flights := make([]*fileFlight, len(misses))
		for i, r := range misses {
			flights[i] = &fileFlight{done: make(chan struct{})}
			s.flights[r.key] = flights[i]
		}
		l := &lease{store: s}
		for _, r := range hits {
			l.pinned = append(l.pinned, r.entry)
			for range r.windows - 1 {
				r.entry.pins++
				l.pinned = append(l.pinned, r.entry)
			}
			l.charge += r.windowCharge
		}
		s.mu.Unlock()
		if err := s.readMisses(ctx, misses, flights, l, need); err != nil {
			return nil, err
		}
		return l, nil
	}
}

// readMisses reads the call's misses one at a time inside the reservation
// acquire made (need), converting it as each settles: the file's bytes move
// from reserved to cached, pinned by l. On success what stays reserved is
// exactly l.charge. On any failure — or a panic — everything is undone.
func (s *workspaceFileStore) readMisses(ctx context.Context, misses []*fileRequest, flights []*fileFlight, l *lease, need int64) (err error) {
	held := need // what this call has reserved and not yet converted
	settled := 0
	defer func() {
		failed := err != nil
		if r := recover(); r != nil {
			failed = true
			defer panic(r)
		}
		s.mu.Lock()
		for i := settled; i < len(misses); i++ {
			delete(s.flights, misses[i].key)
			if failed && flights[i].err == nil {
				flights[i].err = errors.Join(errors.New("workspace file read failed"), err)
			}
			close(flights[i].done)
		}
		if failed {
			for _, entry := range l.pinned {
				entry.pins--
			}
			s.reserved -= held
			s.trimLocked()
		} else {
			// Release the transient excess; keep exactly the windows.
			s.reserved -= held - l.charge
		}
		s.broadcastLocked()
		s.mu.Unlock()
	}()
	for i, r := range misses {
		data, ferr := r.fill(ctx, r.size)
		if ferr == nil && int64(len(data)) > r.size {
			ferr = fmt.Errorf("workspace file exceeds %d bytes", r.size)
		}
		if ferr != nil {
			flights[i].err = ferr
			return ferr
		}
		s.mu.Lock()
		if el, ok := s.byKey[r.key]; ok {
			s.removeLocked(el) // an expired, unpinned twin
		}
		entry := &verifiedFile{key: r.key, data: data, expires: s.now().Add(s.ttl), pins: r.windows}
		r.entry = entry
		s.byKey[r.key] = s.order.PushFront(entry)
		size := int64(len(data))
		s.cached += size
		s.reserved -= size
		held -= size
		for range r.windows {
			l.pinned = append(l.pinned, entry)
		}
		l.charge += r.windowCharge
		s.peak = max(s.peak, s.cached+s.reserved)
		delete(s.flights, r.key)
		close(flights[i].done)
		settled = i + 1
		s.broadcastLocked()
		s.mu.Unlock()
	}
	return nil
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
// process-wide memory budget before it starts (ADR-0088).
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
	return func(ctx context.Context, refs []WorkspaceFileRef, maxFileBytes int64) ([][]byte, func(), error) {
		if maxFileBytes <= 0 {
			return nil, nil, fmt.Errorf("workspace file reader requires a positive byte limit")
		}
		if src == nil {
			return nil, nil, fmt.Errorf("workspace file reference requires a sandbox")
		}
		root := ForcedWorkingDirFromContext(ctx)
		if root == "" && ConversationIDFromContext(ctx) != "" {
			root = WorkspaceDirForConversation(ConversationIDFromContext(ctx))
		}
		if root == "" {
			return nil, nil, fmt.Errorf("workspace file reference requires an active workspace")
		}
		byKey := map[verifiedFileKey]*fileRequest{}
		var reqs []*fileRequest
		forRef := make([]*fileRequest, len(refs))
		for i, ref := range refs {
			if ref.Offset < 0 || ref.Length <= 0 || ref.Offset > maxFileBytes || ref.Length > maxFileBytes-ref.Offset {
				return nil, nil, fmt.Errorf("workspace file reader requires a byte range within %d bytes", maxFileBytes)
			}
			if !filepath.IsLocal(ref.Path) || containsDotDotComponent(ref.Path) {
				return nil, nil, fmt.Errorf("workspace_file must be a relative path without '..'")
			}
			key := verifiedFileKey{reader: id, root: root, path: filepath.Clean(ref.Path), sha256: ref.SHA256}
			r, ok := byKey[key]
			if !ok {
				path, sum := ref.Path, ref.SHA256
				r = &fileRequest{key: key,
					probe: func(ctx context.Context) (int64, error) {
						size, err := src.size(ctx, root, path)
						switch {
						case err != nil:
						case size < 0:
							err = fmt.Errorf("workspace file reported a negative size (%d)", size)
						case size > maxFileBytes:
							err = fmt.Errorf("workspace file is %d bytes, exceeding the %d-byte cap", size, maxFileBytes)
						}
						return size, err
					},
					fill: func(ctx context.Context, readCap int64) ([]byte, error) {
						read, err := src.read(ctx, root, path, readCap)
						if err != nil {
							return nil, err
						}
						if digest := sha256.Sum256(read); hex.EncodeToString(digest[:]) != sum {
							return nil, ErrWorkspaceFileHashMismatch
						}
						return read, nil
					}}
				byKey[key] = r
				reqs = append(reqs, r)
			}
			r.windows++
			r.windowCharge += WorkspaceWindowCharge(ref.Length)
			forRef[i] = r
		}
		l, err := store.acquire(ctx, reqs)
		if err != nil {
			return nil, nil, err
		}
		windows := make([][]byte, len(refs))
		for i, ref := range refs {
			data := forRef[i].entry.data
			size := int64(len(data))
			start, end := min(ref.Offset, size), min(ref.Offset+ref.Length, size)
			// Full slice expression: the caller must not be able to append
			// into the cached, shared backing array.
			windows[i] = data[start:end:end]
		}
		return windows, l.release, nil
	}
}
