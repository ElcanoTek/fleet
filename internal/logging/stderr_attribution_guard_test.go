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

// stderrUnattributedAllowed lists the packages (repo-relative dirs) that may
// create a StderrLogger without ever calling Attribute on one, each with its
// reason: a process-scope child whose stderr belongs to no single request
// or run. Empty today — every subprocess fleet logs stderr for serves calls
// (connector calls, sandbox bridge requests) and attributes them.
var stderrUnattributedAllowed = map[string]string{}

// TestStderrLoggersAreAttributedToTheirCalls closes the "subprocess stderr
// logged with a background context" class (#2074 review, twice: the sandbox
// bridge, then stdio connectors): a package outside internal/logging that
// creates a StderrLogger (NewStderrLogger, NewStderrLoggerContext) must also
// call Attribute — binding the subprocess's stderr to the request or call in
// flight, so a traceback carries its conv_id, turn_id, run_id — unless it is
// a reviewed process-scope child in stderrUnattributedAllowed.
func TestStderrLoggersAreAttributedToTheirCalls(t *testing.T) {
	root := filepath.Join("..", "..")
	creates := map[string][]string{} // dir -> creation sites
	attributes := map[string]bool{}  // dir -> calls Attribute somewhere
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
			if strings.HasPrefix(rel, "internal/logging/") {
				return nil
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				t.Fatalf("parse %s: %v", path, perr)
			}
			pkg := filepath.ToSlash(filepath.Dir(rel))
			ast.Inspect(f, func(n ast.Node) bool {
				c, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				se, ok := c.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch se.Sel.Name {
				case "NewStderrLogger", "NewStderrLoggerContext":
					creates[pkg] = append(creates[pkg], rel+":"+strconv.Itoa(fset.Position(c.Pos()).Line))
				case "Attribute":
					attributes[pkg] = true
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	var bad []string
	for pkg, sites := range creates {
		if attributes[pkg] {
			continue
		}
		if _, ok := stderrUnattributedAllowed[pkg]; ok {
			continue
		}
		bad = append(bad, sites...)
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("StderrLogger created but never attributed to a call — call Attribute(ctx) for each request the subprocess serves (and ClearAttribution after), or add a reasoned stderrUnattributedAllowed entry:\n  %s", strings.Join(bad, "\n  "))
	}
	if len(creates) == 0 {
		t.Fatal("found no StderrLogger creation at all: the guard is not looking where it should")
	}
}
