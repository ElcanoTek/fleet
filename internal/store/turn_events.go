package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// TurnEvent mirrors one bufferedEvent row in the SSE replay ledger.
// Kept as a plain struct so the httpapi layer can shuttle values in
// and out without importing anything else from here.
//
// Sequence, ConversationID, and TurnIndex are populated by the read path
// (GetTurnEventPage) and left zero by the incremental insert path — the store
// derives those columns from the owning turn at insert time, so callers of
// InsertTurnEvents need not set them.
type TurnEvent struct {
	TurnID         string
	EventID        uint64
	Name           string
	Data           []byte // already-serialized JSON payload
	CreatedAt      int64
	Sequence       int64  // per-conversation monotonic cursor (read path only)
	ConversationID string // owning conversation (read path only)
	TurnIndex      int    // ordinal of the turn within the conversation (read path only)
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
//
// turn_index is assigned as the count of pre-existing turns for the
// conversation (0-based ordinal). Turns in a conversation are created
// serially — there is one governed agent loop per conversation, so no two
// turns of the same conversation create concurrently — which keeps the
// ordinal gap-free without a separate lock.
func (s *Store) CreateTurn(ctx context.Context, turnID, convID string, startedAt int64) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO turns (turn_id, conversation_id, started_at, status, turn_index)
		 VALUES ($1, $2, $3, $4,
		         (SELECT COUNT(*) FROM turns WHERE conversation_id = $2))`,
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
// The batch is built as a single multi-row INSERT…SELECT rather than a
// prepared statement per row — one round trip per flush dominates, and the
// batch sizes the persister uses (≤64 rows) are small enough that parameter
// count stays well under Postgres' 65535 cap.
//
// conversation_id, turn_index, and the per-conversation `sequence` cursor are
// DERIVED here from the owning turn rather than supplied by the caller (the
// httpapi buffer doesn't know them). A batch is always for a single turn, so
// every row shares one conversation_id/turn_index. sequence is assigned as the
// conversation's current MAX(sequence) plus the row's 1-based position in the
// batch, giving contiguous, gap-free sequences. Because there is one governed
// loop per conversation, no two batches for the same conversation race, so the
// turn_events_conv_seq unique index never trips on the happy path; a retry of
// the SAME rows is absorbed by ON CONFLICT (turn_id, event_id) DO NOTHING
// before any sequence is consumed.
func (s *Store) InsertTurnEvents(ctx context.Context, events []TurnEvent) error {
	if len(events) == 0 {
		return nil
	}
	var b strings.Builder
	// VALUES rows carry the per-row data plus a 1-based ordinal (rn) used to
	// fan the conversation's base sequence out across the batch. The explicit
	// ::-casts pin the VALUES column types so Postgres doesn't infer "unknown".
	b.WriteString(`INSERT INTO turn_events
		(turn_id, conversation_id, turn_index, sequence, event_id, event_name, data_json, created_at)
		SELECT v.turn_id, t.conversation_id, t.turn_index,
		       (SELECT COALESCE(MAX(te.sequence), 0)
		          FROM turn_events te
		         WHERE te.conversation_id = t.conversation_id) + v.rn,
		       v.event_id, v.event_name, v.data_json, v.created_at
		  FROM (VALUES `)
	args := make([]any, 0, len(events)*6)
	for i, e := range events {
		if i > 0 {
			b.WriteString(", ")
		}
		base := i*6 + 1
		fmt.Fprintf(&b, "($%d::text, $%d::bigint, $%d::text, $%d::text, $%d::bigint, $%d::bigint)",
			base, base+1, base+2, base+3, base+4, base+5)
		args = append(args, e.TurnID, int64(e.EventID), e.Name, string(e.Data), e.CreatedAt, int64(i+1)) //nolint:gosec // event IDs / ordinals are small monotonic counters; postgres BIGINT is signed
	}
	b.WriteString(`) AS v(turn_id, event_id, event_name, data_json, created_at, rn)
		JOIN turns t ON t.turn_id = v.turn_id
		ON CONFLICT (turn_id, event_id) DO NOTHING`)
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

// DefaultTurnEventPageLimit is the page size used when a caller passes
// limit <= 0 to GetTurnEventPage. Mirrors Paseo's
// DEFAULT_TIMELINE_FETCH_LIMIT so the two timelines page identically.
const DefaultTurnEventPageLimit = 200

// MaxTurnEventPageLimit caps a single page so a hostile or buggy client
// can't ask for an unbounded result set.
const MaxTurnEventPageLimit = 500

// GetTurnEventPage returns one page of a conversation's turn events ordered by
// the per-conversation `sequence` cursor (#189).
//
// The returned slice is ALWAYS in ascending sequence order regardless of
// direction, so callers render it the same way; only the cursor semantics
// differ:
//
//   - asc=true  (forward / catch-up): events with sequence > cursor, ascending.
//     A cursor of 0 means "from the very beginning".
//   - asc=false (backward / scroll-up): events with sequence < cursor, i.e. the
//     `limit` newest events strictly below the cursor. A cursor of 0 means
//     "from the very end" (the newest events). The rows are reversed before
//     returning so the slice stays ascending.
//
// nextCursor is the boundary sequence the caller passes to fetch the adjacent
// page in the SAME direction: for asc it is the last (highest) returned
// sequence; for desc it is the first (lowest) returned sequence. It is 0 when
// there are no further pages in that direction (fewer than `limit` rows came
// back). limit <= 0 falls back to DefaultTurnEventPageLimit and is clamped to
// MaxTurnEventPageLimit.
func (s *Store) GetTurnEventPage(ctx context.Context, conversationID string, cursor int64, limit int, asc bool) ([]TurnEvent, int64, error) {
	if limit <= 0 {
		limit = DefaultTurnEventPageLimit
	}
	if limit > MaxTurnEventPageLimit {
		limit = MaxTurnEventPageLimit
	}

	var query string
	if asc {
		query = `SELECT sequence, turn_id, turn_index, event_id, event_name, data_json, created_at
		         FROM turn_events
		         WHERE conversation_id = $1 AND sequence > $2
		         ORDER BY sequence ASC
		         LIMIT $3`
	} else {
		// For "from the end" (cursor 0) we want everything below +infinity, so
		// substitute a sentinel larger than any real sequence. Postgres BIGINT
		// max works; we never assign sequences that high.
		if cursor <= 0 {
			cursor = math.MaxInt64
		}
		query = `SELECT sequence, turn_id, turn_index, event_id, event_name, data_json, created_at
		         FROM turn_events
		         WHERE conversation_id = $1 AND sequence < $2
		         ORDER BY sequence DESC
		         LIMIT $3`
	}

	rows, err := s.db.QueryContext(ctx, query, conversationID, cursor, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []TurnEvent
	for rows.Next() {
		var e TurnEvent
		e.ConversationID = conversationID
		var seq, eid int64
		var data string
		if err := rows.Scan(&seq, &e.TurnID, &e.TurnIndex, &eid, &e.Name, &data, &e.CreatedAt); err != nil {
			return nil, 0, err
		}
		e.Sequence = seq
		e.EventID = uint64(eid) //nolint:gosec // postgres BIGINT round-trips the value we wrote; never negative in practice
		e.Data = []byte(data)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	// nextCursor: only advertise a further page when we filled the limit. The
	// boundary is the last row as the query ORDERED it (the highest sequence for
	// asc, the lowest for desc) — i.e. the edge to resume scanning from in the
	// SAME direction. Computed BEFORE the desc reversal so it reads that edge.
	var nextCursor int64
	if len(out) == limit {
		nextCursor = out[len(out)-1].Sequence
	}

	if !asc {
		// Reverse in place so the returned slice is ascending like the asc case.
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}

	return out, nextCursor, nil
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
	return s.lookupTurn(ctx, turnID, "")
}

// LookupTurnInConversation is the scoped lookup used by the stream
// DB-fallback. conversation_id lives in the query so a future refactor
// cannot drop the handler's equality check and leak another conversation's
// turn events (#1112). Returns (nil, nil) when the turn is missing or
// belongs to a different conversation.
func (s *Store) LookupTurnInConversation(ctx context.Context, turnID, conversationID string) (*TurnRecord, error) {
	if conversationID == "" {
		return nil, nil
	}
	return s.lookupTurn(ctx, turnID, conversationID)
}

func (s *Store) lookupTurn(ctx context.Context, turnID, conversationID string) (*TurnRecord, error) {
	q := `SELECT turn_id, conversation_id, started_at, finished_at, status, lossy
		 FROM turns WHERE turn_id = $1`
	args := []any{turnID}
	if conversationID != "" {
		q += ` AND conversation_id = $2`
		args = append(args, conversationID)
	}
	row := s.db.QueryRowContext(ctx, q, args...)
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

// SweepTurnEvents deletes finished turns — and, via FK cascade, their
// turn_events ledger rows and turn_journal records — that reached a terminal
// state more than ttl ago. Called from the post-turn retention sweep
// (httpapi.sweepRetention), alongside the conversation and input-queue
// sweeps; cheap enough at chat scale not to warrant a separate janitor.
//
// Running turns are never swept, so startup recovery (RecoverStrandedTurns,
// which scans status='running' before the server takes traffic) always sees
// its inputs; a swept turn is by definition one whose recovery window closed
// ttl ago. Canonical history is unaffected: messages rows are the durable
// record, the ledger is a delivery/replay layer (see docs/TURN-JOURNAL.md).
// A non-positive ttl disables the sweep, mirroring PurgeTerminalInputs.
func (s *Store) SweepTurnEvents(ctx context.Context, ttl time.Duration) (int, error) {
	if ttl <= 0 {
		return 0, nil
	}
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
