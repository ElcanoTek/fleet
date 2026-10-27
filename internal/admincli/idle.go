package admincli

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	defaultIdleTurnWindow     = 15 * time.Minute
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

// ActiveWork inspects the sched and chat databases for in-flight work that
// would be interrupted by stopping or restarting fleet.service:
//   - sched.tasks with status IN ('running', 'leased', 'assigned')
//   - chat.turns with status = 'running' AND finished_at IS NULL started within 15m
//   - chat.approvals with status = 'approved' AND is_err IS NULL AND
//     result_text LIKE 'Approved%executing%' within 2h
func ActiveWork(ctx context.Context, schedDSNVal, chatDSNVal string) ([]string, error) {
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
		found, err := queryChatActiveWork(ctx, chatDSNVal)
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
		       COALESCE(NULLIF(regexp_replace(title, '[[:space:]]+', ' ', 'g'), ''), 'untitled'),
		       status,
		       GREATEST(0, EXTRACT(EPOCH FROM (now() - COALESCE(started_at, updated_at, created_at)))::bigint)
		  FROM tasks
		 WHERE status IN ('running', 'leased', 'assigned')
		 ORDER BY COALESCE(started_at, updated_at, created_at) ASC
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
		out = append(out, fmt.Sprintf("scheduled task %s (%q, %s, %ds)", id, title, status, ageSec))
	}
	return out, rows.Err()
}

func queryChatActiveWork(ctx context.Context, dsn string) ([]string, error) {
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
		turnCutoff := nowUnix - int64(defaultIdleTurnWindow/time.Second)
		rows, err := db.QueryContext(qctx, `
			SELECT turn_id,
			       conversation_id,
			       GREATEST(0, $1 - started_at)
			  FROM turns
			 WHERE status = 'running'
			   AND finished_at IS NULL
			   AND started_at > $2
			 ORDER BY started_at ASC
			 LIMIT 5`, nowUnix, turnCutoff)
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
			out = append(out, fmt.Sprintf("chat turn %s (conv %s, %ds)", turnID, convID, ageSec))
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
		approvalCutoff := nowUnix - int64(defaultIdleApprovalWindow/time.Second)
		rows, err := db.QueryContext(qctx, `
			SELECT id,
			       tool_name,
			       GREATEST(0, $1 - COALESCE(resolved_at, created_at))
			  FROM approvals
			 WHERE status = 'approved'
			   AND is_err IS NULL
			   AND result_text LIKE 'Approved%executing%'
			   AND COALESCE(resolved_at, created_at) > $2
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
			out = append(out, fmt.Sprintf("approval execution %s (%s, %ds)", id, toolName, ageSec))
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return out, err
		}
		_ = rows.Close()
	}

	return out, nil
}

// WaitIdle polls ActiveWork until no tasks, turns, or approval executions are
// in flight, or until maxWait elapses. When work remains in flight at the
// deadline, it returns *ActiveWorkError. Database reachability errors alone do
// not return an error so that `fleet restart` can still recover a box whose
// database is down.
func WaitIdle(ctx context.Context, schedDSNVal, chatDSNVal string, maxWait, pollInterval time.Duration, out io.Writer) error {
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}
	start := time.Now()
	deadline := start.Add(max(maxWait, 0))
	for {
		items, _ := ActiveWork(ctx, schedDSNVal, chatDSNVal)
		if len(items) == 0 {
			return nil
		}
		if maxWait <= 0 || !time.Now().Before(deadline) {
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
			return ctx.Err()
		case <-timer.C:
		}
	}
}
