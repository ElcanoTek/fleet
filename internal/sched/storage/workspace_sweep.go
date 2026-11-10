package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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
func (s *Storage) SweepTaskWorkspaces(ctx context.Context, workspaceRoot string, olderThan time.Duration) (int, error) {
	if strings.TrimSpace(workspaceRoot) == "" || s == nil || s.db == nil || s.db.Conn() == nil {
		return 0, nil
	}

	activeTasks, activeLineages, lineageLastDone, err := s.loadTaskWorkspaceState(ctx)
	if err != nil {
		return 0, fmt.Errorf("query task workspace state: %w", err)
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
			if err := removeTaskWorkspaceTree(mcpRunsDir, name); err != nil {
				errs = append(errs, fmt.Errorf("remove mcp-run workspace %s: %w", name, err))
				continue
			}
			removed++
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
			if olderThan > 0 {
				if lastDone, ok := lineageLastDone[name]; ok && lastDone.After(cutoff) {
					continue
				}
			}
			if err := removeTaskWorkspaceTree(tasksDir, name); err != nil {
				errs = append(errs, fmt.Errorf("remove task workspace %s: %w", name, err))
				continue
			}
			removed++
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
		switch status {
		case "pending", "scheduled", "leased", "running", "paused_awaiting_input", "paused_awaiting_wake", "dead_lettered":
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
