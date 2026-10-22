package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// User-authored Agent Skills (the skills builder, docs/SKILLS.md phase 2).
// DB-owned, per-user: a user's ACTIVE skills are materialized into their own
// conversation workspaces at turn start and listed in their prompt roster —
// they never enter other users' runs and never touch the operator's bundle.
// Graduating a good skill to the whole deployment stays an operator action
// (copy it into the bundle's skills/ dir).

// User-skill statuses. A PROPOSED skill was drafted by the agent
// (propose_skill) and is inert until its owner approves it in the builder —
// same trust posture as memory/note proposals: the agent suggests, the human
// decides.
const (
	UserSkillStatusActive   = "active"
	UserSkillStatusDisabled = "disabled"
	UserSkillStatusProposed = "proposed"

	UserSkillVisibilityPrivate   = "private"
	UserSkillVisibilityWorkspace = "workspace"
)

// userSkillNameShape mirrors the bundle skill-name contract
// (clientconfig.validSkillName): lowercase kebab, ≤64 chars. The shape halves are
// held in step by TestValidSkillNameAgreesWithUserSkillShape, which compares the
// two over a generated corpus — change this pattern and that test tells you which
// side moved. Only the length cap lives on one side alone: the bundle loader
// reports an over-long name separately, against clientconfig.maxSkillNameLen.
var userSkillNameShape = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

// Size caps: a skill is instructions, not a document store.
const (
	maxUserSkillDescription = 1024
	maxUserSkillBody        = 64 * 1024
)

// ErrUserSkillInvalid is returned for a malformed skill write.
var ErrUserSkillInvalid = errors.New("invalid skill")

// ErrUserSkillNotFound is returned when an id isn't owned by the user.
var ErrUserSkillNotFound = errors.New("skill not found")

// UserSkill is one user-authored skill.
type UserSkill struct {
	ID          string `json:"id"`
	UserEmail   string `json:"-"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
	Status      string `json:"status"`
	Visibility  string `json:"visibility"`
	SharedBy    string `json:"shared_by,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

func normalizeUserSkillVisibility(visibility string) (string, error) {
	v := strings.TrimSpace(visibility)
	if v == "" {
		return UserSkillVisibilityPrivate, nil
	}
	if v != UserSkillVisibilityPrivate && v != UserSkillVisibilityWorkspace {
		return "", fmt.Errorf("%w: unknown visibility %q", ErrUserSkillInvalid, visibility)
	}
	return v, nil
}

func validateUserSkill(name, description, body string) error {
	switch {
	case !userSkillNameShape.MatchString(name):
		return fmt.Errorf("%w: name must be lowercase kebab-case (a-z, 0-9, hyphens), max 64 chars", ErrUserSkillInvalid)
	case strings.TrimSpace(description) == "":
		return fmt.Errorf("%w: description is required (it is how the agent decides the skill applies)", ErrUserSkillInvalid)
	case len(description) > maxUserSkillDescription:
		return fmt.Errorf("%w: description exceeds %d characters", ErrUserSkillInvalid, maxUserSkillDescription)
	case strings.TrimSpace(body) == "":
		return fmt.Errorf("%w: body is required", ErrUserSkillInvalid)
	case len(body) > maxUserSkillBody:
		return fmt.Errorf("%w: body exceeds %d bytes", ErrUserSkillInvalid, maxUserSkillBody)
	case strings.Contains(description, "\n"):
		return fmt.Errorf("%w: description must be a single line", ErrUserSkillInvalid)
	}
	return nil
}

// CreateUserSkill inserts a new active private skill.
func (s *Store) CreateUserSkill(ctx context.Context, userEmail, name, description, body string) (*UserSkill, error) {
	return s.createUserSkill(ctx, userEmail, name, description, body, UserSkillStatusActive, UserSkillVisibilityPrivate)
}

// CreateUserSkillWithVisibility inserts a new active skill with explicit visibility ("private" | "workspace").
func (s *Store) CreateUserSkillWithVisibility(ctx context.Context, userEmail, name, description, body, visibility string) (*UserSkill, error) {
	vis, err := normalizeUserSkillVisibility(visibility)
	if err != nil {
		return nil, err
	}
	return s.createUserSkill(ctx, userEmail, name, description, body, UserSkillStatusActive, vis)
}

// CreateUserSkillProposal stages an agent-drafted skill for the owner's
// review: it is stored but inert (never materialized, never invocable) until
// approved in the builder.
func (s *Store) CreateUserSkillProposal(ctx context.Context, userEmail, name, description, body string) (*UserSkill, error) {
	return s.createUserSkill(ctx, userEmail, name, description, body, UserSkillStatusProposed, UserSkillVisibilityPrivate)
}

func (s *Store) createUserSkill(ctx context.Context, userEmail, name, description, body, status, visibility string) (*UserSkill, error) {
	email := normalizeEmail(userEmail)
	name = strings.TrimSpace(name)
	if err := validateUserSkill(name, description, body); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	sk := &UserSkill{
		ID: uuid.NewString(), UserEmail: email, Name: name,
		Description: strings.TrimSpace(description), Body: body,
		Status: status, Visibility: visibility, CreatedAt: now, UpdatedAt: now,
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO user_skills (id, user_email, name, description, body, status, visibility, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$8)`,
		sk.ID, sk.UserEmail, sk.Name, sk.Description, sk.Body, sk.Status, sk.Visibility, now)
	if err != nil {
		if pgUniqueViolation(err) {
			return nil, fmt.Errorf("%w: you already have a skill named %q", ErrUserSkillInvalid, name)
		}
		return nil, err
	}
	return sk, nil
}

// UpdateUserSkill rewrites an owned skill's fields (full replace), preserving its visibility.
func (s *Store) UpdateUserSkill(ctx context.Context, userEmail, id, name, description, body, status string) (*UserSkill, error) {
	return s.UpdateUserSkillWithVisibility(ctx, userEmail, id, name, description, body, status, "")
}

// UpdateUserSkillWithVisibility rewrites an owned skill's fields, updating visibility when non-empty.
func (s *Store) UpdateUserSkillWithVisibility(ctx context.Context, userEmail, id, name, description, body, status, visibility string) (*UserSkill, error) {
	name = strings.TrimSpace(name)
	if err := validateUserSkill(name, description, body); err != nil {
		return nil, err
	}
	// Owners may set active/disabled (approving a proposal = setting it
	// active); nothing can move a skill BACK to proposed — that state is
	// created only by the agent's propose_skill path.
	if status != UserSkillStatusActive && status != UserSkillStatusDisabled {
		return nil, fmt.Errorf("%w: unknown status %q", ErrUserSkillInvalid, status)
	}
	vis := strings.TrimSpace(visibility)
	if vis != "" && vis != UserSkillVisibilityPrivate && vis != UserSkillVisibilityWorkspace {
		return nil, fmt.Errorf("%w: unknown visibility %q", ErrUserSkillInvalid, visibility)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE user_skills
		SET name=$3, description=$4, body=$5, status=$6,
		    visibility=COALESCE(NULLIF($7, ''), visibility),
		    updated_at=$8
		WHERE user_email=$1 AND id=$2`,
		normalizeEmail(userEmail), id, name, strings.TrimSpace(description), body, status, vis, time.Now().Unix())
	if err != nil {
		if pgUniqueViolation(err) {
			return nil, fmt.Errorf("%w: you already have a skill named %q", ErrUserSkillInvalid, name)
		}
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, ErrUserSkillNotFound
	}
	return s.GetUserSkill(ctx, userEmail, id)
}

// GetUserSkill fetches one owned skill.
func (s *Store) GetUserSkill(ctx context.Context, userEmail, id string) (*UserSkill, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, user_email, name, description, body, status, visibility, created_at, updated_at
		FROM user_skills WHERE user_email=$1 AND id=$2`,
		normalizeEmail(userEmail), id)
	var sk UserSkill
	err := row.Scan(&sk.ID, &sk.UserEmail, &sk.Name, &sk.Description, &sk.Body, &sk.Status, &sk.Visibility, &sk.CreatedAt, &sk.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUserSkillNotFound
	}
	if err != nil {
		return nil, err
	}
	return &sk, nil
}

// ListUserSkills returns all of a user's skills, name order.
func (s *Store) ListUserSkills(ctx context.Context, userEmail string) ([]UserSkill, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_email, name, description, body, status, visibility, created_at, updated_at
		FROM user_skills WHERE user_email=$1 ORDER BY name`,
		normalizeEmail(userEmail))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserSkill{}
	for rows.Next() {
		var sk UserSkill
		if err := rows.Scan(&sk.ID, &sk.UserEmail, &sk.Name, &sk.Description, &sk.Body, &sk.Status, &sk.Visibility, &sk.CreatedAt, &sk.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, sk)
	}
	return out, rows.Err()
}

// MaxWorkspaceSharedSkills caps the workspace-shared skill catalog returned by
// ListWorkspaceSharedUserSkills so the shared roster stays bounded.
const MaxWorkspaceSharedSkills = 100

// ListWorkspaceSharedUserSkills returns active workspace-shared skills across
// all users in the deployment, deduplicated by name (most recently updated
// wins), ordered by name and bounded to MaxWorkspaceSharedSkills.
func (s *Store) ListWorkspaceSharedUserSkills(ctx context.Context) ([]UserSkill, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT ON (name) id, user_email, name, description, body, status, visibility, created_at, updated_at
		FROM user_skills
		WHERE status = $1 AND visibility = $2
		ORDER BY name, updated_at DESC, id ASC
		LIMIT $3`,
		UserSkillStatusActive, UserSkillVisibilityWorkspace, MaxWorkspaceSharedSkills)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserSkill{}
	for rows.Next() {
		var sk UserSkill
		if err := rows.Scan(&sk.ID, &sk.UserEmail, &sk.Name, &sk.Description, &sk.Body, &sk.Status, &sk.Visibility, &sk.CreatedAt, &sk.UpdatedAt); err != nil {
			return nil, err
		}
		sk.SharedBy = sk.UserEmail
		out = append(out, sk)
	}
	return out, rows.Err()
}

// ListActiveUserSkills returns a user's ACTIVE skills (the subset that
// participates in runs), ordered by name. Teammate-shared workspace skills are
// published to the Workspace pack catalog for 1-click opt-in ("Add to my
// skills") rather than auto-injected without recipient consent.
func (s *Store) ListActiveUserSkills(ctx context.Context, userEmail string) ([]UserSkill, error) {
	all, err := s.ListUserSkills(ctx, userEmail)
	if err != nil {
		return nil, err
	}
	out := make([]UserSkill, 0, len(all))
	for _, sk := range all {
		if sk.Status == UserSkillStatusActive {
			out = append(out, sk)
		}
	}
	return out, nil
}

// DeleteUserSkill removes an owned skill.
func (s *Store) DeleteUserSkill(ctx context.Context, userEmail, id string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM user_skills WHERE user_email=$1 AND id=$2`,
		normalizeEmail(userEmail), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrUserSkillNotFound
	}
	return nil
}

// RenderUserSkillMarkdown produces the on-disk SKILL.md for materialization:
// generated frontmatter (name/description are the DB columns — the single
// source of truth) + the stored body.
func RenderUserSkillMarkdown(sk *UserSkill) string {
	var b strings.Builder
	b.WriteString("---\nname: ")
	b.WriteString(sk.Name)
	b.WriteString("\ndescription: ")
	b.WriteString(sk.Description)
	b.WriteString("\n---\n\n")
	b.WriteString(sk.Body)
	if !strings.HasSuffix(sk.Body, "\n") {
		b.WriteString("\n")
	}
	return b.String()
}
