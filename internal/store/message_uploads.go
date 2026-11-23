package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ElcanoTek/fleet/internal/logging"
)

// Uploads a message refers to (migration 0211, chat.message_uploads).
//
// A user message that attached images records each file's path
// (agent.TextContent.Images) and every later turn reads those files again, so
// an upload is in use for as long as a message naming it exists. The hourly
// attachment cleanup asks this table before it removes an upload
// (AttachmentKeeper), and the rows go with their message (ON DELETE CASCADE),
// so the cleanup follows whatever the conversation retention decided instead
// of a copy of its rules: a chat the retention sweep keeps keeps its uploads,
// and a chat that expires, is evicted, deleted or purged releases them.
//
// Every path is stored and looked up in one form, normalizeUploadPath, on the
// write side (insert and backfill) and the read side (lookup and snapshot).

// messageUploadRef is one (message, upload) row.
type messageUploadRef struct {
	messageID int64
	path      string
}

// normalizeUploadPath is the one form an upload path is stored and compared
// in: absolute and clean. "" for a blank path.
func normalizeUploadPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs // Abs cleans
	}
	return filepath.Clean(p)
}

// maxUploadPathBytes is the longest path recorded as an upload. A real one is
// the uploads root plus about 60 bytes plus a file name capped at 200 bytes
// (httpapi.sanitizeFilename); anything far longer names no upload, and an
// index entry has a size limit of its own (about 2.7 KB).
const maxUploadPathBytes = 2048

// recordableUploadPath reports whether p can name an upload and be stored:
// valid UTF-8 with no control character (Postgres refuses U+0000 in text, see
// internal/pgtext, and no upload name holds one — sanitizeFilename drops
// them) and no longer than maxUploadPathBytes. A path that fails names no
// upload, so skipping it loses nothing; storing it would fail the whole
// message write.
func recordableUploadPath(p string) bool {
	return p != "" && len(p) <= maxUploadPathBytes && utf8.ValidString(p) &&
		!strings.ContainsFunc(p, unicode.IsControl)
}

// uploadPathsOf returns the upload paths a stored message refers to: the
// images of a text entry. Anything that does not parse names nothing, and a
// path no upload can have (recordableUploadPath) is left out.
func uploadPathsOf(typ string, content []byte) []string {
	// Nearly every entry names no image: skip the parse for those.
	if typ != "text" || !bytes.Contains(content, []byte(`"images"`)) {
		return nil
	}
	var c struct {
		Images []struct {
			Path string `json:"path"`
		} `json:"images"`
	}
	if err := json.Unmarshal(content, &c); err != nil || len(c.Images) == 0 {
		return nil
	}
	out := make([]string, 0, len(c.Images))
	seen := make(map[string]bool, len(c.Images))
	for _, img := range c.Images {
		if !recordableUploadPath(img.Path) {
			continue
		}
		p := normalizeUploadPath(img.Path)
		if !recordableUploadPath(p) || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// insertMessageUploads records refs in the caller's transaction — the one that
// inserted the messages, so a message never exists without its rows. Two
// array parameters however many rows (a large import stays far below
// Postgres's bind-parameter limit). A no-op when no message refers to an
// upload, which is nearly every append.
func insertMessageUploads(ctx context.Context, tx *sql.Tx, refs []messageUploadRef) error {
	if len(refs) == 0 {
		return nil
	}
	ids := make([]int64, len(refs))
	paths := make([]string, len(refs))
	for i, r := range refs {
		ids[i], paths[i] = r.messageID, r.path
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO message_uploads (message_id, upload_path)
		 SELECT * FROM unnest($1::bigint[], $2::text[])
		 ON CONFLICT DO NOTHING`,
		ids, paths)
	return err
}

// messageUploadRefs pairs each inserted message id with the uploads its entry
// names. typ and content are read per index so every insert path (live turn
// projection, plain append, legacy import, the backfill) can share it.
func messageUploadRefs(ids []int64, entry func(i int) (typ string, content []byte)) []messageUploadRef {
	var refs []messageUploadRef
	for i, id := range ids {
		typ, content := entry(i)
		for _, p := range uploadPathsOf(typ, content) {
			refs = append(refs, messageUploadRef{messageID: id, path: p})
		}
	}
	return refs
}

// UploadReferenced reports whether a message of a conversation that still
// exists refers to the upload at path. That includes a soft-deleted
// conversation inside its 30-day window, whose rows (and workspace) are kept
// until the purge: its files stay with them. A nil store, which cannot tell,
// answers true, so a caller without a database keeps the file.
func (s *Store) UploadReferenced(ctx context.Context, path string) (bool, error) {
	if s == nil || s.db == nil {
		return true, nil
	}
	var referenced bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM message_uploads WHERE upload_path = $1)`,
		normalizeUploadPath(path),
	).Scan(&referenced)
	if err != nil {
		return true, fmt.Errorf("upload reference lookup: %w", err)
	}
	return referenced, nil
}

// ReferencedUploads returns every upload path a message of an existing
// conversation refers to, in one query, and whether that record is complete.
// It is incomplete until BackfillMessageUploads has recorded the messages
// written before migration 0211; until then a caller must treat every upload
// as in use. A nil store answers incomplete.
//
// The set is held in memory for one pass (about 150 MB per million referenced
// paths). Past that size, page it by uploads subdirectory (one owner's
// uploads/<user>/ at a time) rather than loading it whole.
func (s *Store) ReferencedUploads(ctx context.Context) (map[string]struct{}, bool, error) {
	if s == nil || s.db == nil {
		return nil, false, nil
	}
	state, err := s.MessageUploadsBackfillState(ctx)
	if err != nil {
		return nil, false, err
	}
	if !state.Complete {
		return nil, false, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT upload_path FROM message_uploads`)
	if err != nil {
		return nil, false, fmt.Errorf("upload references: %w", err)
	}
	defer rows.Close()
	out := make(map[string]struct{})
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, false, fmt.Errorf("upload references: %w", err)
		}
		out[p] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("upload references: %w", err)
	}
	return out, true, nil
}

// MessageUploadsBackfill is how far BackfillMessageUploads has got. Until it
// is Complete, the attachment cleanup keeps every upload.
type MessageUploadsBackfill struct {
	Complete  bool  `json:"complete"`
	ThroughID int64 `json:"through_id"` // the last message id it has read
	UptoID    int64 `json:"upto_id"`    // the last message id it must read
}

// MessageUploadsBackfillState reads the backfill's position. A database
// without the row (it cannot have been migrated) reads as not started.
func (s *Store) MessageUploadsBackfillState(ctx context.Context) (MessageUploadsBackfill, error) {
	var st MessageUploadsBackfill
	var completed sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT through_id, upto_id, completed_at FROM message_uploads_backfill WHERE id = 1`,
	).Scan(&st.ThroughID, &st.UptoID, &completed)
	if errors.Is(err, sql.ErrNoRows) {
		return MessageUploadsBackfill{}, nil
	}
	if err != nil {
		return MessageUploadsBackfill{}, fmt.Errorf("message uploads backfill state: %w", err)
	}
	st.Complete = completed.Valid
	return st, nil
}

// messageUploadsBackfillBatch is how many message ids one backfill transaction
// covers. A range, not a row count, so each query reads a bounded slice of the
// primary key however rare images are.
const messageUploadsBackfillBatch int64 = 10000

// BackfillMessageUploads records the uploads of the messages written before
// migration 0211 (every later message written by this release is recorded as
// it is written), then marks the record complete; until then AttachmentKeeper
// keeps every upload.
//
// Its upper bound is raised to the newest message id each time it starts, so
// messages a server of the previous release wrote after a command-line run of
// this release had already migrated the database are recorded too. It walks
// message ids in ranges of messageUploadsBackfillBatch, one transaction each,
// and saves its position after each, so a stop (shutdown, an error, ctx)
// resumes where it left off on the next call. It parses with the same
// uploadPathsOf the insert paths use. A range that Postgres refuses as data
// is recorded one message at a time, and a message it still refuses as data
// (isRowDataError) is logged and skipped, so one bad row cannot hold the
// record incomplete; any other failure (a cancel, a timeout, a full disk)
// rolls the range back and is returned, to be retried. Returns the number of
// references recorded; a no-op once complete.
func (s *Store) BackfillMessageUploads(ctx context.Context) (int, error) {
	return s.backfillMessageUploads(ctx, messageUploadsBackfillBatch)
}

func (s *Store) backfillMessageUploads(ctx context.Context, batch int64) (int, error) {
	var through, upto int64
	err := s.db.QueryRowContext(ctx,
		`UPDATE message_uploads_backfill
		    SET upto_id = GREATEST(upto_id, (SELECT COALESCE(MAX(id), 0) FROM messages))
		  WHERE id = 1 AND completed_at IS NULL
		  RETURNING through_id, upto_id`,
	).Scan(&through, &upto)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil // complete
	}
	if err != nil {
		return 0, fmt.Errorf("message uploads backfill state: %w", err)
	}
	recorded := 0
	for through < upto {
		if err := ctx.Err(); err != nil {
			return recorded, err
		}
		end := min(through+batch, upto)
		n, err := s.backfillMessageUploadsRange(ctx, through, end)
		if err != nil {
			return recorded, err
		}
		recorded += n
		through = end
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE message_uploads_backfill SET completed_at = $1 WHERE id = 1 AND completed_at IS NULL`,
		time.Now().Unix()); err != nil {
		return recorded, fmt.Errorf("message uploads backfill: mark complete: %w", err)
	}
	return recorded, nil
}

// backfillInsert records one batch of backfill references; a test seam over
// insertMessageUploads.
var backfillInsert = insertMessageUploads

// backfillMessageUploadsRange records the uploads of messages with ids in
// (after, end] and saves end as the position, in one transaction. The rows
// read are locked FOR KEY SHARE in id order — the order a conversation delete
// or a rewind takes them in — so a deletion meanwhile waits for the commit
// instead of failing the insert, and the two cannot deadlock. The batch insert
// runs under a savepoint; if Postgres refuses it as data, each message is
// recorded under a savepoint of its own, and one it still refuses as data is
// logged and skipped. Any other error returns at once, so the range rolls
// back and its position is not saved.
func (s *Store) backfillMessageUploadsRange(ctx context.Context, after, end int64) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx,
		`SELECT id, content FROM messages
		 WHERE id > $1 AND id <= $2 AND type = 'text' AND content LIKE '%"images"%'
		 ORDER BY id
		 FOR KEY SHARE`,
		after, end)
	if err != nil {
		return 0, fmt.Errorf("message uploads backfill: read: %w", err)
	}
	var (
		ids      []int64
		contents [][]byte
	)
	for rows.Next() {
		var id int64
		var content []byte
		if err := rows.Scan(&id, &content); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("message uploads backfill: scan: %w", err)
		}
		ids = append(ids, id)
		contents = append(contents, content)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, fmt.Errorf("message uploads backfill: read: %w", err)
	}
	_ = rows.Close()
	refs := messageUploadRefs(ids, func(i int) (string, []byte) { return "text", contents[i] })
	recorded, err := insertUnderSavepoint(ctx, tx, refs)
	if err != nil {
		if !isRowDataError(err) {
			return 0, fmt.Errorf("message uploads backfill: record: %w", err)
		}
		recorded = 0
		for i, id := range ids {
			one := messageUploadRefs([]int64{id}, func(int) (string, []byte) { return "text", contents[i] })
			n, err := insertUnderSavepoint(ctx, tx, one)
			switch {
			case err == nil:
				recorded += n
			case isRowDataError(err):
				logging.From(ctx).Warn("message uploads backfill: skipped a message whose uploads cannot be stored", "message_id", id, "err", err)
			default:
				return 0, fmt.Errorf("message uploads backfill: record message %d: %w", id, err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE message_uploads_backfill SET through_id = $1 WHERE id = 1`, end); err != nil {
		return 0, fmt.Errorf("message uploads backfill: save position: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return recorded, nil
}

// isRowDataError reports whether err is Postgres refusing the data itself:
// class 22 (a data exception — an invalid byte sequence, a value too long) or
// 54000 (a program limit — an index row too large). Only such an error is
// about the message, and skipping that message is safe. Anything else — a
// cancel or a timeout, a full disk, a lost connection, a context that ended —
// is about the attempt: the range must roll back and be retried, never
// skipped, or the record could be marked complete without a message a kept
// chat still uses.
func isRowDataError(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return strings.HasPrefix(pgErr.Code, "22") || pgErr.Code == "54000"
}

// insertUnderSavepoint records refs under a savepoint, rolling back to it and
// releasing it on failure so the transaction stays usable. A failure to roll
// back is returned in place of the insert's error: the transaction is then
// unusable, which is never a data error.
func insertUnderSavepoint(ctx context.Context, tx *sql.Tx, refs []messageUploadRef) (int, error) {
	if len(refs) == 0 {
		return 0, nil
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT message_uploads_backfill`); err != nil {
		return 0, err
	}
	if err := backfillInsert(ctx, tx, refs); err != nil {
		if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT message_uploads_backfill`); rbErr != nil {
			// The insert's error as text only: it must not read as a data
			// error, the transaction is unusable now.
			return 0, fmt.Errorf("roll back after %s: %w", err.Error(), rbErr)
		}
		if _, relErr := tx.ExecContext(ctx, `RELEASE SAVEPOINT message_uploads_backfill`); relErr != nil {
			return 0, fmt.Errorf("release after %s: %w", err.Error(), relErr)
		}
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT message_uploads_backfill`); err != nil {
		return 0, err
	}
	return len(refs), nil
}
