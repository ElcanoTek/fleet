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

// User-skill statuses.
const (
	UserSkillStatusActive   = "active"
	UserSkillStatusDisabled = "disabled"
)

// userSkillNameShape mirrors the bundle skill-name contract
// (clientconfig.validSkillName): lowercase kebab, ≤64 chars.
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
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
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

// CreateUserSkill inserts a new active skill.
func (s *Store) CreateUserSkill(ctx context.Context, userEmail, name, description, body string) (*UserSkill, error) {
	email := normalizeEmail(userEmail)
	name = strings.TrimSpace(name)
	if err := validateUserSkill(name, description, body); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	sk := &UserSkill{
		ID: uuid.NewString(), UserEmail: email, Name: name,
		Description: strings.TrimSpace(description), Body: body,
		Status: UserSkillStatusActive, CreatedAt: now, UpdatedAt: now,
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO user_skills (id, user_email, name, description, body, status, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$7)`,
		sk.ID, sk.UserEmail, sk.Name, sk.Description, sk.Body, sk.Status, now)
	if err != nil {
		if pgUniqueViolation(err) {
			return nil, fmt.Errorf("%w: you already have a skill named %q", ErrUserSkillInvalid, name)
		}
		return nil, err
	}
	return sk, nil
}

// UpdateUserSkill rewrites an owned skill's fields (full replace).
func (s *Store) UpdateUserSkill(ctx context.Context, userEmail, id, name, description, body, status string) (*UserSkill, error) {
	name = strings.TrimSpace(name)
	if err := validateUserSkill(name, description, body); err != nil {
		return nil, err
	}
	if status != UserSkillStatusActive && status != UserSkillStatusDisabled {
		return nil, fmt.Errorf("%w: unknown status %q", ErrUserSkillInvalid, status)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE user_skills SET name=$3, description=$4, body=$5, status=$6, updated_at=$7
		WHERE user_email=$1 AND id=$2`,
		normalizeEmail(userEmail), id, name, strings.TrimSpace(description), body, status, time.Now().Unix())
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
		SELECT id, user_email, name, description, body, status, created_at, updated_at
		FROM user_skills WHERE user_email=$1 AND id=$2`,
		normalizeEmail(userEmail), id)
	var sk UserSkill
	err := row.Scan(&sk.ID, &sk.UserEmail, &sk.Name, &sk.Description, &sk.Body, &sk.Status, &sk.CreatedAt, &sk.UpdatedAt)
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
		SELECT id, user_email, name, description, body, status, created_at, updated_at
		FROM user_skills WHERE user_email=$1 ORDER BY name`,
		normalizeEmail(userEmail))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserSkill{}
	for rows.Next() {
		var sk UserSkill
		if err := rows.Scan(&sk.ID, &sk.UserEmail, &sk.Name, &sk.Description, &sk.Body, &sk.Status, &sk.CreatedAt, &sk.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, sk)
	}
	return out, rows.Err()
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
