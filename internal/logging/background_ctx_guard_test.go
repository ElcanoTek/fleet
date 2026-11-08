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

// backgroundCtxAllowed lists the functions ("<file>.<Func>") that may build
// a context from context.Background()/TODO() although a context (or an
// *http.Request) is in scope, each with its reason.
var backgroundCtxAllowed = map[string]string{
	"internal/runner/runner.go.Run":                       "the pool's root task context: the parent of every task run, outliving the Run caller's context by design (drain grace)",
	"internal/httpapi/chat.go.startTurn":                  "the fallback for a nil request, and the turn's root context: detached from the request's lifetime on purpose, with its correlation re-attached explicitly (logging.WithTurn + the request's Attrs)",
	"internal/httpapi/webhook.go.postWebhook":             "the webhook turn's root context: detached from the POST on purpose, with its correlation re-attached explicitly (logging.WithTurn + the request's Attrs)",
	"internal/httpapi/server.go.requestContext":           "the fallback for a nil request",
	"internal/sched/handlers/handlers.go.requestContext":  "the fallback for a nil request",
	"internal/sandbox/bridge_stderr.go.sessionContext":    "builds a fresh root holding only the session-level ids it copies from ctx: a finished request's ids must not outlive it",
	"internal/sandbox/persistent.go.newPersistentSandbox": "the fallback for a nil ctx",
	"internal/sandbox/pool.go.coldStart":                  "the fallback for a nil ctx",
	"internal/logging/context.go.WithAttrs":               "the fallback for a nil ctx",
	"internal/safe/safe.go.RecoverContext":                "the fallback for a nil ctx",
}

// TestNoBackgroundContextWhereOneIsInScope closes the "detached work that
// dropped the run's correlation" class (#2074 review, many findings): inside
// a function that takes a context.Context (or an *http.Request), work that
// must outlive that context's cancellation detaches with
// context.WithoutCancel(ctx) — which keeps conv_id, turn_id, run_id,
// request_id and the trace — never context.Background()/TODO(), which drops
// them. Function literals that take a context count too. Test files and the
// test database helpers are out of scope; the reviewed exceptions are in
// backgroundCtxAllowed.
func TestNoBackgroundContextWhereOneIsInScope(t *testing.T) {
	root := filepath.Join("..", "..")
	seen := map[string]bool{}
	var bad []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case "testdata", "node_modules", "testdb":
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			for _, site := range backgroundInContextScope(t, path) {
				key := rel + "." + site.fn
				if _, ok := backgroundCtxAllowed[key]; ok {
					seen[key] = true
					continue
				}
				bad = append(bad, rel+":"+strconv.Itoa(site.line)+" "+site.fn)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for key := range backgroundCtxAllowed {
		if !seen[key] {
			bad = append(bad, "stale backgroundCtxAllowed entry: "+key)
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("context.Background()/TODO() where a context is in scope — detach with context.WithoutCancel(ctx) so the work keeps the run's/request's correlation, or add a reasoned backgroundCtxAllowed entry:\n  %s", strings.Join(bad, "\n  "))
	}
}

type backgroundSite struct {
	fn   string
	line int
}

// takesContext is true for a function type with a context.Context or an
// *http.Request parameter.
func takesContextOrRequest(ft *ast.FuncType) bool {
	if ft == nil || ft.Params == nil {
		return false
	}
	for _, f := range ft.Params.List {
		switch t := f.Type.(type) {
		case *ast.SelectorExpr:
			if id, ok := t.X.(*ast.Ident); ok && id.Name == "context" && t.Sel.Name == "Context" {
				return true
			}
		case *ast.StarExpr:
			if se, ok := t.X.(*ast.SelectorExpr); ok {
				if id, ok := se.X.(*ast.Ident); ok && id.Name == "http" && se.Sel.Name == "Request" {
					return true
				}
			}
		}
	}
	return false
}

func backgroundInContextScope(t *testing.T, path string) []backgroundSite {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []backgroundSite
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		name := fd.Name.Name
		var walk func(n ast.Node, inScope bool)
		walk = func(n ast.Node, inScope bool) {
			ast.Inspect(n, func(m ast.Node) bool {
				if fl, ok := m.(*ast.FuncLit); ok && m != n {
					walk(fl.Body, inScope || takesContextOrRequest(fl.Type))
					return false
				}
				c, ok := m.(*ast.CallExpr)
				if !ok || !inScope {
					return true
				}
				if se, ok := c.Fun.(*ast.SelectorExpr); ok {
					if id, ok := se.X.(*ast.Ident); ok && id.Name == "context" && (se.Sel.Name == "Background" || se.Sel.Name == "TODO") {
						out = append(out, backgroundSite{fn: name, line: fset.Position(c.Pos()).Line})
					}
				}
				return true
			})
		}
		walk(fd.Body, takesContextOrRequest(fd.Type))
	}
	return out
}

// contextlessRecoverAllowed lists the functions ("<file>.<Func>") that may
// recover with the contextless safe.Recover although a context is at hand,
// each with its reason.
var contextlessRecoverAllowed = map[string]string{}

// TestNoContextlessRecoverWhereAContextIsAtHand is the panic-boundary half of
// the guard above (#2074 review): a goroutine that serves one turn, run or
// request recovers with safe.RecoverContext(ctx, …), so its "panic recovered"
// record carries that turn's or run's correlation. A function is "at hand"
// when it takes a context.Context or an *http.Request, or logs through
// logging.From — it has a correlated context, so its recoverer must use it.
func TestNoContextlessRecoverWhereAContextIsAtHand(t *testing.T) {
	root := filepath.Join("..", "..")
	seen := map[string]bool{}
	var bad []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case "testdata", "node_modules", "testdb":
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			for _, site := range contextlessRecoverWithContext(t, path) {
				key := rel + "." + site.fn
				if _, ok := contextlessRecoverAllowed[key]; ok {
					seen[key] = true
					continue
				}
				bad = append(bad, rel+":"+strconv.Itoa(site.line)+" "+site.fn)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for key := range contextlessRecoverAllowed {
		if !seen[key] {
			bad = append(bad, "stale contextlessRecoverAllowed entry: "+key)
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("safe.Recover where a correlated context is at hand — use safe.RecoverContext(ctx, …) so the panic record carries the turn's/run's ids, or add a reasoned contextlessRecoverAllowed entry:\n  %s", strings.Join(bad, "\n  "))
	}
}

func isPkgCall(c *ast.CallExpr, pkg, fn string) bool {
	se, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := se.X.(*ast.Ident)
	return ok && id.Name == pkg && se.Sel.Name == fn
}

func contextlessRecoverWithContext(t *testing.T, path string) []backgroundSite {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	// logsCorrelated reports whether body — closures included, since a
	// closure logging through logging.From means the function holds a
	// correlated context — logs via logging.From.
	logsCorrelated := func(body ast.Node) bool {
		found := false
		ast.Inspect(body, func(m ast.Node) bool {
			if c, ok := m.(*ast.CallExpr); ok && isPkgCall(c, "logging", "From") {
				found = true
			}
			return !found
		})
		return found
	}
	var out []backgroundSite
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		name := fd.Name.Name
		var walk func(body *ast.BlockStmt, inScope bool)
		walk = func(body *ast.BlockStmt, inScope bool) {
			atHand := inScope || logsCorrelated(body)
			ast.Inspect(body, func(m ast.Node) bool {
				if fl, ok := m.(*ast.FuncLit); ok {
					walk(fl.Body, inScope || takesContextOrRequest(fl.Type))
					return false
				}
				if c, ok := m.(*ast.CallExpr); ok && atHand && isPkgCall(c, "safe", "Recover") {
					out = append(out, backgroundSite{fn: name, line: fset.Position(c.Pos()).Line})
				}
				return true
			})
		}
		walk(fd.Body, takesContextOrRequest(fd.Type))
	}
	return out
}
