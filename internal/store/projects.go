package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Projects / Spaces (#509): the binding object that turns scattered
// primitives (per-conversation MCP opt-in, memories, team RBAC #237) into a
// shared team workspace. A project carries standing instructions, a curated
// connector selection, default persona/model, and a shared memory scope;
// every conversation created in it inherits that context.
//
// Membership model (deliberately NOT a new membership table): a project with
// a TeamID is shared with every user whose users.team_id matches (the ADR-0013
// trust-group), plus the owner; an empty TeamID is a personal project. Only
// the owner mutates the definition — members read and use it.

// Project is one shared workspace definition.
type Project struct {
	ID           string `json:"id"`
	OwnerEmail   string `json:"owner_email"`
	Name         string `json:"name"`
	Instructions string `json:"instructions,omitempty"`
	TeamID       string `json:"team_id,omitempty"`
	// DefaultPersona / DefaultModel seed a new conversation created in the
	// project when the creator did not choose their own.
	DefaultPersona string `json:"default_persona,omitempty"`
	DefaultModel   string `json:"default_model,omitempty"`
	// MCPServers is the curated optional-MCP enablement inherited by new
	// conversations (names from the global catalog; credentials host-side).
	MCPServers []string `json:"mcp_servers"`
	// Pinned floats the project to the top of the rail's Projects section.
	// Owner-only like every other project mutation; on a team-shared project
	// the owner's pin orders the rail for all members.
	Pinned    bool  `json:"pinned"`
	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
}

// MemberOf reports whether the user (email + resolved team) can see/use the
// project: the owner always; otherwise a shared team_id. Edit rights are
// owner-only and enforced by the owner-scoped UPDATE/DELETE statements.
func (p *Project) MemberOf(email, teamID string) bool {
	if p.OwnerEmail == normalizeEmail(email) {
		return true
	}
	return p.TeamID != "" && teamID == p.TeamID
}

const (
	maxProjectNameLen         = 128
	maxProjectInstructionsLen = 8000
)

const projectColumns = `id, owner_email, name, instructions, team_id, default_persona, default_model, mcp_servers, pinned, created_at, updated_at`

func scanProject(scanner interface{ Scan(...any) error }) (*Project, error) {
	var (
		p       Project
		mcpsRaw []byte
	)
	if err := scanner.Scan(&p.ID, &p.OwnerEmail, &p.Name, &p.Instructions, &p.TeamID,
		&p.DefaultPersona, &p.DefaultModel, &mcpsRaw, &p.Pinned, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	if len(mcpsRaw) > 0 {
		_ = json.Unmarshal(mcpsRaw, &p.MCPServers)
	}
	if p.MCPServers == nil {
		p.MCPServers = []string{}
	}
	return &p, nil
}

// CreateProject persists a project owned by ownerEmail. TeamID must be the
// OWNER'S resolved team (the handler enforces this — a caller can never share
// into a team it does not belong to).
func (s *Store) CreateProject(ctx context.Context, p *Project) (*Project, error) {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || len(p.Name) > maxProjectNameLen {
		return nil, errors.New("project name required (≤128 chars)")
	}
	if len(p.Instructions) > maxProjectInstructionsLen {
		return nil, errors.New("project instructions too long (≤8000 chars)")
	}
	if p.MCPServers == nil {
		p.MCPServers = []string{}
	}
	mcps, err := json.Marshal(p.MCPServers)
	if err != nil {
		return nil, err
	}
	p.ID = uuid.NewString()
	p.OwnerEmail = normalizeEmail(p.OwnerEmail)
	now := time.Now().Unix()
	p.CreatedAt, p.UpdatedAt = now, now
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO projects (id, owner_email, name, instructions, team_id, default_persona, default_model, mcp_servers, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)`,
		p.ID, p.OwnerEmail, p.Name, p.Instructions, p.TeamID, p.DefaultPersona, p.DefaultModel, string(mcps), now,
	)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// GetProject fetches one project (no membership filter — the caller checks
// MemberOf with the requesting user's team).
func (s *Store) GetProject(ctx context.Context, id string) (*Project, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = $1`, id)
	p, err := scanProject(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return p, nil
}

// ListProjectsForUser returns the projects the user can see: owned, plus any
// shared with the user's team. Newest first.
func (s *Store) ListProjectsForUser(ctx context.Context, email, teamID string) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+projectColumns+` FROM projects
		 WHERE owner_email = $1 OR (team_id != '' AND team_id = $2)
		 ORDER BY created_at DESC`,
		normalizeEmail(email), teamID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// ProjectPatch is a partial update; nil = untouched. TeamShared toggles team
// sharing (true → the owner's CURRENT team, resolved by the handler into
// TeamID; false → personal).
type ProjectPatch struct {
	Name           *string
	Instructions   *string
	TeamID         *string
	DefaultPersona *string
	DefaultModel   *string
	MCPServers     []string // nil = untouched; empty slice = clear
	Pinned         *bool
}

// ── the filing invariant: project_id never outlives its owner's access ──
//
// A chat's project_id must NEVER point at a project its owner cannot see, and
// no revocation may reach that state by DELETING a chat: it unfiles it
// (project_id → NULL), which is where the chat came from and where the rail
// already has a home for it (Temporary).
//
// The bug this exists to close: the rail's own lists are read through the
// caller's visible projects, so a chat filed in a project the caller can no
// longer see is rendered by NOTHING — not Projects, not Temporary, not
// Archived. Turning off "Share with my team" on a project therefore made every
// teammate's chat in it vanish from their own rail. The data was intact (the
// owner re-ticking the box brought them back), which is precisely what made it
// so bad: the teammate lost access to chats THEY own, with no trace and no
// explanation, for as long as someone else's setting stayed off.
//
// Unfiled chats become temporary again — TTL-eligible, expiring unless pinned.
// That is the accepted cost and it is exactly what deleting a project already
// does to members' chats; updated_at is bumped for the same reason
// DeleteProject bumps it, so the retention clock starts when access is lost
// instead of leaving an old chat instantly sweep-eligible.
//
// Two shapes of the same rule, one per revocation axis:
//
//   - the PROJECT changed hands or audience → unfileChatsWithoutProjectAccessTx
//     (UpdateProject, TransferProjectOwnership),
//   - the USER's team changed → unfileChatsInInaccessibleProjectsTx
//     (SetOwnTeam / SetUserRoleTeam, through unshareOnTeamChangeTx).
//
// Both take the caller's tx: the unfile commits with the revocation that caused
// it, or neither happens. Migration 055 backfills the rows written before this
// rule existed.
//
// "Can see" is ListProjectsForUser's rule, restated: the project's owner
// always, otherwise an EXACT match between the project's non-empty team_id and
// the chat owner's users.team_id. Exact, because every other team gate in the
// store is exact — a loose compare here would leave a chat filed in a project
// the read path refuses to list.

// unfileChatsWithoutProjectAccessTx unfiles every conversation filed in
// projectID whose OWNER can no longer see it, reading the project row as the
// caller's transaction has already left it.
func unfileChatsWithoutProjectAccessTx(ctx context.Context, tx *sql.Tx, projectID string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE conversations c SET
			project_id = NULL,
			team_visible = FALSE,
			team_shared_with = NULL,
			updated_at = $2
		WHERE c.project_id = $1
		  AND NOT EXISTS (
			SELECT 1 FROM projects p
			 WHERE p.id = $1
			   AND (p.owner_email = c.user_email
			        OR (p.team_id <> '' AND p.team_id = (
			              SELECT u.team_id FROM users u WHERE u.email = c.user_email)))
		  )`,
		projectID, time.Now().Unix())
	return err
}

// unfileChatsInInaccessibleProjectsTx unfiles every conversation owned by email
// that is filed in a project the user will not be able to see once their team
// is newTeam (empty = no team).
//
// newTeam is passed rather than read from users, because the callers run this
// BEFORE their own write to users.team_id — the whole point is that the unfile
// and the team write commit together.
func unfileChatsInInaccessibleProjectsTx(ctx context.Context, tx *sql.Tx, email, newTeam string) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE conversations c SET
			project_id = NULL,
			team_visible = FALSE,
			team_shared_with = NULL,
			updated_at = $2
		WHERE c.user_email = $1
		  AND c.project_id IS NOT NULL
		  AND NOT EXISTS (
			SELECT 1 FROM projects p
			 WHERE p.id = c.project_id
			   AND (p.owner_email = c.user_email
			        OR (p.team_id <> '' AND p.team_id = NULLIF($3::text, '')))
		  )`,
		normalizeEmail(email), time.Now().Unix(), strings.TrimSpace(newTeam))
	return err
}

// UpdateProject applies a partial update, owner-only (the WHERE enforces it).
//
// Changing who the project is shared with also unshares every team-visible
// chat in it whose named audience no longer matches, in the same transaction:
// the project is where a teammate would have found them, and ADR-0057's
// pairing says a team-shared chat never outlives the place it appears. That
// covers turning sharing off AND handing the project to a different team —
// the second case is reachable whenever the owner's own team changes.
//
// The same change also revokes ACCESS for everyone who saw the project through
// the old team, so every chat of theirs filed here is unfiled in the same
// transaction (see the filing-invariant block above). Unshared and still filed
// was the wrong half of the fix: the chat is its owner's, it stays theirs, and
// it goes back to their Temporary list rather than into a project only somebody
// else can see.
func (s *Store) UpdateProject(ctx context.Context, ownerEmail, id string, patch ProjectPatch) (*Project, error) {
	if patch.Name != nil {
		n := strings.TrimSpace(*patch.Name)
		if n == "" || len(n) > maxProjectNameLen {
			return nil, errors.New("project name required (≤128 chars)")
		}
		patch.Name = &n
	}
	if patch.Instructions != nil && len(*patch.Instructions) > maxProjectInstructionsLen {
		return nil, errors.New("project instructions too long (≤8000 chars)")
	}
	var mcpsArg any
	if patch.MCPServers != nil {
		raw, err := json.Marshal(patch.MCPServers)
		if err != nil {
			return nil, err
		}
		mcpsArg = string(raw)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit
	row := tx.QueryRowContext(ctx,
		`UPDATE projects SET
			name            = COALESCE($1::text, name),
			instructions    = COALESCE($2::text, instructions),
			team_id         = COALESCE($3::text, team_id),
			default_persona = COALESCE($4::text, default_persona),
			default_model   = COALESCE($5::text, default_model),
			mcp_servers     = COALESCE($6::jsonb, mcp_servers),
			pinned          = COALESCE($7::boolean, pinned),
			updated_at      = $8
		 WHERE id = $9 AND owner_email = $10
		 RETURNING `+projectColumns,
		patch.Name, patch.Instructions, patch.TeamID, patch.DefaultPersona, patch.DefaultModel,
		mcpsArg, patch.Pinned, time.Now().Unix(), id, normalizeEmail(ownerEmail),
	)
	p, err := scanProject(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("project not found (or not the owner)")
		}
		return nil, err
	}
	if patch.TeamID != nil {
		// Unshare every chat here whose named audience is no longer this
		// project's team — which covers turning sharing off (new team_id '',
		// so nothing matches) AND re-sharing the project with a DIFFERENT
		// team, where the old audience would otherwise keep reading a chat
		// that no longer appears in anyone's Team section.
		if _, err := tx.ExecContext(ctx,
			`UPDATE conversations SET team_visible = FALSE, team_shared_with = NULL, updated_at = $1
			 WHERE project_id = $2 AND team_visible = TRUE
			   AND team_shared_with IS DISTINCT FROM NULLIF($3::text, '')`,
			time.Now().Unix(), id, p.TeamID); err != nil {
			return nil, err
		}
		// …and unfile every chat whose owner just lost sight of the project.
		// Making it personal strands every OTHER member's chats; re-pointing it
		// at a different team strands the old team's. Never a delete: the chat
		// belongs to its owner and lands back in their Temporary list.
		if err := unfileChatsWithoutProjectAccessTx(ctx, tx, id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return p, nil
}

// DeleteProject removes a project (owner-only): conversations are DETACHED
// (the history belongs to their users), the project's shared memories are
// deleted with it (they are project state).
func (s *Store) DeleteProject(ctx context.Context, ownerEmail, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx,
		`DELETE FROM projects WHERE id = $1 AND owner_email = $2`, id, normalizeEmail(ownerEmail))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("project not found (or not the owner)")
	}
	// Detaching also unshares: a chat that leaves a project has nowhere for a
	// teammate to find it, so team_visible must not outlive the project
	// (ADR-0057, the same pairing SetConversationProject enforces).
	//
	// updated_at is bumped, and that is not cosmetic. project_id IS NULL is
	// exactly what re-arms the TTL sweep and the unpinned cap, so detaching a
	// four-month-old chat with its original timestamp made it sweep-eligible
	// AT ONCE — the next turn any user on the box takes hard-deletes it. The
	// confirm promises members their chats "become temporary, and expire
	// unless pinned"; without this bump there was no window in which to pin
	// one. Now the clock starts when they lose the project.
	//
	// This is also the filing invariant at its simplest: the project stops
	// existing, so NOBODY can see it, so every chat in it is unfiled — which
	// is why delete never needed the access-scoped sweep UpdateProject and
	// TransferProjectOwnership run. It is the behavior the other revocation
	// paths were brought into line with, not an exception to them.
	if _, err := tx.ExecContext(ctx,
		`UPDATE conversations SET project_id = NULL, team_visible = FALSE, team_shared_with = NULL, updated_at = $2
		 WHERE project_id = $1`, id, time.Now().Unix()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM memories WHERE project_id = $1`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// CreateProjectConversation inserts a conversation bound to a project. The
// handler validates membership + resolves inherited persona/model/connectors
// before calling.
func (s *Store) CreateProjectConversation(ctx context.Context, userEmail, title, persona, model string, lockdown bool, projectID string, mcpServers []string) (*Conversation, error) {
	id := uuid.NewString()
	now := time.Now().Unix()
	if mcpServers == nil {
		mcpServers = []string{}
	}
	mcps, err := json.Marshal(mcpServers)
	if err != nil {
		return nil, err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO conversations (id, user_email, title, persona, model, pinned, lockdown, project_id, optional_mcp_servers_enabled, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, FALSE, $6, $7, $8, $9, $9)`,
		id, userEmail, title, persona, model, lockdown, projectID, string(mcps), now,
	)
	if err != nil {
		return nil, err
	}
	return &Conversation{
		ID: id, UserEmail: userEmail, Title: title, Persona: persona, Model: model,
		Lockdown: lockdown, ProjectID: projectID, OptionalMCPServersEnabled: mcpServers,
		CreatedAt: now, UpdatedAt: now,
	}, nil
}

// SetConversationProject files an existing conversation into a project, or
// unfiles it (projectID "" → NULL). The handler validates membership before
// calling — this mirrors CreateProjectConversation's split, and the
// owner-scoped WHERE keeps a caller from moving someone else's chat. Unlike
// creation, re-filing inherits nothing (persona/model/connectors stay as they
// are); it only binds the project's instructions + shared memory from the
// next turn on. Filing also clears the pin (mirroring SetArchived): a
// project chat lives only under its project, so a stale pinned flag would
// make the chat pop back into Pinned on a later unfile. A soft-deleted
// conversation is not mutable (#596).
//
// Filing also enforces the ADR-0057 pairing: a team-shared chat must live in a
// project shared WITH THE TEAM IT WAS SHARED WITH, so moving one to no
// project, to a personal one, or to a project belonging to a DIFFERENT team
// clears team_visible in the same statement. Without that a chat stays
// readable by the team while no surface lists it (the Team section matches on
// the caller's team, which is no longer the project's), and its owner has no
// affordance left to revoke it. Testing "the destination is shared with SOME
// team" is not enough — that is the state a user reaches by refiling across
// teams after an admin moves them.
func (s *Store) SetConversationProject(ctx context.Context, userEmail, convID, projectID string) error {
	var pid any // NULL when unfiling, matching the column's created-without-project state
	if projectID != "" {
		pid = projectID
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit
	if projectID != "" {
		// Lock the destination project row for the length of this move. Under
		// READ COMMITTED the pairing CASE below reads `projects` as it was
		// when the statement started, so a concurrent "stop sharing this
		// project" — whose own unshare sweeps `WHERE project_id = ...` and
		// cannot see a chat that has not landed yet — left a team-visible chat
		// in a project that is no longer shared. The lock makes the two
		// statements take turns. A missing project is not an error here: the
		// UPDATE's own FK/EXISTS handling still decides the outcome.
		if _, err := tx.ExecContext(ctx,
			`SELECT 1 FROM projects WHERE id = $1 FOR SHARE`, projectID); err != nil {
			return err
		}
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE conversations SET
			project_id = $1,
			pinned = (CASE WHEN $1::text IS NULL THEN pinned ELSE FALSE END),
			team_visible = (CASE
				WHEN $1::text IS NOT NULL
				 AND EXISTS (SELECT 1 FROM projects p
				              WHERE p.id = $1::text
				                AND p.team_id <> ''
				                AND p.team_id = conversations.team_shared_with)
				THEN team_visible ELSE FALSE END),
			team_shared_with = (CASE
				WHEN $1::text IS NOT NULL
				 AND EXISTS (SELECT 1 FROM projects p
				              WHERE p.id = $1::text
				                AND p.team_id <> ''
				                AND p.team_id = conversations.team_shared_with)
				THEN team_shared_with ELSE NULL END),
			updated_at = $2
		 WHERE id = $3 AND user_email = $4 AND deleted_at IS NULL`,
		pid, time.Now().Unix(), convID, userEmail,
	)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrConversationNotFound
	}
	return tx.Commit()
}

// ── project-scoped memory (#509 + #515 scope-awareness) ──

// CreateProjectMemory adds a SHARED memory to the project. creatorEmail is
// provenance (who wrote it); the row belongs to the project, not the creator's
// personal memory.
func (s *Store) CreateProjectMemory(ctx context.Context, projectID, creatorEmail, content, kind string) (*Memory, error) {
	content = normalizeMemoryContent(content)
	if content == "" {
		return nil, errors.New("memory content required")
	}
	kind = NormalizeMemoryKind(kind)
	id := uuid.NewString()
	now := time.Now().Unix()
	row := s.db.QueryRowContext(ctx,
		`INSERT INTO memories (id, user_email, project_id, content, source, kind, origin, learned_at, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, 'manual', $5, 'manual', $6, $6, $6)
		 RETURNING `+memoryColumns,
		id, normalizeEmail(creatorEmail), projectID, content, kind, now,
	)
	return scanMemory(row)
}

// ListProjectMemories returns the project's shared memories (active first,
// like the personal list).
func (s *Store) ListProjectMemories(ctx context.Context, projectID string) ([]Memory, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+memoryColumns+`
		 FROM memories WHERE project_id = $1
		 ORDER BY (retired_at IS NOT NULL) ASC, pinned DESC, updated_at DESC, id DESC`,
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// GetProjectMemory fetches one shared memory of the project, or (nil, nil)
// when the id names no row in it. The handler reads it to decide who may
// mutate the entry: its writer, or the project owner (ADR-0057).
func (s *Store) GetProjectMemory(ctx context.Context, projectID, memoryID string) (*Memory, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+memoryColumns+` FROM memories WHERE id = $1 AND project_id = $2`,
		memoryID, projectID)
	m, err := scanMemory(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return m, nil
}

// UpdateProjectMemory applies a partial update to one team learning. The
// project-scoped WHERE is the mirror image of UpdateMemory's `project_id IS
// NULL`: the shared API can only touch shared rows, so neither scope can reach
// the other. WHO may call this (the entry's writer, or the project owner) is
// the handler's gate — the store enforces only the scope.
//
// Retirement is the intended "remove" for a team learning: the entry stops
// being injected but the record — and who wrote it — survives.
func (s *Store) UpdateProjectMemory(ctx context.Context, projectID, memoryID string, patch MemoryPatch) (*Memory, error) {
	if patch.Content == nil && patch.Kind == nil && patch.Pinned == nil &&
		patch.Retired == nil && patch.ValidFrom == nil && patch.ValidTo == nil {
		return nil, errors.New("empty memory patch")
	}
	var content *string
	if patch.Content != nil {
		c := normalizeMemoryContent(*patch.Content)
		if c == "" {
			return nil, errors.New("memory content required")
		}
		content = &c
	}
	var kind *string
	if patch.Kind != nil {
		k := NormalizeMemoryKind(*patch.Kind)
		kind = &k
	}
	now := time.Now().Unix()
	row := s.db.QueryRowContext(ctx,
		`UPDATE memories SET
			content    = COALESCE($1::text, content),
			kind       = COALESCE($2::text, kind),
			pinned     = COALESCE($3::boolean, pinned),
			retired_at = CASE
				WHEN $4::boolean IS NULL THEN retired_at
				WHEN $4::boolean THEN COALESCE(retired_at, $5)
				ELSE NULL END,
			retired_by = CASE
				WHEN $4::boolean IS NULL THEN retired_by
				WHEN $4::boolean THEN retired_by
				ELSE NULL END,
			valid_from = CASE
				WHEN $6::bigint IS NULL THEN valid_from
				WHEN $6::bigint = 0 THEN NULL
				ELSE $6::bigint END,
			valid_to = CASE
				WHEN $7::bigint IS NULL THEN valid_to
				WHEN $7::bigint = 0 THEN NULL
				ELSE $7::bigint END,
			updated_at = $5
		 WHERE id = $8 AND project_id = $9
		 RETURNING `+memoryColumns,
		content, kind, patch.Pinned, patch.Retired, now, patch.ValidFrom, patch.ValidTo, memoryID, projectID,
	)
	m, err := scanMemory(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("memory not found")
		}
		return nil, err
	}
	return m, nil
}

// MoveMemoryToProject promotes one of the caller's PERSONAL memories into a
// project's shared memory — the migration path for team facts saved to
// personal memory before the destination picker existed (Item D5). It MOVES
// rather than copies: leaving the personal row behind would inject the same
// fact twice in every project chat.
//
// The caller's ownership of the personal row is the store-side gate
// (`user_email = $2 AND project_id IS NULL`); project membership is the
// handler's. user_email survives the move as provenance — the project list
// shows who contributed the learning — exactly as CreateProjectMemory records
// it. A proposal (source 'proposed') is refused: only a fact the user has
// actually accepted can be handed to the team.
func (s *Store) MoveMemoryToProject(ctx context.Context, userEmail, memoryID, projectID string) (*Memory, error) {
	if projectID == "" {
		return nil, errors.New("project id required")
	}
	row := s.db.QueryRowContext(ctx,
		`UPDATE memories SET project_id = $1, updated_at = $2
		 WHERE id = $3 AND user_email = $4 AND project_id IS NULL AND source <> 'proposed'
		 RETURNING `+memoryColumns,
		projectID, time.Now().Unix(), memoryID, normalizeEmail(userEmail),
	)
	m, err := scanMemory(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("memory not found")
		}
		return nil, err
	}
	return m, nil
}

// AcceptMemoryProposalIntoProject accepts one of the caller's pending memory
// proposals INTO a project's shared memory instead of their personal memory
// (Item D1: the "Save to: My memory | Team learnings" choice on the approval
// card). One statement, so a proposal is either accepted somewhere or nowhere.
//
// The supersede machinery deliberately does not run here: a proposal's
// supersedes claim is resolved against the user's PERSONAL memories, and
// retiring a personal fact because its replacement was handed to the team
// would silently drop context from every one of that user's other chats. The
// accepted entry simply becomes a team learning; the personal fact it
// contradicts stays for the user to retire themselves.
func (s *Store) AcceptMemoryProposalIntoProject(ctx context.Context, userEmail, memoryID, projectID string) (*Memory, error) {
	if projectID == "" {
		return nil, errors.New("project id required")
	}
	row := s.db.QueryRowContext(ctx,
		`UPDATE memories SET source = 'chat', project_id = $1, updated_at = $2
		 WHERE id = $3 AND user_email = $4 AND source = 'proposed'
		 RETURNING `+memoryColumns,
		projectID, time.Now().Unix(), memoryID, normalizeEmail(userEmail),
	)
	m, err := scanMemory(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("memory proposal not found")
		}
		return nil, err
	}
	return m, nil
}

// DeleteProjectMemory removes one shared memory from the project. Any member
// may delete (the handler enforces membership); the project-scoped WHERE keeps
// it from touching personal rows.
func (s *Store) DeleteProjectMemory(ctx context.Context, projectID, memoryID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM memories WHERE id = $1 AND project_id = $2`, memoryID, projectID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("memory not found")
	}
	return nil
}

// ListProjectConversationsForUser returns the CALLER'S OWN live conversations
// in the project, newest first. Deliberately owner-scoped even though project
// members share the project definition: conversations stay private to their
// creators (#237's rule), so a project surface must never enumerate another
// member's chats — each member sees only their own slice.
func (s *Store) ListProjectConversationsForUser(ctx context.Context, userEmail, projectID string) ([]Conversation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+conversationColumns+` FROM conversations
		 WHERE user_email = $1 AND project_id = $2 AND deleted_at IS NULL AND archived_at IS NULL
		 ORDER BY updated_at DESC, id DESC`,
		userEmail, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanConversationRows(rows)
}

// ListProjectConversationPreviews returns, per conversation id, a short
// plaintext snippet of the LAST text message in each of the caller's own
// conversations in the project — the project home's 1–2 line chat history.
// One lateral-join query, not N; the same owner scoping as
// ListProjectConversationsForUser. Conversations with no text messages yet
// are simply absent from the map.
func (s *Store) ListProjectConversationPreviews(ctx context.Context, userEmail, projectID string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT c.id, m.role, m.content FROM conversations c
		 JOIN LATERAL (
		   SELECT role, content FROM messages
		   WHERE conversation_id = c.id AND type = 'text'
		   ORDER BY id DESC LIMIT 1
		 ) m ON true
		 WHERE c.user_email = $1 AND c.project_id = $2 AND c.deleted_at IS NULL AND c.archived_at IS NULL`,
		userEmail, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, role string
		var raw []byte
		if err := rows.Scan(&id, &role, &raw); err != nil {
			return nil, err
		}
		var c struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			continue
		}
		text := strings.Join(strings.Fields(c.Text), " ") // collapse newlines/runs
		if text == "" {
			continue
		}
		const maxPreview = 200
		if r := []rune(text); len(r) > maxPreview {
			text = string(r[:maxPreview]) + "…"
		}
		if role == "user" {
			text = "You: " + text
		}
		out[id] = text
	}
	return out, rows.Err()
}

// ListProjectConversationIDs returns the ids of conversations currently in
// the project — the runtime-state references the export endpoint reports.
func (s *Store) ListProjectConversationIDs(ctx context.Context, projectID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM conversations WHERE project_id = $1 AND deleted_at IS NULL ORDER BY updated_at DESC`, projectID)
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
		out = append(out, id)
	}
	return out, rows.Err()
}
