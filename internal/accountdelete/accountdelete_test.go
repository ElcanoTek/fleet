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
)

type fakeStore struct {
	err     error
	deleted []string
}

func (s *fakeStore) DeleteUser(_ context.Context, email string) error {
	if s.err != nil {
		return s.err
	}
	s.deleted = append(s.deleted, email)
	return nil
}

type fakeKeys struct {
	n     int
	err   error
	calls []string
}

func (k *fakeKeys) DeleteUserKeys(_ context.Context, email string) (int, error) {
	k.calls = append(k.calls, email)
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
		if len(st.deleted) != 1 || len(keys.calls) != 1 {
			t.Fatalf("store calls %v, key calls %v", st.deleted, keys.calls)
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
			ServerCleanup: func(string) { cleaned = true }})
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
			ServerCleanup: func(email string) { got = email }}); err != nil {
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

// TestEveryAccountDeletionGoesThroughDelete: outside the store (which defines
// DeleteUser and calls it in its own tests) and this package, no code reaches
// DeleteUser directly — neither a call nor a method value (f := st.DeleteUser)
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
					if n.Sel.Name == "DeleteUser" {
						offenders = append(offenders, fmt.Sprintf("%s:%d: DeleteUser", rel, fset.Position(n.Pos()).Line))
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
	var listedAfterRead bool
	n, err := SweepOrphanUploads(att, func() ([]string, error) {
		_, statErr := os.Stat(UploadsDir(att, "gone@x.com"))
		listedAfterRead = statErr == nil
		return []string{"live@x.com"}, nil
	})
	if err != nil || n != 1 {
		t.Fatalf("SweepOrphanUploads = %d, %v; want 1", n, err)
	}
	if !listedAfterRead {
		t.Error("accounts were listed before the directory was read")
	}
	if _, err := os.Stat(UploadsDir(att, "gone@x.com")); !os.IsNotExist(err) {
		t.Errorf("the deleted address's uploads survived (err %v)", err)
	}
	for _, keep := range []string{UploadsDir(att, "live@x.com"), legacy} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("%s was removed: %v", keep, err)
		}
	}
	if n, err := SweepOrphanUploads(filepath.Join(att, "none"), func() ([]string, error) { return nil, nil }); err != nil || n != 0 {
		t.Errorf("a missing directory: %d, %v", n, err)
	}
}
