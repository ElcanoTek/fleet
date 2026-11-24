// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package accountdelete

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
)

type fakeStore struct {
	err     error
	deleted []string
	// live is the set of digests an account has now (AccountDigestLive);
	// liveErr makes that lookup fail.
	live    map[string]bool
	liveErr error
}

func (s *fakeStore) AccountForDeletion(context.Context, string) (string, error) {
	return "deleted-account-id", nil
}

func (s *fakeStore) DeleteAccount(_ context.Context, email, _ string) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	s.deleted = append(s.deleted, email)
	return "deleted-account-id", nil
}

func (s *fakeStore) AccountDigestLive(_ context.Context, digest string) (bool, error) {
	if s.liveErr != nil {
		return false, s.liveErr
	}
	return s.live[digest], nil
}

type fakeKeys struct {
	n       int
	err     error
	bindErr error
	calls   []string
	ids     []string
}

func (k *fakeKeys) BindUnboundUserKeys(context.Context, string, string) (int, error) {
	return 0, k.bindErr
}

func (k *fakeKeys) DeleteUserKeys(_ context.Context, email, accountID string) (int, error) {
	k.calls = append(k.calls, email)
	k.ids = append(k.ids, accountID)
	return k.n, k.err
}

// seedFiles makes the upload and private workspace trees of email under att
// and returns their paths.
func seedFiles(t *testing.T, att, email string) []string {
	t.Helper()
	dirs := []string{
		filepath.Join(UploadsDir(att, email), "tok"),
		filepath.Join(PrivateWorkspaceRoot(att), "conv-1", "participants", Digest(email), "attachments"),
		filepath.Join(PrivateWorkspaceRoot(att), "conv-2", "participants", Digest(email)),
	}
	files := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(d, "f.txt")
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return files
}

func TestDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("deletes the rows, then the keys and files", func(t *testing.T) {
		att := t.TempDir()
		goneFiles := seedFiles(t, att, "gone@x.com")
		stayFiles := seedFiles(t, att, "stay@x.com")
		st, keys := &fakeStore{}, &fakeKeys{n: 2}
		res, err := Delete(ctx, st, " Gone@x.com ", Options{Keys: keys, AttachmentDir: att})
		if err != nil || res.KeysDeleted != 2 || len(res.Problems) != 0 {
			t.Fatalf("Delete = %+v, %v", res, err)
		}
		if len(st.deleted) != 1 || len(keys.calls) != 1 || keys.ids[0] != "deleted-account-id" {
			t.Fatalf("store calls %v, key calls %v for %v; want the deleted account's id", st.deleted, keys.calls, keys.ids)
		}
		for _, f := range goneFiles {
			if _, err := os.Stat(f); !os.IsNotExist(err) {
				t.Errorf("%s survived (err %v)", f, err)
			}
		}
		for _, f := range stayFiles {
			if _, err := os.Stat(f); err != nil {
				t.Errorf("another account's %s was removed: %v", f, err)
			}
		}
	})
	t.Run("a refused database deletion touches nothing else", func(t *testing.T) {
		att := t.TempDir()
		files := seedFiles(t, att, "gone@x.com")
		refusal := errors.New("owns a team-shared project")
		keys := &fakeKeys{}
		cleaned := false
		_, err := Delete(ctx, &fakeStore{err: refusal}, "gone@x.com", Options{Keys: keys, AttachmentDir: att,
			ServerCleanup: func(string, func(string) (bool, error)) error { cleaned = true; return nil }})
		if !errors.Is(err, refusal) {
			t.Fatalf("err = %v, want the store's refusal", err)
		}
		if len(keys.calls) != 0 || cleaned {
			t.Fatalf("keys deleted %v / server cleanup ran %v after a refusal", keys.calls, cleaned)
		}
		for _, f := range files {
			if _, err := os.Stat(f); err != nil {
				t.Errorf("%s was removed after a refusal: %v", f, err)
			}
		}
	})
	t.Run("a key store failure is reported, not fatal", func(t *testing.T) {
		res, err := Delete(ctx, &fakeStore{}, "gone@x.com", Options{Keys: &fakeKeys{err: errors.New("disk full")}})
		if err != nil || len(res.Problems) != 1 || res.Problems[0].Kind != KeysFailed || !strings.Contains(res.Problems[0].String(), "fleet sched apikey") {
			t.Fatalf("Delete = %+v, %v", res, err)
		}
		res, err = Delete(ctx, &fakeStore{}, "gone@x.com", Options{})
		if err != nil || len(res.Problems) != 1 || res.Problems[0].Kind != KeysUnavailable {
			t.Fatalf("no key store: %+v, %v", res, err)
		}
	})
	t.Run("the server's own cleanup replaces the private workspace removal", func(t *testing.T) {
		att := t.TempDir()
		files := seedFiles(t, att, "gone@x.com")
		var got string
		if _, err := Delete(ctx, &fakeStore{}, "gone@x.com", Options{Keys: &fakeKeys{}, AttachmentDir: att,
			ServerCleanup: func(email string, _ func(string) (bool, error)) error { got = email; return nil }}); err != nil {
			t.Fatal(err)
		}
		if got != "gone@x.com" {
			t.Fatalf("server cleanup got %q", got)
		}
		if _, err := os.Stat(files[0]); !os.IsNotExist(err) {
			t.Errorf("uploads survived with a server cleanup set (err %v)", err)
		}
	})
}

// skipAsRoot skips a test that relies on directory permissions, which root
// ignores.
func skipAsRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not stop root")
	}
}

// readOnlyDir makes dir unwritable (nothing in it can be renamed or removed)
// for the rest of the test.
func readOnlyDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}

// A file removal after the commit that fails is reported (with what the
// maintenance pass does about it): a tree written under the address after
// the move that cannot be removed, or the server's own cleanup failing. Not
// parallel: it uses afterDatabaseDeletion.
func TestDeleteReportsFailedRemovals(t *testing.T) {
	skipAsRoot(t)
	ctx := context.Background()
	att := t.TempDir()
	seedFiles(t, att, "gone@x.com")
	uploads := filepath.Join(att, "uploads")
	participants := filepath.Join(PrivateWorkspaceRoot(att), "conv-1", "participants")
	afterDatabaseDeletion = func() {
		for _, d := range []string{filepath.Join(UploadsDir(att, "gone@x.com"), "late"), filepath.Join(participants, Digest("gone@x.com"), "late")} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				t.Error(err)
			}
		}
		readOnlyDir(t, uploads)
		readOnlyDir(t, participants)
	}
	defer func() { afterDatabaseDeletion = nil }()
	res, err := Delete(ctx, &fakeStore{}, "gone@x.com", Options{Keys: &fakeKeys{}, AttachmentDir: att})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[ProblemKind]bool{}
	for _, p := range res.Problems {
		kinds[p.Kind] = true
		if !strings.Contains(p.String(), "maintenance pass tries again") {
			t.Errorf("%s does not say the maintenance pass tries again", p.String())
		}
	}
	if len(res.Problems) != 2 || !kinds[UploadsFailed] || !kinds[PrivateWorkspacesFailed] {
		t.Fatalf("problems = %+v, want an upload and a private workspace failure", res.Problems)
	}
	afterDatabaseDeletion = nil
	// The server's own cleanup failing is reported the same way.
	res, err = Delete(ctx, &fakeStore{}, "gone@x.com", Options{Keys: &fakeKeys{},
		ServerCleanup: func(string, func(string) (bool, error)) error { return errors.New("read-only file system") }})
	if err != nil || len(res.Problems) != 1 || res.Problems[0].Kind != PrivateWorkspacesFailed {
		t.Fatalf("server step failure: %+v, %v", res, err)
	}
}

// A tree that cannot be moved aside before the commit stops the deletion:
// nothing is deleted, the trees already moved go back, the server's marks
// are taken back, and nothing is left aside. A tree that is not there is no
// failure.
func TestDeleteRefusedWhenATreeCannotBeMovedAside(t *testing.T) {
	skipAsRoot(t)
	t.Parallel()
	ctx := context.Background()

	att := t.TempDir()
	files := seedFiles(t, att, "gone@x.com")
	readOnlyDir(t, filepath.Join(PrivateWorkspaceRoot(att), "conv-2", "participants"))
	st, keys := &fakeStore{}, &fakeKeys{}
	_, err := Delete(ctx, st, "gone@x.com", Options{Keys: keys, AttachmentDir: att})
	var notMoved *FilesNotSetAsideError
	if !errors.As(err, &notMoved) || !strings.Contains(err.Error(), "nothing was deleted") {
		t.Fatalf("err = %v, want a FilesNotSetAsideError saying nothing was deleted", err)
	}
	if len(st.deleted) != 0 || len(keys.calls) != 0 {
		t.Fatalf("store deletions %v, key deletions %v; want none", st.deleted, keys.calls)
	}
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s is not where it was: %v", f, err)
		}
	}
	for _, dir := range []string{filepath.Join(att, "uploads"), filepath.Join(PrivateWorkspaceRoot(att), "conv-1", "participants"), filepath.Join(PrivateWorkspaceRoot(att), "conv-2", "participants")} {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if IsAside(e.Name()) {
				t.Errorf("%s was left aside in %s", e.Name(), dir)
			}
		}
	}

	// The server's step failing after it moved a tree and marked a turn: the
	// tree goes back and the mark is taken back.
	att2 := t.TempDir()
	files2 := seedFiles(t, att2, "gone@x.com")
	undone := false
	_, err = Delete(ctx, &fakeStore{}, "gone@x.com", Options{Keys: &fakeKeys{}, AttachmentDir: att2,
		ServerSetAside: func(email string) ([]AsideTree, func(), error) {
			tr, _, err := SetAside(filepath.Join(PrivateWorkspaceRoot(att2), "conv-1", "participants", Digest(email)), Digest(email))
			if err != nil {
				t.Error(err)
			}
			return []AsideTree{tr}, func() { undone = true }, errors.New("permission denied")
		}})
	if !errors.As(err, &notMoved) {
		t.Fatalf("server step failure: err %v, want a FilesNotSetAsideError", err)
	}
	if !undone {
		t.Error("the server's marks were not taken back")
	}
	for _, f := range files2 {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s is not where it was: %v", f, err)
		}
	}
}

// An attachment directory that exists but cannot be read refuses the
// deletion: only a directory that is not there counts as nothing to move.
// Covers the directory itself unreadable and a parent that cannot be looked
// into (so even its existence cannot be confirmed).
func TestDeleteRefusedWhenTheAttachmentDirCannotBeRead(t *testing.T) {
	skipAsRoot(t)
	t.Parallel()
	for _, tc := range []struct {
		name   string
		closed func(parent, att string) string
	}{
		{"the directory", func(_, att string) string { return att }},
		{"its parent", func(parent, _ string) string { return parent }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parent := t.TempDir()
			att := filepath.Join(parent, "attachments")
			files := seedFiles(t, att, "gone@x.com")
			closed := tc.closed(parent, att)
			if err := os.Chmod(closed, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(closed, 0o700) })

			st, keys := &fakeStore{}, &fakeKeys{}
			_, err := Delete(context.Background(), st, "gone@x.com", Options{Keys: keys, AttachmentDir: att})
			var notMoved *FilesNotSetAsideError
			if !errors.As(err, &notMoved) || !strings.Contains(err.Error(), "nothing was deleted") {
				t.Fatalf("err = %v, want a FilesNotSetAsideError saying nothing was deleted", err)
			}
			if len(st.deleted) != 0 || len(keys.calls) != 0 {
				t.Fatalf("store deletions %v, key deletions %v; want none", st.deleted, keys.calls)
			}
			if err := os.Chmod(closed, 0o700); err != nil {
				t.Fatal(err)
			}
			for _, f := range files {
				if _, err := os.Stat(f); err != nil {
					t.Errorf("%s is not where it was: %v", f, err)
				}
			}
		})
	}
}

// The account's trees are moved aside before the database deletion: a
// deletion that does not commit moves them back, and leaves nothing aside.
func TestDeleteThatDoesNotCommitPutsTheFilesBack(t *testing.T) {
	t.Parallel()
	att := t.TempDir()
	files := seedFiles(t, att, "gone@x.com")
	_, err := Delete(context.Background(), &fakeStore{err: errors.New("owns a team-shared project")}, "gone@x.com", Options{Keys: &fakeKeys{}, AttachmentDir: att})
	if err == nil {
		t.Fatal("the refused deletion reported success")
	}
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s is not back where it was: %v", f, err)
		}
	}
	for _, dir := range []string{filepath.Join(att, "uploads"), filepath.Join(PrivateWorkspaceRoot(att), "conv-1", "participants"), filepath.Join(PrivateWorkspaceRoot(att), "conv-2", "participants")} {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if IsAside(e.Name()) {
				t.Errorf("%s was left aside in %s", e.Name(), dir)
			}
		}
	}
}

// racedStore is a store in which another deletion of the account commits
// between this deletion's read and its own database step: the first read
// finds the account, every read after it finds after (or afterErr), and the
// database step fails with fakeStore.err.
type racedStore struct {
	fakeStore
	reads    int
	after    string
	afterErr error
}

func (s *racedStore) AccountForDeletion(context.Context, string) (string, error) {
	s.reads++
	if s.reads == 1 {
		return "deleted-account-id", nil
	}
	return s.after, s.afterErr
}

// A deletion whose database step fails because another deletion of the
// account committed meanwhile removes the trees it moved aside instead of
// putting them back: the address no longer holds the account they belong
// to. The same when a new account holds the address by then. The server's
// marks stay.
func TestDeleteAfterAnotherDeletionRemovesTheTreesItMovedAside(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		after    string
		afterErr error
		err      error
	}{
		{"the account is gone", "", errors.New("user not found"), errors.New("user not found")},
		{"a new account holds the address", "new-account-id", nil, errors.New("the account changed while it was being deleted")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			att := t.TempDir()
			files := seedFiles(t, att, "gone@x.com")
			st := &racedStore{fakeStore: fakeStore{err: tc.err}, after: tc.after, afterErr: tc.afterErr}
			undone := false
			_, err := Delete(context.Background(), st, "gone@x.com", Options{Keys: &fakeKeys{}, AttachmentDir: att,
				ServerSetAside: func(email string) ([]AsideTree, func(), error) {
					aside, err := setAsidePrivateWorkspaces(PrivateWorkspaceRoot(att), email)
					return aside, func() { undone = true }, err
				}})
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want the database's %v", err, tc.err)
			}
			var unconfirmed *OutcomeUnconfirmedError
			if errors.As(err, &unconfirmed) {
				t.Fatalf("err = %v, want a confirmed outcome", err)
			}
			for _, f := range files {
				if _, err := os.Stat(f); !os.IsNotExist(err) {
					t.Errorf("%s is under the address again (stat err %v)", f, err)
				}
			}
			assertNothingAside(t, att)
			if undone {
				t.Error("the server's marks were taken back")
			}
		})
	}
}

// A deletion whose database step fails and after which the account cannot
// be read back holds the trees it moved aside instead of putting them back:
// an address that answers as live may hold a new account created after a
// deletion that did commit. The held trees record the account's id, survive
// the sweep of set-aside trees, and the server's marks stay.
func TestDeleteWithAnUnconfirmedOutcomeHoldsTheTrees(t *testing.T) {
	t.Parallel()
	down := errors.New("connection refused")
	for _, tc := range []struct {
		name    string
		live    map[string]bool
		liveErr error
	}{
		{"the address is live but the account cannot be read", map[string]bool{Digest("gone@x.com"): true}, nil},
		{"nothing can be read", nil, down},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			att := t.TempDir()
			files := seedFiles(t, att, "gone@x.com")
			st := &racedStore{fakeStore: fakeStore{err: errors.New("statement timeout"), live: tc.live, liveErr: tc.liveErr}, afterErr: down}
			undone := false
			_, err := Delete(context.Background(), st, "gone@x.com", Options{Keys: &fakeKeys{}, AttachmentDir: att,
				ServerSetAside: func(email string) ([]AsideTree, func(), error) {
					aside, err := setAsidePrivateWorkspaces(PrivateWorkspaceRoot(att), email)
					return aside, func() { undone = true }, err
				}})
			var unconfirmed *OutcomeUnconfirmedError
			if !errors.As(err, &unconfirmed) || !errors.Is(unconfirmed.ReadErr, down) ||
				!strings.Contains(err.Error(), "statement timeout") || !strings.Contains(err.Error(), "held") {
				t.Fatalf("err = %v, want an OutcomeUnconfirmedError naming both causes", err)
			}
			for _, f := range files {
				if _, err := os.Stat(f); !os.IsNotExist(err) {
					t.Errorf("%s was put back under the address (stat err %v)", f, err)
				}
			}
			assertNothingAside(t, att)
			dirs := []string{filepath.Join(att, "uploads"), filepath.Join(PrivateWorkspaceRoot(att), "conv-1", "participants"), filepath.Join(PrivateWorkspaceRoot(att), "conv-2", "participants")}
			for _, dir := range dirs {
				if err := RemoveAside(dir); err != nil {
					t.Fatal(err)
				}
			}
			for _, dir := range dirs {
				if got := heldIn(t, dir); len(got) != 1 || got[0] != "deleted-account-id" {
					t.Errorf("held in %s: %v, want one tree recording deleted-account-id", dir, got)
				}
			}
			if undone {
				t.Error("the server's marks were taken back")
			}
		})
	}
}

// A deletion with an unconfirmed outcome holds every tree it moved aside even
// when holding one fails: that tree is tried under other names and, failing
// every time, put back where it was, never left under its set-aside name
// (which the sweeps remove whatever the account). Every other tree is held,
// the error names the one put back, and no file is lost. Not parallel: it
// replaces holdRename.
func TestHoldingATreeThatFails(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failures int
		putBack  bool
	}{
		{"it fails twice, then is held", 2, false},
		{"it always fails", 1 << 30, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			att := t.TempDir()
			files := seedFiles(t, att, "gone@x.com")
			conv1 := filepath.Join(PrivateWorkspaceRoot(att), "conv-1", "participants")
			failures := tc.failures
			holdRename = func(from, to string) error {
				if filepath.Dir(from) == conv1 && failures > 0 {
					failures--
					return errors.New("no space left on device")
				}
				return os.Rename(from, to)
			}
			defer func() { holdRename = os.Rename }()
			down := errors.New("connection refused")
			st := &racedStore{fakeStore: fakeStore{err: errors.New("statement timeout"), liveErr: down}, afterErr: down}
			_, err := Delete(context.Background(), st, "gone@x.com", Options{Keys: &fakeKeys{}, AttachmentDir: att})
			var unconfirmed *OutcomeUnconfirmedError
			if !errors.As(err, &unconfirmed) {
				t.Fatalf("err = %v, want an OutcomeUnconfirmedError", err)
			}
			assertNothingAside(t, att)
			uploads, conv2 := filepath.Join(att, "uploads"), filepath.Join(PrivateWorkspaceRoot(att), "conv-2", "participants")
			for _, dir := range []string{uploads, conv2} {
				if got := heldIn(t, dir); len(got) != 1 {
					t.Errorf("held in %s: %v, want one tree", dir, got)
				}
			}
			if tc.putBack {
				if unconfirmed.HoldErr == nil || !strings.Contains(unconfirmed.HoldErr.Error(), "put back") {
					t.Fatalf("HoldErr = %v, want it to name the tree put back", unconfirmed.HoldErr)
				}
				if _, err := os.Stat(files[1]); err != nil {
					t.Errorf("the tree that could not be held is not back: %v", err)
				}
				if got := heldIn(t, conv1); len(got) != 0 {
					t.Errorf("held in %s: %v, want none", conv1, got)
				}
			} else {
				if unconfirmed.HoldErr != nil {
					t.Fatalf("HoldErr = %v, want every tree held", unconfirmed.HoldErr)
				}
				if got := heldIn(t, conv1); len(got) != 1 {
					t.Errorf("held in %s: %v, want one tree", conv1, got)
				}
			}
			n := 0
			_ = filepath.WalkDir(att, func(_ string, d os.DirEntry, err error) error {
				if err == nil && d.Name() == "f.txt" {
					n++
				}
				return nil
			})
			if n != len(files) {
				t.Errorf("%d of the account's %d files are left", n, len(files))
			}
		})
	}
}

// heldIn lists the account ids the trees held in dir record.
func heldIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, e := range entries {
		if digest, id, ok := parseHeld(e.Name()); ok {
			if digest != Digest("gone@x.com") {
				t.Errorf("%s records digest %s", e.Name(), digest)
			}
			ids = append(ids, id)
		}
	}
	return ids
}

// The account TestResolveHeldIn's trees are held for.
const heldEmail, heldID = "gone@x.com", "acct-1"

// holdTree writes dir/<digest of heldEmail>/<names...> and holds it for
// heldID, as a deletion with an unconfirmed outcome does.
func holdTree(t *testing.T, dir string, names ...string) {
	t.Helper()
	email, id := heldEmail, heldID
	path := filepath.Join(dir, Digest(email))
	for _, n := range names {
		f := filepath.Join(path, n, "f.txt")
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("held"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tr, moved, err := SetAside(path, Digest(email))
	if err != nil || !moved {
		t.Fatalf("SetAside = %v, %v", moved, err)
	}
	if err := holdAside(tr, Digest(email), id); err != nil {
		t.Fatal(err)
	}
}

// The maintenance pass resolves a held tree by the account id it records:
// back to the address when that account is still there (merged into what it
// wrote since, never over it), removed when the account is gone or another
// one holds the address, left held when the account cannot be read or now
// has another address. It handles at most its budget.
func TestResolveHeldIn(t *testing.T) {
	t.Parallel()
	email, id := heldEmail, heldID
	type withAccountFunc = func(string, func(string) error) (bool, error)
	lookupAs := func(found bool, addr string, err error) withAccountFunc {
		return func(got string, fn func(string) error) (bool, error) {
			if got != id {
				t.Errorf("looked up %q, want %q", got, id)
			}
			if err != nil || !found {
				return found, err
			}
			return true, fn(addr)
		}
	}
	resolve := func(t *testing.T, dir string, withAccount withAccountFunc) (HeldStats, error) {
		t.Helper()
		budget := 10
		return ResolveHeldIn(dir, withAccount, &budget)
	}
	t.Run("the account is still there", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		holdTree(t, dir, "tok")
		st, err := resolve(t, dir, lookupAs(true, email, nil))
		if err != nil || st != (HeldStats{Restored: 1}) {
			t.Fatalf("stats %+v, err %v; want one restored", st, err)
		}
		if _, err := os.Stat(filepath.Join(dir, Digest(email), "tok", "f.txt")); err != nil {
			t.Fatalf("the tree is not back: %v", err)
		}
		if got := heldIn(t, dir); len(got) != 0 {
			t.Fatalf("still held: %v", got)
		}
	})
	t.Run("the account wrote files since", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		holdTree(t, dir, "tok-old", "attachments")
		newer := filepath.Join(dir, Digest(email), "attachments", "f.txt")
		if err := os.MkdirAll(filepath.Dir(newer), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(newer, []byte("newer"), 0o600); err != nil {
			t.Fatal(err)
		}
		st, err := resolve(t, dir, lookupAs(true, email, nil))
		if !errors.Is(err, ErrHeldEntriesLeft) || st != (HeldStats{Left: 1}) {
			t.Fatalf("stats %+v, err %v; want one left with ErrHeldEntriesLeft", st, err)
		}
		if _, err := os.Stat(filepath.Join(dir, Digest(email), "tok-old", "f.txt")); err != nil {
			t.Errorf("the entry the account did not have was not moved back: %v", err)
		}
		if got, _ := os.ReadFile(newer); string(got) != "newer" {
			t.Errorf("the newer file reads %q, want it untouched", got)
		}
		if got := heldIn(t, dir); len(got) != 1 {
			t.Errorf("held: %v, want the entry the account has again left held", got)
		}
	})
	// The account gone, or another account holding the address since: either
	// way no account has the recorded id any more.
	t.Run("the account is gone or replaced", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		holdTree(t, dir, "tok")
		st, err := resolve(t, dir, lookupAs(false, "", nil))
		if err != nil || st != (HeldStats{Removed: 1}) {
			t.Fatalf("stats %+v, err %v; want one removed", st, err)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 0 {
			t.Fatalf("left in the directory: %v", entries)
		}
	})
	t.Run("the account cannot be read", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		holdTree(t, dir, "tok")
		down := errors.New("connection refused")
		st, err := resolve(t, dir, lookupAs(false, "", down))
		if !errors.Is(err, down) || st != (HeldStats{Left: 1}) {
			t.Fatalf("stats %+v, err %v; want one left with the read's error", st, err)
		}
		if got := heldIn(t, dir); len(got) != 1 {
			t.Fatalf("held: %v, want the tree still held", got)
		}
	})
	t.Run("a deletion of the account is running", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		holdTree(t, dir, "tok")
		st, err := resolve(t, dir, lookupAs(false, "", ErrAccountBusy))
		if err != nil || st != (HeldStats{Left: 1}) {
			t.Fatalf("stats %+v, err %v; want one left, no error", st, err)
		}
		if got := heldIn(t, dir); len(got) != 1 {
			t.Fatalf("held: %v, want the tree still held", got)
		}
	})
	t.Run("the account has another address", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		holdTree(t, dir, "tok")
		st, err := resolve(t, dir, lookupAs(true, "renamed@x.com", nil))
		if err != nil || st != (HeldStats{Left: 1}) {
			t.Fatalf("stats %+v, err %v; want one left", st, err)
		}
		if got := heldIn(t, dir); len(got) != 1 {
			t.Fatalf("held: %v, want the tree still held", got)
		}
	})
	t.Run("the budget", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		holdTree(t, dir, "tok-1")
		holdTree(t, dir, "tok-2")
		budget := 1
		st, err := ResolveHeldIn(dir, lookupAs(false, "", nil), &budget)
		if err != nil || st != (HeldStats{Removed: 1}) || budget != 0 {
			t.Fatalf("stats %+v, err %v, budget %d; want one removed and the budget spent", st, err, budget)
		}
		if got := heldIn(t, dir); len(got) != 1 {
			t.Fatalf("held: %v, want one left for the next pass", got)
		}
	})
}

// assertNothingAside fails when a tree is left set aside under att.
func assertNothingAside(t *testing.T, att string) {
	t.Helper()
	for _, dir := range []string{filepath.Join(att, "uploads"), filepath.Join(PrivateWorkspaceRoot(att), "conv-1", "participants"), filepath.Join(PrivateWorkspaceRoot(att), "conv-2", "participants")} {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if IsAside(e.Name()) {
				t.Errorf("%s was left aside in %s", e.Name(), dir)
			}
		}
	}
}

// A tree written under the address after the account's trees were moved
// aside — by something racing the deletion — is found after the commit and
// removed while no account holds the address. Not parallel: it uses
// afterDatabaseDeletion.
func TestDeleteRemovesATreeWrittenAfterTheMove(t *testing.T) {
	att := t.TempDir()
	seedFiles(t, att, "gone@x.com")
	late := []string{
		filepath.Join(UploadsDir(att, "gone@x.com"), "late-tok", "late.txt"),
		filepath.Join(PrivateWorkspaceRoot(att), "conv-1", "participants", Digest("gone@x.com"), "late.txt"),
	}
	afterDatabaseDeletion = func() {
		for _, f := range late {
			if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
				t.Error(err)
			}
			if err := os.WriteFile(f, []byte("late"), 0o600); err != nil {
				t.Error(err)
			}
		}
	}
	defer func() { afterDatabaseDeletion = nil }()
	res, err := Delete(context.Background(), &fakeStore{}, "gone@x.com", Options{Keys: &fakeKeys{}, AttachmentDir: att})
	if err != nil || len(res.Problems) != 0 {
		t.Fatalf("Delete = %+v, %v", res, err)
	}
	for _, f := range late {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("%s, written after the move, survived (err %v)", f, err)
		}
	}
}

// A deletion binds the address's keys that name no account to the account
// before it deletes anything; if the key store cannot be written (another
// process holds it), the deletion is refused with nothing deleted, rather
// than leave those keys for a later account under the address.
func TestDeleteRefusesWhenTheKeysCannotBeBound(t *testing.T) {
	t.Parallel()
	att := t.TempDir()
	files := seedFiles(t, att, "gone@x.com")
	st, keys := &fakeStore{}, &fakeKeys{bindErr: apikeys.ErrStoreLocked}
	_, err := Delete(context.Background(), st, "gone@x.com", Options{Keys: keys, AttachmentDir: att})
	var notBound *KeysNotBoundError
	if !errors.As(err, &notBound) || !errors.Is(err, apikeys.ErrStoreLocked) {
		t.Fatalf("err = %v, want a KeysNotBoundError wrapping the busy key store", err)
	}
	if len(st.deleted) != 0 || len(keys.calls) != 0 {
		t.Fatalf("store deletions %v, key deletions %v; want none", st.deleted, keys.calls)
	}
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("%s was removed although the deletion was refused: %v", f, err)
		}
	}
}

// TestEveryAccountDeletionGoesThroughDelete: outside the store (which defines
// DeleteUser and DeleteAccount and calls them in its own tests) and this
// package, no code reaches either directly — neither a call nor a method value
// (f := st.DeleteUser)
// — and no code outside the store deletes from the identity table in SQL. A
// new deletion path that did would delete the rows and leave the account's
// API keys and files behind.
func TestEveryAccountDeletionGoesThroughDelete(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		filepath.Join(root, "internal", "store"):         true,
		filepath.Join(root, "internal", "accountdelete"): true,
	}
	rawDelete := regexp.MustCompile(`(?is)delete\s+from\s+(core\.)?users\b`)
	var offenders []string
	fset := token.NewFileSet()
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if allowed[path] || d.Name() == "testdata" || d.Name() == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			ast.Inspect(f, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.SelectorExpr:
					if n.Sel.Name == "DeleteUser" || n.Sel.Name == "DeleteAccount" {
						offenders = append(offenders, fmt.Sprintf("%s:%d: %s", rel, fset.Position(n.Pos()).Line, n.Sel.Name))
					}
				case *ast.BasicLit:
					if n.Kind == token.STRING && rawDelete.MatchString(n.Value) {
						offenders = append(offenders, fmt.Sprintf("%s:%d: a DELETE from the identity table", rel, fset.Position(n.Pos()).Line))
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("account deletion outside accountdelete.Delete (use it: it also deletes the account's API keys and files):\n%s",
			strings.Join(offenders, "\n"))
	}
}

// An attachment directory that does not exist is reported, not silently
// "cleaned": removing a missing path succeeds, so a caller pointed at the
// wrong directory would otherwise report nothing.
func TestDeleteReportsAMissingAttachmentDir(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "not-here")
	res, err := Delete(context.Background(), &fakeStore{}, "gone@x.com", Options{Keys: &fakeKeys{}, AttachmentDir: missing})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Problems) != 1 || res.Problems[0].Kind != AttachmentDirUnreadable || !strings.Contains(res.Problems[0].String(), missing) {
		t.Fatalf("problems = %+v, want one naming %s", res.Problems, missing)
	}
}

// SweepOrphanUploads removes the upload trees of addresses with no account
// and nothing else: live accounts' trees and entries that are not a digest
// (an upload token from before uploads were kept per account) stay. The
// directory is read before the accounts are listed.
func TestSweepOrphanUploads(t *testing.T) {
	t.Parallel()
	att := t.TempDir()
	for _, e := range []string{"live@x.com", "gone@x.com"} {
		if err := os.MkdirAll(filepath.Join(UploadsDir(att, e), "tok"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	legacy := filepath.Join(att, "uploads", "abcdefghijklmnopqrstuvwxyz")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	aside := filepath.Join(att, "uploads", asidePrefix+"0123-left-by-an-interrupted-removal")
	if err := os.MkdirAll(filepath.Join(aside, "tok"), 0o700); err != nil {
		t.Fatal(err)
	}
	settledLongAgo(t, aside) // a tree set aside just now may still go back
	var listedAfterRead bool
	n, err := SweepOrphanUploads(att, func() ([]string, error) {
		_, statErr := os.Stat(UploadsDir(att, "gone@x.com"))
		listedAfterRead = statErr == nil
		return []string{"live@x.com"}, nil
	}, func(string) (bool, error) { return false, nil })
	if err != nil || n != 1 {
		t.Fatalf("SweepOrphanUploads = %d, %v; want 1", n, err)
	}
	if !listedAfterRead {
		t.Error("accounts were listed before the directory was read")
	}
	if _, err := os.Stat(UploadsDir(att, "gone@x.com")); !os.IsNotExist(err) {
		t.Errorf("the deleted address's uploads survived (err %v)", err)
	}
	if _, err := os.Stat(aside); !os.IsNotExist(err) {
		t.Errorf("a tree an interrupted removal set aside survived (err %v)", err)
	}
	for _, keep := range []string{UploadsDir(att, "live@x.com"), legacy} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("%s was removed: %v", keep, err)
		}
	}
	if n, err := SweepOrphanUploads(filepath.Join(att, "none"), func() ([]string, error) { return nil, nil }, func(string) (bool, error) { return false, nil }); err != nil || n != 0 {
		t.Errorf("a missing directory: %d, %v", n, err)
	}
}

// A tree an interrupted removal set aside is removed on the next pass even
// when there is no orphan to remove — here no digest tree at all.
func TestSweepOrphanUploadsRemovesSetAsideTreesOnEveryPass(t *testing.T) {
	t.Parallel()
	att := t.TempDir()
	aside := filepath.Join(att, "uploads", asidePrefix+Digest("gone@x.com")+"-0123")
	if err := os.MkdirAll(filepath.Join(aside, "tok"), 0o700); err != nil {
		t.Fatal(err)
	}
	settledLongAgo(t, aside) // a tree set aside just now may still go back
	listed := false
	n, err := SweepOrphanUploads(att, func() ([]string, error) { listed = true; return nil, nil }, func(string) (bool, error) { return false, nil })
	if err != nil || n != 0 {
		t.Fatalf("SweepOrphanUploads = %d, %v; want 0", n, err)
	}
	if listed {
		t.Error("the accounts were listed with no digest tree to judge")
	}
	if _, err := os.Stat(aside); !os.IsNotExist(err) {
		t.Fatalf("the set-aside tree survived a pass with no orphan (err %v)", err)
	}
}

// An account created under an address after the sweep listed the accounts,
// but before the address's old tree is removed, keeps the tree and the file
// it wrote there: the sweep asks once more right before each removal.
func TestSweepOrphanUploadsKeepsAnAccountCreatedMeanwhile(t *testing.T) {
	t.Parallel()
	att := t.TempDir()
	old := filepath.Join(UploadsDir(att, "back@x.com"), "old-tok")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(UploadsDir(att, "back@x.com"), "fresh-tok")
	created := false
	var asked []string
	n, err := SweepOrphanUploads(att, func() ([]string, error) {
		// The listing the sweep judges by has no account for the address;
		// the account is created, and uploads, right after.
		if err := os.MkdirAll(fresh, 0o700); err != nil {
			t.Fatal(err)
		}
		created = true
		return nil, nil
	}, func(digest string) (bool, error) {
		asked = append(asked, digest)
		return created && digest == Digest("back@x.com"), nil
	})
	if err != nil || n != 0 {
		t.Fatalf("SweepOrphanUploads = %d, %v; want 0", n, err)
	}
	if len(asked) != 1 || asked[0] != Digest("back@x.com") {
		t.Fatalf("asked about %v; want one look at the tree's digest before the removal", asked)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("the upload of the account created meanwhile was removed: %v", err)
	}
}

// RemoveOrphanTree removes an orphan tree and the copy it set aside to do so,
// and leaves a live address's tree, and a tree already gone, alone.
func TestRemoveOrphanTree(t *testing.T) {
	t.Parallel()
	att := t.TempDir()
	dir := UploadsDir(att, "gone@x.com")
	if err := os.MkdirAll(filepath.Join(dir, "old"), 0o700); err != nil {
		t.Fatal(err)
	}
	d := Digest("gone@x.com")
	gone, err := RemoveOrphanTree(dir, d, func(string) (bool, error) { return false, nil })
	if err != nil || !gone {
		t.Fatalf("RemoveOrphanTree = %v, %v; want removed", gone, err)
	}
	entries, err := os.ReadDir(filepath.Join(att, "uploads"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("left behind %v", entries)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if gone, err := RemoveOrphanTree(dir, d, func(string) (bool, error) { return true, nil }); gone || err != nil {
		t.Fatalf("a live address: %v, %v", gone, err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("a live address's tree was removed: %v", err)
	}
	if gone, err := RemoveOrphanTree(filepath.Join(att, "none"), d, func(string) (bool, error) { return false, nil }); gone || err != nil {
		t.Fatalf("a missing tree: %v, %v", gone, err)
	}
}

// After the database deletion commits, the address may hold a new account
// before the keys and files are reached. That account keeps what is its own —
// the user keys bound to its id, and what it writes under its digest — and
// gets nothing of the deleted one: a key minted before keys named their
// account is bound to the deleted account before the deletion and goes with
// its keys, and the deleted account's trees were moved aside before the
// commit, so the new account's trees start empty. Not parallel: it uses
// afterDatabaseDeletion.
func TestDeleteSparesAnAccountCreatedAfterTheCommit(t *testing.T) {
	ctx := context.Background()
	for _, recreated := range []bool{true, false} {
		att := t.TempDir()
		files := seedFiles(t, att, "back@x.com")
		dir := t.TempDir()
		mgr, err := apikeys.NewManager(filepath.Join(dir, "keys.json"), filepath.Join(dir, "audit.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		st := &fakeStore{live: map[string]bool{}}
		old, _, err := mgr.CreateBoundUserKey(ctx, "old", "back@x.com", "deleted-account-id", 0, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		legacy, _, err := mgr.CreateUserKey(ctx, "legacy", "back@x.com", 0, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		var fresh *apikeys.APIKey
		newFiles := []string{
			filepath.Join(UploadsDir(att, "back@x.com"), "new-tok", "new.txt"),
			filepath.Join(PrivateWorkspaceRoot(att), "conv-1", "participants", Digest("back@x.com"), "new.txt"),
		}
		afterDatabaseDeletion = func() {
			// The deleted account's trees are aside already.
			for _, f := range files {
				if _, err := os.Stat(f); !os.IsNotExist(err) {
					t.Errorf("recreated=%v: at the commit %s is still under the address (err %v)", recreated, f, err)
				}
			}
			// Bound to the account being deleted before its rows went.
			if k := mgr.GetKey(legacy.KeyID); k == nil || k.AccountID != "deleted-account-id" {
				t.Errorf("recreated=%v: at the commit the unbound key is %+v, want bound to the deleted account", recreated, k)
			}
			if !recreated {
				return
			}
			st.live[Digest("back@x.com")] = true
			if fresh, _, err = mgr.CreateBoundUserKey(ctx, "new", "back@x.com", "new-account-id", 0, nil, ""); err != nil {
				t.Error(err)
			}
			for _, f := range newFiles {
				if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
					t.Error(err)
				}
				if err := os.WriteFile(f, []byte("new"), 0o600); err != nil {
					t.Error(err)
				}
			}
		}
		res, err := Delete(ctx, st, "back@x.com", Options{Keys: mgr, AttachmentDir: att})
		afterDatabaseDeletion = nil
		if err != nil {
			t.Fatalf("recreated=%v: Delete: %v", recreated, err)
		}
		// The new account's own trees, found under the address after the
		// commit, are left in place and named in a warning.
		for _, p := range res.Problems {
			if !recreated || p.Kind != FilesHeldByNewAccount {
				t.Errorf("recreated=%v: problem %+v", recreated, p)
			}
		}
		if mgr.GetKey(old.KeyID) != nil {
			t.Errorf("recreated=%v: the deleted account's key survived", recreated)
		}
		if mgr.GetKey(legacy.KeyID) != nil {
			t.Errorf("recreated=%v: the key minted before keys named their account survived", recreated)
		}
		if recreated && (fresh == nil || mgr.GetKey(fresh.KeyID) == nil) {
			t.Error("the new account's key was deleted")
		}
		for _, f := range files {
			if _, err := os.Stat(f); !os.IsNotExist(err) {
				t.Errorf("recreated=%v: the deleted account's %s survived (err %v)", recreated, f, err)
			}
		}
		if recreated {
			for _, f := range newFiles {
				if _, err := os.Stat(f); err != nil {
					t.Errorf("the new account's %s was removed: %v", f, err)
				}
			}
		}
	}
}

// settledLongAgo backdates path's modification time well past reclaim.Grace:
// a tree set aside by a deletion that was interrupted long ago.
func settledLongAgo(t *testing.T, path string) {
	t.Helper()
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

// TestRemoveAsideLeavesADeletionInFlightAlone: a deletion moves the account's
// trees aside before its database deletion commits, and moves them back when
// it does not (a refused deletion, ErrAccountChanged). A maintenance pass
// running in between leaves them: only a tree set aside longer ago than
// reclaim.Grace — one an interrupted deletion left — is removed.
func TestRemoveAsideLeavesADeletionInFlightAlone(t *testing.T) {
	t.Parallel()
	att := t.TempDir()
	const email = "kept@x.com"
	tree := UploadsDir(att, email)
	file := filepath.Join(tree, "tok", "a.png")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	settledLongAgo(t, tree) // uploaded long ago

	aside, moved, err := SetAside(tree, Digest(email))
	if err != nil || !moved {
		t.Fatalf("SetAside = %v, %v", moved, err)
	}
	if err := RemoveSetAsideUploads(att); err != nil {
		t.Fatal(err)
	}
	if err := restoreAside([]AsideTree{aside}); err != nil {
		t.Fatalf("the tree a running deletion set aside was removed under it: %v", err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("the account's upload is gone after its deletion did not commit: %v", err)
	}

	left, moved, err := SetAside(tree, Digest(email))
	if err != nil || !moved {
		t.Fatalf("SetAside = %v, %v", moved, err)
	}
	settledLongAgo(t, left.To)
	if err := RemoveSetAsideUploads(att); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(left.To); !os.IsNotExist(err) {
		t.Errorf("a tree an interrupted deletion set aside long ago survived (err %v)", err)
	}
}

// A tree whose time this process may not set is still moved aside, not a
// refused deletion: the sweep then counts its hour from the tree's last
// change. A symbolic link or a file at the path is moved without being
// stamped (and a link is not followed to stamp what it names).
func TestSetAsideStampIsBestEffort(t *testing.T) {
	// Not parallel: it replaces asideChtimes.
	att := t.TempDir()
	old := time.Now().Add(-48 * time.Hour).Truncate(time.Second)

	tree := UploadsDir(att, "noperm@x.com")
	if err := os.MkdirAll(filepath.Join(tree, "tok"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tree, old, old); err != nil {
		t.Fatal(err)
	}
	stamp := asideChtimes
	t.Cleanup(func() { asideChtimes = stamp })
	asideChtimes = func(*os.Root, string, time.Time) error {
		return &fs.PathError{Op: "chtimes", Path: "x", Err: fs.ErrPermission}
	}
	aside, moved, err := SetAside(tree, Digest("noperm@x.com"))
	asideChtimes = stamp
	if err != nil || !moved {
		t.Fatalf("SetAside with a time it may not set = %v, %v; want moved", moved, err)
	}
	if !errors.Is(aside.StampErr, fs.ErrPermission) {
		t.Errorf("StampErr = %v, want the permission error (Delete logs it)", aside.StampErr)
	}
	if info, err := os.Lstat(aside.To); err != nil || !info.ModTime().Equal(old) {
		t.Fatalf("aside tree mtime = %v, %v; want the old %v", info.ModTime(), err, old)
	}

	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(target, old, old); err != nil {
		t.Fatal(err)
	}
	link := UploadsDir(att, "link@x.com")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	file := UploadsDir(att, "file@x.com")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ path, email string }{{link, "link@x.com"}, {file, "file@x.com"}} {
		a, moved, err := SetAside(c.path, Digest(c.email))
		if err != nil || !moved {
			t.Fatalf("SetAside(%s) = %v, %v", c.path, moved, err)
		}
		info, err := os.Lstat(a.To)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().IsRegular() && !info.ModTime().Equal(old) {
			t.Errorf("a file at the path was stamped: mtime %v", info.ModTime())
		}
	}
	if info, err := os.Stat(target); err != nil || !info.ModTime().Equal(old) {
		t.Errorf("the directory a link at the path names was stamped: mtime %v, %v", info.ModTime(), err)
	}
}
