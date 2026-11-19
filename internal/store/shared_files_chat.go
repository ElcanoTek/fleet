package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Shared files a chat writes (migration 0205, ADR-0114, docs/SHARED-FILES.md
// "Files chats write"). The shared_file_write tool creates a TEXT file in the
// shared library, appends to it, replaces it, or undoes the last replace. The
// text lives in chat.shared_files.content so it is in the nightly pg_dump, and
// every write is one transaction holding the row's lock, so two chats appending
// at once cannot lose each other's lines — whichever replica they run on.
//
// The store enforces the relational rules (who may write which row, the
// compare-and-swap on replace, the size caps, one index per folder). Name and
// folder validation, text validation, and staging the sandbox-visible copy
// belong to the callers (internal/sharedfiles, internal/httpapi).

// Chat shared-file write modes.
const (
	SharedFileWriteCreate  = "create"
	SharedFileWriteAppend  = "append"
	SharedFileWriteReplace = "replace"
	SharedFileWriteUndo    = "undo"
)

// ErrSharedFileAdminOwned refuses a chat write to a file an administrator
// uploaded: those stay read-only to chats.
var ErrSharedFileAdminOwned = errors.New("that shared file was published by an administrator and is read-only to chats")

// ErrSharedFileDescriptionRequired refuses creating a chat file without a
// description: every conversation on the server is told what the file is by
// that line, so a file without one would be undiscoverable.
var ErrSharedFileDescriptionRequired = errors.New("a description is required when creating a shared file")

// ErrSharedFileExpectedHashRequired refuses a replace without the sha256 of the
// version being replaced. Replacing blind would let a chat overwrite a newer
// version another chat wrote since it last read the file.
var ErrSharedFileExpectedHashRequired = errors.New("replacing a shared file requires expected_sha256: the sha256 of the version you read")

// ErrSharedFileNoPrevious reports an undo with nothing to restore.
var ErrSharedFileNoPrevious = errors.New("there is no previous version of that shared file to restore")

// ErrSharedFileWriteConflict reports that a concurrent writer created the same
// path first. The caller may retry once (an append then lands on the new row).
var ErrSharedFileWriteConflict = errors.New("another writer created that shared file at the same moment")

// SharedFileStaleError refuses a write whose expected_sha256 does not match the
// stored version. Current is the sha256 the caller should read and merge.
type SharedFileStaleError struct{ Current string }

func (e *SharedFileStaleError) Error() string {
	return fmt.Sprintf("the shared file changed since you read it (its sha256 is now %s): read it again, merge your change, and retry with that sha256", e.Current)
}

// SharedFileTooLargeError refuses a write that would make the file exceed the
// per-file cap.
type SharedFileTooLargeError struct{ Size, Limit int64 }

func (e *SharedFileTooLargeError) Error() string {
	return fmt.Sprintf("the shared file would be %d bytes, over the %d-byte limit for a file chats write: start a new file (for example one per month or per campaign)", e.Size, e.Limit)
}

// SharedFileLibraryFullError refuses a write that would push the library past
// its total-size cap.
type SharedFileLibraryFullError struct{ Total, Limit int64 }

func (e *SharedFileLibraryFullError) Error() string {
	return fmt.Sprintf("the shared library would be %d bytes, over its %d-byte cap: an administrator can remove files or raise shared_files_max_total_mb", e.Total, e.Limit)
}

// ChatSharedFileWrite is one shared_file_write call, already validated.
type ChatSharedFileWrite struct {
	Folder, Name string
	// Mode is one of the SharedFileWrite* constants.
	Mode string
	// Text is the content to write (create/replace) or add (append). Ignored
	// by undo.
	Text string
	// Description replaces the file's description when non-nil (create
	// requires a non-empty one).
	Description *string
	// FolderIndex sets or clears the folder-index mark when non-nil.
	FolderIndex *bool
	// ExpectedSHA256 is the version the caller read. Required for replace;
	// optional for append and undo, where it guards the same way.
	ExpectedSHA256 string
	// ContentType is recorded on create (download Content-Type).
	ContentType string
	// Actor is the writer's email; ConversationID the chat (or "task:<id>").
	Actor, ConversationID string
	// NewID is the id to give the row if the write creates it.
	NewID string
	// MaxFileBytes caps the file's size after the write (> 0 required).
	// MaxTotalBytes caps the library total (0 = unlimited).
	MaxFileBytes, MaxTotalBytes int64
}

// ChatSharedFileWriteResult is the row after a write.
type ChatSharedFileWriteResult struct {
	File SharedFile
	// Created reports that this write created the file.
	Created bool
	// PreviousSHA256 is the sha256 of the version this write displaced (""
	// when it created the file).
	PreviousSHA256 string
}

// sha256Hex is the library's content hash.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// joinAppend adds text to cur, starting it on a new line when cur does not
// already end with one — appending a CSV row to a file whose last row lacks a
// trailing newline must not glue the two rows together.
func joinAppend(cur, text string) string {
	if cur != "" && !strings.HasSuffix(cur, "\n") {
		return cur + "\n" + text
	}
	return cur + text
}

// WriteChatSharedFile performs one chat write in a single transaction that
// holds the target row's lock (SELECT … FOR UPDATE), so concurrent writers to
// one file serialize in the database rather than in any one process.
func (s *Store) WriteChatSharedFile(ctx context.Context, w ChatSharedFileWrite) (ChatSharedFileWriteResult, error) {
	if w.MaxFileBytes <= 0 {
		return ChatSharedFileWriteResult{}, errors.New("shared file write: MaxFileBytes must be positive")
	}
	switch w.Mode {
	case SharedFileWriteCreate, SharedFileWriteAppend, SharedFileWriteReplace, SharedFileWriteUndo:
	default:
		return ChatSharedFileWriteResult{}, fmt.Errorf("shared file write: unknown mode %q", w.Mode)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ChatSharedFileWriteResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		cur        SharedFile
		curContent sql.NullString
		prev       sql.NullString
	)
	row := tx.QueryRowContext(ctx, `
		SELECT `+sharedFileSelect+`, content, previous_content
		FROM shared_files WHERE folder = $1 AND name = $2
		FOR UPDATE`, w.Folder, w.Name)
	err = row.Scan(&cur.ID, &cur.Name, &cur.Folder, &cur.Description, &cur.SizeBytes,
		&cur.ContentType, &cur.SHA256, &cur.UploadedBy, &cur.CreatedAt, &cur.UpdatedAt,
		&cur.Origin, &cur.UpdatedBy, &cur.UpdatedConversationID, &cur.FolderIndex, &cur.HasPrevious,
		&curContent, &prev)
	exists := true
	if errors.Is(err, sql.ErrNoRows) {
		exists = false
	} else if err != nil {
		return ChatSharedFileWriteResult{}, err
	}

	now := time.Now().Unix()
	var res ChatSharedFileWriteResult
	if !exists {
		res, err = s.createChatSharedFile(ctx, tx, w, now)
	} else {
		res, err = s.updateChatSharedFile(ctx, tx, w, cur, curContent, prev, now)
	}
	if err != nil {
		return ChatSharedFileWriteResult{}, err
	}
	if err := tx.Commit(); err != nil {
		if pgUniqueViolation(err) {
			return ChatSharedFileWriteResult{}, ErrSharedFileWriteConflict
		}
		return ChatSharedFileWriteResult{}, err
	}
	return res, nil
}

func (s *Store) createChatSharedFile(ctx context.Context, tx *sql.Tx, w ChatSharedFileWrite, now int64) (ChatSharedFileWriteResult, error) {
	if w.Mode == SharedFileWriteReplace || w.Mode == SharedFileWriteUndo {
		return ChatSharedFileWriteResult{}, ErrSharedFileNotFound
	}
	if w.Description == nil || strings.TrimSpace(*w.Description) == "" {
		return ChatSharedFileWriteResult{}, ErrSharedFileDescriptionRequired
	}
	if w.NewID == "" {
		return ChatSharedFileWriteResult{}, errors.New("shared file write: NewID is required to create a file")
	}
	size := int64(len(w.Text))
	if size > w.MaxFileBytes {
		return ChatSharedFileWriteResult{}, &SharedFileTooLargeError{Size: size, Limit: w.MaxFileBytes}
	}
	if err := checkLibraryTotal(ctx, tx, size, w.MaxTotalBytes); err != nil {
		return ChatSharedFileWriteResult{}, err
	}
	var conflict int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (`+sharedPathNamespaceConflict+`)::int`,
		w.NewID, w.Name, w.Folder).Scan(&conflict); err != nil {
		return ChatSharedFileWriteResult{}, err
	}
	if conflict == 1 {
		return ChatSharedFileWriteResult{}, ErrSharedFileNameIsFolder
	}
	index := w.FolderIndex != nil && *w.FolderIndex
	if index {
		if err := clearFolderIndex(ctx, tx, w.Folder, w.NewID); err != nil {
			return ChatSharedFileWriteResult{}, err
		}
	}
	contentType := w.ContentType
	if contentType == "" {
		contentType = "text/plain; charset=utf-8"
	}
	row := tx.QueryRowContext(ctx, `
		INSERT INTO shared_files (`+sharedFileColumns+`, content)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9, 'chat', $8, $10, $11, $12)
		RETURNING `+sharedFileSelect,
		w.NewID, w.Name, w.Folder, strings.TrimSpace(*w.Description), size, contentType,
		sha256Hex(w.Text), w.Actor, now, w.ConversationID, index, w.Text)
	f, err := scanSharedFile(row)
	if pgUniqueViolation(err) {
		// Either the path was created by a writer whose row this transaction's
		// FOR UPDATE could not see (it did not exist yet), or another file in
		// the folder took the index in between. Both are a lost race.
		return ChatSharedFileWriteResult{}, ErrSharedFileWriteConflict
	}
	if err != nil {
		return ChatSharedFileWriteResult{}, err
	}
	return ChatSharedFileWriteResult{File: f, Created: true}, nil
}

func (s *Store) updateChatSharedFile(ctx context.Context, tx *sql.Tx, w ChatSharedFileWrite, cur SharedFile, curContent, prev sql.NullString, now int64) (ChatSharedFileWriteResult, error) {
	if cur.Origin != SharedFileOriginChat {
		return ChatSharedFileWriteResult{}, ErrSharedFileAdminOwned
	}
	if w.Mode == SharedFileWriteCreate {
		return ChatSharedFileWriteResult{}, ErrSharedFileExists
	}
	if w.Mode == SharedFileWriteReplace && strings.TrimSpace(w.ExpectedSHA256) == "" {
		return ChatSharedFileWriteResult{}, ErrSharedFileExpectedHashRequired
	}
	if exp := strings.ToLower(strings.TrimSpace(w.ExpectedSHA256)); exp != "" && exp != cur.SHA256 {
		return ChatSharedFileWriteResult{}, &SharedFileStaleError{Current: cur.SHA256}
	}
	old := curContent.String
	var next string
	// newPrev is what previous_content becomes. Append keeps whatever a
	// replace displaced (an append never needs undoing — it only adds), so a
	// replace followed by appends can still be undone to the pre-replace text.
	newPrev := prev
	switch w.Mode {
	case SharedFileWriteAppend:
		next = joinAppend(old, w.Text)
	case SharedFileWriteReplace:
		next = w.Text
		newPrev = sql.NullString{String: old, Valid: true}
	case SharedFileWriteUndo:
		if !prev.Valid {
			return ChatSharedFileWriteResult{}, ErrSharedFileNoPrevious
		}
		next = prev.String
		newPrev = sql.NullString{String: old, Valid: true}
	}
	size := int64(len(next))
	if size > w.MaxFileBytes {
		return ChatSharedFileWriteResult{}, &SharedFileTooLargeError{Size: size, Limit: w.MaxFileBytes}
	}
	if err := checkLibraryTotal(ctx, tx, size-cur.SizeBytes, w.MaxTotalBytes); err != nil {
		return ChatSharedFileWriteResult{}, err
	}
	description := cur.Description
	if w.Description != nil && strings.TrimSpace(*w.Description) != "" {
		description = strings.TrimSpace(*w.Description)
	}
	index := cur.FolderIndex
	if w.FolderIndex != nil {
		index = *w.FolderIndex
		if index && !cur.FolderIndex {
			if err := clearFolderIndex(ctx, tx, cur.Folder, cur.ID); err != nil {
				return ChatSharedFileWriteResult{}, err
			}
		}
	}
	prevSHA := ""
	if newPrev.Valid {
		prevSHA = sha256Hex(newPrev.String)
	}
	row := tx.QueryRowContext(ctx, `
		UPDATE shared_files
		SET content = $2, size_bytes = $3, sha256 = $4,
		    previous_content = $5, previous_sha256 = $6,
		    description = $7, folder_index = $8,
		    updated_by = $9, updated_conversation_id = $10, updated_at = $11
		WHERE id = $1
		RETURNING `+sharedFileSelect,
		cur.ID, next, size, sha256Hex(next), newPrev, prevSHA,
		description, index, w.Actor, w.ConversationID, now)
	f, err := scanSharedFile(row)
	if pgUniqueViolation(err) {
		return ChatSharedFileWriteResult{}, ErrSharedFileWriteConflict
	}
	if err != nil {
		return ChatSharedFileWriteResult{}, err
	}
	return ChatSharedFileWriteResult{File: f, PreviousSHA256: cur.SHA256}, nil
}

// checkLibraryTotal refuses a write that grows the library past maxTotal
// (0 = unlimited). delta may be negative (a replace that shrinks the file).
// The sum is read inside the writer's transaction; two writers in different
// transactions can each pass it, so the cap is a governance bound, not an exact
// one — the same posture as the admin upload path.
func checkLibraryTotal(ctx context.Context, tx *sql.Tx, delta, maxTotal int64) error {
	if maxTotal <= 0 || delta <= 0 {
		return nil
	}
	var total int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(size_bytes), 0) FROM shared_files`).Scan(&total); err != nil {
		return err
	}
	if total+delta > maxTotal {
		return &SharedFileLibraryFullError{Total: total + delta, Limit: maxTotal}
	}
	return nil
}

// clearFolderIndex removes the index mark from every other file in folder, so
// the partial unique index (one index per folder) admits the new one.
func clearFolderIndex(ctx context.Context, tx *sql.Tx, folder, keepID string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE shared_files SET folder_index = FALSE WHERE folder = $1 AND id <> $2 AND folder_index`,
		folder, keepID)
	return err
}

// SharedFileContent returns a chat-written file's text and its sha256 as one
// consistent read. ok is false for an admin upload (whose bytes live in the
// data dir) — never an error, so a caller can branch on origin cheaply.
func (s *Store) SharedFileContent(ctx context.Context, id string) (content, sha string, ok bool, err error) {
	var c sql.NullString
	err = s.db.QueryRowContext(ctx,
		`SELECT content, sha256 FROM shared_files WHERE id = $1`, id).Scan(&c, &sha)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, ErrSharedFileNotFound
	}
	if err != nil {
		return "", "", false, err
	}
	if !c.Valid {
		return "", sha, false, nil
	}
	return c.String, sha, true, nil
}
