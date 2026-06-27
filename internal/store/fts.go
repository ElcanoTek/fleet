package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// SearchResult is one ranked full-text match (#308): a conversation whose title
// or message content matched the query, with a highlighted preview snippet.
type SearchResult struct {
	ConversationID string `json:"conversation_id"`
	Title          string `json:"title"`
	// Preview is a short snippet with the matched terms wrapped in <mark>…</mark>.
	// Produced by ts_headline; the HTTP layer passes it through to the client,
	// which must sanitize to allow ONLY <mark> tags.
	Preview string `json:"match_preview"`
	// MatchedAt is the conversation's updated_at (unix seconds).
	MatchedAt int64 `json:"matched_at"`
}

// searchableType reports whether a message type carries user-facing plaintext
// worth indexing for search: user/assistant text and user-initiated summaries.
// Reasoning, tool calls/results, and turn-cost metadata are intentionally excluded.
func searchableType(typ string) bool {
	return typ == "text" || typ == "summary"
}

// extractSearchText pulls the plaintext to index from a history entry's JSON
// content. Both TextContent and SummaryContent marshal a {"text": "..."} field.
// Returns ("", false) for non-searchable types or empty text.
func extractSearchText(typ string, content []byte) (string, bool) {
	if !searchableType(typ) {
		return "", false
	}
	var c struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &c); err != nil {
		return "", false
	}
	t := strings.TrimSpace(c.Text)
	return t, t != ""
}

// insertSearchContent extracts the searchable plaintext from entries (aligned
// with their inserted message ids) and bulk-inserts it into
// message_search_content within the caller's transaction. Idempotent via
// ON CONFLICT (message_id). No-op when nothing is searchable.
func insertSearchContent(ctx context.Context, tx *sql.Tx, convID string, now int64, entries []agent.HistoryEntry, ids []int64) error {
	var b strings.Builder
	b.WriteString(`INSERT INTO message_search_content (conversation_id, message_id, content, created_at) VALUES `)
	args := make([]any, 0, len(entries)*4)
	n := 0
	for i, e := range entries {
		text, ok := extractSearchText(e.Type, e.Content)
		if !ok {
			continue
		}
		if n > 0 {
			b.WriteString(", ")
		}
		base := n*4 + 1
		fmt.Fprintf(&b, "($%d, $%d, $%d, $%d)", base, base+1, base+2, base+3)
		args = append(args, convID, ids[i], text, now)
		n++
	}
	if n == 0 {
		return nil
	}
	b.WriteString(" ON CONFLICT (message_id) DO NOTHING")
	_, err := tx.ExecContext(ctx, b.String(), args...)
	return err
}

// SearchConversations returns conversations owned by userEmail whose TITLE or
// message CONTENT matches query, ranked by ts_rank_cd (title + best body match)
// and paginated. The second return value is the total match count (for paging).
// Each result carries a <mark>-highlighted ts_headline preview.
//
// The query is parsed with websearch_to_tsquery, which accepts free-form input
// ("foo bar", quoted phrases, OR, -negation) without throwing on punctuation —
// so a raw user string never errors the query. An empty/blank query returns no
// results rather than every row.
func (s *Store) SearchConversations(ctx context.Context, userEmail, query string, limit, offset int) ([]SearchResult, int, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		return nil, 0, nil
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}

	// total: distinct conversations that match in title OR any message body.
	const countSQL = `
SELECT COUNT(DISTINCT c.id)
FROM conversations c
CROSS JOIN websearch_to_tsquery('english', $1) AS q
LEFT JOIN message_search_content m
       ON m.conversation_id = c.id AND m.search_vector @@ q
WHERE c.user_email = $2
  AND (c.search_vector @@ q OR m.search_vector @@ q)`
	var total int
	if err := s.db.QueryRowContext(ctx, countSQL, q, userEmail).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("search count: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	const searchSQL = `
SELECT c.id, c.title, c.updated_at,
       ts_headline('english',
                   COALESCE((array_agg(m.content ORDER BY ts_rank_cd(m.search_vector, q) DESC)
                             FILTER (WHERE m.search_vector @@ q))[1], c.title),
                   q,
                   'MaxWords=30, MinWords=15, ShortWord=3, MaxFragments=1, StartSel=<mark>, StopSel=</mark>') AS preview
FROM conversations c
CROSS JOIN websearch_to_tsquery('english', $1) AS q
LEFT JOIN message_search_content m
       ON m.conversation_id = c.id AND m.search_vector @@ q
WHERE c.user_email = $2
  AND (c.search_vector @@ q OR m.search_vector @@ q)
GROUP BY c.id, c.title, c.updated_at, c.search_vector, q
ORDER BY (ts_rank_cd(c.search_vector, q) + COALESCE(MAX(ts_rank_cd(m.search_vector, q)), 0)) DESC,
         c.updated_at DESC
LIMIT $3 OFFSET $4`

	rows, err := s.db.QueryContext(ctx, searchSQL, q, userEmail, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	results := make([]SearchResult, 0, limit)
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.ConversationID, &r.Title, &r.MatchedAt, &r.Preview); err != nil {
			return nil, 0, fmt.Errorf("search scan: %w", err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("search rows: %w", err)
	}
	return results, total, nil
}

// backfillBatchSize bounds each backfill pass so the index build never holds a
// long lock or a huge transaction.
const backfillBatchSize = 500

// backfillRow is one candidate message in a backfill batch.
type backfillRow struct {
	id       int64
	convID   string
	text     string
	created  int64
	haveText bool // false → no searchable plaintext; skip the insert
}

// BackfillSearchContent walks existing messages and populates
// message_search_content for any that predate FTS (or were written while search
// was disabled). It pages by an ascending id high-water mark — which guarantees
// forward progress even for messages with no extractable text — and inserts with
// ON CONFLICT DO NOTHING so re-runs are idempotent. Batched so it never holds a
// long lock; safe to run on every startup. No-op when search is off. Returns the
// number of rows inserted.
func (s *Store) BackfillSearchContent(ctx context.Context) (int, error) {
	if !s.searchEnabled {
		return 0, nil
	}
	inserted := 0
	var lastID int64
	for {
		select {
		case <-ctx.Done():
			return inserted, ctx.Err()
		default:
		}
		// Page strictly forward by id (skipping rows already extracted) so a
		// non-extractable message can never be reselected within this run.
		const selectSQL = `
SELECT m.id, m.conversation_id, m.type, m.content, m.created_at
FROM messages m
LEFT JOIN message_search_content s ON s.message_id = m.id
WHERE m.id > $1
  AND s.message_id IS NULL
  AND m.type IN ('text', 'summary')
ORDER BY m.id
LIMIT $2`
		rows, err := s.db.QueryContext(ctx, selectSQL, lastID, backfillBatchSize)
		if err != nil {
			return inserted, fmt.Errorf("backfill select: %w", err)
		}
		batch := make([]backfillRow, 0, backfillBatchSize)
		for rows.Next() {
			var (
				id      int64
				convID  string
				typ     string
				content []byte
				created int64
			)
			if err := rows.Scan(&id, &convID, &typ, &content, &created); err != nil {
				_ = rows.Close()
				return inserted, fmt.Errorf("backfill scan: %w", err)
			}
			text, ok := extractSearchText(typ, content)
			batch = append(batch, backfillRow{id: id, convID: convID, text: text, created: created, haveText: ok})
			lastID = id
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return inserted, fmt.Errorf("backfill rows: %w", err)
		}
		_ = rows.Close()
		if len(batch) == 0 {
			return inserted, nil // caught up
		}
		n, err := s.insertBackfillBatch(ctx, batch)
		if err != nil {
			return inserted, err
		}
		inserted += n
		if len(batch) < backfillBatchSize {
			return inserted, nil
		}
	}
}

// insertBackfillBatch bulk-inserts the extractable rows of a backfill batch.
func (s *Store) insertBackfillBatch(ctx context.Context, batch []backfillRow) (int, error) {
	var b strings.Builder
	b.WriteString(`INSERT INTO message_search_content (conversation_id, message_id, content, created_at) VALUES `)
	args := make([]any, 0, len(batch)*4)
	n := 0
	for _, r := range batch {
		if !r.haveText {
			continue
		}
		if n > 0 {
			b.WriteString(", ")
		}
		base := n*4 + 1
		fmt.Fprintf(&b, "($%d, $%d, $%d, $%d)", base, base+1, base+2, base+3)
		args = append(args, r.convID, r.id, r.text, r.created)
		n++
	}
	if n == 0 {
		return 0, nil
	}
	b.WriteString(" ON CONFLICT (message_id) DO NOTHING")
	if _, err := s.db.ExecContext(ctx, b.String(), args...); err != nil {
		return 0, fmt.Errorf("backfill insert: %w", err)
	}
	return n, nil
}
