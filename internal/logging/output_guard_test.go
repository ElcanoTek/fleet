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

// directOutputAllowed lists the non-test files (paths relative to the repo
// root; a trailing "/" covers a directory) that may write to the process's
// stdout/stderr directly. Everything else logs through slog, so its output
// takes the configured format, level, redactor and sinks. Each entry says why.
var directOutputAllowed = map[string]string{
	"internal/logging/":               "the logging package owns the process streams (the locked stderr sink)",
	"internal/safe/":                  "the panic logger's last-resort fallback when the configured handler fails",
	"internal/admincli/":              "the operator CLI: its output IS the user interface of each subcommand",
	"internal/chattui/":               "`fleet chat`, an interactive terminal UI drawn on stdout",
	"internal/acp/run.go":             "`fleet acp` speaks the Agent Client Protocol over stdin/stdout",
	"internal/taskrun/":               "`fleet task run`: a one-shot CLI reporting its progress to the terminal",
	"internal/testdb/":                "test-database harness for TestMain, outside the server process",
	"internal/mcpbroker/supervise.go": "the broker child's protocol frames ride its own stdin/stdout",
	"cmd/fleet/main.go":               "bootstrap: version and usage output before logging is configured",
	"cmd/fleet/mcp_probe.go":          "`fleet mcp test` CLI report",
	"cmd/fleet/validate_config.go":    "`fleet validate-config` CLI report",
	"cmd/fleet-bench/":                "benchmark harness binary, prints its results",
	"cmd/sandbox-probe/":              "sandbox probe harness binary, prints its results",
}

// TestNoDirectProcessOutput is the guard for "every line goes through the
// logging contract" (#2074 review): outside directOutputAllowed, no
// non-test file in internal/ or cmd/ references os.Stderr or os.Stdout, calls
// fmt.Print/Printf/Println, the print/println builtins, debug.PrintStack, or
// the standard log package's default logger. A raw write skips the configured
// format and level, the file sink and the redactor.
func TestNoDirectProcessOutput(t *testing.T) {
	root := filepath.Join("..", "..")
	var bad []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case "testdata", "node_modules":
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if outputAllowed(rel) {
				return nil
			}
			bad = append(bad, directOutputUses(t, path, rel)...)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("direct writes to the process streams — log through slog/logging.From(ctx), or add a justified directOutputAllowed entry:\n  %s", strings.Join(bad, "\n  "))
	}
}

func outputAllowed(rel string) bool {
	for p := range directOutputAllowed {
		if rel == p || (strings.HasSuffix(p, "/") && strings.HasPrefix(rel, p)) {
			return true
		}
	}
	return false
}

// importNames maps each std package path of interest to the name it is
// imported under in f ("" when not imported).
func importNames(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, imp := range f.Imports {
		p, _ := strconv.Unquote(imp.Path.Value)
		name := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		out[p] = name
	}
	return out
}

func directOutputUses(t *testing.T, path, rel string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	names := importNames(f)
	pos := func(n ast.Node, what string) string {
		p := fset.Position(n.Pos())
		return rel + ":" + strconv.Itoa(p.Line) + " " + what
	}
	isPkg := func(e ast.Expr, path string) bool {
		id, ok := e.(*ast.Ident)
		return ok && names[path] != "" && id.Name == names[path] && id.Obj == nil
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if isPkg(x.X, "os") && (x.Sel.Name == "Stderr" || x.Sel.Name == "Stdout") {
				out = append(out, pos(x, "os."+x.Sel.Name))
			}
		case *ast.CallExpr:
			switch fn := x.Fun.(type) {
			case *ast.Ident:
				if (fn.Name == "print" || fn.Name == "println") && fn.Obj == nil {
					out = append(out, pos(x, fn.Name+"()"))
				}
			case *ast.SelectorExpr:
				switch {
				case isPkg(fn.X, "fmt") && (fn.Sel.Name == "Print" || fn.Sel.Name == "Printf" || fn.Sel.Name == "Println"):
					out = append(out, pos(x, "fmt."+fn.Sel.Name))
				case isPkg(fn.X, "runtime/debug") && fn.Sel.Name == "PrintStack":
					out = append(out, pos(x, "debug.PrintStack"))
				case isPkg(fn.X, "log") && (strings.HasPrefix(fn.Sel.Name, "Print") || strings.HasPrefix(fn.Sel.Name, "Fatal") || strings.HasPrefix(fn.Sel.Name, "Panic")):
					out = append(out, pos(x, "log."+fn.Sel.Name))
				}
			}
		}
		return true
	})
	return out
}

// TestPanicRecoveryGoesThroughSafe is the guard for recovered panics
// (#2074 review): every recover() in internal/ or cmd/ either hands the
// panic to internal/safe (safe.EmitPanic, EmitPanicWithMetadata, Recover,
// RecoverValue …) in the same function — which logs it through the
// configured handler with the value and stack withheld.
// A recover that prints the value itself (fmt.Fprintf(os.Stderr, "panic:
// %v", …)) or swallows it silently fails here. recoverAllowed lists the
// reviewed exceptions.
var recoverAllowed = map[string]string{
	"internal/safe/":                     "the panic emitter itself",
	"internal/logging/redact.go":         "errorText, stringText and resolveOne guard a caller's Error(), String() or LogValue() while formatting an attribute (a placeholder, never the panic value), not a panic boundary",
	"internal/tools/workspace_reader.go": "readMisses undoes its reservations and re-panics the value unchanged (defer panic(r)); nothing is logged or swallowed",
	"internal/telemetry/telemetry.go":    "Finish marks the span panicked (class only) and re-panics the value unchanged; the enclosing safe boundary logs the panic",
	"internal/agentcore/telemetry.go":    "tracedModel marks the model.* span panicked (class only) and re-panics the value unchanged",
	"internal/dbtrace/dbtrace.go":        "BeginTx and finish mark the db.tx span panicked (class only) and re-panic the value unchanged",
	"internal/mcpbroker/client.go":       "callMCP marks the client span panicked (class only) and re-panics the value unchanged",
	"internal/otelsetup/middleware.go":   "Middleware marks the HTTP server span panicked (class only) and re-panics the value unchanged to httpmw.Recoverer",
	"internal/runner/runner.go":          "recordRunPanic marks the task.run span panicked and re-panics the value unchanged to tryClaim's safe recovery",
	"internal/sandbox/fileops.go":        "RunFileOp marks the sandbox.exec span panicked (class only) and re-panics the value unchanged",
	"internal/sandbox/sandbox.go":        "RunBash and RunPython mark the sandbox.exec span panicked (class only) and re-panic the value unchanged",
}

func TestPanicRecoveryGoesThroughSafe(t *testing.T) {
	root := filepath.Join("..", "..")
	var bad []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case "testdata", "node_modules":
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			for p := range recoverAllowed {
				if rel == p || (strings.HasSuffix(p, "/") && strings.HasPrefix(rel, p)) {
					return nil
				}
			}
			bad = append(bad, unroutedRecovers(t, path, rel)...)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(bad) > 0 {
		t.Fatalf("recover() not routed through internal/safe (nor re-panicked):\n  %s", strings.Join(bad, "\n  "))
	}
}

// unroutedRecovers reports each function body that calls recover() directly
// but does not call into package safe. A re-panic does not count: a recover
// that re-panics one value (http.ErrAbortHandler) and prints the rest would
// pass a "panics again somewhere" rule.
func unroutedRecovers(t *testing.T, path, rel string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	safeName := importNames(f)["github.com/ElcanoTek/fleet/internal/safe"]
	var out []string
	check := func(body *ast.BlockStmt) {
		if body == nil {
			return
		}
		var recoverAt ast.Node
		routed := false
		ast.Inspect(body, func(n ast.Node) bool {
			if fl, ok := n.(*ast.FuncLit); ok && fl.Body != body {
				return false // its own body is checked on its own
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				if fn.Name == "recover" && fn.Obj == nil {
					recoverAt = call
				}
			case *ast.SelectorExpr:
				if id, ok := fn.X.(*ast.Ident); ok && safeName != "" && id.Name == safeName {
					routed = true
				}
			}
			return true
		})
		if recoverAt != nil && !routed {
			out = append(out, rel+":"+strconv.Itoa(fset.Position(recoverAt.Pos()).Line))
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			check(x.Body)
		case *ast.FuncLit:
			check(x.Body)
		}
		return true
	})
	return out
}
