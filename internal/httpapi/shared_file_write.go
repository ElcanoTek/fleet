package httpapi

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/sharedfiles"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// Files chats write (ADR-0114, docs/SHARED-FILES.md "Files chats write"): the
// host side of the shared_file_write tool. A chat turn and a scheduled run get
// the same handler (SharedFileWriteHandlerFor), bound to whoever the turn runs
// as, so both write through one path:
//
//  1. the tool's arguments are checked (path, text, description),
//  2. the store applies the write in one row-locked transaction — the text
//     lives in Postgres, so it is in the nightly backup and two writers
//     cannot interleave,
//  3. the sandbox-visible copy is restaged from the database under
//     sharedFilesMu, so every conversation reads the new version on its next
//     tool call.
//
// The staged tree stays read-only to sandboxes (docs/SHARED-FILES.md): this
// tool is the only way a turn changes it.

// sharedFileWriteRetries bounds the retry after losing a create race: the
// second attempt finds the row the winner created and appends to it.
const sharedFileWriteRetries = 1

// newSharedFileWriteHandler binds shared_file_write to a chat turn.
func (s *Server) newSharedFileWriteHandler(conv *store.Conversation, user string) tools.SharedFileWriteHandler {
	if conv == nil {
		return nil
	}
	return s.SharedFileWriteHandlerFor(user, conv.ID)
}

// SharedFileWriteHandlerFor returns the shared_file_write handler for a writer
// (an email) and the run it writes from: a conversation id, or "task:<id>" for
// a scheduled run (cmd/fleet wires the scheduled runner through this). The
// handler re-checks the admin switch and the writer's role on every call, so
// a setting flipped mid-run governs the next write.
func (s *Server) SharedFileWriteHandlerFor(email, origin string) tools.SharedFileWriteHandler {
	email = strings.TrimSpace(email)
	return func(ctx context.Context, req tools.SharedFileWriteRequest) (*tools.SharedFileWriteResult, error) {
		return s.writeChatSharedFile(ctx, email, origin, req)
	}
}

func (s *Server) writeChatSharedFile(ctx context.Context, email, origin string, req tools.SharedFileWriteRequest) (*tools.SharedFileWriteResult, error) {
	if s.cfg != nil && !s.cfg.LiveSharedFilesChatWritesEnabled() {
		return nil, errors.New("an administrator has turned off writing shared files from chats on this server (Settings → Admin → Features)")
	}
	if email == "" {
		return nil, errors.New("shared_file_write needs a signed-in user to record as the writer")
	}
	u, err := s.store.GetUser(ctx, email)
	if err != nil || u == nil {
		return nil, fmt.Errorf("shared_file_write: the writer %s is not a user of this server", email)
	}
	if u.Role == store.RoleViewer {
		return nil, errors.New("read-only (Viewer) accounts cannot write shared files")
	}
	folder, name, err := sharedfiles.ParseChatPath(req.Path)
	if err != nil {
		return nil, err
	}
	if err := sharedfiles.ValidateChatText(req.Content); err != nil {
		return nil, err
	}
	var description *string
	if d := sharedfiles.SanitizeChatDescription(req.Description); d != "" {
		description = &d
	}
	write := store.ChatSharedFileWrite{
		Folder: folder, Name: name, Mode: req.Mode, Text: req.Content,
		Description: description, FolderIndex: req.FolderIndex,
		ExpectedSHA256: req.ExpectedSHA256,
		ContentType:    sharedfiles.ChatContentType(name),
		Actor:          email, ConversationID: origin,
		MaxFileBytes:  sharedfiles.MaxChatFileBytes,
		MaxTotalBytes: s.sharedFilesMaxTotalBytes(),
	}
	path := sharedfiles.ChatPromptPath(folder, name)

	// The write and the restage run under sharedFilesMu, like every other
	// library mutation (upload, rename, delete, the reconciler), so this
	// process never interleaves them. Across processes and replicas the store
	// serializes path claims with a transaction-scoped advisory lock and each
	// file's writes with its row lock.
	s.sharedFilesMu.Lock()
	defer s.sharedFilesMu.Unlock()

	var res store.ChatSharedFileWriteResult
	for attempt := 0; ; attempt++ {
		if write.NewID, err = randomToken(); err != nil {
			return nil, fmt.Errorf("shared_file_write: %w", err)
		}
		res, err = s.store.WriteChatSharedFile(ctx, write)
		if errors.Is(err, store.ErrSharedFileWriteConflict) && attempt < sharedFileWriteRetries {
			continue
		}
		break
	}
	if err != nil {
		return nil, sharedFileWriteError(err, path, req.Mode)
	}

	staged := true
	if serr := s.restageChatSharedFileLocked(ctx, res.File.ID); serr != nil {
		// The write is durable; only the sandbox-visible copy lags, and the
		// hourly reconciler (SyncSharedFiles) restages it from the database.
		staged = false
		logging.From(ctx).Warn("shared files: stage after chat write failed (will self-heal on the next maintenance pass)",
			"path", safe.LogSafe(path), "err", serr)
	}
	logging.From(ctx).Info("shared files: chat write",
		"path", safe.LogSafe(path), "mode", req.Mode, "created", res.Created,
		"size_bytes", res.File.SizeBytes, "origin", safe.LogSafe(origin))

	return sharedFileWriteResult(res, path, req.Mode, staged), nil
}

// restageChatSharedFileLocked restages one chat-written file from the
// database. The store reads the row and its text under a row lock (FOR SHARE)
// and stages while still holding it, so the staged copy is never older than
// the database when it finishes — even when another replica writes, renames or
// deletes the file at the same moment: that writer waits for this stage and
// restages after it. A row deleted in the meantime is not an error (whoever
// deleted it unstaged it). Caller holds sharedFilesMu.
func (s *Server) restageChatSharedFileLocked(ctx context.Context, id string) error {
	lib := s.sharedFilesLibrary()
	err := s.store.StageChatSharedFileLocked(ctx, id, lib.StageContent)
	if errors.Is(err, store.ErrSharedFileNotFound) {
		return nil
	}
	return err
}

// stageSharedFileLocked stages any row: an upload from its canonical bytes, a
// chat-written file from its text in the database (under the row lock — see
// restageChatSharedFileLocked). Caller holds sharedFilesMu.
func (s *Server) stageSharedFileLocked(ctx context.Context, lib sharedfiles.Library, f store.SharedFile) error {
	if !f.IsChatWritten() {
		return lib.Stage(f)
	}
	return s.restageChatSharedFileLocked(ctx, f.ID)
}

// unstageFreedPathLocked removes the staged copy at a path a rename or delete
// just freed — unless another row owns that path by now (on another replica, a
// chat may have created a new file there in between), in which case it
// restages that row instead. The check and the filesystem change run under the
// store's namespace lock, which every path claim also takes, so no new owner
// can appear between them. Caller holds sharedFilesMu.
func (s *Server) unstageFreedPathLocked(ctx context.Context, lib sharedfiles.Library, freed store.SharedFile) error {
	return s.store.SharedFilePathOwnerLocked(ctx, freed.Folder, freed.Name, func(owner *store.SharedFile) error {
		if owner == nil {
			return lib.Unstage(freed)
		}
		if owner.ID == freed.ID {
			return nil // not actually freed (a no-op rename): nothing to do
		}
		return s.stageSharedFileLocked(ctx, lib, *owner)
	})
}

func sharedFileWriteResult(res store.ChatSharedFileWriteResult, path, mode string, staged bool) *tools.SharedFileWriteResult {
	f := res.File
	out := &tools.SharedFileWriteResult{
		Status:         "ok",
		Path:           path,
		Mode:           mode,
		Created:        res.Created,
		SizeBytes:      f.SizeBytes,
		SHA256:         f.SHA256,
		PreviousSHA256: res.PreviousSHA256,
		CanUndo:        f.HasPrevious,
		Description:    f.Description,
		FolderIndex:    f.FolderIndex,
	}
	verb := map[string]string{
		tools.SharedFileModeCreate:  "Created",
		tools.SharedFileModeAppend:  "Appended to",
		tools.SharedFileModeReplace: "Replaced",
		tools.SharedFileModeUndo:    "Restored the previous version of",
	}[mode]
	if res.Created {
		verb = "Created"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s (%d bytes). Every conversation and scheduled task on this server can read it there", verb, path, f.SizeBytes)
	if staged {
		b.WriteString(" now")
	} else {
		b.WriteString(" — its readable copy is refreshing and may lag for up to an hour, but the write is saved")
	}
	fmt.Fprintf(&b, ". Its sha256 is %s: pass it as expected_sha256 the next time you replace it.", f.SHA256)
	out.Message = b.String()
	return out
}

// sharedFileWriteError turns a store refusal into the sentence the model acts
// on: what went wrong and the call that fixes it.
func sharedFileWriteError(err error, path, mode string) error {
	var stale *store.SharedFileStaleError
	var tooBig *store.SharedFileTooLargeError
	var full *store.SharedFileLibraryFullError
	switch {
	case errors.As(err, &stale), errors.As(err, &tooBig), errors.As(err, &full):
		return err
	case errors.Is(err, store.ErrSharedFileNotFound):
		return fmt.Errorf("there is no shared file at %s yet: create it first (mode 'create', or 'append' with a description)", path)
	case errors.Is(err, store.ErrSharedFileExists):
		return fmt.Errorf("%s already exists: use mode 'append' to add to it, or read it and use 'replace' with its expected_sha256", path)
	case errors.Is(err, store.ErrSharedFileNameIsFolder):
		return fmt.Errorf("%s collides with a folder of the same name in the shared library: choose another name", path)
	case errors.Is(err, store.ErrSharedFileAdminOwned):
		return fmt.Errorf("%s was published by an administrator and is read-only: copy it into your workspace, or write your version under another name", path)
	case errors.Is(err, store.ErrSharedFileDescriptionRequired):
		return fmt.Errorf("creating %s needs a description: one short line saying what the file is and when to read it — every conversation on this server is shown it", path)
	case errors.Is(err, store.ErrSharedFileExpectedHashRequired):
		return fmt.Errorf("replacing %s needs expected_sha256: read the file first (view_file prints its sha256) so you never overwrite a newer version", path)
	case errors.Is(err, store.ErrSharedFileNoPrevious):
		return fmt.Errorf("%s has no previous version to restore (only a replace or an undo leaves one)", path)
	case errors.Is(err, store.ErrSharedFileWriteConflict):
		return fmt.Errorf("another conversation wrote %s at the same moment: try the %s again", path, mode)
	default:
		return fmt.Errorf("shared_file_write failed: %w", err)
	}
}
