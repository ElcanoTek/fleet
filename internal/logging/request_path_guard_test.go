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

// requestPathSanitizers are the functions that turn a request URL into
// something safe to log: a matched route pattern, or a path with every
// segment that could carry a credential (a share, invite or reset token, an
// OAuth code) and the query removed. A raw path passed to one of them is not
// logged raw.
var requestPathSanitizers = map[string]bool{
	"RouteLabel":    true, // httpmw: route pattern, else SanitizePath
	"SanitizePath":  true, // httpmw
	"logSafeURL":    true, // remotemcp: scheme://host/path of a server URL, no query or userinfo
	"RedactDSN":     true, // config: userinfo and secret parameters removed
	"RedactURL":     true,
	"redactURLPath": true,
}

// TestNoLoggingCallTakesARawRequestPath: a request's path or query is never
// handed to a log call (or a panic event's metadata) as is. A path segment
// can be a live capability — /shared/<token>, an invite or reset token — and
// a query can carry an OAuth code, a state or ?token= (#2074 review: the
// panic recoverer logged r.URL.Path for /shared/<token>). Log the matched
// route (httpmw.RouteLabel) or a sanitised path instead.
//
// It flags, inside a log call's arguments (slog's functions and any
// Info/Warn/Error/Debug/Log/LogAttrs/…Context/With method) and inside a
// safe.PanicMetadata literal: x.URL.Path, x.URL.RawPath, x.URL.RawQuery,
// x.URL.Opaque, x.URL.String(), x.RequestURI, and a request's URL itself
// (r.URL, req.URL, x.Request.URL) — except as the argument of a sanitiser
// (requestPathSanitizers).
func TestNoLoggingCallTakesARawRequestPath(t *testing.T) {
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
			bad = append(bad, rawRequestPathLogs(t, root, path)...)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("log calls taking a raw request path, query or URL — log the route pattern (httpmw.RouteLabel) or a sanitised path:\n  %s", strings.Join(bad, "\n  "))
	}
}

var logMethodNames = map[string]bool{
	"Info": true, "Warn": true, "Error": true, "Debug": true, "Log": true, "LogAttrs": true,
	"InfoContext": true, "WarnContext": true, "ErrorContext": true, "DebugContext": true, "With": true,
}

func rawRequestPathLogs(t *testing.T, root, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	rel, _ := filepath.Rel(root, path)
	var out []string
	report := func(n ast.Node, what string) {
		out = append(out, filepath.ToSlash(rel)+":"+strconv.Itoa(fset.Position(n.Pos()).Line)+" "+what)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			if se, ok := x.Fun.(*ast.SelectorExpr); ok && logMethodNames[se.Sel.Name] {
				for _, a := range x.Args {
					findRawRequestPath(a, report)
				}
			}
		case *ast.CompositeLit:
			if se, ok := x.Type.(*ast.SelectorExpr); ok && se.Sel.Name == "PanicMetadata" {
				findRawRequestPath(x, report)
			}
		}
		return true
	})
	return out
}

// findRawRequestPath reports each raw request path, query or URL in e.
func findRawRequestPath(e ast.Node, report func(ast.Node, string)) {
	ast.Inspect(e, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			if name := calleeName(x); requestPathSanitizers[name] {
				return false
			}
			// x.URL.String() / x.URL.Redacted()
			if se, ok := x.Fun.(*ast.SelectorExpr); ok && (se.Sel.Name == "String" || se.Sel.Name == "Redacted") && isURLField(se.X) {
				report(x, "URL."+se.Sel.Name+"()")
				return false
			}
		case *ast.SelectorExpr:
			switch {
			case x.Sel.Name == "RequestURI":
				report(x, "RequestURI")
				return false
			case isURLField(x.X) && (x.Sel.Name == "Path" || x.Sel.Name == "RawPath" || x.Sel.Name == "RawQuery" || x.Sel.Name == "Opaque" || x.Sel.Name == "EscapedPath"):
				report(x, "URL."+x.Sel.Name)
				return false
			case isRequestURL(x):
				report(x, "request URL")
				return false
			}
		}
		return true
	})
}

func calleeName(c *ast.CallExpr) string {
	switch fn := c.Fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

// isURLField reports x.URL (any x).
func isURLField(e ast.Expr) bool {
	se, ok := e.(*ast.SelectorExpr)
	return ok && se.Sel.Name == "URL"
}

// isRequestURL reports a request's URL itself: r.URL, req.URL,
// request.URL, x.Request.URL.
func isRequestURL(se *ast.SelectorExpr) bool {
	if se.Sel.Name != "URL" {
		return false
	}
	switch x := se.X.(type) {
	case *ast.Ident:
		return x.Name == "r" || x.Name == "req" || x.Name == "request"
	case *ast.SelectorExpr:
		return x.Sel.Name == "Request"
	}
	return false
}
