package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// A windowed read of a conversation's history (docs/CHAT-HISTORY-WINDOW.md).
//
// LoadHistory returns every stored entry, which for a long thread is tens of
// megabytes of tool output the reader will mostly never scroll back to. The
// reads here return a suffix of the history instead, and they only ever cut it
// where a turn opens.
//
// The cut is load-bearing, not cosmetic. What a participant may see of an
// entry depends on who sent the turn it belongs to (historyForParticipant in
// internal/httpapi), and that is decided by the user message that opened the
// turn. A window beginning partway through a turn would be read as the
// conversation owner's, so a teammate's working trace could reach the wrong
// screen. Starting at a turn's opening message leaves nothing to carry in.

// HistoryWindow is a suffix of a conversation's history that begins where a
// turn opens.
type HistoryWindow struct {
	// Entries are the window's stored entries, oldest first.
	Entries []agent.HistoryEntry
	// StartID is the id the window begins at: the opening message of its
	// first turn, or the conversation's first entry when no turn opens
	// earlier. 0 when the window is empty.
	StartID int64
	// HasMore reports whether entries older than StartID exist.
	HasMore bool
}

// opensTurn reports whether a stored entry is a turn's opening message: a user
// message that was not steered into a turn already running. A user row whose
// content does not parse counts as one, as the per-participant projection
// reads it.
func opensTurn(role, typ string, content []byte) bool {
	if role != "user" || typ != "text" {
		return false
	}
	var c agent.TextContent
	return json.Unmarshal(content, &c) != nil || !c.Steered
}

// historyScanChunk is how many entries one step of the backward search for a
// turn's opening message reads. The search reads ids, roles and types, and
// content for user messages only, so a step never touches a tool result's
// stored text.
const historyScanChunk = 256

// historyTurnStart walks back from the newest entry older than upper (0 = the
// newest entry) and returns the id of the first turn-opening message it meets
// once at least minEntries entries have been passed. When the history runs
// out first it returns the oldest entry's id, and 0 for no entries at all.
func (s *Store) historyTurnStart(ctx context.Context, convID string, upper int64, minEntries int) (int64, error) {
	var oldest int64
	seen := 0
	for {
		chunk := max(historyScanChunk, minEntries-seen+1)
		start, last, n, err := s.scanTurnStart(ctx, convID, upper, chunk, minEntries-seen)
		if err != nil {
			return 0, err
		}
		if start != 0 {
			return start, nil
		}
		if n > 0 {
			oldest = last
		}
		if n < chunk {
			return oldest, nil
		}
		seen += n
		upper = last
	}
}

// scanTurnStart reads up to chunk entries older than upper, newest first. It
// returns the id of the first turn-opening message at or past the need-th
// entry read (0 when the chunk holds none), the id of the oldest entry read
// and how many were read.
func (s *Store) scanTurnStart(ctx context.Context, convID string, upper int64, chunk, need int) (start, last int64, n int, err error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, role, type,
		        CASE WHEN role = 'user' AND type = 'text' THEN content END
		   FROM messages
		  WHERE conversation_id = $1 AND ($2 = 0 OR id < $2)
		  ORDER BY id DESC LIMIT $3`,
		convID, upper, chunk,
	)
	if err != nil {
		return 0, 0, 0, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			id        int64
			role, typ string
			content   sql.NullString
		)
		if err := rows.Scan(&id, &role, &typ, &content); err != nil {
			return 0, 0, 0, err
		}
		n++
		last = id
		if start == 0 && n >= need && opensTurn(role, typ, []byte(content.String)) {
			start = id
		}
	}
	return start, last, n, rows.Err()
}

// LoadHistoryTail returns the newest part of a conversation's history: at
// least limit entries older than beforeID (0 = from the newest entry), widened
// back to the opening message of the turn the limit-th falls in. A turn is
// never split, so a window can hold more than limit entries, and holds the
// whole remaining history when no turn opens earlier.
func (s *Store) LoadHistoryTail(ctx context.Context, convID string, limit int, beforeID int64) (HistoryWindow, error) {
	if limit < 1 {
		limit = 1
	}
	start, err := s.historyTurnStart(ctx, convID, beforeID, limit)
	if err != nil {
		return HistoryWindow{}, err
	}
	return s.loadHistoryWindow(ctx, convID, start, beforeID)
}

// LoadHistoryFrom returns the history from the turn that holds entry fromID
// onward: fromID is moved back to that turn's opening message, so a caller
// cannot begin a window partway through a turn. It is how a reader that
// already holds a window re-reads exactly the range it holds.
func (s *Store) LoadHistoryFrom(ctx context.Context, convID string, fromID int64) (HistoryWindow, error) {
	upper := int64(0)
	// The newest entry at or before fromID is the first one older than
	// fromID+1; the largest id has no successor to name, and means "newest".
	if fromID > 0 && fromID < 1<<62 {
		upper = fromID + 1
	}
	start, err := s.historyTurnStart(ctx, convID, upper, 1)
	if err != nil {
		return HistoryWindow{}, err
	}
	return s.loadHistoryWindow(ctx, convID, start, 0)
}

// loadHistoryWindow reads the entries from start up to (not including)
// beforeID, 0 meaning through the newest.
func (s *Store) loadHistoryWindow(ctx context.Context, convID string, start, beforeID int64) (HistoryWindow, error) {
	if start == 0 {
		return HistoryWindow{}, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, role, type, content, injected_context
		   FROM messages
		  WHERE conversation_id = $1 AND id >= $2 AND ($3 = 0 OR id < $3)
		  ORDER BY id ASC`,
		convID, start, beforeID,
	)
	if err != nil {
		return HistoryWindow{}, err
	}
	entries, err := scanHistoryRows(rows)
	if err != nil {
		return HistoryWindow{}, err
	}
	w := HistoryWindow{Entries: entries, StartID: start}
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM messages WHERE conversation_id = $1 AND id < $2)`,
		convID, start,
	).Scan(&w.HasMore); err != nil {
		return HistoryWindow{}, err
	}
	return w, nil
}

// scanHistoryRows reads (id, role, type, content, injected_context) rows into
// history entries and closes rows.
func scanHistoryRows(rows *sql.Rows) ([]agent.HistoryEntry, error) {
	defer func() { _ = rows.Close() }()
	var out []agent.HistoryEntry
	for rows.Next() {
		var e agent.HistoryEntry
		var content string
		// NULL for a pre-056 row (see the migration): those keep their blocks
		// inside content.text, so an empty string here is the honest read —
		// there is no separately-stored suffix to hand back.
		var injected sql.NullString
		if err := rows.Scan(&e.ID, &e.Role, &e.Type, &content, &injected); err != nil {
			return nil, err
		}
		e.InjectedContext = injected.String
		e.Content = json.RawMessage(content)
		out = append(out, e)
	}
	return out, rows.Err()
}

// LoadHistoryOutline returns, of the entries older than beforeID, only the
// ones a view of the whole conversation is built from: the user messages
// (which open turns and answer cards), each turn's summary, the show_ui calls
// and results a dashboard panel is drawn from, and where a compaction summary
// sits (its text is not returned). Tool output, reasoning and replies are
// left in the database, which is the point: the outline of a thread whose
// history is megabytes is a few kilobytes.
//
// The entries come back oldest first with no injected context. A show_ui row
// is recognized by the quoted tool name in its stored JSON; a quote inside a
// JSON string is escaped, so the match cannot come from a tool's input or
// output text, and a caller still reads the name from the decoded content.
func (s *Store) LoadHistoryOutline(ctx context.Context, convID string, beforeID int64) ([]agent.HistoryEntry, error) {
	if beforeID <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, role, type,
		        CASE WHEN type = 'summary' THEN '{}' ELSE content END,
		        NULL::text
		   FROM messages
		  WHERE conversation_id = $1 AND id < $2
		    AND ((role = 'user' AND type = 'text')
		      OR type IN ('turn_summary', 'summary', 'summary_boundary')
		      OR (type IN ('tool_call', 'tool_result') AND strpos(content, '"show_ui"') > 0))
		  ORDER BY id ASC`,
		convID, beforeID,
	)
	if err != nil {
		return nil, err
	}
	return scanHistoryRows(rows)
}
