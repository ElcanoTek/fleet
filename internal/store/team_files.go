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
	"strings"
	"time"
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

// ── per-file share state ────────────────────────────────────────────────────

// maxOutputPathLen bounds one stored exclusion path. Workspace paths are
// agent-chosen but nothing legitimate is anywhere near this long; the cap
// keeps a hostile client from parking megabytes in the table.
const maxOutputPathLen = 1024

// ErrInvalidOutputPath is returned for an exclusion path that is not a clean,
// relative, slash-separated workspace path.
var ErrInvalidOutputPath = errors.New("invalid output path")

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
// insert into a foreign-key error.
func ownsLiveConversationTx(ctx context.Context, tx *sql.Tx, ownerEmail, convID string) error {
	var one int
	err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM conversations WHERE id = $1 AND user_email = $2 AND deleted_at IS NULL FOR SHARE`,
		convID, normalizeEmail(ownerEmail)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrConversationNotFound
	}
	return err
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
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// maxExclusionsPerRequest bounds ReplaceOutputExclusions — the share dialog's
// checklist, which lists one chat's outputs.
const maxExclusionsPerRequest = 1000

// ReplaceOutputExclusions makes the chat's exclusion set EXACTLY paths — the
// share dialog's checklist, applied as one decision. Owner-only, atomic.
func (s *Store) ReplaceOutputExclusions(ctx context.Context, ownerEmail, convID string, paths []string) error {
	if len(paths) > maxExclusionsPerRequest {
		return ErrInvalidOutputPath
	}
	for _, p := range paths {
		if !ValidOutputPath(p) {
			return ErrInvalidOutputPath
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
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM conversation_output_exclusions WHERE conversation_id = $1`, convID); err != nil {
		return err
	}
	now := time.Now().Unix()
	for _, p := range paths {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO conversation_output_exclusions (conversation_id, path, created_at)
			 VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, convID, p, now); err != nil {
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
			 branched_at, copied_files, withheld_files, files_announced)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7::jsonb, FALSE)
		ON CONFLICT (conversation_id) DO UPDATE SET
			source_conversation_id = EXCLUDED.source_conversation_id,
			source_owner_email = EXCLUDED.source_owner_email,
			source_title = EXCLUDED.source_title,
			branched_at = EXCLUDED.branched_at,
			copied_files = EXCLUDED.copied_files,
			withheld_files = EXCLUDED.withheld_files`,
		branchConvID, o.SourceConversationID, normalizeEmail(o.SourceOwnerEmail), o.SourceTitle,
		o.BranchedAt, string(copied), string(withheld))
	return err
}

func scanBranchOrigin(sc rowScanner) (*BranchOrigin, error) {
	var o BranchOrigin
	var copied, withheld []byte
	if err := sc.Scan(&o.SourceConversationID, &o.SourceOwnerEmail, &o.SourceTitle,
		&o.BranchedAt, &copied, &withheld); err != nil {
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
		o.branched_at, o.copied_files, o.withheld_files`

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

// ClaimBranchFilesAnnouncement flips the one-shot latch on a teammate
// branch's first-turn file note and returns the origin when THIS call flipped
// it — nil when convID is not a teammate branch or the note was already
// claimed. Atomic, so two racing turns cannot both inject it.
func (s *Store) ClaimBranchFilesAnnouncement(ctx context.Context, convID string) (*BranchOrigin, error) {
	o, err := scanBranchOrigin(s.db.QueryRowContext(ctx, `
		UPDATE conversation_branch_origins o SET files_announced = TRUE
		WHERE o.conversation_id = $1 AND o.files_announced = FALSE
		RETURNING `+branchOriginColumns, convID))
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
	// it is shown.
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
		       COALESCE((SELECT MAX(m.created_at) FROM messages m
		                 WHERE m.conversation_id = o.source_conversation_id), 0) > o.branched_at
		FROM conversation_branch_origins o
		JOIN conversations c ON c.id = o.conversation_id
		WHERE c.user_email = $1 AND c.deleted_at IS NULL
		  AND o.source_conversation_id = ANY($2)
		ORDER BY o.source_conversation_id, o.branched_at DESC, o.conversation_id DESC`,
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
// group a person ever toggled; a project with more chats than this simply
// forgets the oldest-written choices' surplus.
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
// one group sends one key). has_shared_chat is not client-writable — see
// MarkProjectSharedChat.
func (s *Store) UpdateProjectUserState(ctx context.Context, projectID, email string, keptPersonal *bool, sourcesOpen map[string]bool) (ProjectUserState, error) {
	email = normalizeEmail(email)
	cur, err := s.GetProjectUserState(ctx, projectID, email)
	if err != nil {
		return cur, err
	}
	if keptPersonal != nil {
		cur.KeptPersonal = *keptPersonal
	}
	for k, v := range sourcesOpen {
		if k == "" || len(k) > 128 {
			continue
		}
		if _, exists := cur.SourcesOpen[k]; !exists && len(cur.SourcesOpen) >= maxSourcesOpenEntries {
			continue
		}
		cur.SourcesOpen[k] = v
	}
	raw, err := json.Marshal(cur.SourcesOpen)
	if err != nil {
		return cur, err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO project_user_state (project_id, user_email, kept_personal, has_shared_chat, sources_open, updated_at)
		VALUES ($1, $2, $3, FALSE, $4::jsonb, $5)
		ON CONFLICT (project_id, user_email) DO UPDATE SET
			kept_personal = EXCLUDED.kept_personal,
			sources_open = EXCLUDED.sources_open,
			updated_at = EXCLUDED.updated_at`,
		projectID, email, cur.KeptPersonal, string(raw), time.Now().Unix())
	if err != nil {
		return cur, err
	}
	return s.GetProjectUserState(ctx, projectID, email)
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
