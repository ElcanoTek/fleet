package apikeys

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestKeyEdgesNameTheirSurface fails on any non-test code outside this
// package that validates or looks up a key without naming its surface
// (ValidateKey, LookupKeyType, LookupKeyMeta instead of the *On forms), so
// KeyType.AllowsSurface stays the one place that decides which key types an
// edge recognises.
func TestKeyEdgesNameTheirSurface(t *testing.T) {
	banned := map[string]bool{"ValidateKey": true, "LookupKeyType": true, "LookupKeyMeta": true}
	root := filepath.Join("..", "..", "..")
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if n := d.Name(); n == "testdata" || n == "node_modules" || strings.HasPrefix(n, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
				filepath.ToSlash(filepath.Dir(path)) == filepath.ToSlash(filepath.Join(root, "internal", "sched", "apikeys")) {
				return nil
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && banned[sel.Sel.Name] {
					t.Errorf("%s calls %s: use %sOn with the edge's Surface", path, sel.Sel.Name, sel.Sel.Name)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
