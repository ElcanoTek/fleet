package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// tempUploadChecksumsDir is where POST /upload writes each file's checksum
// sidecar, <name>.sha256 (handlers.HandleUpload).
const tempUploadChecksumsDir = ".checksums"

// keptJobLineagesSQL selects the jobs (lineages) whose temp_uploads files are
// kept: a job with a task in one of liveTaskStatuses ($1) — pending,
// scheduled, leased, running, paused, or dead-lettered awaiting replay; the
// bound SweepTaskWorkspaces keeps a job's workspace by — or with a finished
// occurrence of a recurring job (status in $3,
// models.RecurrenceSpawnTaskStatuses) whose next occurrence has not been
// spawned yet — written as GetUnspawnedRecurringTasks writes it, so the
// partial index idx_tasks_recurrence_unspawned serves this arm; or a job one of whose tasks finished at or after $2, the start
// of the retention window, so a run that failed can be fixed and rerun. A job
// that finished longer ago than that keeps nothing: rerunning or cloning it
// then may find its input gone.
const keptJobLineagesSQL = `
	SELECT COALESCE(NULLIF(lineage_id::text, ''), id::text) AS lineage
	  FROM tasks
	 WHERE status = ANY($1)
	    OR (recurrence IS NOT NULL AND recurrence <> '' AND NOT recurrence_spawned AND status = ANY($3))
	    OR completed_at >= $2`

// recurrenceSpawnStatuses is models.RecurrenceSpawnTaskStatuses as query text.
func recurrenceSpawnStatuses() []string {
	out := make([]string, len(models.RecurrenceSpawnTaskStatuses))
	for i, st := range models.RecurrenceSpawnTaskStatuses {
		out[i] = string(st)
	}
	return out
}

// keptTaskFilesSQL is every temp_uploads name a task of a kept job names.
const keptTaskFilesSQL = `
WITH kept AS (` + keptJobLineagesSQL + `
)
SELECT DISTINCT f.name
  FROM tasks t
  JOIN (SELECT DISTINCT lineage FROM kept) k ON k.lineage = COALESCE(NULLIF(t.lineage_id::text, ''), t.id::text)
 CROSS JOIN LATERAL jsonb_array_elements_text(
       CASE WHEN jsonb_typeof(t.files) = 'array' THEN t.files ELSE '[]'::jsonb END) AS f(name)`

// TempUploadKeeper returns the keep rule for one sweep of the orchestrator's
// upload directory tempDir (temp_uploads) whose retention is window: a file
// stays while a task of a kept job names it (keptJobLineagesSQL: live, a
// recurring job's pending next occurrence, or finished within window), and a
// checksum sidecar under .checksums/ stays while the file it describes does.
// A task copies its files from temp_uploads at the start of every run, so
// removing one a kept job names makes its next run (or a rerun after a fix)
// fail to stage its inputs.
//
// The cost does not grow with the files it keeps: the first aged file loads
// every kept name in one query, and only a file missing from that set — one
// about to be removed — is asked about again on its own, which also covers a
// task created after the set was loaded. The returned function serves one
// walk; it is not safe for concurrent use. A store that cannot be asked keeps
// every file.
func (s *Storage) TempUploadKeeper(ctx context.Context, tempDir string, window time.Duration) func(path string) (bool, error) {
	if s == nil || s.db == nil || s.db.Conn() == nil {
		return func(string) (bool, error) { return true, nil }
	}
	since := time.Now().Add(-window)
	return newTempUploadKeeper(tempDir,
		func() (map[string]struct{}, error) { return s.keptTaskFiles(ctx, since) },
		func(name string) (bool, error) { return s.keptTaskFileNamed(ctx, since, name) })
}

// newTempUploadKeeper is TempUploadKeeper over its two lookups: snapshot (every
// live name, asked once) and recheck (one name, asked only for a file about to
// be removed).
func newTempUploadKeeper(tempDir string, snapshot func() (map[string]struct{}, error), recheck func(name string) (bool, error)) func(path string) (bool, error) {
	var (
		loaded  bool
		live    map[string]struct{}
		loadErr error
	)
	return func(path string) (bool, error) {
		name, ok := tempUploadName(tempDir, path)
		if !ok {
			return true, fmt.Errorf("temp upload %s is not under %s", path, tempDir)
		}
		if !loaded {
			loaded = true
			live, loadErr = snapshot()
		}
		if loadErr != nil {
			return true, loadErr
		}
		if _, ok := live[name]; ok {
			return true, nil
		}
		return recheck(name)
	}
}

// tempUploadName is the task file name a path under tempDir stands for: the
// file's own name, or for a checksum sidecar the file it describes.
func tempUploadName(tempDir, path string) (string, bool) {
	rel, err := filepath.Rel(tempDir, path)
	if err != nil || !filepath.IsLocal(rel) {
		return "", false
	}
	name := filepath.ToSlash(rel)
	if sidecar, ok := strings.CutPrefix(name, tempUploadChecksumsDir+"/"); ok {
		name = strings.TrimSuffix(sidecar, ".sha256")
	}
	return name, true
}

// keptTaskFiles is every temp_uploads name a task of a kept job names.
func (s *Storage) keptTaskFiles(ctx context.Context, since time.Time) (map[string]struct{}, error) {
	rows, err := s.db.Conn().QueryContext(ctx, keptTaskFilesSQL, liveTaskStatuses, since, recurrenceSpawnStatuses())
	if err != nil {
		return nil, fmt.Errorf("kept task files: %w", err)
	}
	defer rows.Close()
	out := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("kept task files: %w", err)
		}
		out[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("kept task files: %w", err)
	}
	return out, nil
}

// keptTaskFileNamed reports whether a task of a kept job names the file: the
// same rule as keptTaskFilesSQL for one name, found through the GIN index on
// tasks.files.
func (s *Storage) keptTaskFileNamed(ctx context.Context, since time.Time, name string) (bool, error) {
	var named bool
	if err := s.db.Conn().QueryRowContext(ctx, `
SELECT EXISTS (
	SELECT 1
	  FROM tasks t
	 WHERE t.files @> jsonb_build_array($4::text)
	   AND COALESCE(NULLIF(t.lineage_id::text, ''), t.id::text) IN (`+keptJobLineagesSQL+`))`,
		liveTaskStatuses, since, recurrenceSpawnStatuses(), name,
	).Scan(&named); err != nil {
		return true, fmt.Errorf("kept task file lookup: %w", err)
	}
	return named, nil
}
