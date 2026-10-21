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

// WorkspaceFileReadFunc returns [offset, offset+length) of the workspace file
// at path, provided the WHOLE file hashes (in Fleet, not in the sandbox) to
// sha256Hex and is at most maxFileBytes. The window may be shorter than length
// when the range runs past the end of the file.
type WorkspaceFileReadFunc func(ctx context.Context, path, sha256Hex string, offset, length, maxFileBytes int64) ([]byte, error)

// rawWorkspaceRead reads one whole workspace file (beneath root) out of the
// sandbox, refusing a file over maxFileBytes. Anything it reports about the
// bytes — size, digest — comes from model-writable territory and is not
// trusted; only the bytes themselves are used, and Fleet hashes them.
type rawWorkspaceRead func(ctx context.Context, root, path string, maxFileBytes int64) ([]byte, error)

// Verified-bytes cache bounds (ADR-0085). A chunked upload names one file many
// times; the first reference reads and hashes it once, later windows are
// sliced from those verified bytes. Two whole 64 MiB files at most, process
// wide, and a short TTL: it is a de-duplication buffer for one upload loop,
// not a store.
const (
	workspaceFileCacheMaxBytes   = 128 << 20
	workspaceFileCacheMaxEntries = 2
	workspaceFileCacheTTL        = 5 * time.Minute
)

// verifiedFileCache is an LRU of whole files whose bytes Fleet hashed itself.
// Entries are immutable once inserted. The key carries the reader's identity
// (one per run/turn binding to a sandbox), the workspace root, the path and
// the hash, so a run can never be served another run's file and a different
// version of the file is a different key.
type verifiedFileCache struct {
	mu         sync.Mutex
	maxBytes   int64
	maxEntries int
	ttl        time.Duration
	now        func() time.Time
	order      *list.List // front = most recently used; values are *verifiedFile
	byKey      map[verifiedFileKey]*list.Element
	bytes      int64
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

func newVerifiedFileCache(maxBytes int64, maxEntries int, ttl time.Duration, now func() time.Time) *verifiedFileCache {
	return &verifiedFileCache{maxBytes: maxBytes, maxEntries: maxEntries, ttl: ttl, now: now,
		order: list.New(), byKey: map[verifiedFileKey]*list.Element{}}
}

func (c *verifiedFileCache) get(key verifiedFileKey) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.byKey[key]
	if !ok {
		return nil, false
	}
	entry := el.Value.(*verifiedFile)
	if !c.now().Before(entry.expires) {
		c.removeLocked(el)
		return nil, false
	}
	c.order.MoveToFront(el)
	return entry.data, true
}

func (c *verifiedFileCache) put(key verifiedFileKey, data []byte) {
	size := int64(len(data))
	if size > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.byKey[key]; ok {
		c.removeLocked(el)
	}
	for c.order.Len() > 0 && (c.order.Len() >= c.maxEntries || c.bytes+size > c.maxBytes) {
		c.removeLocked(c.order.Back())
	}
	c.byKey[key] = c.order.PushFront(&verifiedFile{key: key, data: data, expires: c.now().Add(c.ttl)})
	c.bytes += size
}

func (c *verifiedFileCache) removeLocked(el *list.Element) {
	entry := c.order.Remove(el).(*verifiedFile)
	delete(c.byKey, entry.key)
	c.bytes -= int64(len(entry.data))
}

var (
	workspaceFileCache = newVerifiedFileCache(workspaceFileCacheMaxBytes, workspaceFileCacheMaxEntries, workspaceFileCacheTTL, time.Now)
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
// instead of re-reading the file (ADR-0085).
func WorkspaceFileReader(sb *sandbox.Sandbox) WorkspaceFileReadFunc {
	var raw rawWorkspaceRead
	if sb != nil {
		raw = func(ctx context.Context, root, path string, maxFileBytes int64) ([]byte, error) {
			result, err := sb.RunFileOp(ctx, sandbox.FileOpRequest{
				Op: sandbox.FileOpRead, Root: root, Path: filepath.Join(root, path), MaxSize: maxFileBytes,
			})
			if err != nil {
				return nil, fmt.Errorf("cannot read workspace file: %w", err)
			}
			return result.Data, nil
		}
	}
	return newVerifiedWorkspaceReader(raw, workspaceFileCache)
}

func newVerifiedWorkspaceReader(raw rawWorkspaceRead, cache *verifiedFileCache) WorkspaceFileReadFunc {
	id := workspaceReaderIDs.Add(1)
	return func(ctx context.Context, path, sha256Hex string, offset, length, maxFileBytes int64) ([]byte, error) {
		if maxFileBytes <= 0 {
			return nil, fmt.Errorf("workspace file reader requires a positive byte limit")
		}
		if offset < 0 || length <= 0 || offset > maxFileBytes || length > maxFileBytes-offset {
			return nil, fmt.Errorf("workspace file reader requires a byte range within %d bytes", maxFileBytes)
		}
		if raw == nil {
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
		data, ok := cache.get(key)
		if !ok {
			read, err := raw(ctx, root, path, maxFileBytes)
			if err != nil {
				return nil, err
			}
			if int64(len(read)) > maxFileBytes {
				return nil, fmt.Errorf("workspace file exceeds %d bytes", maxFileBytes)
			}
			sum := sha256.Sum256(read)
			if hex.EncodeToString(sum[:]) != sha256Hex {
				return nil, ErrWorkspaceFileHashMismatch
			}
			data = read
			cache.put(key, data)
		}
		size := int64(len(data))
		start, end := min(offset, size), min(offset+length, size)
		// Full slice expression: the caller must not be able to append into
		// the cached, shared backing array.
		return data[start:end:end], nil
	}
}
