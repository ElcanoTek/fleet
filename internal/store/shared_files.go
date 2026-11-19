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

	// Origin is SharedFileOriginAdmin (an upload: bytes in the data dir) or
	// SharedFileOriginChat (written by shared_file_write: text in the row).
	// Chats may write only chat rows (migration 0205, ADR-0114).
	Origin string `json:"origin"`
	// UpdatedBy / UpdatedConversationID name the last writer of a chat row:
	// an email, and the conversation id (or "task:<id>" for a scheduled
	// run). Both are "" on an admin upload.
	UpdatedBy             string `json:"updated_by,omitempty"`
	UpdatedConversationID string `json:"updated_conversation_id,omitempty"`
	// FolderIndex marks the file its folder's entry point: a folder with many
	// files is announced to the model as one line naming this file.
	FolderIndex bool `json:"folder_index,omitempty"`
	// HasPrevious reports a displaced version one undo can restore.
	HasPrevious bool `json:"has_previous,omitempty"`
}

// Shared-file origins (chat.shared_files.origin).
const (
	SharedFileOriginAdmin = "admin"
	SharedFileOriginChat  = "chat"
)

// IsChatWritten reports whether a row was written by a chat (its text lives in
// the database, and chats may change it).
func (f SharedFile) IsChatWritten() bool { return f.Origin == SharedFileOriginChat }

// ErrSharedFileNotFound reports an id no row carries.
var ErrSharedFileNotFound = errors.New("shared file not found")

// ErrSharedFileExists reports a (folder, name) collision — the staged path is
// already claimed by another row.
var ErrSharedFileExists = errors.New("a shared file with that name already exists in that folder")

// ErrSharedFileNameIsFolder reports the OTHER way two rows can claim one path:
// a root-level file named "q3" and a folder named "q3" are distinct (folder,
// name) pairs, so UNIQUE (folder, name) admits both — but the staged tree is a
// real filesystem, where "shared/q3" cannot be a file and a directory at once.
// Left to the reconciler that is unrecoverable, not cosmetic: Stage fails
// ("not a directory" one way round, "file exists" the other), so every Sync
// pass returns an error forever and WHICH of the two rows reaches the sandbox
// flaps with map iteration order. Rejecting the second writer is the only place
// the conflict can still be reported to a human.
//
// Unlike the (folder, name) collision, this one has no constraint behind it —
// "name must not equal any folder" is not expressible as a unique index — so the
// guard is check-then-write, made race-free by the namespace lock: every write
// that can claim a path (upload, rename, chat create) takes
// lockSharedFilesNamespace inside its transaction before checking, so two
// writers — chats, admins, or replicas — can never both pass the check.
var ErrSharedFileNameIsFolder = errors.New("that path is already claimed: a file and a folder in the shared library cannot share a name")

// sharedPathNamespaceConflict matches a row whose existence would make the
// requested (folder, name) unrepresentable as a path. The placeholders are
// numbered to match CreateSharedFile's column order so the predicate can be
// spliced straight into that INSERT: $1 = the id to exclude (the row being
// updated; on insert it is the new id, which matches nothing), $2 = requested
// name, $3 = requested folder.
//
// Two shapes, one per direction:
//   - a root file named "X" is requested while some row lives in folder "X"
//   - a row in folder "X" is requested while a root file is named "X"
const sharedPathNamespaceConflict = `
	SELECT 1 FROM shared_files
	WHERE id <> $1 AND (
		($3 = '' AND folder = $2)
		OR ($3 <> '' AND folder = '' AND name = $3)
	)`

// sharedFilesNamespaceLockKey is the transaction-scoped advisory lock every
// path-claiming shared-file write holds while it checks and claims its path
// (ADR-0114). An arbitrary constant: "shfl" in ASCII.
const sharedFilesNamespaceLockKey int64 = 0x7368666c

// lockSharedFilesNamespace serializes path-claiming writes across processes and
// replicas; released when tx ends.
func lockSharedFilesNamespace(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, sharedFilesNamespaceLockKey)
	return err
}

const sharedFileColumns = `id, name, folder, description, size_bytes, content_type, sha256, uploaded_by, created_at, updated_at, origin, updated_by, updated_conversation_id, folder_index`

// sharedFileSelect is the read projection: the stored columns plus whether a
// previous version exists. It never selects content itself — listing the
// library (every turn's prompt block) must not drag every chat file's text
// out of the database.
const sharedFileSelect = sharedFileColumns + `, previous_content IS NOT NULL`

func scanSharedFile(row interface{ Scan(...any) error }) (SharedFile, error) {
	var f SharedFile
	err := row.Scan(&f.ID, &f.Name, &f.Folder, &f.Description, &f.SizeBytes,
		&f.ContentType, &f.SHA256, &f.UploadedBy, &f.CreatedAt, &f.UpdatedAt,
		&f.Origin, &f.UpdatedBy, &f.UpdatedConversationID, &f.FolderIndex, &f.HasPrevious)
	return f, err
}

// CreateSharedFile inserts one row. The caller supplies the id (an unguessable
// token minted at the API layer) and the already-validated name/folder.
func (s *Store) CreateSharedFile(ctx context.Context, f SharedFile) (SharedFile, error) {
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SharedFile{}, err
	}
	defer func() { _ = tx.Rollback() }()
	// The namespace guard below is only race-free while every path-claiming
	// write holds the namespace lock (lockSharedFilesNamespace).
	if err := lockSharedFilesNamespace(ctx, tx); err != nil {
		return SharedFile{}, err
	}
	// An upload is always an admin row; chat rows are created only by
	// WriteChatSharedFile, which also stores their text.
	row := tx.QueryRowContext(ctx, `
		INSERT INTO shared_files (`+sharedFileColumns+`)
		SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'admin', '', '', FALSE
		WHERE NOT EXISTS (`+sharedPathNamespaceConflict+`)
		RETURNING `+sharedFileSelect,
		f.ID, f.Name, f.Folder, f.Description, f.SizeBytes,
		f.ContentType, f.SHA256, f.UploadedBy, now, now)
	out, err := scanSharedFile(row)
	if pgUniqueViolation(err) {
		return SharedFile{}, ErrSharedFileExists
	}
	// The guard suppressed the insert rather than failing it, so no row came
	// back. Distinguishing this from a genuine ErrNoRows needs no extra query:
	// an INSERT ... RETURNING has nothing else to be silent about.
	if errors.Is(err, sql.ErrNoRows) {
		return SharedFile{}, ErrSharedFileNameIsFolder
	}
	if err != nil {
		return SharedFile{}, err
	}
	if err := tx.Commit(); err != nil {
		if pgUniqueViolation(err) {
			return SharedFile{}, ErrSharedFileExists
		}
		return SharedFile{}, err
	}
	return out, nil
}

// SharedFilePathAvailable reports whether a NEW row at (folder, name) could be
// created right now: nil when the path is free, ErrSharedFileExists when a row
// already claims it, ErrSharedFileNameIsFolder when a file/folder namespace
// collision would make it unrepresentable. It is the same two checks
// CreateSharedFile enforces, run ahead of any write so a multi-file upload can
// refuse the whole batch before file 1 is durably saved (the caller holds the
// library mutex, so the answer stays true until its own inserts land).
func (s *Store) SharedFilePathAvailable(ctx context.Context, folder, name string) error {
	var taken, conflict int
	// The spliced predicate reads $1 = excluded id (none here: a new row
	// excludes nothing, and no id is the empty string), $2 = name, $3 = folder.
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM shared_files WHERE folder = $3 AND name = $2)::int,
		        EXISTS (`+sharedPathNamespaceConflict+`)::int`,
		"", name, folder).Scan(&taken, &conflict)
	if err != nil {
		return err
	}
	if taken == 1 {
		return ErrSharedFileExists
	}
	if conflict == 1 {
		return ErrSharedFileNameIsFolder
	}
	return nil
}

// ListSharedFiles returns the whole library, folder-then-name ordered so the
// listing is stable for both the admin UI and the per-turn prompt block.
func (s *Store) ListSharedFiles(ctx context.Context) ([]SharedFile, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sharedFileSelect+` FROM shared_files ORDER BY folder, name`)
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
		`SELECT `+sharedFileSelect+` FROM shared_files WHERE id = $1`, id)
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SharedFile{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := lockSharedFilesNamespace(ctx, tx); err != nil {
		return SharedFile{}, err
	}
	// The namespace guard runs first and separately: UPDATE ... WHERE NOT EXISTS
	// would make "conflicting" and "no such id" the same empty result, and the
	// two are different HTTP statuses (409 vs 404).
	var conflict int
	err = tx.QueryRowContext(ctx,
		`SELECT EXISTS (`+sharedPathNamespaceConflict+`)::int`, id, name, folder).Scan(&conflict)
	if err != nil {
		return SharedFile{}, err
	}
	if conflict == 1 {
		return SharedFile{}, ErrSharedFileNameIsFolder
	}
	row := tx.QueryRowContext(ctx, `
		UPDATE shared_files
		SET name = $2, folder = $3, description = $4, updated_at = $5
		WHERE id = $1
		RETURNING `+sharedFileSelect,
		id, name, folder, description, time.Now().Unix())
	f, err := scanSharedFile(row)
	if errors.Is(err, sql.ErrNoRows) {
		return SharedFile{}, ErrSharedFileNotFound
	}
	if pgUniqueViolation(err) {
		return SharedFile{}, ErrSharedFileExists
	}
	if err != nil {
		return SharedFile{}, err
	}
	if err := tx.Commit(); err != nil {
		if pgUniqueViolation(err) {
			return SharedFile{}, ErrSharedFileExists
		}
		return SharedFile{}, err
	}
	return f, nil
}

// DeleteSharedFile removes one row and returns it, so the caller can unstage
// the file it described without a prior read.
func (s *Store) DeleteSharedFile(ctx context.Context, id string) (SharedFile, error) {
	row := s.db.QueryRowContext(ctx,
		`DELETE FROM shared_files WHERE id = $1 RETURNING `+sharedFileSelect, id)
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
