package logging

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// contextlessHelperAllowed lists the reviewed exceptions to
// TestNoContextlessLoggingHelperCalledOnlyWithAContext, as
// "<dir relative to repo root>.<function name>" with the reason.
var contextlessHelperAllowed = map[string]string{}

// TestNoContextlessLoggingHelperCalledOnlyWithAContext closes the hole
// TestNoPackageLevelSlogWhereAContextIsInScope cannot see (#2074 review: an
// a2aCreateError, a parsePatrolDraft): a helper that takes no context and
// logs through package-level slog, while every one of its callers in the
// package has a context.Context or *http.Request in scope. Its lines lose
// the request's or run's correlation ids for no reason — the caller has them
// and should pass them in. A helper with at least one context-free caller,
// or none in its package, is left alone.
//
// Calls are matched by name within the package (a function by its
// identifier, a method by its selector name), so two methods sharing a name
// on different types count as one; contextlessHelperAllowed holds any
// resulting false positive, each with a reason.
func TestNoContextlessLoggingHelperCalledOnlyWithAContext(t *testing.T) {
	root := filepath.Join("..", "..")
	pkgFiles := map[string][]string{}
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case "logging", "safe", "testdata", "node_modules":
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			pkgFiles[filepath.Dir(path)] = append(pkgFiles[filepath.Dir(path)], path)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	bad := make([]string, 0, len(pkgFiles))
	for dir, files := range pkgFiles {
		rel, _ := filepath.Rel(root, dir)
		bad = append(bad, contextlessLoggingHelpers(t, filepath.ToSlash(rel), files)...)
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("helpers that log through package-level slog but are only ever called with a context in scope — take ctx (or r) and log via logging.From(ctx):\n  %s", strings.Join(bad, "\n  "))
	}
}

func contextlessLoggingHelpers(t *testing.T, rel string, paths []string) []string {
	t.Helper()
	fset := token.NewFileSet()
	files := make([]*ast.File, 0, len(paths))
	for _, path := range paths {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		files = append(files, f)
	}
	ctxHolders := contextHoldingTypes(files)

	// Candidates: no context parameter, no context-holding receiver, and a
	// package-level slog call in the body proper (a nested literal that takes
	// a context is TestNoPackageLevelSlogWhereAContextIsInScope's business).
	cands := map[string]*ast.FuncDecl{}
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil || takesContext(fd.Type) || ctxHolders[receiverType(fd)] {
				continue
			}
			logs := false
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if fl, ok := n.(*ast.FuncLit); ok && takesContext(fl.Type) {
					return false
				}
				if c, ok := n.(*ast.CallExpr); ok && isPackageSlogCall(c) {
					logs = true
				}
				return true
			})
			if logs {
				cands[fd.Name.Name] = fd
			}
		}
	}
	if len(cands) == 0 {
		return nil
	}

	type tally struct{ withCtx, without int }
	calls := map[string]*tally{}
	var visit func(n ast.Node, hasCtx bool)
	visit = func(n ast.Node, hasCtx bool) {
		ast.Inspect(n, func(m ast.Node) bool {
			if fl, ok := m.(*ast.FuncLit); ok && fl.Body != n {
				visit(fl.Body, hasCtx || takesContext(fl.Type))
				return false
			}
			c, ok := m.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch fn := c.Fun.(type) {
			case *ast.Ident:
				name = fn.Name
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			}
			if cands[name] == nil {
				return true
			}
			tl := calls[name]
			if tl == nil {
				tl = &tally{}
				calls[name] = tl
			}
			if hasCtx {
				tl.withCtx++
			} else {
				tl.without++
			}
			return true
		})
	}
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				visit(fd.Body, takesContext(fd.Type) || ctxHolders[receiverType(fd)])
			}
		}
	}

	var out []string
	for name, fd := range cands {
		tl := calls[name]
		if tl == nil || tl.without > 0 || tl.withCtx == 0 {
			continue
		}
		if _, ok := contextlessHelperAllowed[rel+"."+name]; ok {
			continue
		}
		p := fset.Position(fd.Pos())
		out = append(out, rel+"/"+filepath.Base(p.Filename)+":"+strconv.Itoa(p.Line)+" "+name+" ("+strconv.Itoa(tl.withCtx)+" callers, all with a context)")
	}
	return out
}
