package store

// Files in a team share (ADR-0079, docs/TEAM-SHARING.md "Files in a team
// share"): the persistence half of "sharing a chat shares its outputs".
//
// What an OUTPUT is — a workspace file the agent presented in a reply — is
// decided host-side in the HTTP layer, from the transcript and the workspace
// on disk; neither is something this package reads. What lives here is the
// state the decision needs and the gates around it:
//
//   - per-file share state, stored as EXCLUSIONS (conversation_output_exclusions).
//     Default is shared, so outputs the agent presents after the chat is
//     shared go with it — a shared chat is live, and its files are too. An
//     exclusion is the owner unchecking one file, and it is deliberately NOT
//     tied to team_visible: it survives stop sharing, sharing again, archive
//     and unarchive, so a later one-click share can never quietly re-expose a
//     file the owner held back.
//   - where a teammate's branch came from, and which shared files were copied
//     into it (conversation_branch_origins), so the branch can say so and the
//     agent in it can be told what it actually has.
//   - per-person project UI state (project_user_state).
//
// The read gate for a teammate is the same one team-view uses
// (teamReadableClause): the owner's opt-in AND the caller's team being the
// audience the owner named. A file is served only through that gate AND only
// when it is a current, non-excluded output — see httpapi/team_files.go.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// teamReadableClause is GetTeamVisibleConversation's WHERE, factored so the
// cheap existence checks below (the team link, "is the source still shared?")
// can never drift from the gate that hands out the transcript. Parameters:
// $1 conversation id, $2 caller email, $3 caller team ("" for none).
const teamReadableClause = `c.id = $1
		  AND c.deleted_at IS NULL
		  AND c.archived_at IS NULL
		  AND c.team_visible = TRUE
		  AND (c.user_email = $2 OR ($3 <> '' AND c.team_shared_with = $3))`

// callerTeam resolves a caller's trimmed team id ("" for no team or no user).
func (s *Store) callerTeam(ctx context.Context, email string) (string, error) {
	u, err := s.GetUser(ctx, normalizeEmail(email))
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(u.TeamID), nil
}

// CanTeamRead reports whether callerEmail may read convID through the team
// door right now — exactly GetTeamVisibleConversation's gate, without loading
// the transcript.
func (s *Store) CanTeamRead(ctx context.Context, callerEmail, convID string) (bool, error) {
	if convID == "" {
		return false, nil
	}
	callerEmail = normalizeEmail(callerEmail)
	team, err := s.callerTeam(ctx, callerEmail)
	if err != nil {
		return false, err
	}
	var ok bool
	err = s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM conversations c WHERE `+teamReadableClause+`)`,
		convID, callerEmail, team).Scan(&ok)
	return ok, err
}

// ── output discovery's narrow history read ──────────────────────────────────

// discoveryHistoryPage is how many rows one LoadDiscoveryHistory round trip
// fetches; a var so tests can shrink it and exercise the paging.
var discoveryHistoryPage = 500

// SetDiscoveryHistoryPageForTest shrinks the LoadDiscoveryHistory page so a
// test outside this package can exercise the paging; it returns the restore.
// Test-only — never call it from production code.
func SetDiscoveryHistoryPageForTest(n int) (restore func()) {
	old := discoveryHistoryPage
	discoveryHistoryPage = n
	return func() { discoveryHistoryPage = old }
}

// LoadDiscoveryHistory is the history read behind output discovery — the
// outputs listing, Sources, the team-files download gate and a teammate's
// branch copy — narrowed to the rows discovery actually reads, so none of
// those routes loads a chat's whole history (tool results, reasoning) to
// find the few links its replies present.
//
// Rows come from the same filter teamTranscriptEntry applies: user/assistant
// text, and a compaction summary (or a boundary standing in for one) as a
// content-free agent.EntryTypeSummaryBoundary. Only ASSISTANT text carries its
// content; a user row is returned as a content-free "{}" — discovery uses it
// only as the boundary between rendered replies. This is a discovery input,
// never a transcript: nothing reads it back to a person.
//
// Rows are read NEWEST first, in pages, only through id <= throughID (0 = no
// bound — a branch reads exactly what it copied). Each row is handed to more
// before the next is read; when more returns false the walk stops, keeping
// that row (the caller's budget check needs to see the row that crossed it).
// A page is also cut in SQL at maxBytes of assistant-text bytes counted from
// the newest row, so a page of huge replies is never fetched past the budget
// the caller would stop at anyway. The result is in ascending id order, like
// LoadHistory.
//
// Unscoped by caller: every caller has already passed an ownership or
// team-read gate for convID.
func (s *Store) LoadDiscoveryHistory(ctx context.Context, convID string, throughID int64, maxBytes int64, more func(agent.HistoryEntry) bool) ([]agent.HistoryEntry, error) {
	if throughID <= 0 {
		throughID = math.MaxInt64
	}
	var newestFirst []agent.HistoryEntry
	cursor := throughID
	var used int64 // assistant-text bytes read so far
	for {
		// 'summary_boundary' is agent.EntryTypeSummaryBoundary, as in
		// BranchConversation's redacted copy (a constant literal, so the
		// query is never built by concatenation).
		rows, err := s.db.QueryContext(ctx, `
			SELECT id, role, type, body, sz FROM (
				SELECT id, role, type,
				       CASE WHEN type = 'text' AND role = 'assistant' THEN content ELSE '{}' END AS body,
				       CASE WHEN type = 'text' AND role = 'assistant' THEN octet_length(content) ELSE 0 END AS sz,
				       COALESCE(SUM(CASE WHEN type = 'text' AND role = 'assistant' THEN octet_length(content) ELSE 0 END)
				                OVER (ORDER BY id DESC ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING), 0) AS before
				FROM messages
				WHERE conversation_id = $1 AND id <= $2
				  AND ((type = 'text' AND role IN ('user', 'assistant'))
				       OR type IN ('summary', 'summary_boundary'))
			) t
			WHERE before <= $3
			ORDER BY id DESC
			LIMIT $4`,
			convID, cursor, maxBytes-used, discoveryHistoryPage)
		if err != nil {
			return nil, err
		}
		n := 0
		stop := false
		for rows.Next() {
			var e agent.HistoryEntry
			var body string
			var size int64
			if err := rows.Scan(&e.ID, &e.Role, &e.Type, &body, &size); err != nil {
				_ = rows.Close()
				return nil, err
			}
			n++
			cursor = e.ID - 1
			used += size
			e.Content = json.RawMessage(body)
			if kept, ok := teamTranscriptEntry(e); ok {
				e = kept
			}
			newestFirst = append(newestFirst, e)
			if !more(e) {
				stop = true
				break
			}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		// A short page is the end of the history (or of the byte budget,
		// past which the SQL cut keeps nothing).
		if stop || n < discoveryHistoryPage || used > maxBytes {
			break
		}
	}
	out := make([]agent.HistoryEntry, len(newestFirst))
	for i, e := range newestFirst {
		out[len(newestFirst)-1-i] = e
	}
	return out, nil
}

// ── per-file share state ────────────────────────────────────────────────────

// maxOutputPathLen bounds one stored exclusion path. Workspace paths are
// agent-chosen but nothing legitimate is anywhere near this long; the cap
// keeps a hostile client from parking megabytes in the table.
const maxOutputPathLen = 1024

// ErrInvalidOutputPath is returned for an exclusion path that is not a clean,
// relative, slash-separated workspace path.
var ErrInvalidOutputPath = errors.New("invalid output path")

// ErrTooManyExclusions refuses an exclusion write that would take one chat
// past maxExclusionsPerConversation. Refused, never pruned: dropping an old
// exclusion would quietly re-share that file if it ever came back.
var ErrTooManyExclusions = errors.New("too many unshared files in this chat")

// maxExclusionsPerConversation bounds the exclusion set every outputs read,
// download gate and team-view version materializes. Discovery considers at
// most 500 outputs, so a real chat stays far below it; it exists so repeated
// writes of arbitrary paths cannot grow those hot-path reads without bound.
const maxExclusionsPerConversation = 2000

// ValidOutputPath reports whether p has the shape an output path always has:
// relative, slash-separated, no empty / "." / ".." segment, no NUL or
// backslash, bounded length. Outputs are produced in exactly this shape, so
// anything else cannot name one and is refused rather than stored.
func ValidOutputPath(p string) bool {
	if p == "" || len(p) > maxOutputPathLen || strings.ContainsAny(p, "\x00\\") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// ListOutputExclusions returns the paths the owner has unchecked for convID.
// Unscoped by caller: every caller of it has already passed an ownership or
// team-read gate for this conversation.
func (s *Store) ListOutputExclusions(ctx context.Context, convID string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT path FROM conversation_output_exclusions WHERE conversation_id = $1`, convID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out[p] = true
	}
	return out, rows.Err()
}

// ownsLiveConversationTx is the ownership gate every exclusion write runs
// under. It locks the conversation row so a concurrent delete cannot race the
// insert into a foreign-key error, and so two exclusion writes for one chat
// serialize (FOR NO KEY UPDATE conflicts with itself) — which is what keeps
// the per-chat cap exact under concurrent requests.
func ownsLiveConversationTx(ctx context.Context, tx *sql.Tx, ownerEmail, convID string) error {
	var one int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM conversations WHERE id = $1 AND user_email = $2 AND deleted_at IS NULL FOR NO KEY UPDATE`,
		convID, normalizeEmail(ownerEmail)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConversationNotFound
	}
	return err
}

// exclusionCapTx fails the write when the chat now holds more exclusions than
// maxExclusionsPerConversation. Run after the inserts, inside the same
// transaction, so a refused write leaves nothing behind.
func exclusionCapTx(ctx context.Context, tx *sql.Tx, convID string) error {
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM conversation_output_exclusions WHERE conversation_id = $1`, convID).Scan(&n); err != nil {
		return err
	}
	if n > maxExclusionsPerConversation {
		return ErrTooManyExclusions
	}
	return nil
}

// SetOutputShared records the owner's choice for one output: shared=false
// adds an exclusion, shared=true removes it. Owner-only; idempotent.
func (s *Store) SetOutputShared(ctx context.Context, ownerEmail, convID, path string, shared bool) error {
	if !ValidOutputPath(path) {
		return ErrInvalidOutputPath
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := ownsLiveConversationTx(ctx, tx, ownerEmail, convID); err != nil {
		return err
	}
	if shared {
		_, err = tx.ExecContext(ctx,
			`DELETE FROM conversation_output_exclusions WHERE conversation_id = $1 AND path = $2`,
			convID, path)
	} else {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO conversation_output_exclusions (conversation_id, path, created_at)
			 VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
			convID, path, time.Now().Unix())
		if err == nil {
			err = exclusionCapTx(ctx, tx, convID)
		}
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// maxExclusionsPerRequest bounds ApplyOutputChecklist — the share dialog's
// checklist, which lists one chat's outputs.
const maxExclusionsPerRequest = 1000

// ApplyOutputChecklist applies the share dialog's checklist as one decision,
// over EXACTLY the paths the checklist showed: every path in listed is
// excluded when it is in unshared and shared otherwise. An exclusion for a
// path the checklist did not show — an output missing on disk right now, one
// past the discovery bound, one presented after the dialog loaded — is left
// exactly as it was: the owner made no decision about it, so a re-share with
// an all-checked list must not quietly re-expose it if it comes back. A path
// in unshared counts as listed. Owner-only, atomic.
func (s *Store) ApplyOutputChecklist(ctx context.Context, ownerEmail, convID string, listed, unshared []string) error {
	if len(listed) > maxExclusionsPerRequest || len(unshared) > maxExclusionsPerRequest {
		return ErrInvalidOutputPath
	}
	excluded := make(map[string]bool, len(unshared))
	for _, p := range unshared {
		if !ValidOutputPath(p) {
			return ErrInvalidOutputPath
		}
		excluded[p] = true
	}
	var share []string
	for _, p := range listed {
		if !ValidOutputPath(p) {
			return ErrInvalidOutputPath
		}
		if !excluded[p] {
			share = append(share, p)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := ownsLiveConversationTx(ctx, tx, ownerEmail, convID); err != nil {
		return err
	}
	if len(share) > 0 {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM conversation_output_exclusions WHERE conversation_id = $1 AND path = ANY($2)`,
			convID, share); err != nil {
			return err
		}
	}
	now := time.Now().Unix()
	for p := range excluded {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO conversation_output_exclusions (conversation_id, path, created_at)
			 VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, convID, p, now); err != nil {
			return err
		}
	}
	if len(excluded) > 0 {
		if err := exclusionCapTx(ctx, tx, convID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ── branch origins ──────────────────────────────────────────────────────────

// BranchFile is one shared output copied into a teammate's branch.
type BranchFile struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// BranchOrigin is where a teammate's branch came from (ADR-0079): whose chat,
// when, which shared files came with it as the brancher's own copies, and
// which outputs did not (they were unchecked at branch time, so the branch's
// transcript can mention them but the branch does not have them).
type BranchOrigin struct {
	SourceConversationID string       `json:"source_conversation_id"`
	SourceOwnerEmail     string       `json:"source_owner_email"`
	SourceTitle          string       `json:"source_title"`
	BranchedAt           int64        `json:"branched_at"`
	CopiedFiles          []BranchFile `json:"copied_files"`
	WithheldFiles        []string     `json:"withheld_files"`
	// WithheldTruncated is true when the transcript referenced more workspace
	// files than WithheldFiles records (the list is bounded so one branch
	// cannot park an unbounded JSONB array). A reader must then treat a
	// reference that is in neither CopiedFiles nor WithheldFiles as withheld
	// unless the file is in the branch's own workspace — never as live.
	WithheldTruncated bool `json:"withheld_truncated"`
	// SourceMaxMessageID is the branch point the copy used — the source's
	// highest message id the branch actually contains, and the high-water
	// mark ViewerBranches compares against. The caller passes it explicitly;
	// RecordBranchOrigin never reads the source itself, because the source's
	// MAX(id) (before or after the copy) would count a message the branch
	// never copied — one past the branch point — as seen. Zero
	// means unknown and reports every source message as new. Never sent: a
	// message id of someone else's chat.
	SourceMaxMessageID int64 `json:"-"`
	// SourceStillShared is computed per read: whether the branch's owner can
	// still team-view the source. The banner links back only while it is
	// true, so it never offers a door that 404s.
	SourceStillShared bool `json:"source_still_shared"`
}

// RecordBranchOrigin stores the origin of a freshly created teammate branch.
// branchConvID must be a conversation the brancher owns (the caller just
// created it); a second record for the same branch replaces the first.
func (s *Store) RecordBranchOrigin(ctx context.Context, branchConvID string, o BranchOrigin) error {
	if o.CopiedFiles == nil {
		o.CopiedFiles = []BranchFile{}
	}
	if o.WithheldFiles == nil {
		o.WithheldFiles = []string{}
	}
	copied, err := json.Marshal(o.CopiedFiles)
	if err != nil {
		return err
	}
	withheld, err := json.Marshal(o.WithheldFiles)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO conversation_branch_origins
			(conversation_id, source_conversation_id, source_owner_email, source_title,
			 branched_at, source_max_message_id, copied_files, withheld_files, withheld_truncated,
			 branch_max_message_id)
		VALUES ($1, $2, $3, $4, $5, $8, $6::jsonb, $7::jsonb, $9,
		        (SELECT COALESCE(MAX(m.id), 0) FROM messages m WHERE m.conversation_id = $1))
		ON CONFLICT (conversation_id) DO UPDATE SET
			source_conversation_id = EXCLUDED.source_conversation_id,
			source_owner_email = EXCLUDED.source_owner_email,
			source_title = EXCLUDED.source_title,
			branched_at = EXCLUDED.branched_at,
			source_max_message_id = EXCLUDED.source_max_message_id,
			copied_files = EXCLUDED.copied_files,
			withheld_files = EXCLUDED.withheld_files,
			withheld_truncated = EXCLUDED.withheld_truncated`,
		branchConvID, o.SourceConversationID, normalizeEmail(o.SourceOwnerEmail), o.SourceTitle,
		o.BranchedAt, string(copied), string(withheld), o.SourceMaxMessageID, o.WithheldTruncated)
	return err
}

func scanBranchOrigin(sc rowScanner) (*BranchOrigin, error) {
	var o BranchOrigin
	var copied, withheld []byte
	if err := sc.Scan(&o.SourceConversationID, &o.SourceOwnerEmail, &o.SourceTitle,
		&o.BranchedAt, &copied, &withheld, &o.WithheldTruncated); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(copied, &o.CopiedFiles); err != nil || o.CopiedFiles == nil {
		o.CopiedFiles = []BranchFile{}
	}
	if err := json.Unmarshal(withheld, &o.WithheldFiles); err != nil || o.WithheldFiles == nil {
		o.WithheldFiles = []string{}
	}
	return &o, nil
}

const branchOriginColumns = `o.source_conversation_id, o.source_owner_email, o.source_title,
		o.branched_at, o.copied_files, o.withheld_files, o.withheld_truncated`

// GetBranchOrigin returns the origin of ownerEmail's branch convID, or nil
// when convID is not theirs or is not a teammate branch.
func (s *Store) GetBranchOrigin(ctx context.Context, ownerEmail, convID string) (*BranchOrigin, error) {
	ownerEmail = normalizeEmail(ownerEmail)
	o, err := scanBranchOrigin(s.db.QueryRowContext(ctx, `
		SELECT `+branchOriginColumns+`
		FROM conversation_branch_origins o
		JOIN conversations c ON c.id = o.conversation_id
		WHERE o.conversation_id = $1 AND c.user_email = $2 AND c.deleted_at IS NULL`,
		convID, ownerEmail))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	still, err := s.CanTeamRead(ctx, ownerEmail, o.SourceConversationID)
	if err != nil {
		return nil, err
	}
	o.SourceStillShared = still
	return o, nil
}

// BranchOriginsFor returns the origins of whichever of convIDs are teammate
// branches, keyed by branch id. SourceStillShared is not computed (the
// Sources listing that calls this does not show it).
func (s *Store) BranchOriginsFor(ctx context.Context, convIDs []string) (map[string]*BranchOrigin, error) {
	out := map[string]*BranchOrigin{}
	if len(convIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT o.conversation_id, `+branchOriginColumns+`
		FROM conversation_branch_origins o
		WHERE o.conversation_id = ANY($1)`, convIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		o, err := scanBranchOrigin(prefixedScanner{rows: rows, first: &id})
		if err != nil {
			return nil, err
		}
		out[id] = o
	}
	return out, rows.Err()
}

// prefixedScanner scans one leading column into first, then hands the rest to
// a shared scan function — so BranchOriginsFor reuses scanBranchOrigin.
type prefixedScanner struct {
	rows  *sql.Rows
	first *string
}

func (p prefixedScanner) Scan(dest ...any) error {
	return p.rows.Scan(append([]any{p.first}, dest...)...)
}

// PendingBranchFilesAnnouncement returns the origin of teammate branch convID
// while its first-turn file note is still due — no user message of the
// branch's own has committed past branch_max_message_id — and nil otherwise
// (not a branch, or a turn already carried the note). Derived from committed
// rows, not a latch: a turn that fails, or a process that dies, before its
// user message commits leaves the note due for the next turn, and once one
// commits (the note is its persisted injected context) it is never due again.
func (s *Store) PendingBranchFilesAnnouncement(ctx context.Context, convID string) (*BranchOrigin, error) {
	o, err := scanBranchOrigin(s.db.QueryRowContext(ctx, `
		SELECT `+branchOriginColumns+`
		FROM conversation_branch_origins o
		WHERE o.conversation_id = $1
		  AND NOT EXISTS (SELECT 1 FROM messages m
		                  WHERE m.conversation_id = o.conversation_id
		                    AND m.id > o.branch_max_message_id
		                    AND m.role = 'user')`, convID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return o, nil
}

// ViewerBranch is the viewer's own most recent live branch of a teammate's
// chat — "You branched this", the banner, and "has added messages since".
type ViewerBranch struct {
	ConversationID string `json:"conversation_id"`
	BranchedAt     int64  `json:"branched_at"`
	// ChangedSince is whether the source gained MESSAGES after the branch
	// was made. Messages, not conversations.updated_at: the latter also moves
	// on a rename, a share toggle or an archive, and the banner this feeds
	// says "has added messages since you branched" — which must be true when
	// it is shown. Measured against the source's message-id high-water mark
	// recorded at branch time, not timestamps: created_at and branched_at are
	// whole seconds, so a message in the branch's own second would be missed.
	// Only rows the team view exposes count (user/assistant text — the same
	// filter GetTeamVisibleConversation applies): the branch point is the
	// last VISIBLE text id, and a completed turn persists its turn_summary
	// (and tool/reasoning rows) after that, so counting every row would make
	// a fresh branch of a finished chat report changes nobody can see.
	ChangedSince bool `json:"changed_since"`
}

// ViewerBranches returns, per source id in sourceIDs, viewerEmail's most
// recent branch of it that still exists. Sources the viewer never branched
// are absent from the map.
func (s *Store) ViewerBranches(ctx context.Context, viewerEmail string, sourceIDs []string) (map[string]ViewerBranch, error) {
	out := map[string]ViewerBranch{}
	if len(sourceIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT ON (o.source_conversation_id)
		       o.source_conversation_id, o.conversation_id, o.branched_at,
		       EXISTS (SELECT 1 FROM messages m
		               WHERE m.conversation_id = o.source_conversation_id
		                 AND m.id > o.source_max_message_id
		                 AND m.type = 'text' AND m.role IN ('user', 'assistant'))
		FROM conversation_branch_origins o
		JOIN conversations c ON c.id = o.conversation_id
		WHERE c.user_email = $1 AND c.deleted_at IS NULL
		  AND o.source_conversation_id = ANY($2)
		ORDER BY o.source_conversation_id, o.seq DESC`,
		normalizeEmail(viewerEmail), sourceIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var src string
		var vb ViewerBranch
		if err := rows.Scan(&src, &vb.ConversationID, &vb.BranchedAt, &vb.ChangedSince); err != nil {
			return nil, err
		}
		out[src] = vb
	}
	return out, rows.Err()
}

// ── the team link ───────────────────────────────────────────────────────────

// Team link statuses (GET /conversations/{id}/team-link).
const (
	TeamLinkOwner     = "owner"
	TeamLinkOpen      = "open"
	TeamLinkNotOnTeam = "not_on_team"
	TeamLinkNotShared = "not_shared"
)

// TeamLink is what a team link resolves to for one signed-in caller. It
// reveals nothing beyond the status the caller needs to be routed:
//
//   - not_on_team names the audience team (the B22 page says "only <team>
//     members can open it") and nothing about the chat — no title, no project;
//   - not_shared names the chat's project only when the caller can see that
//     project anyway, so "Open <project>" is never a disclosure.
type TeamLink struct {
	Status  string      `json:"status"`
	TeamID  string      `json:"team_id,omitempty"`
	Project *ProjectRef `json:"project,omitempty"`
}

// ResolveTeamLink classifies convID for callerEmail. An unknown id is
// not_shared with no project — indistinguishable from an unshared chat.
func (s *Store) ResolveTeamLink(ctx context.Context, callerEmail, convID string) (TeamLink, error) {
	callerEmail = normalizeEmail(callerEmail)
	notShared := TeamLink{Status: TeamLinkNotShared}
	if convID == "" {
		return notShared, nil
	}
	var (
		owner, projectID, sharedWith string
		live                         bool
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT c.user_email, COALESCE(c.project_id, ''), COALESCE(c.team_shared_with, ''),
		       (c.deleted_at IS NULL AND c.archived_at IS NULL AND c.team_visible = TRUE)
		FROM conversations c WHERE c.id = $1`, convID,
	).Scan(&owner, &projectID, &sharedWith, &live)
	if errors.Is(err, sql.ErrNoRows) {
		return notShared, nil
	}
	if err != nil {
		return TeamLink{}, err
	}
	if owner == callerEmail {
		// A soft-deleted chat is gone for its owner too.
		own, gerr := s.Get(ctx, callerEmail, convID)
		if gerr != nil {
			return TeamLink{}, gerr
		}
		if own != nil {
			return TeamLink{Status: TeamLinkOwner}, nil
		}
		return notShared, nil
	}
	team, err := s.callerTeam(ctx, callerEmail)
	if err != nil {
		return TeamLink{}, err
	}
	if live && sharedWith != "" {
		if team != "" && team == sharedWith {
			return TeamLink{Status: TeamLinkOpen}, nil
		}
		return TeamLink{Status: TeamLinkNotOnTeam, TeamID: sharedWith}, nil
	}
	if projectID != "" {
		member, merr := s.userIsProjectMember(ctx, callerEmail, projectID)
		if merr != nil {
			return TeamLink{}, merr
		}
		if member {
			p, perr := s.GetProject(ctx, projectID)
			if perr != nil {
				return TeamLink{}, perr
			}
			if p != nil {
				notShared.Project = &ProjectRef{ID: p.ID, Name: p.Name}
			}
		}
	}
	return notShared, nil
}

// ── per-person project UI state ─────────────────────────────────────────────

// ProjectUserState is one person's UI state for one project, stored so it
// follows them across devices: the getting-started card (dismissed for good
// with "Keep personal"; retired once they have shared a chat here) and which
// Sources groups they left open.
type ProjectUserState struct {
	KeptPersonal  bool            `json:"kept_personal"`
	HasSharedChat bool            `json:"has_shared_chat"`
	SourcesOpen   map[string]bool `json:"sources_open"`
}

// maxSourcesOpenEntries bounds the stored open/closed map. One entry per chat
// group a person toggled; keys for chats that left the project or were
// deleted are pruned on every write, and if the map is still over the bound
// the stored choices the write did not change are evicted first — the choice
// being written always lands.
const maxSourcesOpenEntries = 500

// GetProjectUserState returns email's state for projectID (zero value when
// nothing was stored yet).
func (s *Store) GetProjectUserState(ctx context.Context, projectID, email string) (ProjectUserState, error) {
	out := ProjectUserState{SourcesOpen: map[string]bool{}}
	var raw []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT kept_personal, has_shared_chat, sources_open
		FROM project_user_state WHERE project_id = $1 AND user_email = $2`,
		projectID, normalizeEmail(email)).Scan(&out.KeptPersonal, &out.HasSharedChat, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out.SourcesOpen); err != nil || out.SourcesOpen == nil {
		out.SourcesOpen = map[string]bool{}
	}
	return out, nil
}

// UpdateProjectUserState applies a partial update: a nil keptPersonal leaves
// it alone, and sourcesOpen is MERGED into the stored map (a client toggling
// one group may send one key). has_shared_chat is not client-writable — see
// MarkProjectSharedChat.
//
// The merge is a read-modify-write, so it runs under the row's lock
// (SELECT ... FOR UPDATE after an insert-if-absent): two concurrent writes
// from two devices — one setting kept_personal, one toggling a group — both
// land instead of the later one overwriting the earlier with its stale read.
func (s *Store) UpdateProjectUserState(ctx context.Context, projectID, email string, keptPersonal *bool, sourcesOpen map[string]bool) (ProjectUserState, error) {
	email = normalizeEmail(email)
	team, err := s.callerTeam(ctx, email)
	if err != nil {
		return ProjectUserState{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProjectUserState{}, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO project_user_state (project_id, user_email, updated_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (project_id, user_email) DO NOTHING`,
		projectID, email, now); err != nil {
		return ProjectUserState{}, err
	}
	cur := ProjectUserState{SourcesOpen: map[string]bool{}}
	var raw []byte
	if err := tx.QueryRowContext(ctx, `
		SELECT kept_personal, has_shared_chat, sources_open
		FROM project_user_state WHERE project_id = $1 AND user_email = $2
		FOR UPDATE`,
		projectID, email).Scan(&cur.KeptPersonal, &cur.HasSharedChat, &raw); err != nil {
		return ProjectUserState{}, err
	}
	if err := json.Unmarshal(raw, &cur.SourcesOpen); err != nil || cur.SourcesOpen == nil {
		cur.SourcesOpen = map[string]bool{}
	}
	if keptPersonal != nil {
		cur.KeptPersonal = *keptPersonal
	}
	// changed holds the keys this write actually set to a new value: the
	// ones that must survive eviction.
	changed := map[string]bool{}
	for k, v := range sourcesOpen {
		if k == "" || len(k) > 128 {
			continue
		}
		if old, ok := cur.SourcesOpen[k]; !ok || old != v {
			changed[k] = true
		}
		cur.SourcesOpen[k] = v
	}
	if len(sourcesOpen) > 0 {
		if err := pruneSourcesOpen(ctx, tx, projectID, email, team, cur.SourcesOpen, sourcesOpen, changed); err != nil {
			return ProjectUserState{}, err
		}
	}
	merged, err := json.Marshal(cur.SourcesOpen)
	if err != nil {
		return ProjectUserState{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE project_user_state SET kept_personal = $3, sources_open = $4::jsonb, updated_at = $5
		WHERE project_id = $1 AND user_email = $2`,
		projectID, email, cur.KeptPersonal, string(merged), now); err != nil {
		return ProjectUserState{}, err
	}
	if err := tx.Commit(); err != nil {
		return ProjectUserState{}, err
	}
	return cur, nil
}

// pruneSourcesOpen drops keys of open (in place) that do not name a live chat
// in projectID that the caller could see as a Sources group right now —
// their own, or one shared with their team (the listing's own gates). A
// group that cannot be shown has no state worth keeping, and keeping a key
// only because the chat exists would let the returned map tell a member that
// another member's PRIVATE chat is still in the project. Then, if the map is still over maxSourcesOpenEntries, evicts in
// order: stored keys this write did not mention, then keys it re-sent
// unchanged, and only then keys it changed (deterministic within each tier).
// So a person with a full map can always record a new choice.
func pruneSourcesOpen(ctx context.Context, tx *sql.Tx, projectID, email, team string, open, sent, changed map[string]bool) error {
	keys := make([]string, 0, len(open))
	for k := range open {
		keys = append(keys, k)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT c.id FROM conversations c
		WHERE c.id = ANY($1) AND c.project_id = $2 AND c.deleted_at IS NULL
		  AND (c.user_email = $3
		       OR (c.team_visible = TRUE AND c.archived_at IS NULL
		           AND $4 <> '' AND c.team_shared_with = $4))`, keys, projectID, normalizeEmail(email), team)
	if err != nil {
		return err
	}
	live := make(map[string]bool, len(keys))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		live[id] = true
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, k := range keys {
		if !live[k] {
			delete(open, k)
		}
	}
	over := len(open) - maxSourcesOpenEntries
	if over <= 0 {
		return nil
	}
	tier := func(k string) int {
		switch {
		case changed[k]:
			return 2
		case sentKey(sent, k):
			return 1
		default:
			return 0
		}
	}
	victims := make([]string, 0, len(open))
	for k := range open {
		victims = append(victims, k)
	}
	sort.Slice(victims, func(i, j int) bool {
		ti, tj := tier(victims[i]), tier(victims[j])
		if ti != tj {
			return ti < tj
		}
		return victims[i] < victims[j]
	})
	for _, k := range victims[:over] {
		delete(open, k)
	}
	return nil
}

func sentKey(sent map[string]bool, k string) bool {
	_, ok := sent[k]
	return ok
}

// MarkProjectSharedChat records that email has shared a chat in projectID —
// what retires their getting-started card. Server-side only, flipped by a
// successful share-with-team, so the card's "done" state is a fact rather
// than a client claim. Never flipped back.
func (s *Store) MarkProjectSharedChat(ctx context.Context, projectID, email string) error {
	if projectID == "" {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO project_user_state (project_id, user_email, has_shared_chat, updated_at)
		SELECT $1, $2, TRUE, $3 WHERE EXISTS (SELECT 1 FROM projects WHERE id = $1)
		ON CONFLICT (project_id, user_email) DO UPDATE SET
			has_shared_chat = TRUE, updated_at = EXCLUDED.updated_at`,
		projectID, normalizeEmail(email), time.Now().Unix())
	return err
}
