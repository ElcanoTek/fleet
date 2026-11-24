// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

// Package accountdelete deletes an account. Delete is the one function every
// deletion path calls — Settings → Admin → Users (DELETE /admin/users/{email}),
// `fleet admin rm` and `fleet chat user del` — so they cannot differ in what
// they leave behind. A later account created under the same email starts with
// nothing of the deleted one (#563).
//
// An account lives in three places, and Delete reaches each:
//
//   - the database: store.Store.DeleteUser, one transaction over every row
//     that is the account's (its doc comment, and the column list its guard
//     test pins);
//   - the API-key store (api_keys.json): the user keys that act as the
//     account are deleted (apikeys.Manager.DeleteUserKeys);
//   - the attachment directory on disk: the account's upload tree and its
//     private workspace trees in other people's multiplayer chats.
//
// The database goes first and alone decides whether the account is deleted: a
// refusal there (no such account, a team-shared project still owned) leaves
// the keys and files untouched. The key and file steps run after the commit
// and report what they could not do in Result.Problems instead of failing the
// deletion; the server's maintenance pass removes the files later
// (SweepOrphanUploads). A user key left behind that way still cannot act as a
// later account under the email: a user key is bound to its account's id and
// the chat edge admits it only for that account
// (principal.Principal.KeyAccountID).
package accountdelete

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ElcanoTek/fleet/internal/identity"
)

// Store is the database half: *store.Store.
type Store interface {
	// AccountForDeletion reads the id of the account under email
	// (store.Store.AccountForDeletion; ErrUserNotFound when none).
	AccountForDeletion(ctx context.Context, email string) (id string, err error)
	// DeleteAccount deletes the rows of the account expectedID, refusing when
	// the email names another account by then, and reports the deleted
	// account's id (store.Store.DeleteAccount).
	DeleteAccount(ctx context.Context, email, expectedID string) (accountID string, err error)
	// AccountDigestLive reports whether an account's email has the given
	// Digest now (store.Store.AccountDigestLive).
	AccountDigestLive(ctx context.Context, digest string) (bool, error)
}

// Keys is the API-key store: *apikeys.Manager.
type Keys interface {
	BindUnboundUserKeys(ctx context.Context, email, accountID string) (int, error)
	DeleteUserKeys(ctx context.Context, email, accountID string) (int, error)
}

// KeysNotBoundError refuses a deletion before anything is deleted: the
// address's user API keys that name no account yet could not be bound to the
// account first (the key store busy — apikeys.ErrStoreLocked — or not
// writable). Deleting without that would leave them for a later account
// under the address to adopt; the caller tries again.
type KeysNotBoundError struct{ Err error }

func (e *KeysNotBoundError) Error() string {
	return "binding the account's unbound user API keys to it before the deletion failed (" + e.Err.Error() + "); nothing was deleted, try again"
}

func (e *KeysNotBoundError) Unwrap() error { return e.Err }

// Options says where the account's keys and files are.
type Options struct {
	// Keys is the API-key store the server reads. nil is reported in
	// Result.Problems: a caller must not silently skip it.
	Keys Keys
	// AttachmentDir is the server's EMAIL_ATTACHMENT_DIR
	// (config.Config.EmailAttachmentDir), absolute. "" skips the file step.
	AttachmentDir string
	// ServerSetAside is the running server's own step before the database
	// deletion: it moves the account's private workspace trees aside
	// (SetAside) under the locks a turn takes, and returns them with undo,
	// which takes back whatever else it marked for the deletion (a
	// participant turn of the account still running, told not to put its
	// tree back) and is called if the deletion does not commit. nil (a CLI)
	// moves them aside under AttachmentDir.
	ServerSetAside func(email string) (aside []AsideTree, undo func(), err error)
	// ServerCleanup is the running server's own step, run after the database
	// deletion: it forgets the account's in-memory state and removes its
	// private workspace trees under the locks a turn in flight takes, each
	// through RemoveOrphanTree with live (so a tree whose address has an
	// account again stays). An error it returns — a tree it could not remove
	// — is reported as PrivateWorkspacesFailed. nil (a CLI, outside the server
	// process) removes those trees under AttachmentDir.
	ServerCleanup func(email string, live func(digest string) (bool, error)) error
}

// Result is what Delete did after the database deletion committed.
type Result struct {
	// KeysDeleted is how many user API keys were deleted.
	KeysDeleted int
	// Problems lists the steps after the database deletion that did not
	// complete. The account is deleted either way; each says what is left and
	// how to remove it (Problem.String).
	Problems []Problem
}

// ProblemKind names a step after the database deletion that did not complete.
type ProblemKind string

// The steps a deletion can leave.
const (
	// KeysUnavailable: no API-key store was given, so the user keys acting as
	// the account were not deleted.
	KeysUnavailable ProblemKind = "keys_unavailable"
	// KeysFailed: deleting the user keys failed (Err).
	KeysFailed ProblemKind = "keys_failed"
	// AttachmentDirUnreadable: the attachment directory (Path) does not
	// exist, so no file was removed there. Any other error reading it
	// refuses the deletion (FilesNotSetAsideError).
	AttachmentDirUnreadable ProblemKind = "attachment_dir_unreadable"
	// UploadsFailed: removing the upload tree failed (Err).
	UploadsFailed ProblemKind = "uploads_failed"
	// PrivateWorkspacesFailed: removing the private workspace trees failed
	// (Err).
	PrivateWorkspacesFailed ProblemKind = "private_workspaces_failed"
	// FilesHeldByNewAccount: files were under the address's paths after the
	// deletion committed (written after the trees were moved aside — by the
	// new account itself, or by something racing the deletion), and an
	// account created under the address since holds them, so they were left
	// in place (ErrHeldByNewAccount).
	FilesHeldByNewAccount ProblemKind = "files_held_by_new_account"
)

// ErrHeldByNewAccount is RemoveIfNoAccount's answer for a tree an account
// holds now: it was left in place.
var ErrHeldByNewAccount = errors.New("an account created under the address since holds them, so they were left in place")

// Problem is one step a deletion left: what, for which account, and the error
// or directory involved.
type Problem struct {
	Kind  ProblemKind
	Email string
	Path  string
	Err   string
}

// String is the problem in English, with what to do about it: the CLI prints
// it, the server logs it. (The web route words it through its own message
// catalog.)
func (p Problem) String() string {
	switch p.Kind {
	case KeysUnavailable:
		return "the API-key store was not available, so the user API keys acting as " + p.Email +
			" were not deleted; list them with `fleet sched apikey list` and delete them with `fleet sched apikey delete <key_id>`"
	case KeysFailed:
		return fmt.Sprintf("deleting the user API keys acting as %s failed (%s); "+
			"list them with `fleet sched apikey list` and delete them with `fleet sched apikey delete <key_id>`", p.Email, p.Err)
	case AttachmentDirUnreadable:
		return fmt.Sprintf("the attachment directory %s could not be read (%s), so no upload or private workspace file was removed there; "+
			"the server's maintenance pass removes them from the directory it uses", p.Path, p.Err)
	case UploadsFailed:
		return "removing the account's uploads failed: " + p.Err + "; the server's hourly maintenance pass tries again"
	case PrivateWorkspacesFailed:
		return "removing the account's private workspace files failed: " + p.Err + "; the server's hourly maintenance pass tries again"
	case FilesHeldByNewAccount:
		return "files under " + p.Email + " were left in place after the deletion: an account created under the address since holds them"
	}
	return string(p.Kind) + ": " + p.Err
}

// afterDatabaseDeletion is a test seam: it runs in Delete right after the
// database deletion committed, before the keys and files.
var afterDatabaseDeletion func()

// Delete deletes the account email: the database rows, then its user API
// keys, then its files. An error means nothing was deleted: the database's
// (store.ErrUserNotFound, *store.OwnsSharedProjectsError,
// store.ErrAccountChanged, …), a *KeysNotBoundError or a
// *FilesNotSetAsideError.
//
// Before the rows go, the address's user API keys that name no account yet
// (minted before keys named their account) are bound to this account, read
// first and named to the database deletion, which refuses when the address
// holds another account by then. After the commit no unbound key for the
// address is left for a later account to adopt, and the key step deletes the
// keys bound to the deleted account's id. If the database step refuses, the
// keys stay bound to the account, which still exists: they would have bound
// to it anyway.
//
// The address may hold a new account the moment the deletion commits, before
// the steps after it run, so nothing of the deleted account is left under the
// address for those steps to tell apart: its keys are bound to its id first
// and only those go (Keys.DeleteUserKeys), and its upload and private
// workspace trees are moved aside (SetAside) while it still exists — then
// removed after the commit, or moved back if the deletion does not commit.
// A tree that cannot be moved aside stops the deletion before anything is
// deleted (*FilesNotSetAsideError); one written under the address after the
// move is removed after the commit while no account has its digest
// (RemoveIfNoAccount), and reported otherwise.
func Delete(ctx context.Context, st Store, email string, opt Options) (Result, error) {
	// Each step normalizes the address the way its own store keys it.
	email = strings.TrimSpace(email)
	var res Result
	id, err := st.AccountForDeletion(ctx, email)
	if err != nil {
		return res, err
	}
	if opt.Keys != nil {
		if _, err := opt.Keys.BindUnboundUserKeys(ctx, email, id); err != nil {
			return res, &KeysNotBoundError{Err: err}
		}
	}
	// The files are moved aside while the account still exists — nobody can
	// create an account under the address until the commit — so a later
	// account under it never finds them under its digest.
	aside, undo, dirProblem, err := setAsideAccountFiles(email, opt)
	if err != nil {
		// A tree left under the address could be found by an account created
		// under it after the commit: nothing is deleted.
		return res, rollBackSetAside(&FilesNotSetAsideError{Err: err}, aside, undo)
	}
	accountID, err := st.DeleteAccount(ctx, email, id)
	if err != nil {
		return res, rollBackSetAside(err, aside, undo)
	}
	if afterDatabaseDeletion != nil {
		afterDatabaseDeletion()
	}
	live := func(digest string) (bool, error) { return st.AccountDigestLive(ctx, digest) }
	if opt.Keys == nil {
		res.Problems = append(res.Problems, Problem{Kind: KeysUnavailable, Email: email})
	} else if n, err := opt.Keys.DeleteUserKeys(ctx, email, accountID); err != nil {
		res.Problems = append(res.Problems, Problem{Kind: KeysFailed, Email: email, Err: err.Error()})
	} else {
		res.KeysDeleted = n
	}
	res.Problems = append(res.Problems, dirProblem...)
	var uploadsErr, stashErr error
	for _, t := range aside {
		if err := os.RemoveAll(t.To); err != nil {
			if t.Uploads {
				uploadsErr = firstError(uploadsErr, err)
			} else {
				stashErr = firstError(stashErr, err)
			}
		}
	}
	// Whatever is under the address's paths now — a tree that could not be
	// moved, or one written there after the move — is removed while no
	// account has its digest (RemoveIfNoAccount), and reported if it fails
	// or a new account holds it; the maintenance pass tries a failed one
	// again.
	if opt.AttachmentDir != "" && dirProblem == nil {
		uploadsErr = firstError(uploadsErr, removeUploadTree(UploadsDir(opt.AttachmentDir, email), Digest(email), live))
	}
	switch {
	case opt.ServerCleanup != nil:
		// The server forgets the address's in-memory state and removes any
		// private workspace tree still there.
		stashErr = firstError(stashErr, opt.ServerCleanup(email, live))
	case opt.AttachmentDir != "" && dirProblem == nil:
		stashErr = firstError(stashErr, removePrivateWorkspaces(PrivateWorkspaceRoot(opt.AttachmentDir), email, live))
	}
	res.Problems = append(res.Problems, treeProblem(UploadsFailed, email, uploadsErr)...)
	res.Problems = append(res.Problems, treeProblem(PrivateWorkspacesFailed, email, stashErr)...)
	return res, nil
}

// treeProblem is the problem a tree step's error makes: kind, or
// FilesHeldByNewAccount when a new account under the address holds the tree
// (ErrHeldByNewAccount); none for nil.
func treeProblem(kind ProblemKind, email string, err error) []Problem {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrHeldByNewAccount):
		return []Problem{{Kind: FilesHeldByNewAccount, Email: email, Err: err.Error()}}
	}
	return []Problem{{Kind: kind, Email: email, Err: err.Error()}}
}

// firstError is a when it is set, else b.
func firstError(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// FilesNotSetAsideError refuses a deletion before anything is deleted: one of
// the account's trees (its uploads, a private workspace tree) could not be
// moved aside (SetAside). Deleting the account with the tree still under the
// address would leave it for an account created under the address after the
// commit. The trees already moved are put back; the caller tries again once
// the cause (permissions, a full disk) is fixed.
type FilesNotSetAsideError struct{ Err error }

func (e *FilesNotSetAsideError) Error() string {
	return "the account's files could not be moved aside before the deletion (" + e.Err.Error() + "), so nothing was deleted; fix that and try again"
}

func (e *FilesNotSetAsideError) Unwrap() error { return e.Err }

// rollBackSetAside puts the trees moved aside back, takes back the marks the
// server set (undo), and returns err — with what could not be put back, if
// anything.
func rollBackSetAside(err error, aside []AsideTree, undo func()) error {
	rerr := restoreAside(aside)
	if undo != nil {
		undo()
	}
	if rerr != nil {
		return fmt.Errorf("%w; moving the account's files back failed: %w", err, rerr)
	}
	return err
}

// setAsideAccountFiles moves the account's upload tree and its private
// workspace trees aside (SetAside): the server's own step for the latter
// when there is one (Options.ServerSetAside, under the locks a turn takes),
// otherwise under AttachmentDir. It returns what it moved, the server's undo
// of its marks, and the problem of an attachment directory it could not read
// (no tree can be there to move). err is the first tree it could not move:
// the caller then puts the rest back and deletes nothing. A tree that is not
// there is not a failure.
func setAsideAccountFiles(email string, opt Options) (aside []AsideTree, undo func(), dirProblem []Problem, err error) {
	digest := Digest(email)
	dirOK := false
	if opt.AttachmentDir != "" {
		// Moving a path that does not exist moves nothing, so a CLI pointed
		// at the wrong directory would report nothing: say so instead. The
		// server's maintenance pass removes the files of deleted accounts
		// from the directory it really uses.
		// Only a confirmed absence means there is nothing to move: any other
		// error (no permission, an I/O error) refuses the deletion, since a
		// tree may be there that cannot be moved.
		if _, err := os.Stat(opt.AttachmentDir); os.IsNotExist(err) {
			dirProblem = append(dirProblem, Problem{Kind: AttachmentDirUnreadable, Email: email, Path: opt.AttachmentDir, Err: err.Error()})
		} else if err != nil {
			return aside, undo, dirProblem, fmt.Errorf("the attachment directory: %w", err)
		} else {
			dirOK = true
			t, moved, serr := SetAside(UploadsDir(opt.AttachmentDir, email), digest)
			if serr != nil {
				return aside, undo, dirProblem, fmt.Errorf("the upload tree: %w", serr)
			}
			if moved {
				t.Uploads = true
				aside = append(aside, t)
			}
		}
	}
	switch {
	case opt.ServerSetAside != nil:
		ts, u, serr := opt.ServerSetAside(email)
		aside, undo = append(aside, ts...), u
		if serr != nil {
			return aside, undo, dirProblem, fmt.Errorf("a private workspace tree: %w", serr)
		}
	case dirOK:
		ts, serr := setAsidePrivateWorkspaces(PrivateWorkspaceRoot(opt.AttachmentDir), email)
		aside = append(aside, ts...)
		if serr != nil {
			return aside, undo, dirProblem, fmt.Errorf("a private workspace tree: %w", serr)
		}
	}
	return aside, undo, dirProblem, nil
}

// AsideTree is an account tree moved aside before its account's deletion
// (SetAside): removed once the deletion commits, moved back if it does not.
type AsideTree struct {
	// From is where the tree was; To where it is now.
	From, To string
	// Uploads says it is the upload tree (else a private workspace tree).
	Uploads bool
}

// SetAside moves path, a tree of the account whose Digest is digest, to an
// aside name beside it (asidePrefix, which nothing reads and every sweep
// removes) and reports whether there was one to move. Account deletion moves
// the account's trees aside before its database deletion commits, while no
// other account can hold the address, and removes them after; a crash in
// between leaves aside trees, which the maintenance pass removes.
func SetAside(path, digest string) (AsideTree, bool, error) {
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return AsideTree{}, false, nil
	} else if err != nil {
		return AsideTree{}, false, err
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return AsideTree{}, false, err
	}
	to := filepath.Join(filepath.Dir(path), asidePrefix+digest+"-"+hex.EncodeToString(suffix[:]))
	if err := os.Rename(path, to); err != nil {
		if os.IsNotExist(err) {
			return AsideTree{}, false, nil
		}
		return AsideTree{}, false, err
	}
	return AsideTree{From: path, To: to}, true, nil
}

// restoreAside moves the trees back where they were, after a deletion that
// did not commit, and returns the first failure.
func restoreAside(aside []AsideTree) error {
	var firstErr error
	for _, t := range aside {
		if err := os.Rename(t.To, t.From); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// setAsidePrivateWorkspaces moves participants/<Digest(email)> aside in every
// conversation directory under root (SetAside), and returns what it moved
// and the first failure. A missing root has nothing to move.
func setAsidePrivateWorkspaces(root, email string) ([]AsideTree, error) {
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	digest := Digest(email)
	var aside []AsideTree
	var firstErr error
	for _, d := range entries {
		if !d.IsDir() || !filepath.IsLocal(d.Name()) {
			continue
		}
		t, moved, err := SetAside(filepath.Join(root, d.Name(), "participants", digest), digest)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if moved {
			aside = append(aside, t)
		}
	}
	return aside, firstErr
}

// Digest is the directory name an account's files are kept under: the first
// 32 hex characters of sha256 of the email as accounts are keyed
// (identity.NormalizeEmail). A hash, not the address, so a directory listing
// or backup does not enumerate who uses the box (docs/ATTACHMENT-SCOPING.md).
func Digest(email string) string {
	sum := sha256.Sum256([]byte(identity.NormalizeEmail(email)))
	return hex.EncodeToString(sum[:])[:32]
}

// UploadsDir is the account's upload tree:
// <attachmentDir>/uploads/<Digest(email)>.
func UploadsDir(attachmentDir, email string) string {
	return filepath.Join(attachmentDir, "uploads", Digest(email))
}

// PrivateWorkspaceRoot is where per-participant private workspace trees of
// multiplayer chats are kept: <attachmentDir>/multiplayer-private, one
// directory per conversation, holding participants/<Digest(email)>.
func PrivateWorkspaceRoot(attachmentDir string) string {
	return filepath.Join(attachmentDir, "multiplayer-private")
}

// removeUploadTree removes the account's upload tree through
// RemoveOrphanTree (live); a tree that is not there is nothing to remove, and
// asks nothing.
func removeUploadTree(path, digest string, live func(digest string) (bool, error)) error {
	return RemoveIfNoAccount(path, digest, live)
}

// RemoveIfNoAccount removes path, the tree of the address whose Digest is
// digest, unless an account has that digest now (RemoveOrphanTree), and
// reports ErrHeldByNewAccount when one does and the tree is there. A tree
// that is not there is nothing to remove, and asks nothing.
func RemoveIfNoAccount(path, digest string, live func(digest string) (bool, error)) error {
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	gone, err := RemoveOrphanTree(path, digest, live)
	if err != nil || gone {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return ErrHeldByNewAccount
	}
	return nil
}

// removePrivateWorkspaces removes participants/<Digest(email)> from every
// conversation directory under root, each through RemoveIfNoAccount (live).
// A missing root is nothing to remove.
func removePrivateWorkspaces(root, email string, live func(digest string) (bool, error)) error {
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	digest := Digest(email)
	var firstErr error
	for _, d := range entries {
		if !d.IsDir() || !filepath.IsLocal(d.Name()) {
			continue
		}
		if err := RemoveIfNoAccount(filepath.Join(root, d.Name(), "participants", digest), digest, live); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// IsDigest reports whether name has the shape of a Digest.
func IsDigest(name string) bool {
	if len(name) != 32 {
		return false
	}
	for _, c := range name {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// UploadDigests lists the Digest-named upload trees under attachmentDir. A
// missing uploads directory has none.
func UploadDigests(attachmentDir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(attachmentDir, "uploads"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range entries {
		if d.IsDir() && IsDigest(d.Name()) {
			out = append(out, d.Name())
		}
	}
	return out, nil
}

// RemoveUploads removes the upload trees named by digests under
// attachmentDir, each through RemoveOrphanTree, so one whose address has an
// account again by then (live) stays. It returns how many it removed. The
// trees an interrupted removal left set aside are not its business: the
// caller removes them on every pass (RemoveSetAsideUploads), orphans or not.
func RemoveUploads(attachmentDir string, digests []string, live func(digest string) (bool, error)) (int, error) {
	dir := filepath.Join(attachmentDir, "uploads")
	removed := 0
	var firstErr error
	for _, d := range digests {
		if !IsDigest(d) {
			continue
		}
		ok, err := RemoveOrphanTree(filepath.Join(dir, d), d, live)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if ok {
			removed++
		}
	}
	return removed, firstErr
}

// asidePrefix names a tree RemoveOrphanTree moved aside to remove it. Nothing
// reads such a name (it is not a Digest), so one left by an interrupted
// removal is removed by the next sweep (RemoveAside).
const asidePrefix = ".removing-"

// RemoveOrphanTree removes path, the tree of the address whose Digest is
// digest, unless live reports an account for it now. live is asked
// immediately before the removal rather than taken from the caller's earlier
// listing, so an account created under the address since keeps the files it
// has written; and the tree is first renamed aside, so a file written after
// the rename lands in a fresh tree instead of the one being removed. It
// reports whether it removed the tree (a tree already gone is not an error).
func RemoveOrphanTree(path, digest string, live func(digest string) (bool, error)) (bool, error) {
	ok, err := live(digest)
	if err != nil {
		return false, err
	}
	if ok {
		return false, nil
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return false, err
	}
	aside := filepath.Join(filepath.Dir(path), asidePrefix+digest+"-"+hex.EncodeToString(suffix[:]))
	if err := os.Rename(path, aside); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, os.RemoveAll(aside)
}

// IsAside reports whether name is a tree RemoveOrphanTree moved aside.
func IsAside(name string) bool {
	return strings.HasPrefix(name, asidePrefix) && filepath.IsLocal(name) && !strings.ContainsAny(name, `/\`)
}

// RemoveAside removes the trees under dir that a RemoveOrphanTree moved aside
// and did not finish removing, and returns the first failure (a missing dir
// has none). What it leaves is tried again on the next pass.
func RemoveAside(dir string) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var firstErr error
	for _, e := range entries {
		if IsAside(e.Name()) {
			if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// RemoveSetAsideUploads removes the upload trees under attachmentDir that an
// interrupted removal left set aside (RemoveAside on its uploads directory).
// A sweep calls it on every pass, whether or not it finds an orphan.
func RemoveSetAsideUploads(attachmentDir string) error {
	return RemoveAside(filepath.Join(attachmentDir, "uploads"))
}

// SweepOrphanUploads removes the upload trees under attachmentDir that belong
// to no account: those of accounts deleted before deletion removed them, or by
// a CLI that resolved another attachment directory. It removes the trees an
// interrupted removal set aside first, then reads the directory BEFORE
// listing the accounts (accounts, once), so an account created while it runs
// is either in the list or had no directory yet; and right before each
// removal it asks live about that one digest (RemoveOrphanTree), so a tree
// whose address has an account again by then stays. It returns how many it
// removed.
func SweepOrphanUploads(attachmentDir string, accounts func() ([]string, error), live func(digest string) (bool, error)) (int, error) {
	if err := RemoveSetAsideUploads(attachmentDir); err != nil {
		return 0, err
	}
	digests, err := UploadDigests(attachmentDir)
	if err != nil || len(digests) == 0 {
		return 0, err
	}
	emails, err := accounts()
	if err != nil {
		return 0, err
	}
	listed := make(map[string]bool, len(emails))
	for _, e := range emails {
		listed[Digest(e)] = true
	}
	var orphans []string
	for _, d := range digests {
		if !listed[d] {
			orphans = append(orphans, d)
		}
	}
	return RemoveUploads(attachmentDir, orphans, live)
}
