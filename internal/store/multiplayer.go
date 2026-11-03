package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

const multiplayerMCPAccountPrefix = "mp-user:"

// MultiplayerMCPAccountPrefix returns the key prefix used to store a non-owner
// multiplayer participant's per-connector seat overrides inside
// conversations.mcp_accounts without colliding with the owner's or another
// participant's choices.
func MultiplayerMCPAccountPrefix(email string) string {
	return multiplayerMCPAccountPrefix + normalizeEmail(email) + ":"
}

func filterOwnerMCPAccounts(raw map[string]string) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var out map[string]string
	for k, v := range raw {
		if strings.HasPrefix(k, multiplayerMCPAccountPrefix) {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(raw))
		}
		out[k] = v
	}
	return out
}

func filterParticipantMCPAccounts(raw map[string]string, callerEmail string) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	prefix := MultiplayerMCPAccountPrefix(callerEmail)
	var out map[string]string
	for k, v := range raw {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		server := strings.TrimPrefix(k, prefix)
		if server == "" {
			continue
		}
		if out == nil {
			out = make(map[string]string)
		}
		out[server] = v
	}
	return out
}

func scanEmailJSONArray(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "[]" || raw == "null" {
		return nil
	}
	var parsed []string
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil
	}
	return normalizeEmailSlice(parsed, "")
}

func normalizeEmailSlice(in []string, skipEmail string) []string {
	skip := normalizeEmail(skipEmail)
	seen := make(map[string]bool, len(in))
	var out []string
	for _, e := range in {
		n := normalizeEmail(e)
		if n == "" || n == skip || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func marshalEmailSlice(in []string) string {
	if len(in) == 0 {
		return "[]"
	}
	b, err := json.Marshal(in)
	if err != nil {
		return "[]"
	}
	return string(b)
}

func containsEmail(list []string, target string) bool {
	target = normalizeEmail(target)
	if target == "" {
		return false
	}
	for _, e := range list {
		if normalizeEmail(e) == target {
			return true
		}
	}
	return false
}

// SetConversationMultiplayer toggles Multiplayer mode on a conversation owned
// by ownerEmail and optionally updates its explicit participant/exclusion lists
// (when non-nil).
//
// In Multiplayer mode:
//   - By default, everyone on the owner's team (or everyone allowed on the
//     Fleet instance when the owner has no team) can open the conversation and
//     send messages directly using their own account & MCP connectors.
//   - Specific people can be added (multiplayer_participants) or removed
//     (multiplayer_excluded) at will.
//   - Turning Multiplayer mode on also sets team_visible = TRUE when the owner
//     belongs to a team so the conversation appears in team listings, without
//     requiring the conversation to live inside a project first.
func (s *Store) SetConversationMultiplayer(ctx context.Context, ownerEmail, convID string, multiplayer bool, participants, excluded []string) (*Conversation, error) {
	return s.mutateConversationMultiplayer(ctx, ownerEmail, convID, func(_ bool, curParticipants, curExcluded []string) (bool, []string, []string) {
		nextParticipants := curParticipants
		if participants != nil {
			nextParticipants = normalizeEmailSlice(participants, ownerEmail)
		}
		nextExcluded := curExcluded
		if excluded != nil {
			nextExcluded = normalizeEmailSlice(excluded, ownerEmail)
		}
		return multiplayer, nextParticipants, nextExcluded
	})
}

// UpdateConversationMultiplayerAccess adds and/or removes specific user emails
// on a conversation's multiplayer access list atomically under a row lock.
func (s *Store) UpdateConversationMultiplayerAccess(ctx context.Context, ownerEmail, convID string, addEmails, removeEmails []string) (*Conversation, error) {
	ownerEmail = normalizeEmail(ownerEmail)
	return s.mutateConversationMultiplayer(ctx, ownerEmail, convID, func(curMultiplayer bool, curParticipants, curExcluded []string) (bool, []string, []string) {
		partMap := make(map[string]bool)
		for _, p := range curParticipants {
			partMap[normalizeEmail(p)] = true
		}
		exclMap := make(map[string]bool)
		for _, e := range curExcluded {
			exclMap[normalizeEmail(e)] = true
		}
		for _, raw := range addEmails {
			e := normalizeEmail(raw)
			if e == "" || e == ownerEmail {
				continue
			}
			delete(exclMap, e)
			partMap[e] = true
		}
		for _, raw := range removeEmails {
			e := normalizeEmail(raw)
			if e == "" || e == ownerEmail {
				continue
			}
			delete(partMap, e)
			exclMap[e] = true
		}
		parts := make([]string, 0, len(partMap))
		for e := range partMap {
			parts = append(parts, e)
		}
		excls := make([]string, 0, len(exclMap))
		for e := range exclMap {
			excls = append(excls, e)
		}
		return curMultiplayer, normalizeEmailSlice(parts, ownerEmail), normalizeEmailSlice(excls, ownerEmail)
	})
}

func (s *Store) mutateConversationMultiplayer(
	ctx context.Context,
	ownerEmail, convID string,
	mutate func(curMultiplayer bool, curParticipants, curExcluded []string) (bool, []string, []string),
) (*Conversation, error) {
	ownerEmail = normalizeEmail(ownerEmail)
	now := time.Now().Unix()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	if err := lockUserTeamChangeTx(ctx, tx, ownerEmail, ""); err != nil {
		return nil, err
	}
	var ownerTeam sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT team_id FROM users WHERE email = $1 FOR UPDATE`, ownerEmail,
	).Scan(&ownerTeam); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	team := strings.TrimSpace(ownerTeam.String)
	if team != "" {
		if err := lockTeamNamesTx(ctx, tx, team); err != nil {
			return nil, err
		}
	}

	var (
		archivedAt     sql.NullInt64
		lockdown       bool
		curMultiplayer bool
		partRaw        string
		exclRaw        string
	)
	if err := tx.QueryRowContext(ctx,
		`SELECT archived_at, lockdown, multiplayer, COALESCE(multiplayer_participants, '[]'), COALESCE(multiplayer_excluded, '[]')
		   FROM conversations
		  WHERE id = $1 AND user_email = $2 AND deleted_at IS NULL
		    FOR UPDATE`,
		convID, ownerEmail,
	).Scan(&archivedAt, &lockdown, &curMultiplayer, &partRaw, &exclRaw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrConversationNotFound
		}
		return nil, err
	}

	multiplayer, nextParticipants, nextExcluded := mutate(curMultiplayer, scanEmailJSONArray(partRaw), scanEmailJSONArray(exclRaw))
	if multiplayer && (archivedAt.Valid || lockdown) {
		return nil, ErrArchivedNotShareable
	}
	// Anyone explicitly excluded is removed from explicit participants so the
	// two lists never contradict each other.
	if len(nextExcluded) > 0 && len(nextParticipants) > 0 {
		filtered := make([]string, 0, len(nextParticipants))
		for _, p := range nextParticipants {
			if !containsEmail(nextExcluded, p) {
				filtered = append(filtered, p)
			}
		}
		nextParticipants = filtered
	}

	partJSON := marshalEmailSlice(nextParticipants)
	exclJSON := marshalEmailSlice(nextExcluded)

	var res sql.Result
	switch {
	case !multiplayer:
		// Turning off multiplayer clears multiplayer = FALSE. Preserve
		// team_visible and the team audience stamp ONLY when the conversation
		// currently lives in a project that is actually shared with that same
		// team; unfiled chats and personal-project chats become private again.
		res, err = tx.ExecContext(ctx, `
			UPDATE conversations SET
				multiplayer = FALSE,
				multiplayer_participants = $1,
				multiplayer_excluded = $2,
				team_visible = CASE
					WHEN EXISTS (
						SELECT 1 FROM projects p
						JOIN teams t ON t.name = p.team_id
						WHERE p.id = conversations.project_id
						  AND p.team_id = conversations.team_shared_with
						  AND t.id = conversations.team_shared_with_id
					) THEN team_visible
					ELSE FALSE
				END,
				team_shared_with = CASE
					WHEN EXISTS (
						SELECT 1 FROM projects p
						JOIN teams t ON t.name = p.team_id
						WHERE p.id = conversations.project_id
						  AND p.team_id = conversations.team_shared_with
						  AND t.id = conversations.team_shared_with_id
					) THEN team_shared_with
					ELSE NULL
				END,
				team_shared_with_id = CASE
					WHEN EXISTS (
						SELECT 1 FROM projects p
						JOIN teams t ON t.name = p.team_id
						WHERE p.id = conversations.project_id
						  AND p.team_id = conversations.team_shared_with
						  AND t.id = conversations.team_shared_with_id
					) THEN team_shared_with_id
					ELSE NULL
				END,
				updated_at = $3
			WHERE id = $4 AND user_email = $5 AND deleted_at IS NULL`,
			partJSON, exclJSON, now, convID, ownerEmail,
		)
	case team != "":
		res, err = tx.ExecContext(ctx, `
			WITH t AS (
				INSERT INTO teams (name, created_at) VALUES ($1, $2)
				ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
				RETURNING id
			)
			UPDATE conversations SET
				multiplayer = TRUE,
				multiplayer_participants = $3,
				multiplayer_excluded = $4,
				team_visible = TRUE,
				team_shared_with = $1,
				team_shared_with_id = (SELECT id FROM t),
				updated_at = $2
			WHERE id = $5 AND user_email = $6 AND deleted_at IS NULL AND archived_at IS NULL AND lockdown = FALSE`,
			team, now, partJSON, exclJSON, convID, ownerEmail,
		)
	default:
		res, err = tx.ExecContext(ctx, `
			UPDATE conversations SET
				multiplayer = TRUE,
				multiplayer_participants = $1,
				multiplayer_excluded = $2,
				updated_at = $3
			WHERE id = $4 AND user_email = $5 AND deleted_at IS NULL AND archived_at IS NULL AND lockdown = FALSE`,
			partJSON, exclJSON, now, convID, ownerEmail,
		)
	}
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return nil, err
	} else if n == 0 {
		return nil, ErrConversationNotFound
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	updated, err := s.Get(ctx, ownerEmail, convID)
	if err != nil {
		return nil, err
	}
	if updated == nil {
		return nil, ErrConversationNotFound
	}
	return updated, nil
}

// lockAndVerifyOwnedOrMultiplayerTx locks the conversation row for convID and
// verifies that callerEmail is its owner or an authorized multiplayer
// participant, returning the conversation owner's email.
type rowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (s *Store) lockAndVerifyOwnedOrMultiplayerTx(ctx context.Context, tx *sql.Tx, callerEmail, convID string) (string, error) {
	callerEmail = normalizeEmail(callerEmail)
	var (
		ownerEmail   string
		multiplayer  bool
		partRaw      string
		exclRaw      string
		sharedTeam   sql.NullString
		sharedTeamID sql.NullInt64
		archivedAt   sql.NullInt64
		lockdown     bool
	)
	err := tx.QueryRowContext(ctx, `
		SELECT user_email, COALESCE(multiplayer, FALSE),
		       COALESCE(multiplayer_participants, '[]'), COALESCE(multiplayer_excluded, '[]'),
		       team_shared_with, team_shared_with_id, archived_at, COALESCE(lockdown, FALSE)
		  FROM conversations
		 WHERE id = $1 AND deleted_at IS NULL
		 FOR UPDATE`, convID,
	).Scan(&ownerEmail, &multiplayer, &partRaw, &exclRaw, &sharedTeam, &sharedTeamID, &archivedAt, &lockdown)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrConversationNotFound
	}
	if err != nil {
		return "", err
	}
	if normalizeEmail(ownerEmail) == callerEmail {
		return ownerEmail, nil
	}
	var archPtr *int64
	if archivedAt.Valid {
		archPtr = &archivedAt.Int64
	}
	c := &Conversation{
		ID:                      convID,
		UserEmail:               ownerEmail,
		Multiplayer:             multiplayer,
		MultiplayerParticipants: scanEmailJSONArray(partRaw),
		MultiplayerExcluded:     scanEmailJSONArray(exclRaw),
		ArchivedAt:              archPtr,
		Lockdown:                lockdown,
	}
	if !s.canParticipateInMultiplayer(ctx, tx, callerEmail, c, strings.TrimSpace(sharedTeam.String), sharedTeamID) {
		return "", ErrConversationNotFound
	}
	return ownerEmail, nil
}

// GetMultiplayerConversation returns the conversation when convID has
// multiplayer = TRUE (and is not deleted, archived, or lockdown) AND
// callerEmail is allowed to participate in it. Returns (nil, nil) otherwise.
func (s *Store) GetMultiplayerConversation(ctx context.Context, callerEmail, convID string) (*Conversation, error) {
	callerEmail = normalizeEmail(callerEmail)
	if callerEmail == "" || convID == "" {
		return nil, nil
	}
	c, err := scanConversation(s.db.QueryRowContext(ctx,
		`SELECT `+conversationColumns+`
		 FROM conversations
		 WHERE id = $1 AND deleted_at IS NULL AND archived_at IS NULL AND multiplayer = TRUE AND lockdown = FALSE`,
		convID,
	))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	var (
		partRaw      string
		exclRaw      string
		sharedTeam   sql.NullString
		sharedTeamID sql.NullInt64
	)
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(multiplayer_participants, '[]'), COALESCE(multiplayer_excluded, '[]'), team_shared_with, team_shared_with_id
		   FROM conversations
		  WHERE id = $1 AND deleted_at IS NULL AND archived_at IS NULL AND multiplayer = TRUE AND lockdown = FALSE`,
		convID,
	).Scan(&partRaw, &exclRaw, &sharedTeam, &sharedTeamID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	c.MultiplayerParticipants = scanEmailJSONArray(partRaw)
	c.MultiplayerExcluded = scanEmailJSONArray(exclRaw)
	if !s.canParticipateInMultiplayer(ctx, s.db, callerEmail, &c, strings.TrimSpace(sharedTeam.String), sharedTeamID) {
		return nil, nil
	}
	if normalizeEmail(c.UserEmail) != callerEmail {
		// Never leak owner-private fields to a non-owner participant in HTTP
		// JSON responses, while retaining an unexported (`json:"-"`) copy of
		// the enabled optional connector names so participant turns can mount
		// connectors they also have access to, and scoping MCPAccounts to this
		// participant's own persisted seat overrides.
		c.ParticipantOptionalMCPServers = c.OptionalMCPServersEnabled
		c.OptionalMCPServersEnabled = nil
		c.Labels = nil
		c.ShareToken = ""
		c.ParentConversationID = ""
		c.MCPAccounts = filterParticipantMCPAccounts(c.RawMCPAccounts, callerEmail)
		c.AutoApprove = false
	}
	return &c, nil
}

func (s *Store) canParticipateInMultiplayer(ctx context.Context, q rowQueryer, callerEmail string, c *Conversation, sharedTeam string, sharedTeamID sql.NullInt64) bool {
	callerEmail = normalizeEmail(callerEmail)
	if callerEmail == "" || c == nil || !c.Multiplayer || c.ArchivedAt != nil || c.Lockdown {
		return false
	}
	ownerEmail := normalizeEmail(c.UserEmail)
	if callerEmail == ownerEmail {
		return true
	}
	// Explicitly removed by the owner.
	if containsEmail(c.MultiplayerExcluded, callerEmail) {
		return false
	}
	// Require a live, enabled non-viewer user row so a deleted, disabled, or
	// read-only viewer user cannot gain interactive multiplayer access through
	// a pre-revocation session cookie, explicit participant list, or teamless
	// conversation.
	var callerTeam sql.NullString
	if err := q.QueryRowContext(ctx,
		`SELECT team_id FROM users WHERE email = $1 AND enabled = TRUE AND role <> 'viewer'`, callerEmail,
	).Scan(&callerTeam); err != nil {
		return false
	}
	// Explicitly added by the owner.
	if containsEmail(c.MultiplayerParticipants, callerEmail) {
		return true
	}
	cTeam := strings.TrimSpace(callerTeam.String)
	// Default whole-team / whole-fleet inclusion:
	// - When the conversation is stamped to a specific team (sharedTeam != ""),
	//   only members of that same team are included by default (plus anyone
	//   explicitly added in MultiplayerParticipants above).
	// - When the conversation has no team stamp (sharedTeam == ""), anyone on
	//   this Fleet instance can participate unless excluded.
	if sharedTeam != "" {
		if cTeam != sharedTeam {
			return false
		}
		if sharedTeamID.Valid {
			var currentTeamID int64
			if err := q.QueryRowContext(ctx, `SELECT id FROM teams WHERE name = $1`, sharedTeam).Scan(&currentTeamID); err != nil || currentTeamID != sharedTeamID.Int64 {
				return false
			}
		}
	}
	return true
}

// ListMultiplayerRoster returns the sorted list of enabled user emails on the
// conversation's team (or on the Fleet instance) so the Share / Multiplayer
// dialog can show who has access and let the owner remove or add people at will.
func (s *Store) ListMultiplayerRoster(ctx context.Context, callerEmail, convID string) ([]string, error) {
	callerEmail = normalizeEmail(callerEmail)
	conv, err := s.Get(ctx, callerEmail, convID)
	if err != nil {
		return nil, err
	}
	if conv == nil {
		conv, err = s.GetMultiplayerConversation(ctx, callerEmail, convID)
		if err != nil || conv == nil {
			return nil, ErrConversationNotFound
		}
	}
	var sharedTeam sql.NullString
	_ = s.db.QueryRowContext(ctx, `SELECT team_shared_with FROM conversations WHERE id = $1 AND deleted_at IS NULL`, convID).Scan(&sharedTeam)
	team := strings.TrimSpace(sharedTeam.String)
	if !conv.Multiplayer && team == "" {
		var ownerTeam sql.NullString
		_ = s.db.QueryRowContext(ctx, `SELECT team_id FROM users WHERE email = $1`, normalizeEmail(conv.UserEmail)).Scan(&ownerTeam)
		team = strings.TrimSpace(ownerTeam.String)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT email FROM users
		WHERE enabled = TRUE
		  AND role <> 'viewer'
		  AND ($1 = '' OR team_id = $1)
		ORDER BY email ASC`, team)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := make(map[string]bool)
	var out []string
	add := func(e string) {
		e = normalizeEmail(e)
		if e != "" && !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	addIfEnabled := func(e string) {
		e = normalizeEmail(e)
		if e == "" || seen[e] {
			return
		}
		var isEligible bool
		if err := s.db.QueryRowContext(ctx, `SELECT (enabled = TRUE AND role <> 'viewer') FROM users WHERE email = $1`, e).Scan(&isEligible); err == nil && isEligible {
			add(e)
		}
	}
	add(conv.UserEmail)
	addIfEnabled(callerEmail)
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err == nil {
			add(e)
		}
	}
	for _, p := range conv.MultiplayerParticipants {
		addIfEnabled(p)
	}
	for _, ex := range conv.MultiplayerExcluded {
		addIfEnabled(ex)
	}
	sort.Strings(out)
	return out, rows.Err()
}

// ListSharedAndMultiplayerConversations returns all active conversations owned
// by other users that the caller can access via either Multiplayer mode or
// read-only team visibility. Unlike ListTeamConversations, a caller without a
// team_id gets their accessible multiplayer conversations (or an empty slice)
// instead of ErrNoTeam.
func (s *Store) ListSharedAndMultiplayerConversations(ctx context.Context, callerEmail string) ([]Conversation, error) {
	callerEmail = normalizeEmail(callerEmail)
	if callerEmail == "" {
		return []Conversation{}, nil
	}
	u, err := s.GetUser(ctx, callerEmail)
	if errors.Is(err, ErrUserNotFound) || u == nil {
		return []Conversation{}, nil
	}
	if err != nil {
		return nil, err
	}
	callerTeam := strings.TrimSpace(u.TeamID)
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+conversationColumns+`, COALESCE(c.team_shared_with, ''), c.team_shared_with_id
		FROM conversations c
		WHERE c.deleted_at IS NULL
		  AND c.archived_at IS NULL
		  AND c.lockdown = FALSE
		  AND c.user_email <> $1
		  AND (
		    (c.multiplayer = TRUE
		      AND NOT (c.multiplayer_excluded::jsonb ? $1)
		      AND (
		        (c.multiplayer_participants::jsonb ? $1)
		        OR COALESCE(c.team_shared_with, '') = ''
		        OR ($2 <> '' AND c.team_shared_with = $2 AND (c.team_shared_with_id IS NULL OR c.team_shared_with_id = (SELECT t.id FROM teams t WHERE t.name = $2)))
		      )
		    )
		    OR (c.team_visible = TRUE AND $2 <> '' AND c.team_shared_with = $2 AND c.team_shared_with_id = (SELECT t.id FROM teams t WHERE t.name = $2))
		  )
		ORDER BY c.pinned DESC, c.updated_at DESC, c.id DESC`,
		callerEmail, callerTeam,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Conversation{}
	for rows.Next() {
		var c Conversation
		var optionalRaw []byte
		var approvalTimeout sql.NullInt64
		var thinkingRaw []byte
		var accountsRaw []byte
		var mpParticipantsRaw, mpExcludedRaw string
		var sharedTeam string
		var sharedTeamID sql.NullInt64
		if err := rows.Scan(
			&c.ID, &c.UserEmail, &c.Title, &c.Persona, &c.Model, &c.Pinned, &c.Lockdown,
			&c.CreatedAt, &c.UpdatedAt, &c.ArchivedAt, &c.TitleLocked, &optionalRaw,
			textArray{&c.Labels}, &approvalTimeout, &c.ShareToken, &c.ParentConversationID,
			&c.BranchPointMessageID, &thinkingRaw, &c.ProjectID, &accountsRaw,
			&c.TeamVisible, &c.AutoApprove, &c.Multiplayer, &mpParticipantsRaw, &mpExcludedRaw,
			&sharedTeam, &sharedTeamID,
		); err != nil {
			return nil, err
		}
		c.MultiplayerParticipants = scanEmailJSONArray(mpParticipantsRaw)
		c.MultiplayerExcluded = scanEmailJSONArray(mpExcludedRaw)
		if c.Multiplayer {
			if containsEmail(c.MultiplayerExcluded, callerEmail) || !s.canParticipateInMultiplayer(ctx, s.db, callerEmail, &c, strings.TrimSpace(sharedTeam), sharedTeamID) {
				if c.TeamVisible && callerTeam != "" && strings.TrimSpace(sharedTeam) == callerTeam {
					c.Multiplayer = false
				} else {
					continue
				}
			}
		}
		// Blank out owner-private fields before returning to a teammate.
		c.OptionalMCPServersEnabled = nil
		c.Labels = nil
		c.ShareToken = ""
		c.ParentConversationID = ""
		c.MCPAccounts = nil
		c.AutoApprove = false
		c.MultiplayerParticipants = nil
		c.MultiplayerExcluded = nil
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetConversationOwnerAndMultiplayer returns the owner, multiplayer state, and
// participant/exclusion lists for convID regardless of caller so the HTTP
// server can reconcile live turns, SSE streams, queued inputs, and pending
// approvals after indirect audience changes (project/team/user updates).
func (s *Store) GetConversationOwnerAndMultiplayer(ctx context.Context, convID string) (*Conversation, error) {
	if s == nil || s.db == nil || convID == "" {
		return nil, nil
	}
	c, err := scanConversation(s.db.QueryRowContext(ctx,
		`SELECT `+conversationColumns+` FROM conversations WHERE id = $1 AND deleted_at IS NULL`,
		convID,
	))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &c, nil
}

// ListActiveMultiplayerAndPendingConversationIDs returns conversation IDs that
// are currently multiplayer-enabled or have queued inputs / pending approvals /
// active schedules, used to reconcile access after indirect team/project/user
// changes.
func (s *Store) ListActiveMultiplayerAndPendingConversationIDs(ctx context.Context) ([]string, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id FROM conversations WHERE deleted_at IS NULL AND archived_at IS NULL AND multiplayer = TRUE
		UNION
		SELECT DISTINCT conversation_id FROM chat_input_queue WHERE state = 'queued'
		UNION
		SELECT DISTINCT conversation_id FROM approvals WHERE status = 'pending' OR (status = 'approved' AND is_err IS NULL)
		UNION
		SELECT DISTINCT conversation_id FROM chat_schedules WHERE status IN ('active', 'paused')
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if id != "" {
			out = append(out, id)
		}
	}
	return out, rows.Err()
}
