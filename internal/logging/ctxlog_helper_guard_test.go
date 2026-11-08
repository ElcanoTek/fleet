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
var contextlessHelperAllowed = map[string]string{
	"internal/sandbox.Start": "name collision: the in-package \"Start\" calls are exec.Cmd.Start in ensureBridge; EgressProxy.Start itself is called once from internal/agent while the sandbox pool is built at boot, outside any request or run",
}

// TestNoContextlessLoggingHelperCalledOnlyWithAContext closes the hole
// TestNoPackageLevelSlogWhereAContextIsInScope cannot see (#2074 review: an
// a2aCreateError, a parsePatrolDraft, an httpStatusResponse →
// noteStatusBodyMismatch chain): a function that takes no context and logs
// through package-level slog, while every call chain that reaches it from
// inside its package starts where a context.Context or *http.Request is in
// scope. Its lines lose the request's or run's correlation ids for no reason
// — the chain had them and should pass them down.
//
// It is transitive over an in-package call graph. A context-free function is
// "context-reached" when it has at least one call site and every call site
// either has a context in scope or sits in a context-free function that is
// itself context-reached (the least fixpoint; a function's calls to itself
// are ignored). A function that is also referenced as a value (a callback,
// a method value) is never context-reached: it may be called from anywhere.
// A context-reached function that logs through package-level slog fails.
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
	free, logs := contextFreeLoggers(files, ctxHolders)
	if len(logs) == 0 {
		return nil
	}
	g := buildCallSites(files, ctxHolders, free)
	reached := g.contextReached(free)

	var out []string
	for name := range logs {
		if !reached[name] {
			continue
		}
		if _, ok := contextlessHelperAllowed[rel+"."+name]; ok {
			continue
		}
		p := fset.Position(free[name].Pos())
		out = append(out, rel+"/"+filepath.Base(p.Filename)+":"+strconv.Itoa(p.Line)+" "+name)
	}
	return out
}

// contextFreeLoggers returns the package's context-free functions (by name)
// and which of them log through package-level slog in their body proper (a
// nested literal that takes a context is
// TestNoPackageLevelSlogWhereAContextIsInScope's).
func contextFreeLoggers(files []*ast.File, ctxHolders map[string]bool) (map[string]*ast.FuncDecl, map[string]bool) {
	free := map[string]*ast.FuncDecl{}
	logs := map[string]bool{}
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil || takesContext(fd.Type) || ctxHolders[receiverType(fd)] {
				continue
			}
			free[fd.Name.Name] = fd
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if fl, ok := n.(*ast.FuncLit); ok && takesContext(fl.Type) {
					return false
				}
				// slog.Default() alone is not a log line: the run loggers'
				// accessors (engine.log) fall back to it outside a run.
				if c, ok := n.(*ast.CallExpr); ok && isPackageSlogCall(c) && !isSlogDefault(c) {
					logs[fd.Name.Name] = true
				}
				return true
			})
		}
	}
	return free, logs
}

// callSite is one in-package call of a context-free function: whether it has
// a context in scope, and the context-free function it sits in ("" when it
// sits in a function with a context).
type callSite struct {
	hasCtx bool
	in     string
}

type callSites struct {
	sites   map[string][]callSite
	asValue map[string]bool // referenced other than by a call
}

func buildCallSites(files []*ast.File, ctxHolders map[string]bool, free map[string]*ast.FuncDecl) callSites {
	g := callSites{sites: map[string][]callSite{}, asValue: map[string]bool{}}
	for _, f := range files {
		imported := map[string]bool{}
		for _, name := range importNames(f) {
			imported[name] = true
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			hasCtx := takesContext(fd.Type) || ctxHolders[receiverType(fd)]
			in := ""
			if !hasCtx {
				in = fd.Name.Name
			}
			g.visit(fd.Body, hasCtx, in, imported, free)
		}
	}
	return g
}

func (g callSites) visit(n ast.Node, hasCtx bool, in string, imported map[string]bool, free map[string]*ast.FuncDecl) {
	callee := map[ast.Node]bool{}
	ast.Inspect(n, func(m ast.Node) bool {
		if fl, ok := m.(*ast.FuncLit); ok && fl.Body != n {
			g.visit(fl.Body, hasCtx || takesContext(fl.Type), in, imported, free)
			return false
		}
		switch x := m.(type) {
		case *ast.CallExpr:
			name := calledName(x, imported, callee)
			if free[name] != nil && name != in {
				g.sites[name] = append(g.sites[name], callSite{hasCtx: hasCtx, in: in})
			}
		case *ast.Ident:
			if free[x.Name] != nil && !callee[x] && x.Obj == nil {
				g.asValue[x.Name] = true
			}
		}
		return true
	})
}

// calledName is the in-package name c calls ("" for another package's
// function: errors.New is no call of a local New), marking the identifier in
// call position in callee.
func calledName(c *ast.CallExpr, imported map[string]bool, callee map[ast.Node]bool) string {
	switch fn := c.Fun.(type) {
	case *ast.Ident:
		callee[fn] = true
		return fn.Name
	case *ast.SelectorExpr:
		if id, ok := fn.X.(*ast.Ident); ok && id.Obj == nil && imported[id.Name] {
			return ""
		}
		callee[fn.Sel] = true
		return fn.Sel.Name
	}
	return ""
}

// contextReached is the least fixpoint: a context-free function with call
// sites, none of them a value reference, every one with a context in scope or
// inside a context-reached function.
func (g callSites) contextReached(free map[string]*ast.FuncDecl) map[string]bool {
	reached := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for name := range free {
			if reached[name] || g.asValue[name] || len(g.sites[name]) == 0 || !g.allReached(name, reached) {
				continue
			}
			reached[name] = true
			changed = true
		}
	}
	return reached
}

func (g callSites) allReached(name string, reached map[string]bool) bool {
	for _, s := range g.sites[name] {
		if !s.hasCtx && (s.in == "" || !reached[s.in]) {
			return false
		}
	}
	return true
}

func isSlogDefault(c *ast.CallExpr) bool {
	se, ok := c.Fun.(*ast.SelectorExpr)
	return ok && se.Sel.Name == "Default"
}
