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

// A file removal after the commit that fails is reported (with what the
// maintenance pass does about it), and its trees stay for that pass: the
// lookup each removal asks first failing, or the server's own cleanup failing.
func TestDeleteReportsFailedRemovals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	att := t.TempDir()
	// Paths no tree can be moved from or removed at: "uploads" and a
	// conversation's "participants" are files.
	if err := os.WriteFile(filepath.Join(att, "uploads"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	conv := filepath.Join(PrivateWorkspaceRoot(att), "conv-1")
	if err := os.MkdirAll(conv, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(conv, "participants"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
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
	// The server's own steps failing are reported the same way.
	res, err = Delete(ctx, &fakeStore{}, "gone@x.com", Options{Keys: &fakeKeys{},
		ServerSetAside: func(string) ([]AsideTree, error) { return nil, errors.New("read-only file system") },
		ServerCleanup:  func(string, func(string) (bool, error)) error { return errors.New("read-only file system") }})
	if err != nil || len(res.Problems) != 1 || res.Problems[0].Kind != PrivateWorkspacesFailed {
		t.Fatalf("server step failure: %+v, %v", res, err)
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
		if err != nil || len(res.Problems) != 0 {
			t.Fatalf("recreated=%v: Delete = %+v, %v", recreated, res, err)
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
