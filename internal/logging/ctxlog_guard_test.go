package logging

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoPackageLevelSlogWhereAContextIsInScope is the lint guard for the
// context logger (#2052): inside a function that takes a context.Context (or
// an *http.Request), a log line must go through logging.From(ctx) so it
// carries the request's, turn's or run's correlation ids — a package-level
// slog.Info/Warn/Error/Debug/Log there drops them (a retry of a task could not
// be told apart by run_id). Boot, shutdown and other context-free code keeps
// using slog directly; this only fires where a context is right there.
func TestNoPackageLevelSlogWhereAContextIsInScope(t *testing.T) {
	root := filepath.Join("..", "..")
	var bad []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case "logging", "safe", "testdata", "node_modules":
					// logging builds the context logger; safe keeps its own
					// stderr handler by design.
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			bad = append(bad, slogWithContextInScope(t, path)...)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(bad) > 0 {
		t.Fatalf("package-level slog calls where a context is in scope — use logging.From(ctx):\n  %s", strings.Join(bad, "\n  "))
	}
}

func slogWithContextInScope(t *testing.T, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	var visit func(n ast.Node, hasCtx bool)
	visit = func(n ast.Node, hasCtx bool) {
		ast.Inspect(n, func(m ast.Node) bool {
			switch x := m.(type) {
			case *ast.FuncLit:
				if x != n {
					visit(x.Body, hasCtx || takesContext(x.Type))
					return false
				}
			case *ast.CallExpr:
				if hasCtx && isPackageSlogCall(x) {
					out = append(out, fset.Position(x.Pos()).String())
				}
			}
			return true
		})
	}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
			visit(fd.Body, takesContext(fd.Type))
		}
	}
	return out
}

func takesContext(ft *ast.FuncType) bool {
	if ft.Params == nil {
		return false
	}
	for _, p := range ft.Params.List {
		named := false
		for _, n := range p.Names {
			if n.Name != "_" {
				named = true
			}
		}
		if !named {
			continue
		}
		switch typ := p.Type.(type) {
		case *ast.SelectorExpr:
			if id, ok := typ.X.(*ast.Ident); ok && id.Name == "context" && typ.Sel.Name == "Context" {
				return true
			}
		case *ast.StarExpr:
			if se, ok := typ.X.(*ast.SelectorExpr); ok {
				if id, ok := se.X.(*ast.Ident); ok && id.Name == "http" && se.Sel.Name == "Request" {
					return true
				}
			}
		}
	}
	return false
}

func isPackageSlogCall(c *ast.CallExpr) bool {
	se, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := se.X.(*ast.Ident)
	if !ok || id.Name != "slog" {
		return false
	}
	switch se.Sel.Name {
	case "Info", "Warn", "Error", "Debug", "Log", "InfoContext", "WarnContext", "ErrorContext", "DebugContext", "Default":
		return true
	}
	return false
}
