package store

// Each person's Home chat (docs/HOME-CHAT.md, ADR-0113): the one chat the
// rail's Home item opens and the chat a fresh visit lands on. It may be a
// chat they own, a Multiplayer chat they take part in, or a chat a teammate
// shared with their team, so it is a per-person row and not a flag on the
// conversation (a conversation's pin belongs to its owner alone).
//
// Whether the person can still open their Home is decided on every read and
// never stored: a share can be revoked, a Multiplayer invite withdrawn, a
// chat archived, and each of those must hide Home rather than hand over a chat
// the person may no longer read. The row survives the refusal, so access
// coming back (a re-share) brings Home back with it.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// How a person reaches their Home chat, which decides how the client opens
// it: their own chat and a Multiplayer chat open as a normal chat; a chat
// shared with their team opens in the read-only team viewer.
const (
	HomeAccessOwner       = "owner"
	HomeAccessMultiplayer = "multiplayer"
	HomeAccessTeamView    = "team_view"
)

// HomeConversation is a person's Home chat as they can reach it right now.
type HomeConversation struct {
	ConversationID string `json:"conversation_id"`
	// Access is one of the HomeAccess* constants.
	Access string `json:"access"`
	Title  string `json:"title"`
	// OwnerEmail names whose chat it is; it is the caller's own email for
	// HomeAccessOwner.
	OwnerEmail string `json:"owner_email"`
}

// HomeConversation returns the caller's Home chat, or nil when they have none
// or can no longer open the one they chose.
func (s *Store) HomeConversation(ctx context.Context, userEmail string) (*HomeConversation, error) {
	userEmail = normalizeEmail(userEmail)
	var convID string
	err := s.db.QueryRowContext(ctx,
		`SELECT conversation_id FROM user_home_conversation WHERE user_email = $1`, userEmail,
	).Scan(&convID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read home chat: %w", err)
	}
	return s.homeAccess(ctx, userEmail, convID)
}

// SetHomeConversation makes convID the caller's Home, replacing any earlier
// one, and returns it as the caller can reach it. A chat the caller cannot
// open is ErrConversationNotFound, the same answer every other read gives,
// so this is no way to probe for chats.
//
// The caller's own chat is pinned in the same transaction. Pinned is what
// exempts a chat from the retention sweep, the unpinned cap and "Delete
// temporary chats", so pinning keeps every one of those paths from deleting
// Home without each having to know about it; unpinning it (SetPinned,
// BulkPatch, SetArchived) clears it as Home for the same reason. A chat filed
// in a project is pinned too: the project exempts it today, but every path
// that unfiles a chat (a move, a revoked share, a deleted project) would
// otherwise leave Home unprotected, and filing keeps a Home's pin
// (setConversationProject). A pin on a filed chat shows nowhere in the rail.
//
// The Home row is written before the chat is pinned. Its foreign key locks
// the caller's users row, and the Multiplayer mutation locks that row before
// the conversation; taking them in the same order keeps the two from
// deadlocking.
func (s *Store) SetHomeConversation(ctx context.Context, userEmail, convID string) (*HomeConversation, error) {
	userEmail = normalizeEmail(userEmail)
	home, err := s.homeAccess(ctx, userEmail, convID)
	if err != nil {
		return nil, err
	}
	if home == nil {
		return nil, ErrConversationNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO user_home_conversation (user_email, conversation_id, updated_at)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (user_email) DO UPDATE
		   SET conversation_id = EXCLUDED.conversation_id, updated_at = EXCLUDED.updated_at`,
		userEmail, convID, now,
	); err != nil {
		// The chat was hard-deleted since homeAccess read it.
		if pgForeignKeyViolation(err) {
			return nil, ErrConversationNotFound
		}
		return nil, fmt.Errorf("save home chat: %w", err)
	}
	if home.Access == HomeAccessOwner {
		// Re-checks what homeAccess saw, so a chat archived or deleted in
		// between is refused (rolling the Home row back) rather than pinned.
		res, err := tx.ExecContext(ctx,
			`UPDATE conversations SET pinned = TRUE, updated_at = $1
			 WHERE id = $2 AND user_email = $3 AND deleted_at IS NULL AND archived_at IS NULL`,
			now, convID, userEmail,
		)
		if err != nil {
			return nil, fmt.Errorf("pin home chat: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, ErrConversationNotFound
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return home, nil
}

// ClearHomeConversation removes the caller's Home. Having none is not an
// error. The chat itself is left as it is, pin included.
func (s *Store) ClearHomeConversation(ctx context.Context, userEmail string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM user_home_conversation WHERE user_email = $1`, normalizeEmail(userEmail),
	); err != nil {
		return fmt.Errorf("clear home chat: %w", err)
	}
	return nil
}

// homeAccess reports how userEmail can open convID now, in the order the
// client prefers: their own chat, then a Multiplayer chat they take part in,
// then a chat shared with their team. nil means none of them. Each branch is
// the gate that chat's own read path uses, so Home never opens a chat the
// person could not open from the rail.
func (s *Store) homeAccess(ctx context.Context, userEmail, convID string) (*HomeConversation, error) {
	if userEmail == "" || convID == "" {
		return nil, nil
	}
	var title string
	err := s.db.QueryRowContext(ctx,
		`SELECT title FROM conversations
		 WHERE id = $1 AND user_email = $2 AND deleted_at IS NULL AND archived_at IS NULL`,
		convID, userEmail,
	).Scan(&title)
	if err == nil {
		return &HomeConversation{ConversationID: convID, Access: HomeAccessOwner, Title: title, OwnerEmail: userEmail}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("check home chat: %w", err)
	}
	mp, err := s.GetMultiplayerConversation(ctx, userEmail, convID)
	if err != nil {
		return nil, err
	}
	if mp != nil {
		return &HomeConversation{ConversationID: convID, Access: HomeAccessMultiplayer, Title: mp.Title, OwnerEmail: normalizeEmail(mp.UserEmail)}, nil
	}
	tv, err := s.GetTeamVisibleConversationMeta(ctx, userEmail, convID)
	if err != nil {
		return nil, err
	}
	if tv != nil {
		return &HomeConversation{ConversationID: convID, Access: HomeAccessTeamView, Title: tv.Title, OwnerEmail: normalizeEmail(tv.OwnerEmail)}, nil
	}
	return nil, nil
}

// clearOwnHomeTx removes ownerEmail's Home when it is one of convIDs: the
// paths that unpin a chat call it, since an owned Home must stay pinned (see
// SetHomeConversation). Other people who chose the same chat keep it; their
// Home is not protected by the owner's pin and never was.
func clearOwnHomeTx(ctx context.Context, q execer, ownerEmail string, convIDs []string) error {
	if len(convIDs) == 0 {
		return nil
	}
	if _, err := q.ExecContext(ctx,
		`DELETE FROM user_home_conversation WHERE user_email = $1 AND conversation_id = ANY($2)`,
		normalizeEmail(ownerEmail), convIDs,
	); err != nil {
		return fmt.Errorf("clear home chat: %w", err)
	}
	return nil
}

// execer is what clearOwnHomeTx needs from *sql.DB or *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// pgForeignKeyViolation reports whether err wraps a Postgres foreign-key
// violation (SQLSTATE 23503).
func pgForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}
