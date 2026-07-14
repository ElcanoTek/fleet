package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

const maxPromptLibraryContent = 256 << 10

var (
	ErrPromptInvalid  = errors.New("invalid prompt")
	ErrPromptNotFound = errors.New("prompt not found")
	ErrPromptConflict = errors.New("prompt name already exists")
)

func validatePromptLibraryEntry(name, description, content, visibility string) error {
	name = strings.TrimSpace(name)
	switch {
	case name == "" || utf8.RuneCountInString(name) > 120:
		return fmt.Errorf("%w: name must be 1-120 characters", ErrPromptInvalid)
	case len(description) > 1024:
		return fmt.Errorf("%w: description exceeds 1024 bytes", ErrPromptInvalid)
	case strings.TrimSpace(content) == "" || len(content) > maxPromptLibraryContent:
		return fmt.Errorf("%w: content must be 1-%d bytes", ErrPromptInvalid, maxPromptLibraryContent)
	case visibility != "private" && visibility != "workspace":
		return fmt.Errorf("%w: visibility must be private or workspace", ErrPromptInvalid)
	}
	return nil
}

func (s *Storage) ListPromptLibrary(ctx context.Context, username string) ([]models.PromptLibraryEntry, error) {
	rows, err := s.db.Conn().QueryContext(ctx, `
		SELECT id, owner_username, name, description, content, visibility, created_at, updated_at
		FROM prompt_library
		WHERE owner_username=$1 OR visibility='workspace'
		ORDER BY LOWER(name), updated_at DESC`, strings.TrimSpace(username))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]models.PromptLibraryEntry, 0)
	for rows.Next() {
		var p models.PromptLibraryEntry
		if err := rows.Scan(&p.ID, &p.OwnerUsername, &p.Name, &p.Description, &p.Content, &p.Visibility, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Storage) CreatePromptLibrary(ctx context.Context, owner, name, description, content, visibility string) (*models.PromptLibraryEntry, error) {
	owner, name, description = strings.TrimSpace(owner), strings.TrimSpace(name), strings.TrimSpace(description)
	if owner == "" {
		return nil, fmt.Errorf("%w: owner is required", ErrPromptInvalid)
	}
	if err := validatePromptLibraryEntry(name, description, content, visibility); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	p := &models.PromptLibraryEntry{ID: uuid.New(), OwnerUsername: owner, Name: name, Description: description, Content: content, Visibility: visibility, CreatedAt: now, UpdatedAt: now}
	_, err := s.db.Conn().ExecContext(ctx, `
		INSERT INTO prompt_library (id, owner_username, name, description, content, visibility, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$7)`, p.ID, owner, name, description, content, visibility, now)
	if err != nil {
		return nil, promptLibraryDBError(err)
	}
	return p, nil
}

func (s *Storage) UpdatePromptLibrary(ctx context.Context, id uuid.UUID, owner, name, description, content, visibility string, admin bool) (*models.PromptLibraryEntry, error) {
	name, description = strings.TrimSpace(name), strings.TrimSpace(description)
	if err := validatePromptLibraryEntry(name, description, content, visibility); err != nil {
		return nil, err
	}
	query := `UPDATE prompt_library SET name=$3, description=$4, content=$5, visibility=$6, updated_at=$7 WHERE id=$1 AND owner_username=$2 RETURNING id, owner_username, name, description, content, visibility, created_at, updated_at`
	args := []any{id, strings.TrimSpace(owner), name, description, content, visibility, time.Now().UTC()}
	if admin {
		query = `UPDATE prompt_library SET name=$2, description=$3, content=$4, visibility=$5, updated_at=$6 WHERE id=$1 RETURNING id, owner_username, name, description, content, visibility, created_at, updated_at`
		args = []any{id, name, description, content, visibility, time.Now().UTC()}
	}
	var p models.PromptLibraryEntry
	if err := s.db.Conn().QueryRowContext(ctx, query, args...).Scan(&p.ID, &p.OwnerUsername, &p.Name, &p.Description, &p.Content, &p.Visibility, &p.CreatedAt, &p.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPromptNotFound
		}
		return nil, promptLibraryDBError(err)
	}
	return &p, nil
}

func promptLibraryDBError(err error) error {
	var state interface{ SQLState() string }
	if errors.As(err, &state) && state.SQLState() == "23505" {
		return fmt.Errorf("%w: you already have a prompt with that name", ErrPromptConflict)
	}
	return err
}

func (s *Storage) DeletePromptLibrary(ctx context.Context, id uuid.UUID, owner string, admin bool) error {
	query := `DELETE FROM prompt_library WHERE id=$1 AND owner_username=$2`
	args := []any{id, strings.TrimSpace(owner)}
	if admin {
		query, args = `DELETE FROM prompt_library WHERE id=$1`, []any{id}
	}
	res, err := s.db.Conn().ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrPromptNotFound
	}
	return nil
}
