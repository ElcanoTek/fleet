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

// zeroArgLoggerAccessorAllowed lists the reviewed logger accessors that take
// no context, as "<dir relative to repo root>.<Receiver>.<method>", each with
// the reason it is not a correlation leak.
var zeroArgLoggerAccessorAllowed = map[string]string{
	"internal/agentcore.engine.log":             "run-scoped: Run sets engine.logger to logging.From(runCtx) before the engine does any work; slog.Default is only the fallback for an engine built outside Run (tests)",
	"internal/agentcore.orchestrationState.log": "run-scoped: Run stores logging.From(runCtx) with setLogger before any tool runs; slog.Default is only the fallback outside a run",
}

// TestLoggerAccessorsTakeAContext guards the accessor shape of the context
// logger leak (#2074 review: webpush's s.log() fell back to slog.Default(),
// so a relay failure lost the task, run and turn ids although ctx was in
// scope). Call sites read s.log().Warn(…), which the package-slog guards
// cannot see, so it checks the accessors themselves: a method that takes no
// parameters and returns *slog.Logger (a log()/logger() accessor over a
// stored or default logger) must take the context instead — test seam when
// set, else logging.From(ctx) — unless zeroArgLoggerAccessorAllowed explains
// why its logger is already scoped.
func TestLoggerAccessorsTakeAContext(t *testing.T) {
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
			rel, _ := filepath.Rel(root, filepath.Dir(path))
			rel = filepath.ToSlash(rel)
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Recv == nil || fd.Type.Params.NumFields() != 0 || !returnsSlogLogger(fd.Type) {
					continue
				}
				key := rel + "." + receiverType(fd) + "." + fd.Name.Name
				if _, ok := zeroArgLoggerAccessorAllowed[key]; ok {
					continue
				}
				bad = append(bad, filepath.ToSlash(path[len(root)+1:])+":"+strconv.Itoa(fset.Position(fd.Pos()).Line)+" "+key)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("logger accessors that take no context — make them log(ctx) returning the test seam or logging.From(ctx), or add a reasoned zeroArgLoggerAccessorAllowed entry:\n  %s", strings.Join(bad, "\n  "))
	}
}

func returnsSlogLogger(ft *ast.FuncType) bool {
	if ft.Results == nil || len(ft.Results.List) != 1 {
		return false
	}
	star, ok := ft.Results.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	se, ok := star.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := se.X.(*ast.Ident)
	return ok && id.Name == "slog" && se.Sel.Name == "Logger"
}
