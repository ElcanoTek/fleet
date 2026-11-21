package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ElcanoTek/fleet/internal/pgtext"
)

// Helping a chat's people (ADR-0117): the note an administrator pins to a
// chat, the overview that says which people need help, and the approval cards
// a chat is waiting on. Like the rest of chat administration these address a
// chat by id alone, so they must only be reached from an admin-only route or
// the root-only CLI — except GetAdminNote and AckAdminNote, which the chat's
// own routes call after their access gate has passed.

// MaxAdminNoteRunes bounds a pinned note.
const MaxAdminNoteRunes = 2000

// Errors returned by the pinned-note methods.
var (
	ErrAdminNoteEmpty   = errors.New("the note's text is required")
	ErrAdminNoteTooLong = errors.New("a pinned note is at most 2000 characters")
	// ErrAdminNoteChanged: the note is not the one the caller read (another
	// administrator pinned, replaced or removed it since).
	ErrAdminNoteChanged = errors.New("the pinned note changed since you opened it; read it again before replacing it")
)

// AdminNote is the note an administrator pinned to a chat. The chat's people
// see it above the conversation; the model never does: it is not part of the
// chat's history (chat.messages), which is all the model's context is built
// from.
type AdminNote struct {
	Text   string `json:"text"`
	Author string `json:"author"`
	Via    string `json:"via"`
	// CreatedAt is when a note was first pinned to the chat; UpdatedAt when
	// this text was written. A reader acknowledges the text it was shown by
	// sending UpdatedAt back.
	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
	// SeenAt and SeenBy record the first person in the chat who pressed
	// "Got it" on this text.
	SeenAt *int64 `json:"seen_at,omitempty"`
	SeenBy string `json:"seen_by,omitempty"`
}

func scanAdminNote(row interface{ Scan(dest ...any) error }) (*AdminNote, error) {
	var n AdminNote
	var seenAt sql.NullInt64
	err := row.Scan(&n.Text, &n.Author, &n.Via, &n.CreatedAt, &n.UpdatedAt, &seenAt, &n.SeenBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if seenAt.Valid {
		n.SeenAt = &seenAt.Int64
	}
	return &n, nil
}

const adminNoteSelect = `SELECT body, author, via, created_at, updated_at, seen_at, seen_by FROM chat.admin_notes WHERE conversation_id = $1`

// GetAdminNote returns the note pinned to a chat, or nil when there is none.
//
// It is read on every load of a chat, so it answers "no note" on a store with
// no database behind it (a test double that embeds a nil *Store) instead of
// failing the whole load.
func (s *Store) GetAdminNote(ctx context.Context, convID string) (*AdminNote, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	return scanAdminNote(s.db.QueryRowContext(ctx, adminNoteSelect, convID))
}

// AdminSetNote pins a note to a chat, replacing any note already there, and
// records it. New text is unread: the previous note's "seen" mark is cleared.
// Setting the text a chat already carries changes nothing.
func (s *Store) AdminSetNote(ctx context.Context, a AdminActor, convID, text string) (AdminNote, error) {
	return s.AdminSetNoteIfUnchanged(ctx, a, convID, text, nil)
}

// AdminSetNoteIfUnchanged is AdminSetNote as a compare-and-swap. expect names
// the note the caller read: its UpdatedAt, or 0 for "no note was pinned". If
// the chat's note is not that one, nothing is written and ErrAdminNoteChanged
// is returned, so one administrator's edit cannot silently replace another's.
// A nil expect pins unconditionally.
func (s *Store) AdminSetNoteIfUnchanged(ctx context.Context, a AdminActor, convID, text string, expect *int64) (AdminNote, error) {
	if err := a.validate(); err != nil {
		return AdminNote{}, err
	}
	t := pgtext.Content(strings.TrimSpace(text))
	if t == "" {
		return AdminNote{}, ErrAdminNoteEmpty
	}
	if utf8.RuneCountInString(t) > MaxAdminNoteRunes {
		return AdminNote{}, ErrAdminNoteTooLong
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AdminNote{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockLiveConversation(ctx, tx, convID); err != nil {
		return AdminNote{}, err
	}
	cur, err := scanAdminNote(tx.QueryRowContext(ctx, adminNoteSelect, convID))
	if err != nil {
		return AdminNote{}, err
	}
	if expect != nil {
		var have int64
		if cur != nil {
			have = cur.UpdatedAt
		}
		if have != *expect {
			return AdminNote{}, ErrAdminNoteChanged
		}
	}
	if cur != nil && cur.Text == t {
		return *cur, nil
	}
	// UpdatedAt names the text a reader acknowledges and an administrator
	// edits against, so no two texts of one chat may ever share it: not the
	// text this replaces, and not a note that was removed earlier in the same
	// second either. A removal is recorded at no earlier than the version it
	// removed (AdminClearNoteIfUnchanged), and that record outlives the note,
	// so the newest note record bounds every version this chat has used.
	now := time.Now().Unix()
	if cur != nil && now <= cur.UpdatedAt {
		now = cur.UpdatedAt + 1
	}
	var lastChange sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT max(created_at) FROM chat.admin_actions
		  WHERE conversation_id = $1 AND action IN ('note_set', 'note_replace', 'note_clear')`,
		convID).Scan(&lastChange); err != nil {
		return AdminNote{}, err
	}
	if lastChange.Valid && now <= lastChange.Int64 {
		now = lastChange.Int64 + 1
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO chat.admin_notes (conversation_id, body, author, via, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $5)
		 ON CONFLICT (conversation_id) DO UPDATE
		   SET body = EXCLUDED.body, author = EXCLUDED.author, via = EXCLUDED.via,
		       updated_at = EXCLUDED.updated_at, seen_at = NULL, seen_by = ''`,
		convID, t, a.Email, a.Via, now); err != nil {
		return AdminNote{}, err
	}
	action := "note_set"
	if cur != nil {
		action = "note_replace"
	}
	if err := recordAdminAction(ctx, tx, a, action, convID, "", "", t); err != nil {
		return AdminNote{}, err
	}
	out, err := scanAdminNote(tx.QueryRowContext(ctx, adminNoteSelect, convID))
	if err != nil {
		return AdminNote{}, err
	}
	if out == nil {
		return AdminNote{}, errors.New("the note was not stored")
	}
	return *out, tx.Commit()
}

// AdminClearNote removes the note pinned to a chat and records it. It reports
// whether there was a note to remove.
func (s *Store) AdminClearNote(ctx context.Context, a AdminActor, convID string) (bool, error) {
	return s.AdminClearNoteIfUnchanged(ctx, a, convID, nil)
}

// AdminClearNoteIfUnchanged is AdminClearNote as a compare-and-swap: with a
// non-nil expect, the note is removed only if it is still the one the caller
// read (its UpdatedAt). Otherwise nothing is removed and ErrAdminNoteChanged
// is returned, so confirming "unpin" on a note another administrator has just
// replaced does not delete the replacement.
func (s *Store) AdminClearNoteIfUnchanged(ctx context.Context, a AdminActor, convID string, expect *int64) (bool, error) {
	if err := a.validate(); err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockLiveConversation(ctx, tx, convID); err != nil {
		return false, err
	}
	if expect != nil {
		cur, err := scanAdminNote(tx.QueryRowContext(ctx, adminNoteSelect, convID))
		if err != nil {
			return false, err
		}
		var have int64
		if cur != nil {
			have = cur.UpdatedAt
		}
		if have != *expect {
			return false, ErrAdminNoteChanged
		}
	}
	var body string
	var removed int64
	err = tx.QueryRowContext(ctx, `DELETE FROM chat.admin_notes WHERE conversation_id = $1 RETURNING body, updated_at`, convID).Scan(&body, &removed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Recorded at no earlier than the removed note's version, which can run a
	// second ahead of the clock (a replacement made in the same second as the
	// text before it). The next note takes a version above this record, so it
	// can never reuse the removed one.
	at := time.Now().Unix()
	if removed > at {
		at = removed
	}
	if err := recordAdminActionAt(ctx, tx, at, a, "note_clear", convID, "", "", body); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// AckAdminNote records that userEmail has read the note pinned to a chat.
// updatedAt names the text they were shown (AdminNote.UpdatedAt): a note
// replaced since then stays unread. Only the first acknowledgement is kept.
// It returns the note as it stands (nil when the chat has none).
//
// The caller has already checked that userEmail may open the chat.
func (s *Store) AckAdminNote(ctx context.Context, convID, userEmail string, updatedAt int64) (*AdminNote, error) {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE chat.admin_notes SET seen_at = $2, seen_by = $3
		  WHERE conversation_id = $1 AND seen_at IS NULL AND updated_at = $4`,
		convID, time.Now().Unix(), strings.ToLower(strings.TrimSpace(userEmail)), updatedAt); err != nil {
		return nil, err
	}
	return s.GetAdminNote(ctx, convID)
}

// ---------------------------------------------------------------------------
// What a chat is waiting on

// AdminPendingCard is an approval card waiting for an answer in a chat. An
// administrator can see that it is waiting and for whom; answering it stays
// with the chat's people (ADR-0115).
type AdminPendingCard struct {
	ID        string `json:"id"`
	Tool      string `json:"tool"`
	MCPServer string `json:"mcp_server,omitempty"`
	// ForEmail is whose turn staged the card.
	ForEmail  string `json:"for_email"`
	CreatedAt int64  `json:"created_at"`
	// ExpiresAt is when the card is denied unanswered (0 = never).
	ExpiresAt int64 `json:"expires_at,omitempty"`
}

// AdminListPendingCards lists a chat's waiting approval cards, oldest first.
func (s *Store) AdminListPendingCards(ctx context.Context, convID string) ([]AdminPendingCard, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, tool_name, COALESCE(mcp_server, ''), user_email, created_at, COALESCE(expires_at, 0)
		   FROM chat.approvals
		  WHERE conversation_id = $1 AND status = 'pending'
		  ORDER BY created_at, id LIMIT 50`, convID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []AdminPendingCard{}
	for rows.Next() {
		var c AdminPendingCard
		if err := rows.Scan(&c.ID, &c.Tool, &c.MCPServer, &c.ForEmail, &c.CreatedAt, &c.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Who needs help

// AdminChatCounts counts chats by the states a listing filters on.
type AdminChatCounts struct {
	// Chats is every chat in scope (archived ones only when the scope
	// includes them; never the recycle bin).
	Chats     int `json:"chats"`
	Attention int `json:"attention"`
	Running   int `json:"running"`
	Patrols   int `json:"patrols"`
	Noted     int `json:"noted"`
	// CardsWaiting is the number of approval cards waiting, across the chats.
	CardsWaiting int `json:"cards_waiting"`
}

// AdminChatPerson is one owner's chats, counted.
type AdminChatPerson struct {
	Email string `json:"email"`
	AdminChatCounts
	// LastActive is when their most recently active chat was last updated.
	LastActive int64 `json:"last_active"`
}

// AdminChatOverview is the workspace's chats counted as a whole and per
// owner: the answer to "who needs help?".
type AdminChatOverview struct {
	AdminChatCounts
	// Archived and Deleted count the chats a default listing leaves out:
	// archived chats and the recycle bin.
	Archived int `json:"archived"`
	Deleted  int `json:"deleted"`
	// People is every owner with a chat in scope: those with chats needing
	// attention first, then the most recently active.
	People []AdminChatPerson `json:"people"`
}

// adminOverviewPeopleLimit bounds the owners an overview lists.
const adminOverviewPeopleLimit = 2000

// AdminChatOverview counts the chats a filter covers, as a whole and per
// owner. It reads the filter's Search, OwnerEmail, ActiveWithinDays and
// IncludeArchived; State, Deleted and Limit are a listing's and are ignored.
func (s *Store) AdminChatOverview(ctx context.Context, f AdminChatFilter) (AdminChatOverview, error) {
	search := ""
	if q := strings.TrimSpace(f.Search); q != "" {
		search = "%" + likeEscape(q) + "%"
	}
	var since int64
	if f.ActiveWithinDays > 0 {
		since = time.Now().Add(-time.Duration(f.ActiveWithinDays) * 24 * time.Hour).Unix()
	}
	const scoped = `
		WITH scoped AS (
		  SELECT lower(c.user_email) AS owner, c.updated_at,
		         c.archived_at IS NOT NULL AS archived, c.deleted_at IS NOT NULL AS deleted,
		         ` + adminChatAttnSQL + ` AS attention,
		         ` + adminChatRunningSQL + ` AS running,
		         ` + adminChatPatrolsSQL + ` AS patrols,
		         ` + adminChatNotedSQL + ` AS noted,
		         (SELECT count(*) FROM chat.approvals a WHERE a.conversation_id = c.id AND a.status = 'pending') AS cards
		    FROM chat.conversations c
		   WHERE ($1 = '' OR lower(c.user_email) = lower($1))
		     AND ($2 = '' OR c.title ILIKE $2 OR c.user_email ILIKE $2 OR c.id LIKE $2)
		     AND ($3 = 0 OR c.updated_at >= $3)
		), shown AS (
		  SELECT * FROM scoped WHERE NOT deleted AND ($4 OR NOT archived)
		)`
	args := []any{strings.TrimSpace(f.OwnerEmail), search, since, f.IncludeArchived}
	var o AdminChatOverview
	err := s.db.QueryRowContext(ctx, scoped+`
		SELECT (SELECT count(*) FROM shown),
		       (SELECT count(*) FROM shown WHERE attention),
		       (SELECT count(*) FROM shown WHERE running),
		       (SELECT count(*) FROM shown WHERE patrols),
		       (SELECT count(*) FROM shown WHERE noted),
		       (SELECT COALESCE(sum(cards), 0) FROM shown),
		       (SELECT count(*) FROM scoped WHERE archived AND NOT deleted),
		       (SELECT count(*) FROM scoped WHERE deleted)`, args...).
		Scan(&o.Chats, &o.Attention, &o.Running, &o.Patrols, &o.Noted, &o.CardsWaiting, &o.Archived, &o.Deleted)
	if err != nil {
		return AdminChatOverview{}, err
	}
	rows, err := s.db.QueryContext(ctx, scoped+`
		SELECT owner, count(*), count(*) FILTER (WHERE attention), count(*) FILTER (WHERE running),
		       count(*) FILTER (WHERE patrols), count(*) FILTER (WHERE noted), COALESCE(sum(cards), 0), max(updated_at)
		  FROM shown
		 GROUP BY owner
		 ORDER BY count(*) FILTER (WHERE attention) DESC, max(updated_at) DESC, owner
		 LIMIT $5`, append(args, adminOverviewPeopleLimit)...)
	if err != nil {
		return AdminChatOverview{}, err
	}
	defer func() { _ = rows.Close() }()
	o.People = []AdminChatPerson{}
	for rows.Next() {
		var p AdminChatPerson
		if err := rows.Scan(&p.Email, &p.Chats, &p.Attention, &p.Running, &p.Patrols, &p.Noted, &p.CardsWaiting, &p.LastActive); err != nil {
			return AdminChatOverview{}, err
		}
		o.People = append(o.People, p)
	}
	return o, rows.Err()
}
