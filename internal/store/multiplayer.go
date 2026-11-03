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
	ownerEmail = normalizeEmail(ownerEmail)
	now := time.Now().Unix()

	cur, err := s.Get(ctx, ownerEmail, convID)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, ErrConversationNotFound
	}
	if multiplayer && (cur.ArchivedAt != nil || cur.Lockdown) {
		return nil, ErrArchivedNotShareable
	}

	nextParticipants := cur.MultiplayerParticipants
	if participants != nil {
		nextParticipants = normalizeEmailSlice(participants, ownerEmail)
	}
	nextExcluded := cur.MultiplayerExcluded
	if excluded != nil {
		nextExcluded = normalizeEmailSlice(excluded, ownerEmail)
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

	if !multiplayer {
		// Turning off multiplayer clears multiplayer = FALSE. Preserve
		// team_visible and the team audience stamp ONLY when the conversation
		// currently lives in a project that is actually shared with that same
		// team; unfiled chats and personal-project chats become private again.
		_, err := s.db.ExecContext(ctx, `
			UPDATE conversations SET
				multiplayer = FALSE,
				multiplayer_participants = $1,
				multiplayer_excluded = $2,
				team_visible = CASE
					WHEN EXISTS (
						SELECT 1 FROM projects p
						WHERE p.id = conversations.project_id
						  AND p.deleted_at IS NULL
						  AND p.team_shared_with = conversations.team_shared_with
						  AND p.team_shared_with_id = conversations.team_shared_with_id
					) THEN team_visible
					ELSE FALSE
				END,
				team_shared_with = CASE
					WHEN EXISTS (
						SELECT 1 FROM projects p
						WHERE p.id = conversations.project_id
						  AND p.deleted_at IS NULL
						  AND p.team_shared_with = conversations.team_shared_with
						  AND p.team_shared_with_id = conversations.team_shared_with_id
					) THEN team_shared_with
					ELSE NULL
				END,
				team_shared_with_id = CASE
					WHEN EXISTS (
						SELECT 1 FROM projects p
						WHERE p.id = conversations.project_id
						  AND p.deleted_at IS NULL
						  AND p.team_shared_with = conversations.team_shared_with
						  AND p.team_shared_with_id = conversations.team_shared_with_id
					) THEN team_shared_with_id
					ELSE NULL
				END,
				updated_at = $3
			WHERE id = $4 AND user_email = $5 AND deleted_at IS NULL`,
			partJSON, exclJSON, now, convID, ownerEmail,
		)
		if err != nil {
			return nil, err
		}
		return s.Get(ctx, ownerEmail, convID)
	}

	var ownerTeam sql.NullString
	if err := s.db.QueryRowContext(ctx,
		`SELECT team_id FROM users WHERE email = $1`, ownerEmail,
	).Scan(&ownerTeam); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	team := strings.TrimSpace(ownerTeam.String)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	if team != "" {
		if err := lockTeamNamesTx(ctx, tx, team); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
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
		); err != nil {
			return nil, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `
			UPDATE conversations SET
				multiplayer = TRUE,
				multiplayer_participants = $1,
				multiplayer_excluded = $2,
				updated_at = $3
			WHERE id = $4 AND user_email = $5 AND deleted_at IS NULL AND archived_at IS NULL AND lockdown = FALSE`,
			partJSON, exclJSON, now, convID, ownerEmail,
		); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.Get(ctx, ownerEmail, convID)
}

// UpdateConversationMultiplayerAccess adds and/or removes specific user emails
// on a conversation's multiplayer access list.
func (s *Store) UpdateConversationMultiplayerAccess(ctx context.Context, ownerEmail, convID string, addEmails, removeEmails []string) (*Conversation, error) {
	ownerEmail = normalizeEmail(ownerEmail)
	cur, err := s.Get(ctx, ownerEmail, convID)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, ErrConversationNotFound
	}
	partMap := make(map[string]bool)
	for _, p := range cur.MultiplayerParticipants {
		partMap[normalizeEmail(p)] = true
	}
	exclMap := make(map[string]bool)
	for _, e := range cur.MultiplayerExcluded {
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
	return s.SetConversationMultiplayer(ctx, ownerEmail, convID, cur.Multiplayer, parts, excls)
}

// GetMultiplayerConversation returns the conversation when convID has
// multiplayer = TRUE (and is not deleted, archived, or lockdown) AND
// callerEmail is allowed to participate in it. Returns (nil, nil) otherwise.
func (s *Store) GetMultiplayerConversation(ctx context.Context, callerEmail, convID string) (*Conversation, error) {
	callerEmail = normalizeEmail(callerEmail)
	if callerEmail == "" || convID == "" {
		return nil, nil
	}
	var sharedTeam sql.NullString
	var sharedTeamID sql.NullInt64
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
	if err := s.db.QueryRowContext(ctx,
		`SELECT team_shared_with, team_shared_with_id FROM conversations WHERE id = $1`, convID,
	).Scan(&sharedTeam, &sharedTeamID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if !s.canParticipateInMultiplayer(ctx, callerEmail, &c, strings.TrimSpace(sharedTeam.String), sharedTeamID) {
		return nil, nil
	}
	if normalizeEmail(c.UserEmail) != callerEmail {
		// Never leak owner-private fields to a non-owner participant in HTTP
		// JSON responses, while retaining an unexported (`json:"-"`) copy of
		// the enabled optional connector names so participant turns can mount
		// connectors they also have access to.
		c.ParticipantOptionalMCPServers = c.OptionalMCPServersEnabled
		c.OptionalMCPServersEnabled = nil
		c.Labels = nil
		c.ShareToken = ""
		c.ParentConversationID = ""
		c.MCPAccounts = nil
		c.AutoApprove = false
	}
	return &c, nil
}

func (s *Store) canParticipateInMultiplayer(ctx context.Context, callerEmail string, c *Conversation, sharedTeam string, sharedTeamID sql.NullInt64) bool {
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
	// Explicitly added by the owner.
	if containsEmail(c.MultiplayerParticipants, callerEmail) {
		return true
	}
	var callerTeam sql.NullString
	_ = s.db.QueryRowContext(ctx,
		`SELECT team_id FROM users WHERE email = $1`, callerEmail,
	).Scan(&callerTeam)
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
			if err := s.db.QueryRowContext(ctx, `SELECT id FROM teams WHERE name = $1`, sharedTeam).Scan(&currentTeamID); err != nil || currentTeamID != sharedTeamID.Int64 {
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
	var ownerTeam sql.NullString
	_ = s.db.QueryRowContext(ctx, `SELECT team_id FROM users WHERE email = $1`, normalizeEmail(conv.UserEmail)).Scan(&ownerTeam)
	team := strings.TrimSpace(ownerTeam.String)

	rows, err := s.db.QueryContext(ctx, `
		SELECT email FROM users
		WHERE ($1 = '' OR team_id = $1)
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
	add(conv.UserEmail)
	add(callerEmail)
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err == nil {
			add(e)
		}
	}
	for _, p := range conv.MultiplayerParticipants {
		add(p)
	}
	for _, ex := range conv.MultiplayerExcluded {
		add(ex)
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
	var callerTeam string
	if u, err := s.GetUser(ctx, callerEmail); err == nil && u != nil {
		callerTeam = strings.TrimSpace(u.TeamID)
	}
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
			if containsEmail(c.MultiplayerExcluded, callerEmail) {
				continue
			}
			if !s.canParticipateInMultiplayer(ctx, callerEmail, &c, strings.TrimSpace(sharedTeam), sharedTeamID) {
				continue
			}
		}
		// Blank out owner-private fields before returning to a teammate.
		c.OptionalMCPServersEnabled = nil
		c.Labels = nil
		c.ShareToken = ""
		c.ParentConversationID = ""
		c.MCPAccounts = nil
		c.AutoApprove = false
		out = append(out, c)
	}
	return out, rows.Err()
}
