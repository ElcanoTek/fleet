package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// ToolCallEntry is one row in the tool-call audit ledger (#224): a single tool
// invocation within an interactive chat turn. Entries are derived after the turn
// completes by pairing the turn's tool_call history entry (args) with its
// matching tool_result entry (outcome), so an entry exists even for a call whose
// result never arrived (an interrupted/cancelled turn) — in that case IsError is
// true, ResultSummary is empty, and DurationMS is nil.
//
// ArgsSummary and ResultSummary are already REDACTED, length-capped text. Raw
// secret values are scrubbed by the shared internal/redact pass before an entry
// is built, so this ledger never stores credential material — consistent with
// the host-side-credentials invariant.
type ToolCallEntry struct {
	ID             int64
	ConversationID string
	TurnID         string
	UserEmail      string
	ToolName       string
	ArgsSummary    string
	ResultSummary  string
	IsError        bool
	StartedAt      int64  // unix seconds
	DurationMS     *int64 // nil when not derivable
}

// RecordToolCalls appends a batch of tool-call audit rows. Built as a single
// multi-row INSERT (one round trip) mirroring InsertTurnEvents; the per-turn
// batch is tiny (a handful of tool calls), so the parameter count stays far
// under Postgres' 65535 cap. A no-op for an empty batch.
//
// Called best-effort from the post-turn persistence path: a failure here is
// logged by the caller and never fails the turn — the audit ledger is
// observability, not a turn-blocking dependency.
func (s *Store) RecordToolCalls(ctx context.Context, entries []ToolCallEntry) error {
	if len(entries) == 0 {
		return nil
	}
	const cols = 8
	var b strings.Builder
	b.WriteString(`INSERT INTO tool_call_log
		(conversation_id, turn_id, user_email, tool_name, args_summary, result_summary, is_error, started_at, duration_ms) VALUES `)
	// duration_ms is appended as an extra positional param per row (9th column),
	// handled separately because it is nullable.
	args := make([]any, 0, len(entries)*(cols+1))
	for i, e := range entries {
		if i > 0 {
			b.WriteString(", ")
		}
		base := i*(cols+1) + 1
		fmt.Fprintf(&b, "($%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d)",
			base, base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8)
		var dur any
		if e.DurationMS != nil {
			dur = *e.DurationMS
		}
		args = append(args,
			e.ConversationID, e.TurnID, e.UserEmail, e.ToolName,
			e.ArgsSummary, e.ResultSummary, e.IsError, e.StartedAt, dur)
	}
	_, err := s.db.ExecContext(ctx, b.String(), args...)
	return err
}

// ListToolCalls returns the audit history for one conversation, newest first.
// Membership is enforced by the caller (the HTTP handler 404s a conversation the
// user doesn't own before calling this) — this method is scoped by
// conversationID only, mirroring LoadHistory / LoadTurnEvents.
//
// toolFilter (optional) restricts to a single tool name; fromUnix (optional, 0 =
// no floor) restricts to rows started at/after that unix-second timestamp; limit
// caps the result count (callers clamp to a sane max).
func (s *Store) ListToolCalls(ctx context.Context, conversationID, toolFilter string, fromUnix int64, limit int) ([]ToolCallEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	var b strings.Builder
	b.WriteString(`SELECT id, conversation_id, turn_id, user_email, tool_name,
		args_summary, result_summary, is_error, started_at, duration_ms
		FROM tool_call_log WHERE conversation_id = $1`)
	args := []any{conversationID}
	if toolFilter != "" {
		args = append(args, toolFilter)
		fmt.Fprintf(&b, " AND tool_name = $%d", len(args))
	}
	if fromUnix > 0 {
		args = append(args, fromUnix)
		fmt.Fprintf(&b, " AND started_at >= $%d", len(args))
	}
	args = append(args, limit)
	fmt.Fprintf(&b, " ORDER BY started_at DESC, id DESC LIMIT $%d", len(args))

	rows, err := s.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ToolCallEntry, 0, limit)
	for rows.Next() {
		var e ToolCallEntry
		var dur sql.NullInt64
		if err := rows.Scan(&e.ID, &e.ConversationID, &e.TurnID, &e.UserEmail, &e.ToolName,
			&e.ArgsSummary, &e.ResultSummary, &e.IsError, &e.StartedAt, &dur); err != nil {
			return nil, err
		}
		if dur.Valid {
			v := dur.Int64
			e.DurationMS = &v
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
