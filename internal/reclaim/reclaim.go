// Package reclaim is what the sweeps that remove directories other parts of
// fleet make have in common: how long a new directory is left alone, and the
// locks that keep a directory from being removed while it is being made.
//
// A sweep finds an orphan by reading the database once and listing the
// directories after: a directory no row owns is removed. Both are snapshots,
// and making a directory and writing the row that owns it are two steps too,
// so a directory can look orphaned when its owner is moments old. A sweep
// built on this package therefore removes an entry only when
//
//   - the entry was last modified more than Grace ago (Settled). A directory
//     can exist before its row is visible, or be made by another process
//     whose rows this database never holds and which takes none of this
//     process's locks — `fleet task run` started with the server's
//     FLEET_WORKSPACE_ROOT makes its runs' MCP directories in the server's
//     mcp-runs/ — and for such a directory the grace is the only
//     protection; and
//   - where there is an owner to ask, asking the database again about that
//     one entry, just before removing it, still finds none. Where later rows
//     reuse a directory name (one job directory serves every run of the
//     job), that question and the removal hold the lock this process's
//     creators hold while they make it (TaskDirs). A creator writes its row
//     before it takes the lock, so the sweep either sees the row and keeps
//     the directory, or removes it before the creator makes a fresh one.
//
// storage.SweepTaskWorkspaces applies both. The trees an account deletion
// moves aside (accountdelete.SetAside) have no row to ask about: the
// deletion that moved one may still move it back, so the grace alone keeps
// them (accountdelete.RemoveAside).
//
// Removal itself goes through the callers' no-follow openers; nothing here
// touches the filesystem.
package reclaim

import (
	"io/fs"
	"sync"
	"time"
)

// Grace is how long after its last modification a directory is left alone by
// a sweep built on this package, whatever the database says about its owner.
// It is far longer than the gap between making a directory and its owner's
// row becoming visible, and short against what the sweep keeps anyway (days),
// so an orphan only waits for a later pass.
const Grace = time.Hour

// Settled reports whether info, from an Lstat of a sweep candidate, was last
// modified at least Grace before now — or more than Grace after now. A time
// that far ahead was not written by a creator a moment ago: it is a clock
// far ahead (a network filesystem's) or a time set by hand, which a sandbox
// can do to the directory it is given (`touch -d 2099`). Counting it as new
// would keep that directory forever; the sweep's owner lookup and lock still
// protect a directory something live owns.
func Settled(info fs.FileInfo, now time.Time) bool {
	mtime := info.ModTime()
	return !mtime.After(now.Add(-Grace)) || mtime.After(now.Add(Grace))
}

// Locks is a set of mutexes by key, each dropped once nobody holds or waits on
// it. The zero value is ready to use.
type Locks struct {
	mu sync.Mutex
	m  map[string]*keyLock
}

type keyLock struct {
	mu   sync.Mutex
	refs int
}

// Lock blocks until key is free, then holds it; the returned func releases it.
func (l *Locks) Lock(key string) (unlock func()) {
	k := l.ref(key)
	k.mu.Lock()
	return func() { l.release(key, k) }
}

// TryLock holds key if it is free and reports whether it did; it never waits.
func (l *Locks) TryLock(key string) (unlock func(), ok bool) {
	k := l.ref(key)
	if !k.mu.TryLock() {
		l.unref(key, k)
		return nil, false
	}
	return func() { l.release(key, k) }, true
}

func (l *Locks) ref(key string) *keyLock {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = make(map[string]*keyLock)
	}
	k := l.m[key]
	if k == nil {
		k = &keyLock{}
		l.m[key] = k
	}
	k.refs++
	return k
}

func (l *Locks) release(key string, k *keyLock) {
	k.mu.Unlock()
	l.unref(key, k)
}

func (l *Locks) unref(key string, k *keyLock) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if k.refs--; k.refs == 0 {
		delete(l.m, key)
	}
}

// TaskDirs serializes a scheduled run making its directories under the
// workspace root with the task-workspace sweep removing them
// (storage.SweepTaskWorkspaces). Keys are TaskWorkspaceKey and MCPRunKey.
// One fleet process runs both the scheduler and the sweep (the control plane
// is a single replica), so the lock covers the server's own runs. A
// directory another process makes under the same root (`fleet task run`'s
// mcp-runs/task-<id>, when it runs with the server's FLEET_WORKSPACE_ROOT)
// takes no part in it and is protected only by Grace.
var TaskDirs Locks

// TaskWorkspaceKey is the TaskDirs key of a job directory,
// <workspace root>/tasks/<lineage> (tools.TaskWorkspaceDir).
func TaskWorkspaceKey(lineage string) string { return "tasks/" + lineage }

// MCPRunKey is the TaskDirs key of a run's MCP directory,
// <workspace root>/mcp-runs/<name> (agentcore.OpenStableMCPWorkspace).
func MCPRunKey(name string) string { return "mcp-runs/" + name }
