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
// be told apart by run_id). A method whose receiver type holds a context
// (a field of type context.Context, like scheduledPolicy.runCtx) counts as
// having one in scope: log through logging.From(recv.thatCtx). Boot, shutdown
// and other context-free code keeps using slog directly; this only fires
// where a context is right there.
func TestNoPackageLevelSlogWhereAContextIsInScope(t *testing.T) {
	root := filepath.Join("..", "..")
	pkgFiles := map[string][]string{} // directory -> its non-test Go files
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
			pkgFiles[filepath.Dir(path)] = append(pkgFiles[filepath.Dir(path)], path)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	bad := make([]string, 0, len(pkgFiles))
	for _, files := range pkgFiles {
		bad = append(bad, slogWithContextInScope(t, files)...)
	}
	if len(bad) > 0 {
		t.Fatalf("package-level slog calls where a context is in scope — use logging.From(ctx):\n  %s", strings.Join(bad, "\n  "))
	}
}

// slogWithContextInScope checks one package's files (they are parsed
// together so a method in one file sees a context-holding struct declared in
// another).
func slogWithContextInScope(t *testing.T, paths []string) []string {
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
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				visit(fd.Body, takesContext(fd.Type) || ctxHolders[receiverType(fd)])
			}
		}
	}
	return out
}

// contextHoldingTypes names the package's struct types with a context.Context
// field (named or embedded).
func contextHoldingTypes(files []*ast.File) map[string]bool {
	out := map[string]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				return true
			}
			for _, field := range st.Fields.List {
				if isContextType(field.Type) {
					out[ts.Name.Name] = true
				}
			}
			return true
		})
	}
	return out
}

// receiverType is a method's receiver base type name ("" for a function).
func receiverType(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return ""
	}
	typ := fd.Recv.List[0].Type
	if se, ok := typ.(*ast.StarExpr); ok {
		typ = se.X
	}
	switch x := typ.(type) {
	case *ast.IndexExpr: // generic receiver T[P]
		typ = x.X
	case *ast.IndexListExpr:
		typ = x.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func isContextType(e ast.Expr) bool {
	se, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := se.X.(*ast.Ident)
	return ok && id.Name == "context" && se.Sel.Name == "Context"
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
			if isContextType(typ) {
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

// shadowingKeys are the correlation keys a call site must never set itself.
// The context logger prepends the context's attributes but lets a call-site
// key win (ctxHandler), so a call site logging "request_id" for some other
// id (an MCP JSON-RPC id) silently replaced the HTTP X-Request-Id the line
// should have been joined on. request_id, trace_id and span_id only ever mean
// the request or span the context carries. task_id, conv_id and turn_id are
// deliberately not here: a handler that names the task or conversation it
// acted on logs the same entity under the same key, which is why the call
// site is allowed to win for them.
var shadowingKeys = map[string]bool{KeyRequestID: true, KeyTraceID: true, KeySpanID: true}

// TestNoCallSiteShadowsACorrelationKey fails on a string literal naming a
// shadowing key passed to a logging call (logger methods, slog attribute
// constructors) anywhere in internal/ or cmd/ outside this package.
func TestNoCallSiteShadowsACorrelationKey(t *testing.T) {
	root := filepath.Join("..", "..")
	var bad []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case "logging", "testdata", "node_modules":
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return perr
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !isLoggingCall(call) {
					return true
				}
				for _, arg := range call.Args {
					if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING && shadowingKeys[strings.Trim(lit.Value, "`\"")] {
						bad = append(bad, fset.Position(lit.Pos()).String()+" "+lit.Value)
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
	if len(bad) > 0 {
		t.Fatalf("log attributes that shadow a context correlation key — rename them (e.g. mcp_request_id):\n  %s", strings.Join(bad, "\n  "))
	}
}

// isLoggingCall matches x.Info/Warn/Error/Debug/Log/LogAttrs/With(…), their
// …Context forms, and slog.String/Any/Int/Int64/Group/Attr.
func isLoggingCall(c *ast.CallExpr) bool {
	se, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	switch se.Sel.Name {
	case "Info", "Warn", "Error", "Debug", "Log", "LogAttrs", "With",
		"InfoContext", "WarnContext", "ErrorContext", "DebugContext":
		return true
	case "String", "Any", "Int", "Int64", "Uint64", "Bool", "Group", "Attr":
		id, ok := se.X.(*ast.Ident)
		return ok && id.Name == "slog"
	}
	return false
}
