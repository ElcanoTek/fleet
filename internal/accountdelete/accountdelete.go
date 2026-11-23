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
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ElcanoTek/fleet/internal/identity"
)

// Store is the database half: *store.Store.
type Store interface {
	DeleteUser(ctx context.Context, email string) error
}

// Keys is the API-key store: *apikeys.Manager.
type Keys interface {
	DeleteUserKeys(ctx context.Context, email string) (int, error)
}

// Options says where the account's keys and files are.
type Options struct {
	// Keys is the API-key store the server reads. nil is reported in
	// Result.Problems: a caller must not silently skip it.
	Keys Keys
	// AttachmentDir is the server's EMAIL_ATTACHMENT_DIR
	// (config.Config.EmailAttachmentDir), absolute. "" skips the file step.
	AttachmentDir string
	// ServerCleanup is the running server's own step, run after the database
	// deletion: it forgets the account's in-memory state and removes its
	// private workspace trees under the locks a turn in flight takes. nil (a
	// CLI, outside the server process) removes those trees under
	// AttachmentDir.
	ServerCleanup func(email string)
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
	// AttachmentDirUnreadable: the attachment directory (Path) could not be
	// read, so no file was removed there.
	AttachmentDirUnreadable ProblemKind = "attachment_dir_unreadable"
	// UploadsFailed: removing the upload tree failed (Err).
	UploadsFailed ProblemKind = "uploads_failed"
	// PrivateWorkspacesFailed: removing the private workspace trees failed
	// (Err).
	PrivateWorkspacesFailed ProblemKind = "private_workspaces_failed"
)

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
		return "removing the account's uploads failed: " + p.Err
	case PrivateWorkspacesFailed:
		return "removing the account's private workspace files failed: " + p.Err
	}
	return string(p.Kind) + ": " + p.Err
}

// Delete deletes the account email: the database rows, then its user API
// keys, then its files. An error is the database's (store.ErrUserNotFound,
// *store.OwnsSharedProjectsError, …) and means nothing was deleted.
func Delete(ctx context.Context, st Store, email string, opt Options) (Result, error) {
	// Each step normalizes the address the way its own store keys it.
	email = strings.TrimSpace(email)
	var res Result
	if err := st.DeleteUser(ctx, email); err != nil {
		return res, err
	}
	if opt.Keys == nil {
		res.Problems = append(res.Problems, Problem{Kind: KeysUnavailable, Email: email})
	} else if n, err := opt.Keys.DeleteUserKeys(ctx, email); err != nil {
		res.Problems = append(res.Problems, Problem{Kind: KeysFailed, Email: email, Err: err.Error()})
	} else {
		res.KeysDeleted = n
	}
	if opt.AttachmentDir != "" {
		// Removing a path that does not exist succeeds, so a CLI pointed at
		// the wrong directory would report nothing: say so instead. The
		// server's maintenance pass removes the files of deleted accounts
		// from the directory it really uses.
		if _, err := os.Stat(opt.AttachmentDir); err != nil {
			res.Problems = append(res.Problems, Problem{Kind: AttachmentDirUnreadable, Email: email, Path: opt.AttachmentDir, Err: err.Error()})
		} else if err := os.RemoveAll(UploadsDir(opt.AttachmentDir, email)); err != nil {
			res.Problems = append(res.Problems, Problem{Kind: UploadsFailed, Email: email, Err: err.Error()})
		}
	}
	switch {
	case opt.ServerCleanup != nil:
		opt.ServerCleanup(email)
	case opt.AttachmentDir != "":
		if err := removePrivateWorkspaces(PrivateWorkspaceRoot(opt.AttachmentDir), email); err != nil {
			res.Problems = append(res.Problems, Problem{Kind: PrivateWorkspacesFailed, Email: email, Err: err.Error()})
		}
	}
	return res, nil
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

// removePrivateWorkspaces removes participants/<Digest(email)> from every
// conversation directory under root. A missing root is nothing to remove.
func removePrivateWorkspaces(root, email string) error {
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
		if err := os.RemoveAll(filepath.Join(root, d.Name(), "participants", digest)); err != nil && firstErr == nil {
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
// and did not finish removing.
func RemoveAside(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if IsAside(e.Name()) {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}

// RemoveSetAsideUploads removes the upload trees under attachmentDir that an
// interrupted removal left set aside (RemoveAside on its uploads directory).
// A sweep calls it on every pass, whether or not it finds an orphan.
func RemoveSetAsideUploads(attachmentDir string) {
	RemoveAside(filepath.Join(attachmentDir, "uploads"))
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
	RemoveSetAsideUploads(attachmentDir)
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
