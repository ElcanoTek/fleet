package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TurnEvent mirrors one bufferedEvent row in the SSE replay ledger.
// Kept as a plain struct so the httpapi layer can shuttle values in
// and out without importing anything else from here.
type TurnEvent struct {
	TurnID    string
	EventID   uint64
	Name      string
	Data      []byte // already-serialized JSON payload
	CreatedAt int64
}

// TurnStatus mirrors the check constraint on turns.status. Exported
// as a typed string so callers don't pass arbitrary values.
type TurnStatus string

const (
	TurnStatusRunning   TurnStatus = "running"
	TurnStatusCompleted TurnStatus = "completed"
	TurnStatusCancelled TurnStatus = "cancelled"
	TurnStatusError     TurnStatus = "error"
)

// CreateTurn inserts a fresh turns row in the "running" state. Called
// by postChat once the buffer is registered — BEFORE any events are
// emitted, so the FK from turn_events → turns always resolves.
func (s *Store) CreateTurn(ctx context.Context, turnID, convID string, startedAt int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO turns (turn_id, conversation_id, started_at, status)
		 VALUES ($1, $2, $3, $4)`,
		turnID, convID, startedAt, string(TurnStatusRunning),
	)
	return err
}

// FinishTurn updates the terminal status + finished_at and records whether the
// turn's persisted event stream is lossy (the in-memory buffer could not fully
// persist it — see turnBuffer.Finish). Safe to call multiple times; a second
// update to the same terminal value is a no-op. Never moves a terminal turn back
// to "running".
func (s *Store) FinishTurn(ctx context.Context, turnID string, status TurnStatus, finishedAt int64, lossy bool) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE turns
		    SET status = $1,
		        finished_at = $2,
		        lossy = $3
		  WHERE turn_id = $4
		    AND status = 'running'`,
		string(status), finishedAt, lossy, turnID,
	)
	return err
}

// InsertTurnEvents appends a batch of events to turn_events.
// Deduplicates on (turn_id, event_id) via ON CONFLICT so a crashed
// flush is safe to retry.
//
// The batch is built as a single multi-row INSERT rather than a
// prepared statement per row — one round trip per flush dominates,
// and the batch sizes the persister uses (≤64 rows) are small enough
// that parameter count stays well under Postgres' 65535 cap.
func (s *Store) InsertTurnEvents(ctx context.Context, events []TurnEvent) error {
	if len(events) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString(`INSERT INTO turn_events (turn_id, event_id, event_name, data_json, created_at) VALUES `)
	args := make([]any, 0, len(events)*5)
	for i, e := range events {
		if i > 0 {
			b.WriteString(", ")
		}
		base := i*5 + 1
		fmt.Fprintf(&b, "($%d, $%d, $%d, $%d, $%d)", base, base+1, base+2, base+3, base+4)
		args = append(args, e.TurnID, int64(e.EventID), e.Name, string(e.Data), e.CreatedAt) //nolint:gosec // event IDs are small monotonic counters; postgres BIGINT is signed
	}
	b.WriteString(` ON CONFLICT (turn_id, event_id) DO NOTHING`)
	_, err := s.db.ExecContext(ctx, b.String(), args...)
	return err
}

// LoadTurnEvents returns every event for turnID with event_id >
// afterEventID, in ascending order. Used by the /stream reattach
// handler as a fallback when the in-memory buffer has been evicted
// (beyond retention TTL or after a crash+restart).
func (s *Store) LoadTurnEvents(ctx context.Context, turnID string, afterEventID uint64) ([]TurnEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT event_id, event_name, data_json, created_at
		 FROM turn_events
		 WHERE turn_id = $1 AND event_id > $2
		 ORDER BY event_id ASC`,
		turnID, int64(afterEventID), //nolint:gosec // event IDs are small monotonic counters; postgres BIGINT is signed
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TurnEvent
	for rows.Next() {
		var e TurnEvent
		e.TurnID = turnID
		var eid int64
		var data string
		if err := rows.Scan(&eid, &e.Name, &data, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.EventID = uint64(eid) //nolint:gosec // postgres BIGINT round-trips the value we wrote; never negative in practice
		e.Data = []byte(data)
		out = append(out, e)
	}
	return out, rows.Err()
}

// LookupTurn returns metadata for turnID — status, conversation id,
// started/finished timestamps. Returns (nil, nil) when not found so
// callers can 404 cleanly.
type TurnRecord struct {
	TurnID         string
	ConversationID string
	StartedAt      int64
	FinishedAt     sql.NullInt64
	Status         TurnStatus
	// Lossy is true when the turn's persisted event stream may be incomplete
	// (the buffer's backfill on Finish could not write every event). A
	// reattaching client can use this to warn that the replayed history is partial.
	Lossy bool
}

func (s *Store) LookupTurn(ctx context.Context, turnID string) (*TurnRecord, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT turn_id, conversation_id, started_at, finished_at, status, lossy
		 FROM turns WHERE turn_id = $1`, turnID)
	var r TurnRecord
	var status string
	if err := row.Scan(&r.TurnID, &r.ConversationID, &r.StartedAt, &r.FinishedAt, &status, &r.Lossy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	r.Status = TurnStatus(status)
	return &r, nil
}

// MarkRunningTurnsErrored runs at startup. Any turn still flagged
// 'running' was mid-flight when the previous process died; we upgrade
// it to 'error' and append a synthetic terminal event so a reattaching
// client sees a clean EOF instead of hanging.
// Returns the list of touched turn IDs so the caller can log.
func (s *Store) MarkRunningTurnsErrored(ctx context.Context) ([]string, error) {
	now := time.Now().Unix()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx,
		`SELECT turn_id FROM turns WHERE status = 'running'`)
	if err != nil {
		return nil, err
	}
	var turnIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		turnIDs = append(turnIDs, id)
	}
	_ = rows.Close()
	if len(turnIDs) == 0 {
		return nil, tx.Commit()
	}

	// Flip status + stamp finished_at for every running turn in one
	// statement, then append a synthetic turn.error event to each.
	if _, err := tx.ExecContext(ctx,
		`UPDATE turns SET status = 'error', finished_at = $1 WHERE status = 'running'`,
		now,
	); err != nil {
		return nil, err
	}

	// One synthetic event per turn. Reuse the batch helper shape to
	// stay inside the same transaction.
	for _, id := range turnIDs {
		// Find the next event_id so we don't collide with whatever was
		// already persisted before the crash.
		var maxID sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT MAX(event_id) FROM turn_events WHERE turn_id = $1`, id,
		).Scan(&maxID); err != nil {
			return nil, err
		}
		next := int64(1)
		if maxID.Valid {
			next = maxID.Int64 + 1
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO turn_events (turn_id, event_id, event_name, data_json, created_at)
			 VALUES ($1, $2, 'turn.error', $3, $4)`,
			id, next, `{"message":"server restarted mid-turn"}`, now,
		); err != nil {
			return nil, err
		}
	}

	return turnIDs, tx.Commit()
}

// SweepTurnEvents deletes turns (and their events, via FK cascade)
// that finished more than ttl ago. Called after every successful turn
// — cheap enough at chat scale not to warrant a separate janitor.
// Running turns are never swept.
func (s *Store) SweepTurnEvents(ctx context.Context, ttl time.Duration) (int, error) {
	cutoff := time.Now().Add(-ttl).Unix()
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM turns
		  WHERE status != 'running'
		    AND finished_at IS NOT NULL
		    AND finished_at < $1`,
		cutoff,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
