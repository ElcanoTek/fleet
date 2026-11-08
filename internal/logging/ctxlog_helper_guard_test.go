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
// It is transitive over the repository's call graph, ACROSS packages: a
// context-free function is "context-reached" when it has at least one call
// site and every call site either has a context in scope or sits in a
// context-free function that is itself context-reached (the least fixpoint;
// a function's calls to itself are ignored). A function that is also
// referenced as a value (a callback, a method value) is never
// context-reached: it may be called from anywhere. A context-reached function
// that logs through package-level slog fails. So does one only ever reached
// through an exported helper of another package whose callers all hold a
// context (scheduledrun → agentcore.OpenStableMCPWorkspace →
// removeTopLevelSpecialFiles, #2074 review).
//
// A package-level function called from another package (pkg.Func) is
// resolved through the file's imports. A method is matched by name within its
// own package only (an identifier, or a selector name), so two methods
// sharing a name on different types count as one — and a method called from
// another package is not seen there, so it never becomes context-reached
// through such a call. contextlessHelperAllowed holds any resulting false
// positive, each with a reason.
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
	bad := contextlessLoggingHelpers(t, root, pkgFiles)
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("helpers that log through package-level slog but are only ever called with a context in scope — take ctx (or r) and log via logging.From(ctx):\n  %s", strings.Join(bad, "\n  "))
	}
}

// modulePrefix is the import path of the repository root.
const modulePrefix = "github.com/ElcanoTek/fleet/"

// guardPkg is one parsed package of the call graph.
type guardPkg struct {
	rel        string // directory relative to the repo root, slash-separated
	files      []*ast.File
	ctxHolders map[string]bool
}

func contextlessLoggingHelpers(t *testing.T, root string, pkgFiles map[string][]string) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs := make([]guardPkg, 0, len(pkgFiles))
	for dir, paths := range pkgFiles {
		rel, _ := filepath.Rel(root, dir)
		p := guardPkg{rel: filepath.ToSlash(rel)}
		for _, path := range paths {
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			p.files = append(p.files, f)
		}
		p.ctxHolders = contextHoldingTypes(p.files)
		pkgs = append(pkgs, p)
	}
	// Every context-free function of the repository, by qualified name
	// ("<rel>.<name>"), and which of them log.
	free := map[string]*ast.FuncDecl{}
	logs := map[string]bool{}
	for _, p := range pkgs {
		pf, pl := contextFreeLoggers(p.files, p.ctxHolders)
		for name, fd := range pf {
			free[p.rel+"."+name] = fd
		}
		for name := range pl {
			logs[p.rel+"."+name] = true
		}
	}
	if len(logs) == 0 {
		return nil
	}
	g := callSites{sites: map[string][]callSite{}, asValue: map[string]bool{}}
	for _, p := range pkgs {
		g.addPackage(p, free)
	}
	reached := g.contextReached(free)

	var out []string
	for name := range logs {
		if !reached[name] {
			continue
		}
		if _, ok := contextlessHelperAllowed[name]; ok {
			continue
		}
		pos := fset.Position(free[name].Pos())
		rel := name[:strings.LastIndex(name, ".")]
		out = append(out, rel+"/"+filepath.Base(pos.Filename)+":"+strconv.Itoa(pos.Line)+" "+name[len(rel)+1:])
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

// callSite is one call of a context-free function: whether it has a context
// in scope, and the qualified context-free function it sits in ("" when it
// sits in a function with a context).
type callSite struct {
	hasCtx bool
	in     string
}

type callSites struct {
	sites   map[string][]callSite
	asValue map[string]bool // referenced other than by a call
}

// fileScope resolves names inside one file: its package and its imports of
// this repository's packages (local name → rel dir).
type fileScope struct {
	rel     string
	fleet   map[string]string // import name → rel dir, this module's packages
	imports map[string]bool   // every import name
}

func (g callSites) addPackage(p guardPkg, free map[string]*ast.FuncDecl) {
	for _, f := range p.files {
		sc := fileScope{rel: p.rel, fleet: map[string]string{}, imports: map[string]bool{}}
		for path, name := range importNames(f) {
			sc.imports[name] = true
			if rel, ok := strings.CutPrefix(path, modulePrefix); ok {
				sc.fleet[name] = rel
			}
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			hasCtx := takesContext(fd.Type) || p.ctxHolders[receiverType(fd)]
			in := ""
			if !hasCtx {
				in = p.rel + "." + fd.Name.Name
			}
			g.visit(fd.Body, hasCtx, in, sc, free)
		}
	}
}

func (g callSites) visit(n ast.Node, hasCtx bool, in string, sc fileScope, free map[string]*ast.FuncDecl) {
	callee := map[ast.Node]bool{}
	ast.Inspect(n, func(m ast.Node) bool {
		if fl, ok := m.(*ast.FuncLit); ok && fl.Body != n {
			g.visit(fl.Body, hasCtx || takesContext(fl.Type), in, sc, free)
			return false
		}
		switch x := m.(type) {
		case *ast.CallExpr:
			name := calledName(x, sc, callee)
			if free[name] != nil && name != in {
				g.sites[name] = append(g.sites[name], callSite{hasCtx: hasCtx, in: in})
			}
		case *ast.SelectorExpr:
			// pkg.Func referenced as a value (not called).
			if id, ok := x.X.(*ast.Ident); ok && id.Obj == nil && !callee[x.Sel] {
				if rel, isFleet := sc.fleet[id.Name]; isFleet && free[rel+"."+x.Sel.Name] != nil {
					g.asValue[rel+"."+x.Sel.Name] = true
				}
			}
		case *ast.Ident:
			if name := sc.rel + "." + x.Name; free[name] != nil && !callee[x] && x.Obj == nil {
				g.asValue[name] = true
			}
		}
		return true
	})
}

// calledName is the qualified name c calls: a local function or method by
// name in this package, or another package's function through its import
// ("" for a package outside this repository: errors.New is no call of a
// local New), marking the identifier in call position in callee.
func calledName(c *ast.CallExpr, sc fileScope, callee map[ast.Node]bool) string {
	switch fn := c.Fun.(type) {
	case *ast.Ident:
		callee[fn] = true
		return sc.rel + "." + fn.Name
	case *ast.SelectorExpr:
		if id, ok := fn.X.(*ast.Ident); ok && id.Obj == nil && sc.imports[id.Name] {
			callee[fn.Sel] = true
			if rel, isFleet := sc.fleet[id.Name]; isFleet {
				return rel + "." + fn.Sel.Name
			}
			return ""
		}
		callee[fn.Sel] = true
		return sc.rel + "." + fn.Sel.Name
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
