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
	CreatedAt  int64    `json:"created_at"`
	UpdatedAt  int64    `json:"updated_at"`
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

const projectColumns = `id, owner_email, name, instructions, team_id, default_persona, default_model, mcp_servers, created_at, updated_at`

func scanProject(scanner interface{ Scan(...any) error }) (*Project, error) {
	var (
		p       Project
		mcpsRaw []byte
	)
	if err := scanner.Scan(&p.ID, &p.OwnerEmail, &p.Name, &p.Instructions, &p.TeamID,
		&p.DefaultPersona, &p.DefaultModel, &mcpsRaw, &p.CreatedAt, &p.UpdatedAt); err != nil {
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
}

// UpdateProject applies a partial update, owner-only (the WHERE enforces it).
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
	row := s.db.QueryRowContext(ctx,
		`UPDATE projects SET
			name            = COALESCE($1::text, name),
			instructions    = COALESCE($2::text, instructions),
			team_id         = COALESCE($3::text, team_id),
			default_persona = COALESCE($4::text, default_persona),
			default_model   = COALESCE($5::text, default_model),
			mcp_servers     = COALESCE($6::jsonb, mcp_servers),
			updated_at      = $7
		 WHERE id = $8 AND owner_email = $9
		 RETURNING `+projectColumns,
		patch.Name, patch.Instructions, patch.TeamID, patch.DefaultPersona, patch.DefaultModel,
		mcpsArg, time.Now().Unix(), id, normalizeEmail(ownerEmail),
	)
	p, err := scanProject(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("project not found (or not the owner)")
		}
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
	if _, err := tx.ExecContext(ctx,
		`UPDATE conversations SET project_id = NULL WHERE project_id = $1`, id); err != nil {
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
