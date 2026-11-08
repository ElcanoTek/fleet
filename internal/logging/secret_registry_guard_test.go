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

// directLiteralAllowed lists the functions ("<file>.<Func>") that may register
// literals on a redact.Redactor directly instead of through
// logging.RegisterSecretLiterals, each with its reason.
var directLiteralAllowed = map[string]string{
	"internal/agentcore/redact.go.toolRedactor":                      "the tool-output redactor seeding itself with the process's own secret-named env values at construction (the log seeds the same set at Configure)",
	"internal/agentcore/redact.go.addToolLiterals":                   "the tool-output redactor's subscription to logging.RegisterSecretLiterals",
	"internal/mcpbroker/redact.go.brokerRedactor":                    "the broker redactor seeding itself with the process's own secret-named env values at construction",
	"internal/mcpbroker/redact.go.addBrokerLiterals":                 "the broker redactor's subscription to logging.RegisterSecretLiterals",
	"internal/admincli/diagnose.go.newDiagnoseRedactor":              "a CLI report's own throwaway redactor, seeded from the env and env file it prints; it never sees a tool result or a log line",
	"internal/tools/browserbase_live_view.go.runBrowserbaseLiveView": "a per-call throwaway guard checking one vendor URL; it never registers a process-wide secret",
}

// subscribedRedactorPackages are the packages whose process-wide redactor
// must subscribe to logging.RegisterSecretLiterals (logging.OnSecretLiterals).
var subscribedRedactorPackages = []string{"internal/agentcore", "internal/mcpbroker"}

// TestSecretLiteralsHaveOneRegistrationPoint closes the "registered for one
// redactor, not the other" class (#2074 review: a host-derived connector
// value was registered for the log but not the tool-output redactor, so a tool
// result echoing it reached the model). logging.RegisterSecretLiterals is the
// one registration point: it feeds the log redactor and every redactor
// subscribed with OnSecretLiterals. So:
//   - no non-test code outside internal/logging and internal/redact calls
//     AddLiteral, RegisterSecrets or RegisterEnvLiterals on a redactor of its
//     own, except a reviewed function in directLiteralAllowed;
//   - every process-wide redactor's package subscribes.
func TestSecretLiteralsHaveOneRegistrationPoint(t *testing.T) {
	root := filepath.Join("..", "..")
	seen := map[string]bool{}
	subscribed := map[string]bool{}
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
			if strings.HasPrefix(rel, "internal/logging/") || strings.HasPrefix(rel, "internal/redact/") {
				return nil
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				t.Fatalf("parse %s: %v", path, perr)
			}
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				name := fd.Name.Name
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					c, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					se, ok := c.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					if id, ok := se.X.(*ast.Ident); ok && id.Name == "logging" {
						if se.Sel.Name == "OnSecretLiterals" {
							subscribed[filepath.ToSlash(filepath.Dir(rel))] = true
						}
						return true
					}
					switch se.Sel.Name {
					case "AddLiteral", "RegisterSecrets", "RegisterEnvLiterals":
						key := rel + "." + name
						if _, ok := directLiteralAllowed[key]; ok {
							seen[key] = true
							return true
						}
						bad = append(bad, rel+":"+strconv.Itoa(fset.Position(c.Pos()).Line)+" "+name+" ("+se.Sel.Name+")")
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for key := range directLiteralAllowed {
		if !seen[key] {
			bad = append(bad, "stale directLiteralAllowed entry: "+key)
		}
	}
	for _, pkg := range subscribedRedactorPackages {
		if !subscribed[pkg] {
			bad = append(bad, pkg+": its redactor does not subscribe (logging.OnSecretLiterals)")
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("secret literals registered past the one registration point — use logging.RegisterSecretLiterals (it feeds the log and every subscribed redactor), or add a reasoned directLiteralAllowed entry:\n  %s", strings.Join(bad, "\n  "))
	}
}
