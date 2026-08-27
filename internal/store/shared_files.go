package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Shared file library (migration 053): metadata for admin-uploaded files every
// conversation can read (docs/SHARED-FILES.md). Rows here are the manifest; the
// canonical bytes live under <DataDir>/shared_files/<id> and the sandbox-visible
// staged copy under <WorkspaceRoot>/shared/[folder/]name. The store layer is
// deliberately dumb about both trees — path derivation, staging, and
// reconciliation live in internal/sharedfiles; validation of name/folder lives
// at the API layer. All it enforces is the one relational fact the stager
// depends on: (folder, name) — the staged path — is unique.

// SharedFile is one library entry.
type SharedFile struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Folder      string `json:"folder"`
	Description string `json:"description"`
	SizeBytes   int64  `json:"size_bytes"`
	ContentType string `json:"content_type,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	UploadedBy  string `json:"uploaded_by,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

// ErrSharedFileNotFound reports an id no row carries.
var ErrSharedFileNotFound = errors.New("shared file not found")

// ErrSharedFileExists reports a (folder, name) collision — the staged path is
// already claimed by another row.
var ErrSharedFileExists = errors.New("a shared file with that name already exists in that folder")

const sharedFileColumns = `id, name, folder, description, size_bytes, content_type, sha256, uploaded_by, created_at, updated_at`

func scanSharedFile(row interface{ Scan(...any) error }) (SharedFile, error) {
	var f SharedFile
	err := row.Scan(&f.ID, &f.Name, &f.Folder, &f.Description, &f.SizeBytes,
		&f.ContentType, &f.SHA256, &f.UploadedBy, &f.CreatedAt, &f.UpdatedAt)
	return f, err
}

// CreateSharedFile inserts one row. The caller supplies the id (an unguessable
// token minted at the API layer) and the already-validated name/folder.
func (s *Store) CreateSharedFile(ctx context.Context, f SharedFile) (SharedFile, error) {
	now := time.Now().Unix()
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO shared_files (`+sharedFileColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING `+sharedFileColumns,
		f.ID, f.Name, f.Folder, f.Description, f.SizeBytes,
		f.ContentType, f.SHA256, f.UploadedBy, now, now)
	out, err := scanSharedFile(row)
	if pgUniqueViolation(err) {
		return SharedFile{}, ErrSharedFileExists
	}
	return out, err
}

// ListSharedFiles returns the whole library, folder-then-name ordered so the
// listing is stable for both the admin UI and the per-turn prompt block.
func (s *Store) ListSharedFiles(ctx context.Context) ([]SharedFile, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sharedFileColumns+` FROM shared_files ORDER BY folder, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SharedFile
	for rows.Next() {
		f, err := scanSharedFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// GetSharedFile returns one row by id.
func (s *Store) GetSharedFile(ctx context.Context, id string) (SharedFile, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+sharedFileColumns+` FROM shared_files WHERE id = $1`, id)
	f, err := scanSharedFile(row)
	if errors.Is(err, sql.ErrNoRows) {
		return SharedFile{}, ErrSharedFileNotFound
	}
	return f, err
}

// UpdateSharedFileMeta renames/moves/describes one row (the metadata trio the
// PATCH endpoint exposes) and returns the persisted result. The caller has
// already validated name and folder; the unique constraint still backstops a
// racing rename into an occupied path.
func (s *Store) UpdateSharedFileMeta(ctx context.Context, id, name, folder, description string) (SharedFile, error) {
	row := s.db.QueryRowContext(ctx, `
		UPDATE shared_files
		SET name = $2, folder = $3, description = $4, updated_at = $5
		WHERE id = $1
		RETURNING `+sharedFileColumns,
		id, name, folder, description, time.Now().Unix())
	f, err := scanSharedFile(row)
	if errors.Is(err, sql.ErrNoRows) {
		return SharedFile{}, ErrSharedFileNotFound
	}
	if pgUniqueViolation(err) {
		return SharedFile{}, ErrSharedFileExists
	}
	return f, err
}

// DeleteSharedFile removes one row and returns it, so the caller can unstage
// the file it described without a prior read.
func (s *Store) DeleteSharedFile(ctx context.Context, id string) (SharedFile, error) {
	row := s.db.QueryRowContext(ctx,
		`DELETE FROM shared_files WHERE id = $1 RETURNING `+sharedFileColumns, id)
	f, err := scanSharedFile(row)
	if errors.Is(err, sql.ErrNoRows) {
		return SharedFile{}, ErrSharedFileNotFound
	}
	return f, err
}

// TotalSharedFileBytes sums the library for the admin quota check.
func (s *Store) TotalSharedFileBytes(ctx context.Context) (int64, error) {
	var total int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(size_bytes), 0) FROM shared_files`).Scan(&total)
	return total, err
}
