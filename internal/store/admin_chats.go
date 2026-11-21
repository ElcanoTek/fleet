package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/pgtext"
)

// Chat administration (ADR-0115): what an administrator can see and do in
// any chat on the box, from `fleet chats` on the host or Settings -> Chats in
// the browser. Everything here addresses a chat by id alone (the owner-scoped
// methods elsewhere take the caller's email), so it must only ever be reached
// from an admin-only route or the root-only CLI. Every change is recorded in
// chat.admin_actions and announced in the chat with a UI-only notice.

// Who did an admin change, and through which surface.
const (
	AdminViaCLI = "cli"
	AdminViaWeb = "web"
)

// Admin request kinds and states (chat.admin_requests).
const (
	AdminRequestMessage  = "message"
	AdminRequestStop     = "stop"
	AdminRequestTransfer = "transfer"
	// AdminRequestArchive archives (payload "archive") or restores
	// (payload "unarchive") a chat through the owner's archive path, which
	// also revokes a shared chat's participants.
	AdminRequestArchive = "archive"
	// AdminRequestDelete moves a chat to the recycle bin; AdminRequestRestore
	// takes it back out; AdminRequestPurge deletes it permanently at once.
	AdminRequestDelete  = "delete"
	AdminRequestRestore = "restore"
	AdminRequestPurge   = "purge"

	AdminRequestPending = "pending"
	// AdminRequestRunning is a stop, transfer or archive a server process
	// has claimed and is carrying out, so no other process carries it out too.
	AdminRequestRunning = "running"
	AdminRequestDone    = "done"
	AdminRequestFailed  = "failed"
)

// NoticeAdminUpdated is the UI-only notice kind written into a chat when an
// administrator changes it.
const NoticeAdminUpdated = "admin_updated"

// AdminMessageClientInputPrefix prefixes the client_input_id of the queue row
// an admin message becomes; the rest is the admin request id, so a request
// delivered twice finds its own row.
const AdminMessageClientInputPrefix = "admin-message:"

// AdminRecycleBinDays is how long a deleted chat stays in the recycle bin:
// the tombstone purge in SweepExpired removes it this long after deletion.
const AdminRecycleBinDays = 30

// MaxAdminMessageBytes bounds a message an administrator sends into a chat.
const MaxAdminMessageBytes = 32 * 1024

// MaxAdminReasonRunes bounds the reason recorded with an admin change.
const MaxAdminReasonRunes = 300

// Errors returned by the admin chat methods.
var (
	ErrAdminReasonRequired = errors.New("a reason is required: it is recorded and shown in the chat")
	ErrAdminChatAmbiguous  = errors.New("chat id prefix matches more than one chat")
	ErrAdminChatNotAllowed = errors.New("not allowed on this chat")
)

// AdminActor is the administrator making a change.
type AdminActor struct {
	Email string
	Via   string
}

func (a AdminActor) validate() error {
	if strings.TrimSpace(a.Email) == "" {
		return errors.New("admin actor email is required")
	}
	if a.Via != AdminViaCLI && a.Via != AdminViaWeb {
		return fmt.Errorf("admin actor via must be %q or %q", AdminViaCLI, AdminViaWeb)
	}
	return nil
}

// cleanAdminReason trims a reason to one line and MaxAdminReasonRunes.
func cleanAdminReason(reason string) (string, error) {
	r := strings.Join(strings.Fields(reason), " ")
	if r == "" {
		return "", ErrAdminReasonRequired
	}
	if utf8.RuneCountInString(r) > MaxAdminReasonRunes {
		rs := []rune(r)
		r = string(rs[:MaxAdminReasonRunes-1]) + "…"
	}
	return pgtext.Content(r), nil
}

// AdminAction is one recorded admin change.
type AdminAction struct {
	ID             int64  `json:"id"`
	CreatedAt      int64  `json:"created_at"`
	Actor          string `json:"actor"`
	Via            string `json:"via"`
	Action         string `json:"action"`
	ConversationID string `json:"conversation_id,omitempty"`
	ScheduleID     string `json:"schedule_id,omitempty"`
	Reason         string `json:"reason,omitempty"`
	Detail         string `json:"detail,omitempty"`
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func recordAdminAction(ctx context.Context, db execer, a AdminActor, action, convID, schedID, reason, detail string) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO chat.admin_actions (created_at, actor, via, action, conversation_id, schedule_id, reason, detail)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		time.Now().Unix(), strings.ToLower(strings.TrimSpace(a.Email)), a.Via, action, convID, schedID,
		pgtext.Content(reason), pgtext.Content(detail))
	return err
}

// ListAdminActions returns recorded admin changes, newest first; convID ""
// lists every chat's.
func (s *Store) ListAdminActions(ctx context.Context, convID string, limit int) ([]AdminAction, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, created_at, actor, via, action, conversation_id, schedule_id, reason, detail
		   FROM chat.admin_actions
		  WHERE ($1 = '' OR conversation_id = $1)
		  ORDER BY created_at DESC, id DESC LIMIT $2`, convID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []AdminAction{}
	for rows.Next() {
		var a AdminAction
		if err := rows.Scan(&a.ID, &a.CreatedAt, &a.Actor, &a.Via, &a.Action, &a.ConversationID, &a.ScheduleID, &a.Reason, &a.Detail); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// adminNoticeText is what a chat's people see when an administrator changes
// the chat.
func adminNoticeText(what, reason string) string {
	return fmt.Sprintf("An administrator %s. Reason: %s", what, reason)
}

// ---------------------------------------------------------------------------
// Finding chats

// AdminChat is a chat as an administrator sees it in a listing.
type AdminChat struct {
	ID               string   `json:"id"`
	Title            string   `json:"title"`
	OwnerEmail       string   `json:"owner_email"`
	Model            string   `json:"model"`
	ApprovalMode     string   `json:"approval_mode"`
	Persona          string   `json:"persona"`
	ProjectID        string   `json:"project_id,omitempty"`
	Lockdown         bool     `json:"lockdown"`
	Archived         bool     `json:"archived"`
	Multiplayer      bool     `json:"multiplayer"`
	Participants     []string `json:"participants,omitempty"`
	TeamShared       bool     `json:"team_shared"`
	CreatedAt        int64    `json:"created_at"`
	UpdatedAt        int64    `json:"updated_at"`
	ActivePatrols    int      `json:"active_patrols"`
	PendingApprovals int      `json:"pending_approvals"`
	QueuedInputs     int      `json:"queued_inputs"`
	TurnRunning      bool     `json:"turn_running"`
	// WaitingSince is when the oldest approval card still waiting in the chat
	// was staged (0 = no card is waiting).
	WaitingSince int64 `json:"waiting_since,omitempty"`
	// LastTurnStatus is how the chat's latest turn stands: running, completed,
	// cancelled or error ("" for a chat that has never run a turn).
	LastTurnStatus string `json:"last_turn_status,omitempty"`
	// HasNote reports an administrator's note is pinned to the chat
	// (ADR-0117); NoteSeen that someone in the chat has pressed "Got it".
	HasNote  bool `json:"has_note"`
	NoteSeen bool `json:"note_seen,omitempty"`
	// Attention lists why the chat needs a person's attention (the
	// AdminAttention* reasons); empty when it does not.
	Attention []string `json:"attention,omitempty"`
	// DeletedAt and PurgesAt are set on a chat in the recycle bin: when it was
	// deleted, and when the tombstone purge removes it for good.
	DeletedAt *int64 `json:"deleted_at,omitempty"`
	PurgesAt  *int64 `json:"purges_at,omitempty"`
}

// Why a chat needs attention (AdminChat.Attention). The list is deliberately
// short: each reason is something the chat's owner is stuck behind and an
// administrator can see without reading the transcript.
const (
	// AdminAttentionCardsWaiting: an approval card is waiting for an answer.
	// Only the chat's people can answer it (ADR-0115).
	AdminAttentionCardsWaiting = "cards_waiting"
	// AdminAttentionTurnFailed: the chat's latest turn ended in an error.
	AdminAttentionTurnFailed = "turn_failed"
)

// The states AdminChatFilter.State narrows a listing to.
const (
	AdminChatStateAttention = "attention"
	AdminChatStateRunning   = "running"
	AdminChatStatePatrols   = "patrols"
	AdminChatStateNoted     = "noted"
	AdminChatStateArchived  = "archived"
)

// ErrAdminChatState is an AdminChatFilter.State this server does not know.
var ErrAdminChatState = errors.New("state must be attention, running, patrols, noted or archived")

// ValidAdminChatState reports whether state is one AdminListChats accepts
// ("" lists every state).
func ValidAdminChatState(state string) bool {
	switch state {
	case "", AdminChatStateAttention, AdminChatStateRunning, AdminChatStatePatrols, AdminChatStateNoted, AdminChatStateArchived:
		return true
	}
	return false
}

// AdminChatFilter narrows AdminListChats.
type AdminChatFilter struct {
	// OwnerEmail matches the owner exactly (case-insensitive).
	OwnerEmail string
	// Search matches the title, the owner's email or the id
	// (case-insensitive substring).
	Search string
	// State keeps only chats in one state (the AdminChatState* values): those
	// needing attention, running a turn, with an active patrol, with a pinned
	// note, or archived. "" lists every state.
	State string
	// ActiveWithinDays keeps chats updated in the last N days (0 = any).
	ActiveWithinDays int
	// IncludeArchived also lists archived chats.
	IncludeArchived bool
	// Deleted lists the recycle bin (deleted chats not yet purged) instead.
	Deleted bool
	Limit   int
}

const adminChatSelect = `
	SELECT c.id, c.title, c.user_email, COALESCE(c.model, ''), COALESCE(NULLIF(c.approval_mode, ''), 'manual'),
	       COALESCE(c.persona, ''), COALESCE(c.project_id, ''), c.lockdown, c.archived_at IS NOT NULL,
	       c.multiplayer, COALESCE(c.multiplayer_participants, '[]'), COALESCE(c.team_shared_with, '') <> '' OR c.team_visible,
	       c.created_at, c.updated_at,
	       (SELECT count(*) FROM sched.chat_schedules s WHERE s.conversation_id = c.id AND s.status = 'active' AND s.cron_expr <> ''),
	       (SELECT count(*) FROM chat.approvals a WHERE a.conversation_id = c.id AND a.status = 'pending'),
	       (SELECT count(*) FROM chat.chat_input_queue q WHERE q.conversation_id = c.id AND q.state = 'queued' AND q.mode <> 'direct'),
	       EXISTS (SELECT 1 FROM chat.turns t WHERE t.conversation_id = c.id AND t.status = 'running'),
	       EXTRACT(EPOCH FROM c.deleted_at)::bigint,
	       COALESCE((SELECT min(a.created_at) FROM chat.approvals a WHERE a.conversation_id = c.id AND a.status = 'pending'), 0),
	       ` + adminChatLastTurnSQL + `,
	       (SELECT n.seen_at IS NOT NULL FROM chat.admin_notes n WHERE n.conversation_id = c.id)
	  FROM chat.conversations c`

// The SQL behind the states a listing filters and counts on. Each reads the
// chat as "c"; the attention rule here and in scanAdminChat must agree.
//
// A chat's latest turn is the newest by started_at and, for two that started
// in the same second, the one with the higher turn_index: CreateTurn assigns
// that serially per chat (one above the highest still stored), which
// started_at (seconds) and turn_id (random) cannot order.
const (
	adminChatLastTurnSQL = `COALESCE((SELECT t.status FROM chat.turns t WHERE t.conversation_id = c.id ORDER BY t.started_at DESC, t.turn_index DESC LIMIT 1), '')`
	adminChatCardsSQL    = `EXISTS (SELECT 1 FROM chat.approvals a WHERE a.conversation_id = c.id AND a.status = 'pending')`
	adminChatAttnSQL     = `(` + adminChatCardsSQL + ` OR ` + adminChatLastTurnSQL + ` = 'error')`
	adminChatRunningSQL  = `EXISTS (SELECT 1 FROM chat.turns t WHERE t.conversation_id = c.id AND t.status = 'running')`
	adminChatPatrolsSQL  = `EXISTS (SELECT 1 FROM sched.chat_schedules s WHERE s.conversation_id = c.id AND s.status = 'active' AND s.cron_expr <> '')`
	adminChatNotedSQL    = `EXISTS (SELECT 1 FROM chat.admin_notes n WHERE n.conversation_id = c.id)`
)

func scanAdminChat(row interface{ Scan(dest ...any) error }) (AdminChat, error) {
	var c AdminChat
	var participants []byte
	var deletedAt sql.NullInt64
	var noteSeen sql.NullBool
	err := row.Scan(&c.ID, &c.Title, &c.OwnerEmail, &c.Model, &c.ApprovalMode,
		&c.Persona, &c.ProjectID, &c.Lockdown, &c.Archived,
		&c.Multiplayer, &participants, &c.TeamShared,
		&c.CreatedAt, &c.UpdatedAt,
		&c.ActivePatrols, &c.PendingApprovals, &c.QueuedInputs, &c.TurnRunning, &deletedAt,
		&c.WaitingSince, &c.LastTurnStatus, &noteSeen)
	if err == nil {
		c.HasNote, c.NoteSeen = noteSeen.Valid, noteSeen.Valid && noteSeen.Bool
		if c.PendingApprovals > 0 {
			c.Attention = append(c.Attention, AdminAttentionCardsWaiting)
		}
		if c.LastTurnStatus == "error" {
			c.Attention = append(c.Attention, AdminAttentionTurnFailed)
		}
	}
	if err == nil && deletedAt.Valid {
		d := deletedAt.Int64
		purge := d + int64(AdminRecycleBinDays*24*3600)
		c.DeletedAt, c.PurgesAt = &d, &purge
	}
	if err == nil && len(participants) > 0 {
		_ = json.Unmarshal(participants, &c.Participants)
	}
	return c, err
}

// AdminListChats lists chats across every user, most recently active first.
// Deleted chats are never listed.
func (s *Store) AdminListChats(ctx context.Context, f AdminChatFilter) ([]AdminChat, error) {
	search := ""
	if q := strings.TrimSpace(f.Search); q != "" {
		search = "%" + likeEscape(q) + "%"
	}
	var since int64
	if f.ActiveWithinDays > 0 {
		since = time.Now().Add(-time.Duration(f.ActiveWithinDays) * 24 * time.Hour).Unix()
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	if !ValidAdminChatState(f.State) {
		return nil, ErrAdminChatState
	}
	rows, err := s.db.QueryContext(ctx, adminChatSelect+`
		 WHERE (c.deleted_at IS NULL) <> $6
		   AND ($1 OR $6 OR $7 = 'archived' OR c.archived_at IS NULL)
		   AND ($2 = '' OR lower(c.user_email) = lower($2))
		   AND ($3 = '' OR c.title ILIKE $3 OR c.user_email ILIKE $3 OR c.id LIKE $3)
		   AND ($4 = 0 OR c.updated_at >= $4)
		   AND ($7 = ''
		        OR ($7 = 'attention' AND `+adminChatAttnSQL+`)
		        OR ($7 = 'running' AND `+adminChatRunningSQL+`)
		        OR ($7 = 'patrols' AND `+adminChatPatrolsSQL+`)
		        OR ($7 = 'noted' AND `+adminChatNotedSQL+`)
		        OR ($7 = 'archived' AND c.archived_at IS NOT NULL))
		 ORDER BY c.updated_at DESC, c.id
		 LIMIT $5`,
		f.IncludeArchived, strings.TrimSpace(f.OwnerEmail), search, since, limit, f.Deleted, f.State)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []AdminChat{}
	for rows.Next() {
		c, err := scanAdminChat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func likeEscape(p string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(p)
}

// ResolveAdminChatID turns a chat id or a unique prefix (at least
// MinChatScheduleIDPrefix characters) into the full id of a chat that is not
// deleted.
func (s *Store) ResolveAdminChatID(ctx context.Context, idOrPrefix string) (string, error) {
	return s.resolveAdminChatID(ctx, idOrPrefix, false)
}

func (s *Store) resolveAdminChatID(ctx context.Context, idOrPrefix string, includeDeleted bool) (string, error) {
	p := strings.TrimSpace(idOrPrefix)
	if len(p) < MinChatScheduleIDPrefix {
		return "", fmt.Errorf("%w: give the chat id or at least its first %d characters", ErrInvalidChatSchedule, MinChatScheduleIDPrefix)
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM chat.conversations WHERE ($3 OR deleted_at IS NULL) AND (id = $1 OR id LIKE $2) ORDER BY (id = $1) DESC LIMIT 3`,
		p, likeEscape(p)+"%", includeDeleted)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	switch {
	case len(ids) == 0:
		return "", ErrConversationNotFound
	case ids[0] == p:
		return p, nil
	case len(ids) > 1:
		return "", ErrAdminChatAmbiguous
	}
	return ids[0], nil
}

// AdminGetChatAny returns one chat by id or unique prefix, including a chat
// in the recycle bin (DeletedAt set).
func (s *Store) AdminGetChatAny(ctx context.Context, idOrPrefix string) (AdminChat, error) {
	id, err := s.resolveAdminChatID(ctx, idOrPrefix, true)
	if err != nil {
		return AdminChat{}, err
	}
	c, err := scanAdminChat(s.db.QueryRowContext(ctx, adminChatSelect+` WHERE c.id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return AdminChat{}, ErrConversationNotFound
	}
	return c, err
}

// AdminGetChat returns one chat by id or unique prefix.
func (s *Store) AdminGetChat(ctx context.Context, idOrPrefix string) (AdminChat, error) {
	id, err := s.ResolveAdminChatID(ctx, idOrPrefix)
	if err != nil {
		return AdminChat{}, err
	}
	c, err := scanAdminChat(s.db.QueryRowContext(ctx, adminChatSelect+` WHERE c.id = $1 AND c.deleted_at IS NULL`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return AdminChat{}, ErrConversationNotFound
	}
	return c, err
}

// ---------------------------------------------------------------------------
// Reading a chat

// AdminTranscriptEntry is one history entry, flattened for reading.
type AdminTranscriptEntry struct {
	ID        int64  `json:"id"`
	CreatedAt int64  `json:"created_at"`
	Role      string `json:"role"`
	Type      string `json:"type"`
	// Kind marks a user entry fleet wrote (a patrol run, a resume, an admin
	// message) and a notice's kind.
	Kind   string `json:"kind,omitempty"`
	Sender string `json:"sender,omitempty"`
	// Tool is the tool's name on a tool call or result.
	Tool  string `json:"tool,omitempty"`
	Text  string `json:"text"`
	IsErr bool   `json:"is_error,omitempty"`
	// Images names the images attached to a user message, by file name only:
	// the files stay in the owner's workspace, and their paths are not an
	// administrator's to read. A message can be nothing but images.
	Images []string `json:"images,omitempty"`
	// Truncated reports Text was cut to the caller's limit.
	Truncated bool `json:"truncated,omitempty"`
}

// AdminTranscriptOptions shapes AdminChatTranscript.
type AdminTranscriptOptions struct {
	// Tail is how many of the latest entries to return (default 30, max 500).
	Tail int
	// IncludeTools also returns tool calls and results.
	IncludeTools bool
	// MaxTextRunes cuts each entry's text (default 4000; 0 keeps the default).
	MaxTextRunes int
	// BeforeID returns the entries older than this entry id (0 = the latest),
	// so a reader pages back through a long chat.
	BeforeID int64
}

// AdminChatTranscript returns the latest entries of a chat's history in
// order. Reasoning and summary bookkeeping entries are never returned.
func (s *Store) AdminChatTranscript(ctx context.Context, convID string, o AdminTranscriptOptions) ([]AdminTranscriptEntry, error) {
	tail := o.Tail
	if tail <= 0 {
		tail = 30
	}
	if tail > 500 {
		tail = 500
	}
	maxRunes := o.MaxTextRunes
	if maxRunes <= 0 {
		maxRunes = 4000
	}
	// "summary" rows (a compaction) are returned as content-free boundaries:
	// see decodeTranscriptContent.
	types := []string{"text", agent.EntryTypeNotice, "summary"}
	if o.IncludeTools {
		types = append(types, "tool_call", "tool_result")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, created_at, role, type, content FROM (
		   SELECT id, created_at, role, type, content FROM chat.messages
		    WHERE conversation_id = $1 AND type = ANY($2) AND ($4 = 0 OR id < $4)
		    ORDER BY id DESC LIMIT $3) m
		  ORDER BY id`,
		convID, types, tail, o.BeforeID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []AdminTranscriptEntry{}
	for rows.Next() {
		var e AdminTranscriptEntry
		var content []byte
		if err := rows.Scan(&e.ID, &e.CreatedAt, &e.Role, &e.Type, &content); err != nil {
			return nil, err
		}
		decodeTranscriptContent(&e, content)
		if rs := []rune(e.Text); len(rs) > maxRunes {
			e.Text, e.Truncated = string(rs[:maxRunes])+"…", true
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func decodeTranscriptContent(e *AdminTranscriptEntry, content []byte) {
	switch e.Type {
	case "summary":
		// Where the chat was compacted. The owner's chat starts a new message
		// here, so a reader must too, or the replies either side would be
		// joined into one. Only the position is returned, as in the team and
		// share views (agent.EntryTypeSummaryBoundary).
		e.Type, e.Text = agent.EntryTypeSummaryBoundary, ""
	case "text":
		var c agent.TextContent
		if json.Unmarshal(content, &c) == nil {
			e.Text, e.Kind, e.Sender = c.Text, c.Kind, c.SenderEmail
			for _, img := range c.Images {
				name := strings.TrimSpace(img.Name)
				if name == "" {
					name = path.Base(strings.ReplaceAll(img.Path, `\`, "/"))
				}
				if name == "" || name == "." || name == "/" {
					name = "image"
				}
				e.Images = append(e.Images, path.Base(name))
			}
			if c.AdminEmail != "" {
				e.Sender = c.AdminEmail
			}
		}
	case agent.EntryTypeNotice:
		var c agent.NoticeContent
		if json.Unmarshal(content, &c) == nil {
			e.Text, e.Kind = c.Text, c.Kind
		}
	case "tool_call":
		var c agent.ToolCallContent
		if json.Unmarshal(content, &c) == nil {
			e.Tool, e.Text = c.Name, c.Input
		}
	case "tool_result":
		var c agent.ToolResultContent
		if json.Unmarshal(content, &c) == nil {
			e.Tool, e.Text, e.IsErr = c.Name, c.Text, c.IsErr
		}
	}
}

// ---------------------------------------------------------------------------
// Notices and settings

// AdminAppendNotice writes a UI-only notice into a chat (the model never sees
// it) and records it.
func (s *Store) AdminAppendNotice(ctx context.Context, a AdminActor, convID, text string) error {
	if err := a.validate(); err != nil {
		return err
	}
	t := strings.TrimSpace(text)
	if t == "" {
		return errors.New("notice text is required")
	}
	if utf8.RuneCountInString(t) > 1000 {
		return errors.New("a notice is at most 1000 characters")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockLiveConversation(ctx, tx, convID); err != nil {
		return err
	}
	if err := s.appendNoticeTx(ctx, tx, convID, NoticeAdminUpdated, pgtext.Content(t)); err != nil {
		return err
	}
	if err := recordAdminAction(ctx, tx, a, "notice", convID, "", "", t); err != nil {
		return err
	}
	return tx.Commit()
}

// lockLiveConversation locks a chat that exists and is not deleted.
func (s *Store) lockLiveConversation(ctx context.Context, tx *sql.Tx, convID string) error {
	var id string
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM chat.conversations WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, convID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConversationNotFound
	}
	return err
}

// AdminChatChange is a set of admin edits to a chat's settings; nil fields
// are left alone.
type AdminChatChange struct {
	Title *string
	Model *string
	// ApprovalMode may only be manual: an administrator can take a chat's
	// automatic approvals away, never grant them. Auto-approve and Autonomous
	// let actions run without a card and are the owner's own decision.
	ApprovalMode *string
}

// Archiving is not an AdminChatChange: it must also revoke a shared chat's
// participants, which only the running server can do, so it is an
// AdminRequestArchive.

func (c AdminChatChange) empty() bool {
	return c.Title == nil && c.Model == nil && c.ApprovalMode == nil
}

// AdminUpdateChat applies an admin change to a chat's title, model or
// approval mode, records it, and writes a notice into the chat. It
// returns the chat as it is afterwards.
func (s *Store) AdminUpdateChat(ctx context.Context, a AdminActor, convID string, c AdminChatChange, reason string) (AdminChat, error) {
	if err := a.validate(); err != nil {
		return AdminChat{}, err
	}
	why, err := cleanAdminReason(reason)
	if err != nil {
		return AdminChat{}, err
	}
	if c.empty() {
		return AdminChat{}, errors.New("nothing to change")
	}
	cur, err := s.AdminGetChat(ctx, convID)
	if err != nil {
		return AdminChat{}, err
	}
	owner := cur.OwnerEmail

	// Validate the whole change before writing any of it, so a request with
	// one bad field changes nothing.
	type step struct {
		what  string
		apply func() error
	}
	var steps []step
	if c.Title != nil {
		t := strings.TrimSpace(*c.Title)
		if t == "" {
			return AdminChat{}, errors.New("title cannot be empty")
		}
		if t != cur.Title {
			// The manual rename path: it renames a title the owner locked,
			// and locks the new one against the auto-titler.
			steps = append(steps, step{fmt.Sprintf("renamed the chat to %q", t), func() error { return s.RenameTitle(ctx, owner, cur.ID, t) }})
		}
	}
	if c.Model != nil {
		m := strings.TrimSpace(*c.Model)
		switch {
		case m == "":
			return AdminChat{}, errors.New("model cannot be empty")
		case len(m) > 200 || strings.ContainsAny(m, " \t\r\n"):
			return AdminChat{}, fmt.Errorf("%q is not a model slug", m)
		case cur.Lockdown && m != cur.Model:
			return AdminChat{}, fmt.Errorf("%w: a Lockdown chat keeps its model", ErrAdminChatNotAllowed)
		}
		if m != cur.Model {
			steps = append(steps, step{"switched the chat's model to " + m, func() error { return s.SetModel(ctx, owner, cur.ID, m) }})
		}
	}
	if c.ApprovalMode != nil {
		m := strings.ToLower(strings.TrimSpace(*c.ApprovalMode))
		if m != ConversationApprovalManual {
			return AdminChat{}, fmt.Errorf("%w: an administrator can only turn a chat's approvals down to manual; Auto-approve and Autonomous are the owner's own decision", ErrAdminChatNotAllowed)
		}
		if m != cur.ApprovalMode {
			steps = append(steps, step{"set the chat's approvals to " + m, func() error { return s.SetApprovalMode(ctx, owner, cur.ID, m) }})
		}
	}
	if len(steps) == 0 {
		return cur, nil
	}

	// Apply. Each setting is the owner's own single-statement setter; if one
	// fails part-way (the chat was deleted meanwhile, the database dropped),
	// what did change is still recorded and announced, and the error says
	// the change was partial.
	var did []string
	var applyErr error
	for _, st := range steps {
		if err := st.apply(); err != nil {
			applyErr = err
			break
		}
		did = append(did, st.what)
	}
	if len(did) > 0 {
		what := strings.Join(did, "; ")
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return AdminChat{}, err
		}
		defer func() { _ = tx.Rollback() }()
		if err := s.appendNoticeTx(ctx, tx, cur.ID, NoticeAdminUpdated, adminNoticeText(what, why)); err != nil {
			return AdminChat{}, err
		}
		if err := recordAdminAction(ctx, tx, a, "update", cur.ID, "", why, what); err != nil {
			return AdminChat{}, err
		}
		if err := tx.Commit(); err != nil {
			return AdminChat{}, err
		}
	}
	if applyErr != nil {
		if len(did) > 0 {
			return AdminChat{}, fmt.Errorf("partly applied (%s), then: %w", strings.Join(did, "; "), applyErr)
		}
		return AdminChat{}, applyErr
	}
	return s.AdminGetChat(ctx, cur.ID)
}

// ---------------------------------------------------------------------------
// Requests the running server carries out

// AdminRequest is one queued admin request (chat.admin_requests).
type AdminRequest struct {
	ID             string `json:"id"`
	CreatedAt      int64  `json:"created_at"`
	UpdatedAt      int64  `json:"updated_at"`
	Actor          string `json:"actor"`
	Via            string `json:"via"`
	Kind           string `json:"kind"`
	ConversationID string `json:"conversation_id"`
	Payload        string `json:"payload,omitempty"`
	Reason         string `json:"reason,omitempty"`
	State          string `json:"state"`
	Result         string `json:"result,omitempty"`
	// InputID is the queue row a message became.
	InputID string `json:"input_id,omitempty"`
	// InputState and TurnID follow that row once it exists.
	InputState string `json:"input_state,omitempty"`
	TurnID     string `json:"turn_id,omitempty"`
}

// AdminRequestInput is a new admin request.
type AdminRequestInput struct {
	Actor          AdminActor
	Kind           string
	ConversationID string
	// Payload is the message text (message) or the new owner's email
	// (transfer); empty for stop.
	Payload string
	Reason  string
}

// CreateAdminRequest validates and records a request for the server to carry
// out. The chat must exist; a message also needs a chat that is not archived
// or Lockdown.
func (s *Store) CreateAdminRequest(ctx context.Context, in AdminRequestInput) (AdminRequest, error) {
	if err := in.Actor.validate(); err != nil {
		return AdminRequest{}, err
	}
	why, err := cleanAdminReason(in.Reason)
	if err != nil {
		return AdminRequest{}, err
	}
	payload := strings.TrimSpace(in.Payload)
	switch in.Kind {
	case AdminRequestMessage:
		if payload == "" {
			return AdminRequest{}, errors.New("the message is empty")
		}
		if len(payload) > MaxAdminMessageBytes {
			return AdminRequest{}, fmt.Errorf("the message is %d bytes; the limit is %d", len(payload), MaxAdminMessageBytes)
		}
	case AdminRequestTransfer:
		if payload == "" || !strings.Contains(payload, "@") {
			return AdminRequest{}, errors.New("the new owner's email is required")
		}
		payload = strings.ToLower(payload)
	case AdminRequestStop:
		payload = ""
	case AdminRequestArchive:
		if payload != "archive" && payload != "unarchive" {
			return AdminRequest{}, errors.New(`an archive request's payload is "archive" or "unarchive"`)
		}
	case AdminRequestDelete, AdminRequestRestore, AdminRequestPurge:
		payload = ""
	default:
		return AdminRequest{}, fmt.Errorf("unknown admin request kind %q", in.Kind)
	}
	var chat AdminChat
	switch in.Kind {
	case AdminRequestRestore, AdminRequestPurge:
		// These act on a chat in the recycle bin (a purge also on a live one).
		chat, err = s.AdminGetChatAny(ctx, in.ConversationID)
	default:
		chat, err = s.AdminGetChat(ctx, in.ConversationID)
	}
	if err != nil {
		return AdminRequest{}, err
	}
	if in.Kind == AdminRequestRestore && chat.DeletedAt == nil {
		return AdminRequest{}, fmt.Errorf("%w: this chat is not in the recycle bin", ErrAdminChatNotAllowed)
	}
	if in.Kind == AdminRequestMessage && (chat.Archived || chat.Lockdown) {
		return AdminRequest{}, fmt.Errorf("%w: an archived or Lockdown chat takes no admin messages", ErrAdminChatNotAllowed)
	}
	now := time.Now().Unix()
	r := AdminRequest{
		ID: uuid.NewString(), CreatedAt: now, UpdatedAt: now,
		Actor: strings.ToLower(strings.TrimSpace(in.Actor.Email)), Via: in.Actor.Via,
		Kind: in.Kind, ConversationID: chat.ID, Payload: pgtext.Content(payload), Reason: why,
		State: AdminRequestPending,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AdminRequest{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO chat.admin_requests (id, created_at, updated_at, actor, via, kind, conversation_id, payload, reason, state)
		 VALUES ($1, $2, $2, $3, $4, $5, $6, $7, $8, 'pending')`,
		r.ID, now, r.Actor, r.Via, r.Kind, r.ConversationID, r.Payload, r.Reason); err != nil {
		return AdminRequest{}, err
	}
	detail := r.ID
	if in.Kind == AdminRequestTransfer {
		detail = r.ID + " to " + payload
	}
	if err := recordAdminAction(ctx, tx, in.Actor, "request_"+in.Kind, r.ConversationID, "", why, detail); err != nil {
		return AdminRequest{}, err
	}
	if err := tx.Commit(); err != nil {
		return AdminRequest{}, err
	}
	return r, nil
}

const adminRequestSelect = `
	SELECT r.id, r.created_at, r.updated_at, r.actor, r.via, r.kind, r.conversation_id, r.payload, r.reason,
	       r.state, r.result, r.input_id, COALESCE(q.state, ''), COALESCE(q.turn_id, '')
	  FROM chat.admin_requests r
	  LEFT JOIN chat.chat_input_queue q ON q.id = NULLIF(r.input_id, '')`

func scanAdminRequest(row interface{ Scan(dest ...any) error }) (AdminRequest, error) {
	var r AdminRequest
	err := row.Scan(&r.ID, &r.CreatedAt, &r.UpdatedAt, &r.Actor, &r.Via, &r.Kind, &r.ConversationID, &r.Payload, &r.Reason,
		&r.State, &r.Result, &r.InputID, &r.InputState, &r.TurnID)
	if strings.HasPrefix(r.TurnID, ClaimTurnPrefix) {
		r.TurnID = ""
	}
	return r, err
}

// GetAdminRequest returns one admin request with its queue row's progress.
func (s *Store) GetAdminRequest(ctx context.Context, id string) (AdminRequest, error) {
	r, err := scanAdminRequest(s.db.QueryRowContext(ctx, adminRequestSelect+` WHERE r.id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return AdminRequest{}, errors.New("admin request not found")
	}
	return r, err
}

// ListPendingAdminRequests returns pending requests, oldest first.
func (s *Store) ListPendingAdminRequests(ctx context.Context, limit int) ([]AdminRequest, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, adminRequestSelect+` WHERE r.state = 'pending' ORDER BY r.created_at, r.id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AdminRequest
	for rows.Next() {
		r, err := scanAdminRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ClaimAdminRequest marks a pending request running for the caller, so a stop,
// transfer or archive is carried out by exactly one server process (the
// browser path and the scheduler sweep may both reach it). It reports false
// when the request was no longer pending.
func (s *Store) ClaimAdminRequest(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE chat.admin_requests SET state = 'running', updated_at = $2 WHERE id = $1 AND state = 'pending'`,
		id, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// FailStaleAdminRequests settles requests a server process claimed and never
// finished (it stopped part-way) as failed, rather than carrying a transfer or
// archive out a second time blind, and expires pending requests older than
// AdminRequestPendingTTL. It returns how many it settled.
func (s *Store) FailStaleAdminRequests(ctx context.Context, olderThan time.Duration) (int, error) {
	now := time.Now()
	res, err := s.db.ExecContext(ctx,
		`UPDATE chat.admin_requests
		    SET state = 'failed', result = 'interrupted: the server stopped while carrying it out; check the chat and ask again if needed', updated_at = $1
		  WHERE state = 'running' AND updated_at < $2`,
		now.Unix(), now.Add(-olderThan).Unix())
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	// A request nobody picked up in time (the server was down or draining
	// for an update) expires: it must not act hours later on a chat whose
	// state has moved on.
	res, err = s.db.ExecContext(ctx,
		`UPDATE chat.admin_requests
		    SET state = 'failed', result = 'expired: the server did not pick it up in time; ask again', updated_at = $1
		  WHERE state = 'pending' AND created_at < $2`,
		now.Unix(), now.Add(-AdminRequestPendingTTL).Unix())
	if err != nil {
		return int(n), err
	}
	m, err := res.RowsAffected()
	return int(n + m), err
}

// AdminRequestPendingTTL is how long a request may wait for the server
// before it expires unexecuted. A var so tests can shorten it.
var AdminRequestPendingTTL = 15 * time.Minute

// AppendAdminRequestNotice writes the chat notice for a carried-out admin
// request. The request itself is the audit record (request_<kind>), so this
// adds no second one.
func (s *Store) AppendAdminRequestNotice(ctx context.Context, convID, text string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockLiveConversation(ctx, tx, convID); err != nil {
		return err
	}
	if err := s.appendNoticeTx(ctx, tx, convID, NoticeAdminUpdated, pgtext.Content(text)); err != nil {
		return err
	}
	return tx.Commit()
}

// FinishAdminRequest settles a pending or claimed request. It reports false
// when the request was already settled (another server process settled it
// first).
func (s *Store) FinishAdminRequest(ctx context.Context, id, state, result string) (bool, error) {
	if state != AdminRequestDone && state != AdminRequestFailed {
		return false, fmt.Errorf("invalid admin request state %q", state)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE chat.admin_requests SET state = $2, result = $3, updated_at = $4 WHERE id = $1 AND state IN ('pending', 'running')`,
		id, state, pgtext.Content(result), time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// AdminMessageInputText is the turn input an admin message becomes: the
// administrator's words under a header that says who wrote them, so the
// model never takes them for the owner's.
func AdminMessageInputText(adminEmail, ownerEmail, text string) string {
	return fmt.Sprintf("[Message from an administrator: %s]\n%s\n\n(Sent by a Fleet administrator of this workspace with the admin tools, not by %s, the chat's owner. Treat it as a request from the team that runs this workspace. Any approval card it leads to still goes to %s.)",
		adminEmail, strings.TrimSpace(text), ownerEmail, ownerEmail)
}

// QueueAdminMessage turns a pending message request into a queue row of mode
// 'admin' — the chat's owner as the row's user, the administrator as its
// sender — and marks the request done, in one transaction under the
// request's row lock, so a request is queued exactly once however many
// server processes deliver it. The caller (the running server, which owns
// the acceptance counter) then drains the chat's queue. It returns the row,
// or ok=false when the request was no longer pending.
func (s *Store) QueueAdminMessage(ctx context.Context, requestID string) (row InputQueueRow, ok bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return InputQueueRow{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var r AdminRequest
	err = tx.QueryRowContext(ctx,
		`SELECT id, actor, kind, conversation_id, payload, state FROM chat.admin_requests WHERE id = $1 FOR UPDATE`, requestID).
		Scan(&r.ID, &r.Actor, &r.Kind, &r.ConversationID, &r.Payload, &r.State)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && r.State != AdminRequestPending) {
		return InputQueueRow{}, false, nil
	}
	if err != nil {
		return InputQueueRow{}, false, err
	}
	if r.Kind != AdminRequestMessage {
		return InputQueueRow{}, false, fmt.Errorf("admin request %s is a %s, not a message", r.ID, r.Kind)
	}
	// Read under the chat's row lock (the one insertInputTx takes), so a
	// transfer or archive cannot land between this check and the queue row.
	var owner string
	var archived, lockdown bool
	err = tx.QueryRowContext(ctx,
		`SELECT user_email, archived_at IS NOT NULL, lockdown FROM chat.conversations WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, r.ConversationID).
		Scan(&owner, &archived, &lockdown)
	fail := func(msg string) (InputQueueRow, bool, error) {
		if _, err := tx.ExecContext(ctx,
			`UPDATE chat.admin_requests SET state = 'failed', result = $2, updated_at = $3 WHERE id = $1`,
			r.ID, msg, time.Now().Unix()); err != nil {
			return InputQueueRow{}, false, err
		}
		return InputQueueRow{}, true, tx.Commit()
	}
	if errors.Is(err, sql.ErrNoRows) {
		return fail("the chat no longer exists")
	}
	if err != nil {
		return InputQueueRow{}, false, err
	}
	if archived || lockdown {
		return fail("the chat is archived or Lockdown")
	}
	var pending int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM chat.chat_input_queue WHERE conversation_id = $1 AND state = 'queued' AND mode <> 'direct'`, r.ConversationID).
		Scan(&pending); err != nil {
		return InputQueueRow{}, false, err
	}
	if pending >= MaxAdminQueuedInputs {
		return fail(fmt.Sprintf("the chat's queue is full (%d waiting)", pending))
	}
	row, created, err := s.insertInputTx(ctx, tx, InputQueueRow{
		ID: uuid.NewString(), ConversationID: r.ConversationID, UserEmail: owner,
		ClientInputID: AdminMessageClientInputPrefix + r.ID,
		Message:       AdminMessageInputText(r.Actor, owner, r.Payload),
		Attachments:   "[]", Mode: InputModeAdmin, State: InputStateQueued, SenderEmail: r.Actor,
	})
	if err != nil {
		return InputQueueRow{}, false, err
	}
	_ = created
	if _, err := tx.ExecContext(ctx,
		`UPDATE chat.admin_requests SET state = 'done', input_id = $2, result = 'queued', updated_at = $3 WHERE id = $1`,
		r.ID, row.ID, time.Now().Unix()); err != nil {
		return InputQueueRow{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return InputQueueRow{}, false, err
	}
	return row, true, nil
}

// MaxAdminQueuedInputs is how many inputs may already wait in a chat's queue
// for an admin message to join them (the chat route's own cap).
var MaxAdminQueuedInputs = 20

// AdminMoveChatToBin deletes a chat into the recycle bin, in one transaction
// under the chat's row lock: it pauses every active patrol and timer in the
// chat, whoever owns it (a fire in flight is revoked, their queued turns are
// cancelled), then tombstones the chat (deleted_at = now()). Nothing can be
// created or resumed in between, because both need the same lock. The
// tombstone purge removes it AdminRecycleBinDays later unless it is restored.
// This tombstones whatever FLEET_CONVERSATION_SOFT_DELETE says: an
// administrator's delete can always be undone for that window. Everything
// else (pin, sharing, participants, their paused patrols and pending cards)
// is left as it was, so a restore gives the chat back whole. It returns how
// many schedules it paused.
func (s *Store) AdminMoveChatToBin(ctx context.Context, convID, notice string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockLiveConversation(ctx, tx, convID); err != nil {
		return 0, err
	}
	now := time.Now().Unix()
	res, err := tx.ExecContext(ctx,
		`UPDATE `+chatSchedulesTable+`
		    SET status = 'paused', updated_at = $2,
		        lease_owner = NULL, lease_expires_at = NULL, fire_id = NULL,
		        fire_trigger = NULL, fire_run_number = NULL, fire_failures = 0
		  WHERE conversation_id = $1 AND status = 'active'`, convID, now)
	if err != nil {
		return 0, err
	}
	paused, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx,
		`UPDATE chat_input_queue SET state = 'cancelled', updated_at = $1
		  WHERE conversation_id = $2 AND state = 'queued'`, now, convID); err != nil {
		return 0, err
	}
	// The notice is written in the same transaction, so the chat never says
	// it was moved to the bin unless it was; it is there if it is restored.
	if n := strings.TrimSpace(notice); n != "" {
		if err := s.appendNoticeTx(ctx, tx, convID, NoticeAdminUpdated, pgtext.Content(n)); err != nil {
			return 0, err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE chat.conversations SET deleted_at = NOW(), updated_at = $2 WHERE id = $1 AND deleted_at IS NULL`,
		convID, now); err != nil {
		return 0, err
	}
	return int(paused), tx.Commit()
}

// AdminRestoreChat takes a chat out of the recycle bin. Its patrols stay
// paused (the delete paused them); the owner or an administrator resumes them.
func (s *Store) AdminRestoreChat(ctx context.Context, convID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE chat.conversations SET deleted_at = NULL, updated_at = $2
		  WHERE id = $1 AND deleted_at IS NOT NULL`,
		convID, time.Now().Unix())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: this chat is not in the recycle bin", ErrAdminChatNotAllowed)
	}
	return nil
}

// AdminPurgeChat deletes a chat permanently, live or in the recycle bin: the
// row goes, and its messages, approvals, queue rows, schedules and files'
// rows with it (foreign-key cascade). Its workspace directory is left for
// SweepOrphanWorkspaces. The record of admin actions on it stays.
func (s *Store) AdminPurgeChat(ctx context.Context, convID string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM chat.conversations WHERE id = $1`, convID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrConversationNotFound
	}
	return nil
}
