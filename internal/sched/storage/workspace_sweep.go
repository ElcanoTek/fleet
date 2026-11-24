package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/reclaim"
)

// liveTaskStatuses are the statuses that keep a task's job live: it may still
// run (or be replayed, for dead_lettered), so the job's workspace
// (SweepTaskWorkspaces) and the files it names in temp_uploads
// (TempUploadKeeper) are kept.
var liveTaskStatuses = []string{"pending", "scheduled", "leased", "running", "paused_awaiting_input", "paused_awaiting_wake", "dead_lettered"}

const (
	// taskWorkspaceDefaultRetention caps how long finished task lineage
	// workspaces are retained when an admin or maintenance pass sweeps with a
	// longer chat TTL (such as 30 days).
	taskWorkspaceDefaultRetention = 7 * 24 * time.Hour
)

// SweepTaskWorkspaces reclaims disk space under <workspaceRoot>/mcp-runs and
// <workspaceRoot>/tasks:
//
//  1. <workspaceRoot>/mcp-runs/task-<id>* is per-occurrence MCP tool scratch
//     and ledger storage. Any directory whose task <id> is no longer active or
//     replayable (i.e. terminal in sched.tasks and not dead_lettered, or
//     deleted by retention) is removed.
//  2. <workspaceRoot>/tasks/<lineage> is the per-lineage task workspace. Any
//     directory whose lineage has NO active or dead_lettered task in
//     sched.tasks and whose last task completed before the retention cutoff
//     (or is not in sched.tasks) is removed. Active lineages keep their entire
//     workspace directory intact (ADR-0069).
//
// The tasks are read once, before the directories are listed, so a task
// created in between — a new job, or a rerun of a job whose directory is
// days old — is missing from that read while its run is already working in
// its directory. A directory is therefore removed only once it is settled
// and its tasks, read again just for it, still do not keep it
// (reclaimTaskDir).
func (s *Storage) SweepTaskWorkspaces(ctx context.Context, workspaceRoot string, olderThan time.Duration) (int, error) {
	if strings.TrimSpace(workspaceRoot) == "" || s == nil || s.db == nil || s.db.Conn() == nil {
		return 0, nil
	}

	activeTasks, activeLineages, lineageLastDone, err := s.loadTaskWorkspaceState(ctx)
	if err != nil {
		return 0, fmt.Errorf("query task workspace state: %w", err)
	}
	if afterTaskWorkspaceSnapshot != nil {
		afterTaskWorkspaceSnapshot()
	}

	retention := olderThan
	if retention > taskWorkspaceDefaultRetention {
		retention = taskWorkspaceDefaultRetention
	}
	now := time.Now()
	cutoff := now.Add(-retention)

	removed := 0
	var errs []error

	// 1. Sweep <workspaceRoot>/mcp-runs/task-<uuid>*
	mcpRunsDir := filepath.Join(workspaceRoot, "mcp-runs")
	if entries, rerr := os.ReadDir(mcpRunsDir); rerr == nil {
		for _, e := range entries {
			if ctx.Err() != nil {
				break
			}
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			taskID, ok := parseMCPRunTaskID(name)
			if !ok {
				continue
			}
			if activeTasks[taskID] {
				continue
			}
			gone, err := reclaimTaskDir(mcpRunsDir, name, reclaim.MCPRunKey(name), func() (bool, error) {
				return s.taskLive(ctx, taskID)
			})
			if err != nil {
				errs = append(errs, fmt.Errorf("mcp-run workspace %s: %w", name, err))
				continue
			}
			if gone {
				removed++
			}
		}
	} else if !os.IsNotExist(rerr) {
		errs = append(errs, fmt.Errorf("readdir %s: %w", mcpRunsDir, rerr))
	}

	// 2. Sweep <workspaceRoot>/tasks/<uuid>
	tasksDir := filepath.Join(workspaceRoot, "tasks")
	if entries, rerr := os.ReadDir(tasksDir); rerr == nil {
		for _, e := range entries {
			if ctx.Err() != nil {
				break
			}
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if !looksLikeTaskUUID(name) {
				continue
			}
			if activeLineages[name] {
				continue
			}
			if lastDone, ok := lineageLastDone[name]; keepsFinishedLineage(ok, lastDone, olderThan, cutoff) {
				continue
			}
			gone, err := reclaimTaskDir(tasksDir, name, reclaim.TaskWorkspaceKey(name), func() (bool, error) {
				return s.lineageKept(ctx, name, olderThan, cutoff)
			})
			if err != nil {
				errs = append(errs, fmt.Errorf("task workspace %s: %w", name, err))
				continue
			}
			if gone {
				removed++
			}
		}
	} else if !os.IsNotExist(rerr) {
		errs = append(errs, fmt.Errorf("readdir %s: %w", tasksDir, rerr))
	}

	return removed, errors.Join(errs...)
}

func (s *Storage) loadTaskWorkspaceState(ctx context.Context) (
	activeTasks map[string]bool,
	activeLineages map[string]bool,
	lineageLastDone map[string]time.Time,
	err error,
) {
	activeTasks = make(map[string]bool)
	activeLineages = make(map[string]bool)
	lineageLastDone = make(map[string]time.Time)

	rows, err := s.db.Conn().QueryContext(ctx, `
		SELECT id::text,
		       COALESCE(NULLIF(lineage_id::text, ''), id::text) AS lineage,
		       status,
		       COALESCE(completed_at, dead_lettered_at, created_at) AS ts
		FROM tasks
	`)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var id, lineage, status string
		var ts sql.NullTime
		if err := rows.Scan(&id, &lineage, &status, &ts); err != nil {
			return nil, nil, nil, err
		}
		id = strings.ToLower(strings.TrimSpace(id))
		lineage = strings.ToLower(strings.TrimSpace(lineage))
		switch {
		case slices.Contains(liveTaskStatuses, status):
			activeTasks[id] = true
			activeLineages[lineage] = true
			activeLineages[id] = true
		default:
			if ts.Valid {
				if prev, ok := lineageLastDone[lineage]; !ok || ts.Time.After(prev) {
					lineageLastDone[lineage] = ts.Time
				}
				if prev, ok := lineageLastDone[id]; !ok || ts.Time.After(prev) {
					lineageLastDone[id] = ts.Time
				}
			}
		}
	}
	return activeTasks, activeLineages, lineageLastDone, rows.Err()
}

// keepsFinishedLineage reports whether a job directory with no live task is
// kept for the retention window: its last task (seen) finished after cutoff.
// A non-positive olderThan keeps none.
func keepsFinishedLineage(seen bool, lastDone time.Time, olderThan time.Duration, cutoff time.Time) bool {
	return olderThan > 0 && seen && lastDone.After(cutoff)
}

// reclaimTaskDir removes parent/name, a directory the snapshot found no task
// keeping, if it still is an orphan, and reports whether it did. It holds the
// directory's reclaim.TaskDirs lock, the one a run holds while it makes the
// directory (scheduledrun), and under it requires a real directory (Lstat
// through parent, never following a link) that is settled (reclaim.Settled:
// not modified within reclaim.Grace), then asks kept — the owning tasks, read again — once
// more. A run's task row is live before the run takes that lock, so the
// directory is either removed before the run makes it (the run then makes a
// fresh one) or seen as kept here. A failed lookup keeps the directory and is
// returned.
func reclaimTaskDir(parent, name, key string, kept func() (bool, error)) (bool, error) {
	defer reclaim.TaskDirs.Lock(key)()
	r, err := os.OpenRoot(parent)
	if err != nil {
		return false, err
	}
	info, err := r.Lstat(name)
	_ = r.Close()
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() || !reclaim.Settled(info, time.Now()) {
		return false, nil
	}
	if beforeTaskWorkspaceRecheck != nil {
		beforeTaskWorkspaceRecheck(key)
	}
	keep, err := kept()
	if err != nil {
		return false, fmt.Errorf("recheck: %w", err)
	}
	if keep {
		return false, nil
	}
	if err := removeTaskWorkspaceTree(parent, name); err != nil {
		return false, fmt.Errorf("remove: %w", err)
	}
	return true, nil
}

// taskLive reports whether task id is live or replayable now
// (liveTaskStatuses): the snapshot's rule for an mcp-runs directory, asked
// again for one directory about to be removed.
func (s *Storage) taskLive(ctx context.Context, id string) (bool, error) {
	var live bool
	err := s.db.Conn().QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM tasks WHERE id = $1::uuid AND status = ANY($2))`,
		id, liveTaskStatuses).Scan(&live)
	return live, err
}

// lineageKept reports whether the tasks of one job keep its directory now:
// the snapshot's rule for a tasks/<lineage> directory (a live task, or a
// last task that finished within the retention window), asked again for one
// directory about to be removed. Like the snapshot it counts a task whose
// lineage, or whose own id, is the directory's name. The rows are folded in
// the query, so a recurring job's long history is not read back.
func (s *Storage) lineageKept(ctx context.Context, lineage string, olderThan time.Duration, cutoff time.Time) (bool, error) {
	var live bool
	var lastDone sql.NullTime
	err := s.db.Conn().QueryRowContext(ctx, `
		SELECT COALESCE(bool_or(status = ANY($3)), false),
		       max(COALESCE(completed_at, dead_lettered_at, created_at))
		  FROM tasks
		 WHERE id = $1::uuid OR lineage_id = $2`,
		lineage, strings.ToLower(lineage), liveTaskStatuses).Scan(&live, &lastDone)
	if err != nil {
		return false, err
	}
	// With no live task every row is finished, so the latest time is the
	// last finish, as the snapshot computes it.
	return live || keepsFinishedLineage(lastDone.Valid, lastDone.Time, olderThan, cutoff), nil
}

// afterTaskWorkspaceSnapshot is a test seam run in SweepTaskWorkspaces right
// after the task snapshot is read, before the directories are listed; nil in
// production.
var afterTaskWorkspaceSnapshot func()

// beforeTaskWorkspaceRecheck is a test seam run in reclaimTaskDir with the
// directory's lock held, just before its tasks are read again; nil in
// production.
var beforeTaskWorkspaceRecheck func(key string)

// parseMCPRunTaskID extracts the lowercased 36-char task UUID from an mcp-runs
// subdirectory name ("task-<uuid>" or "task-<uuid>-<suffix>").
func parseMCPRunTaskID(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, "task-")
	if !ok || len(rest) < 36 {
		return "", false
	}
	cand := strings.ToLower(rest[:36])
	if !looksLikeTaskUUID(cand) {
		return "", false
	}
	if len(rest) > 36 && rest[36] != '-' && rest[36] != '_' {
		return "", false
	}
	return cand, true
}

func isHexRune(c rune) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func looksLikeTaskUUID(name string) bool {
	if len(name) != 36 {
		return false
	}
	for i, c := range name {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !isHexRune(c) {
				return false
			}
		}
	}
	return true
}

func removeTaskWorkspaceTree(root, name string) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	if err := r.RemoveAll(name); err == nil {
		return nil
	}
	if info, lerr := r.Lstat(name); lerr == nil && info.IsDir() {
		_ = r.Chmod(name, info.Mode().Perm()|0o700)
	}
	if sub, oerr := r.OpenRoot(name); oerr == nil {
		_ = fs.WalkDir(sub.FS(), ".", func(path string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return nil //nolint:nilerr // best-effort chmod walk continues past unreadable entries
			}
			if info, ierr := d.Info(); ierr == nil && info.Mode().Perm()&0o700 != 0o700 {
				_ = sub.Chmod(path, info.Mode().Perm()|0o700)
			}
			return nil
		})
		_ = sub.Close()
	}
	return r.RemoveAll(name)
}
