// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

var updateLocalizations = flag.Bool("update-localizations", false, "regenerate the browser's known HTTP error messages")

type localizedErrorFormat struct {
	Message string   `json:"message"`
	Fields  []string `json:"fields"`
}

type localizedErrors struct {
	Exact     []string               `json:"exact"`
	Formatted []localizedErrorFormat `json:"formatted"`
}

var goMessageFormat = regexp.MustCompile(`%%|%[-+ #0]*(?:\d+)?(?:\.\d+)?[sqvdwf]`)

// The browser translates known HTTP errors at the display boundary. Keeping
// this inventory tied to the handlers avoids either missing newly-added UI
// validation messages or guessing translations for arbitrary tool output.
func TestBackendErrorCatalog(t *testing.T) {
	root := repoRoot(t)
	messages := map[string]bool{}
	for _, dir := range []string{"internal/httpapi", "internal/sched/handlers", "internal/webui"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			source, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(source, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok {
					if literal := errorMessageLiteral(errorMessageArgument(call)); literal != nil {
						value, err := strconv.Unquote(literal.Value)
						if err != nil {
							t.Fatal(err)
						}
						messages[value] = true
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
	if len(messages) < 500 {
		t.Fatalf("only %d HTTP errors found: inspect the extractor before accepting a smaller catalog", len(messages))
	}
	checkLocalizationInventory(t, root, "backend-messages.json", messages)
	display := map[string]bool{}
	collectServerDisplayMessages(t, root, display)
	checkLocalizationInventory(t, root, "server-display-messages.json", display)
}

func checkLocalizationInventory(t *testing.T, root, filename string, messages map[string]bool) {
	t.Helper()
	keys := make([]string, 0, len(messages))
	for message := range messages {
		keys = append(keys, message)
	}
	slices.Sort(keys)
	want := localizedErrors{Exact: []string{}, Formatted: []localizedErrorFormat{}}
	for _, source := range keys {
		var fields []string
		message := goMessageFormat.ReplaceAllStringFunc(source, func(format string) string {
			if format == "%%" {
				return "%"
			}
			index := len(fields)
			fields = append(fields, format)
			return fmt.Sprintf("{%d}", index)
		})
		if len(fields) == 0 {
			want.Exact = append(want.Exact, message)
		} else {
			want.Formatted = append(want.Formatted, localizedErrorFormat{Message: message, Fields: fields})
		}
	}
	catalogPath := filepath.Join(root, "web/src/app/lib/i18n", filename)
	if *updateLocalizations {
		data, err := json.MarshalIndent(want, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(catalogPath, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatal(err)
	}
	var got localizedErrors
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	actual, _ := json.Marshal(got)
	expected, _ := json.Marshal(want)
	if string(actual) != string(expected) {
		t.Fatal("HTTP error localization inventory is stale: run cd web && npm run gen:localizations, then translate new keys in es.json and pt.json")
	}
}

// These read-only API fields are rendered as administrator diagnostics or
// forecast notes. Their machine values (paths, commands, model IDs) are slots.
func collectServerDisplayMessages(t *testing.T, root string, messages map[string]bool) {
	t.Helper()
	sources := make([]*ast.File, 0, 3)
	constants := map[string]ast.Expr{}
	for _, path := range []string{"internal/boxdoctor/boxdoctor.go", "internal/boxdoctor/caddyfile.go", "internal/agentcore/pricing.go"} {
		source, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, path), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, source)
		for _, declaration := range source.Decls {
			group, ok := declaration.(*ast.GenDecl)
			if !ok || group.Tok != token.CONST {
				continue
			}
			for _, spec := range group.Specs {
				values, ok := spec.(*ast.ValueSpec)
				if !ok || len(values.Names) != len(values.Values) {
					continue
				}
				for index, name := range values.Names {
					constants[name.Name] = values.Values[index]
				}
			}
		}
	}
	for _, source := range sources {
		add := func(expr ast.Expr) {
			message := serverDisplayTemplate(expr, constants)
			if strings.TrimSpace(strings.ReplaceAll(message, "%v", "")) != "" {
				messages[message] = true
			}
		}
		ast.Inspect(source, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.KeyValueExpr:
				if field, ok := typed.Key.(*ast.Ident); ok && serverDisplayField(field.Name) {
					add(typed.Value)
				}
			case *ast.AssignStmt:
				if len(typed.Lhs) != len(typed.Rhs) {
					return true
				}
				for index, lhs := range typed.Lhs {
					if field, ok := lhs.(*ast.SelectorExpr); ok && serverDisplayField(field.Sel.Name) {
						add(typed.Rhs[index])
					}
					if name, ok := lhs.(*ast.Ident); ok && name.Name == "detail" {
						add(typed.Rhs[index])
					}
				}
			}
			return true
		})
	}
}

func serverDisplayField(name string) bool {
	return name == "Name" || name == "Detail" || name == "Fix" || name == "Note"
}

func serverDisplayTemplate(expr ast.Expr, constants map[string]ast.Expr) string {
	if name, ok := expr.(*ast.Ident); ok {
		if value, found := constants[name.Name]; found {
			return serverDisplayTemplate(value, constants)
		}
	}
	if literal := errorMessageLiteral(expr); literal != nil {
		value, err := strconv.Unquote(literal.Value)
		if err == nil {
			return value
		}
	}
	if binary, ok := expr.(*ast.BinaryExpr); ok && binary.Op == token.ADD {
		return serverDisplayTemplate(binary.X, constants) + serverDisplayTemplate(binary.Y, constants)
	}
	return "%v"
}

func errorMessageArgument(call *ast.CallExpr) ast.Expr {
	if name, ok := call.Fun.(*ast.Ident); ok && name.Name == "writeError" && len(call.Args) > 2 {
		return call.Args[2]
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	owner, ok := selector.X.(*ast.Ident)
	if !ok {
		return nil
	}
	switch {
	case owner.Name == "http" && selector.Sel.Name == "Error" && len(call.Args) > 1:
		return call.Args[1]
	case owner.Name == "errors" && selector.Sel.Name == "New" && len(call.Args) > 0,
		owner.Name == "fmt" && selector.Sel.Name == "Errorf" && len(call.Args) > 0:
		return call.Args[0]
	default:
		return nil
	}
}

func errorMessageLiteral(expr ast.Expr) *ast.BasicLit {
	if literal, ok := expr.(*ast.BasicLit); ok && literal.Kind == token.STRING {
		return literal
	}
	if call, ok := expr.(*ast.CallExpr); ok && len(call.Args) > 0 {
		if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Sprintf" {
			return errorMessageLiteral(call.Args[0])
		}
	}
	if name, ok := expr.(*ast.Ident); ok && name.Obj != nil {
		if declaration, ok := name.Obj.Decl.(*ast.ValueSpec); ok {
			for index, candidate := range declaration.Names {
				if candidate.Name == name.Name && index < len(declaration.Values) {
					return errorMessageLiteral(declaration.Values[index])
				}
			}
		}
	}
	return nil
}
