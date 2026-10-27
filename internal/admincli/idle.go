package admincli

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/ElcanoTek/fleet/internal/httpapi"
)

const (
	defaultIdleApprovalWindow = 2 * time.Hour
)

// ActiveWorkError is returned by WaitIdle when scheduled tasks, chat turns, or
// detached approval executions remain in flight after the wait window expires.
type ActiveWorkError struct {
	Items []string
}

func (e *ActiveWorkError) Error() string {
	return strings.Join(e.Items, "; ")
}

// SetUpdateDrain writes the update drain sentinel file so the running Fleet
// server refuses new chat turns/approvals and pauses scheduler task claims.
func SetUpdateDrain() {
	p := httpapi.UpdateDrainFile()
	if p == "" {
		return
	}
	//nolint:gosec // G301,G306: non-secret drain sentinel inspected by the fleet service user.
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	//nolint:gosec // G306: non-secret drain sentinel inspected by the fleet service user.
	_ = os.WriteFile(p, []byte("draining\n"), 0o644)
}

// ClearUpdateDrain removes the update drain sentinel file.
func ClearUpdateDrain() {
	p := httpapi.UpdateDrainFile()
	if p == "" {
		return
	}
	_ = os.Remove(p)
}

// serviceRuntimeState inspects systemd for the unit's current ActiveState and
// ExecMainStartTimestamp. It returns ("active", startUnix), ("inactive", 0) when
// the unit is installed and not active, or ("unknown", 0) when systemd or the
// unit is absent.
func serviceRuntimeState(unit string) (string, int64) {
	if truthy(os.Getenv("FLEET_ASSUME_SERVICE_ACTIVE")) {
		return "active", 0
	}
	if !strings.HasSuffix(unit, ".service") {
		unit += ".service"
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return "unknown", 0
	}
	if !systemctlUnitInstalled(unit) {
		return "unknown", 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	//nolint:gosec // G204: fixed "systemctl" binary; unit is resolved via serviceName.
	out, err := exec.CommandContext(ctx, "systemctl", "show", "-p", "ActiveState", "-p", "ExecMainStartTimestamp", unit).Output()
	if err != nil {
		return "unknown", 0
	}
	var activeState, startTS string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "ActiveState="); ok {
			activeState = strings.TrimSpace(v)
		} else if v, ok := strings.CutPrefix(line, "ExecMainStartTimestamp="); ok {
			startTS = strings.TrimSpace(v)
		}
	}
	switch activeState {
	case "active":
		var startUnix int64
		if startTS != "" && startTS != "n/a" {
			if t, err := time.Parse("Mon 2006-01-02 15:04:05 MST", startTS); err == nil {
				startUnix = t.Unix()
			}
		}
		return "active", startUnix
	case "":
		return "unknown", 0
	default:
		return "inactive", 0
	}
}

func sanitizeSummaryText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// ActiveWork inspects the sched and chat databases for in-flight work that
// would be interrupted by stopping or restarting fleet.service:
//   - sched.tasks with status IN ('running', 'leased', 'assigned') and an unexpired lease
//   - chat.turns with status = 'running' AND finished_at IS NULL in the current service instance
//   - chat.approvals with status = 'approved' AND is_err IS NULL AND
//     result_text LIKE 'Approved%executing%' within 2h in the current service instance
func ActiveWork(ctx context.Context, schedDSNVal, chatDSNVal string) ([]string, error) {
	svcState, svcStartUnix := serviceRuntimeState(serviceName(""))
	if svcState == "inactive" {
		// When the service unit is installed and not active, any running/leased rows
		// in Postgres are crash-stranded and will be recovered on boot.
		return nil, nil
	}

	var items []string
	var firstErr error

	if strings.TrimSpace(schedDSNVal) != "" {
		found, err := querySchedActiveWork(ctx, schedDSNVal)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		items = append(items, found...)
	}

	if strings.TrimSpace(chatDSNVal) != "" {
		found, err := queryChatActiveWork(ctx, chatDSNVal, svcStartUnix)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		items = append(items, found...)
	}

	return items, firstErr
}

func querySchedActiveWork(ctx context.Context, dsn string) ([]string, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var hasTasks bool
	if err := db.QueryRowContext(qctx, "SELECT to_regclass('public.tasks') IS NOT NULL").Scan(&hasTasks); err != nil || !hasTasks {
		return nil, err
	}

	rows, err := db.QueryContext(qctx, `
		SELECT id::text,
		       COALESCE(NULLIF(regexp_replace(regexp_replace(title, '[[:cntrl:]]+', ' ', 'g'), '[[:space:]]+', ' ', 'g'), ''), 'untitled'),
		       status,
		       GREATEST(0, EXTRACT(EPOCH FROM (now() - COALESCE(started_at, created_at)))::bigint)
		  FROM tasks
		 WHERE status IN ('running', 'leased', 'assigned')
		   AND lease_expires_at IS NOT NULL
		   AND lease_expires_at >= now()
		 ORDER BY COALESCE(started_at, created_at) ASC
		 LIMIT 5`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var id, title, status string
		var ageSec int64
		if err := rows.Scan(&id, &title, &status, &ageSec); err != nil {
			return nil, err
		}
		title = strings.TrimSpace(sanitizeSummaryText(title))
		if title == "" {
			title = "untitled"
		}
		out = append(out, fmt.Sprintf("scheduled task %s (%q, %s, %ds)", sanitizeSummaryText(id), title, sanitizeSummaryText(status), ageSec))
	}
	return out, rows.Err()
}

func queryChatActiveWork(ctx context.Context, dsn string, svcStartUnix int64) ([]string, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()

	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	nowUnix := time.Now().Unix()
	var out []string

	var hasTurns bool
	if err := db.QueryRowContext(qctx, "SELECT to_regclass('public.turns') IS NOT NULL").Scan(&hasTurns); err != nil {
		return nil, err
	}
	if hasTurns {
		rows, err := db.QueryContext(qctx, `
			SELECT turn_id,
			       conversation_id,
			       GREATEST(0, $1 - started_at)
			  FROM turns
			 WHERE status = 'running'
			   AND finished_at IS NULL
			   AND started_at >= $2
			 ORDER BY started_at ASC
			 LIMIT 5`, nowUnix, svcStartUnix)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var turnID, convID string
			var ageSec int64
			if err := rows.Scan(&turnID, &convID, &ageSec); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out = append(out, fmt.Sprintf("chat turn %s (conv %s, %ds)", sanitizeSummaryText(turnID), sanitizeSummaryText(convID), ageSec))
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
	}

	var hasApprovals bool
	if err := db.QueryRowContext(qctx, "SELECT to_regclass('public.approvals') IS NOT NULL").Scan(&hasApprovals); err != nil {
		return out, err
	}
	if hasApprovals {
		approvalCutoff := max(svcStartUnix, nowUnix-int64(defaultIdleApprovalWindow/time.Second))
		rows, err := db.QueryContext(qctx, `
			SELECT id,
			       tool_name,
			       GREATEST(0, $1 - COALESCE(resolved_at, created_at))
			  FROM approvals
			 WHERE status = 'approved'
			   AND is_err IS NULL
			   AND result_text LIKE 'Approved%executing%'
			   AND COALESCE(resolved_at, created_at) >= $2
			 ORDER BY COALESCE(resolved_at, created_at) ASC
			 LIMIT 5`, nowUnix, approvalCutoff)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var id, toolName string
			var ageSec int64
			if err := rows.Scan(&id, &toolName, &ageSec); err != nil {
				_ = rows.Close()
				return out, err
			}
			out = append(out, fmt.Sprintf("approval execution %s (%s, %ds)", sanitizeSummaryText(id), sanitizeSummaryText(toolName), ageSec))
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return out, err
		}
		_ = rows.Close()
	}

	return out, nil
}

// WaitIdle sets the update drain sentinel file to quiesce new work admission,
// then polls ActiveWork until no tasks, turns, or approval executions are in
// flight, or until maxWait elapses. When work remains in flight at the
// deadline, it clears the drain sentinel and returns *ActiveWorkError. When it
// succeeds, the drain sentinel remains set until the caller clears it after
// restarting or stopping the service.
func WaitIdle(ctx context.Context, schedDSNVal, chatDSNVal string, maxWait, pollInterval time.Duration, out io.Writer) error {
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}
	SetUpdateDrain()
	svcState, _ := serviceRuntimeState(serviceName(""))
	start := time.Now()
	deadline := start.Add(max(maxWait, 0))
	for {
		items, err := ActiveWork(ctx, schedDSNVal, chatDSNVal)
		if err != nil && svcState == "active" {
			items = append(items, fmt.Sprintf("active-work query failed while service is active: %v", err))
		}
		if len(items) == 0 {
			return nil
		}
		if maxWait <= 0 || !time.Now().Before(deadline) {
			ClearUpdateDrain()
			return &ActiveWorkError{Items: items}
		}
		if out != nil {
			elapsed := int(time.Since(start).Seconds())
			total := int(maxWait.Seconds())
			fmt.Fprintf(out, "waiting for active work to finish (%ds/%ds): %s\n", elapsed, total, strings.Join(items, "; "))
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			ClearUpdateDrain()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
