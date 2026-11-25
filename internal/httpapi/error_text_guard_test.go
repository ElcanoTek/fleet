package httpapi

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// errorTextGuardDirs are the packages whose handlers answer browsers and API
// clients: the chat API, the browser gateway, the orchestrator handlers and
// the fleet binary's own admin routes, probes and chat tools.
var errorTextGuardDirs = []string{
	"internal/httpapi",
	"internal/webui",
	"internal/sched/handlers",
	"cmd/fleet",
}

// errorTextFieldExemptFiles are files whose Detail/Error fields are a CLI
// verb's own terminal output, not a response body: the response-field check
// skips them (calls are still checked).
var errorTextFieldExemptFiles = map[string]string{
	"cmd/fleet/validate_config.go": "`fleet validate-config` prints its checks' details to the operator running it",
	"cmd/fleet/mcp_probe.go":       "`fleet mcp test` prints each server's connect error to the operator running it",
}

// errorTextAllowed lists the reviewed places where an error's text is put in
// a response although TestNoErrorTextInResponses would flag it, keyed
// "<dir>/<file>:<function>" with the reason. A function listed here is exempt
// as a whole, so keep each one small.
var errorTextAllowed = map[string]string{
	"cmd/fleet/health.go:k8sSandboxProbe":                                       "the /readyz sandbox detail: the sandbox client sanitizes the apiserver's version text and errors before they reach the probe",
	"cmd/fleet/health.go:podmanSandboxProbe":                                    "the /readyz sandbox detail names the runtime binary and why `<runtime> --version` did not run (not found, exit status); no store or driver text",
	"cmd/fleet/workspace_settings.go:guardrailState.Probe":                      "the admin-only guardrail probe reports the detector call's outcome to the admin testing that detector: that outcome is the result the probe is run for",
	"internal/httpapi/admin_llm_providers.go:Server.handleLLMProviderModels":    "the slug check's message is CheckModelRoute's local routing verdict (an empty slug, or no provider route plus how to pick one); it reads no store and calls no provider",
	"internal/httpapi/admin_settings.go:httpErrorForSetting":                    "the 500 line answers a settings.ApplyError whose undo write failed: its text is the apply hook's, written from configuration, plus what to do (run Reset); the undo write's own error is logged by settings",
	"internal/httpapi/connector_studio.go:Server.connectorStudioTestCustomTool": "Connector Studio's tool test reports the connector's own error (secrets redacted) to the admin building that connector: that error is the result the test was run for",
	"internal/sched/handlers/a2a.go:a2aCreateError":                             "the flagged line answers a budget refusal (budget.ExceededError) with its own text, the window and limit that refused, as writeBudgetRefusal does over REST; an infrastructure error falls through to a logged, fixed message",
	"internal/sched/handlers/task_gates.go:refuse":                              "refuse wraps the create pipeline's validation errors and its fixed refusals into a createRefusalError; no caller passes a store error",
	"internal/webui/spa.go:Gateway.renderIndex":                                 "the two flagged lines report the Vite dev server (make dev only) to the developer running it; the encode failure on the same path is logged and answered with a fixed message",
}

// TestNoErrorTextInResponses: an internal error is logged with the request's
// correlation ids and answered with a fixed message (writeInternalError in
// internal/httpapi). Error text is not written into a 5xx body, a JSON body,
// a response's error/message/detail field or a stream frame, because it is
// whatever the store, the driver or the filesystem said (a constraint or
// table name, a database host) rather than something the reader can act on.
//
// It flags error text — x.Error() (except on a package-level
// `var errX = errors.New("…")`, or on fmt.Errorf/errors.New of no error
// value), or an error-named value (err, perr, x.Err, …) formatted by
// fmt.Sprintf/Sprint — passed to:
//
//   - http.Error, writeError, writeJSONStatus or webui's writeJSON with a
//     status that is not a constant 4xx (http.StatusBadRequest, …): a 5xx, or
//     a status chosen at run time;
//   - httpapi's writeJSON (always 200), in any argument;
//   - a stream emitter (Emit / emit), in the payload;
//   - an A2A error response (NewErrorResponse) with the internal or server
//     error code;
//   - approvalError and the handlers' local fail closures, with a status that
//     is not a constant 4xx;
//   - a struct field or JSON key named *error, *message or *detail, outside a
//     4xx write above.
//
// A 4xx with a constant status may carry an error's text: those errors are
// the handler's own validation (a decode error, a typed refusal whose message
// is written for the user). An error that can wrap a store, driver,
// filesystem or network failure is not one of them — answer it with
// writeInternalError instead. The guard sees only what is written at the
// call: text passed through a variable is the reviewer's to check.
func TestNoErrorTextInResponses(t *testing.T) {
	root := filepath.Join("..", "..")
	var bad []string
	used := map[string]bool{}
	for _, dir := range errorTextGuardDirs {
		paths, err := filepath.Glob(filepath.Join(root, dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		files := map[string]*ast.File{}
		for _, path := range paths {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			files[path] = f
		}
		fixed := fixedErrorVars(files)
		for path, f := range files {
			rel := filepath.ToSlash(filepath.Join(dir, filepath.Base(path)))
			_, exempt := errorTextFieldExemptFiles[rel]
			g := errorTextGuard{fset: fset, fixed: fixed, fields: !exempt}
			for _, finding := range g.findings(f) {
				key := rel + ":" + finding.fn
				if _, ok := errorTextAllowed[key]; ok {
					used[key] = true
					continue
				}
				bad = append(bad, fmt.Sprintf("%s:%d (%s): %s", rel, finding.line, finding.fn, finding.what))
			}
		}
	}
	for key := range errorTextAllowed {
		if !used[key] {
			bad = append(bad, key+": allowlisted but no longer flagged — remove the entry")
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("error text written to a response or stream frame — log it and answer with writeInternalError (or a fixed message):\n  %s", strings.Join(bad, "\n  "))
	}
}

type errorTextFinding struct {
	line int
	fn   string
	what string
}

type errorTextGuard struct {
	fset *token.FileSet
	// fixed are the package-level variables initialised with errors.New of a
	// string literal: their Error() is fixed text the package wrote.
	fixed map[string]bool
	// fields turns on the response-field check.
	fields bool
}

// fixedErrorVars collects the package-level `var x = errors.New("…")`
// declarations of one package.
func fixedErrorVars(files map[string]*ast.File) map[string]bool {
	fixed := map[string]bool{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) != len(vs.Values) {
					continue
				}
				for i, name := range vs.Names {
					call, ok := vs.Values[i].(*ast.CallExpr)
					if !ok || len(call.Args) != 1 {
						continue
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "New" {
						continue
					}
					if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "errors" {
						continue
					}
					if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						fixed[name.Name] = true
					}
				}
			}
		}
	}
	return fixed
}

func (g errorTextGuard) findings(f *ast.File) []errorTextFinding {
	var out []errorTextFinding
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		name := fd.Name.Name
		if fd.Recv != nil && len(fd.Recv.List) == 1 {
			recv := fd.Recv.List[0].Type
			if star, ok := recv.(*ast.StarExpr); ok {
				recv = star.X
			}
			if id, ok := recv.(*ast.Ident); ok {
				name = id.Name + "." + name
			}
		}
		add := func(n ast.Node, what string) {
			out = append(out, errorTextFinding{line: g.fset.Position(n.Pos()).Line, fn: name, what: what})
		}
		// exempt holds the composite literals of a 4xx write: their fields
		// may carry a validation error's text like the write itself.
		exempt := map[ast.Node]bool{}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if body := clientErrorBody(n); body != nil {
					ast.Inspect(body, func(m ast.Node) bool {
						if kv, ok := m.(*ast.KeyValueExpr); ok {
							exempt[kv] = true
						}
						return true
					})
				}
				if what := g.sink(n); what != "" {
					add(n, what)
				}
			case *ast.AssignStmt:
				if !g.fields {
					return true
				}
				// resp.RolloutError = err.Error(): a response field.
				for i, lhs := range n.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && responseTextField(sel.Sel.Name) && i < len(n.Rhs) && g.errorText(n.Rhs[i]) {
						add(n, "field "+sel.Sel.Name+" set to error text")
					}
				}
			case *ast.KeyValueExpr:
				if !g.fields || exempt[n] {
					return true
				}
				// T{PartialError: err.Error()} or map[string]any{"error": err.Error()}.
				key := ""
				switch k := n.Key.(type) {
				case *ast.Ident:
					key = k.Name
				case *ast.BasicLit:
					if k.Kind == token.STRING {
						key, _ = strconv.Unquote(k.Value)
					}
				}
				if key != "" && responseTextField(key) && g.errorText(n.Value) {
					add(n, "field "+key+" set to error text")
				}
			}
			return true
		})
	}
	return out
}

// clientErrorBody is the body argument of a JSON write with a constant 4xx
// status (writeJSONStatus(w, r, 4xx, body), webui's writeJSON(w, 4xx, body),
// writeError(w, 4xx, detail)), or nil.
func clientErrorBody(call *ast.CallExpr) ast.Expr {
	fn, ok := call.Fun.(*ast.Ident)
	if !ok {
		return nil
	}
	switch {
	case fn.Name == "writeJSONStatus" && len(call.Args) == 4 && clientErrorStatus(call.Args[2]):
		return call.Args[3]
	case (fn.Name == "writeJSON" || fn.Name == "writeError") && len(call.Args) == 3 && clientErrorStatus(call.Args[1]):
		return call.Args[2]
	}
	return nil
}

// responseTextField reports whether a field or JSON key with this name holds
// text a response shows: an error, a message or a detail.
func responseTextField(name string) bool {
	lower := strings.ToLower(name)
	for _, suffix := range []string{"error", "message", "detail"} {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return false
}

// sink reports what call does with error text, or "" when it puts none in a
// response.
func (g errorTextGuard) sink(call *ast.CallExpr) string {
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		return g.selectorSink(call, fn)
	case *ast.Ident:
		switch fn.Name {
		case "writeError":
			if len(call.Args) == 3 && g.errorText(call.Args[2]) && !clientErrorStatus(call.Args[1]) {
				return "writeError with error text and status " + exprString(call.Args[1])
			}
		case "writeJSON":
			// webui's writeJSON(w, status, body); httpapi's writeJSON(w, r, v)
			// always answers 200.
			if len(call.Args) == 3 && isHTTPStatus(call.Args[1]) {
				if g.errorText(call.Args[2]) && !clientErrorStatus(call.Args[1]) {
					return "writeJSON with error text and status " + exprString(call.Args[1])
				}
				return ""
			}
			if g.anyErrorText(call.Args) {
				return "writeJSON body carries error text"
			}
		case "writeJSONStatus":
			if len(call.Args) == 4 && g.errorText(call.Args[3]) && !clientErrorStatus(call.Args[2]) {
				return "writeJSONStatus with error text and status " + exprString(call.Args[2])
			}
		case "emit":
			if len(call.Args) >= 2 && g.anyErrorText(call.Args[1:]) {
				return "stream frame " + exprString(call.Args[0]) + " carries error text"
			}
		case "approvalError", "fail":
			if len(call.Args) == 2 && g.errorText(call.Args[1]) && !clientErrorStatus(call.Args[0]) {
				return fn.Name + " with error text and status " + exprString(call.Args[0])
			}
		}
	}
	return ""
}

// selectorSink is sink for a pkg.Func or x.Method call.
func (g errorTextGuard) selectorSink(call *ast.CallExpr, fn *ast.SelectorExpr) string {
	if pkg, ok := fn.X.(*ast.Ident); ok && pkg.Name == "http" && fn.Sel.Name == "Error" && len(call.Args) == 3 {
		if g.errorText(call.Args[1]) && !clientErrorStatus(call.Args[2]) {
			return "http.Error with error text and status " + exprString(call.Args[2])
		}
		return ""
	}
	if fn.Sel.Name == "Emit" && len(call.Args) >= 2 && g.anyErrorText(call.Args[1:]) {
		return "stream frame " + exprString(call.Args[0]) + " carries error text"
	}
	// An A2A JSON-RPC error with the internal or server error code.
	if fn.Sel.Name == "NewErrorResponse" && len(call.Args) >= 3 && g.errorText(call.Args[2]) {
		if code := exprString(call.Args[1]); code == "wire.ErrInternalError" || code == "wire.ErrServerError" {
			return "A2A " + code + " response carries error text"
		}
	}
	return ""
}

func (g errorTextGuard) anyErrorText(args []ast.Expr) bool {
	for _, a := range args {
		if g.errorText(a) {
			return true
		}
	}
	return false
}

// errorText reports whether e has an error's text in it: a call of a
// no-argument Error method (except on one of the package's fixed errors), or
// an error-named value formatted by fmt.
func (g errorTextGuard) errorText(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name == "Error" && len(call.Args) == 0 {
			switch x := sel.X.(type) {
			case *ast.Ident:
				if g.fixed[x.Name] {
					return false
				}
			case *ast.CallExpr:
				// fmt.Errorf("… %d …", n).Error(): fixed text unless it
				// formats an error.
				if inner, ok := x.Fun.(*ast.SelectorExpr); ok {
					if pkg, ok := inner.X.(*ast.Ident); ok && (pkg.Name == "fmt" && inner.Sel.Name == "Errorf" || pkg.Name == "errors" && inner.Sel.Name == "New") {
						for _, a := range x.Args {
							if errorNamed(a) {
								found = true
							}
						}
						return false
					}
				}
			}
			found = true
			return false
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "fmt" {
			// fmt.Errorf is left out: it makes an error, not text, and its
			// .Error() is matched above.
			switch sel.Sel.Name {
			case "Sprintf", "Sprint", "Sprintln":
				for _, a := range call.Args {
					if errorNamed(a) {
						found = true
						return false
					}
				}
			}
		}
		return true
	})
	return found
}

// errorNamed reports whether e is a value named like an error: err, perr,
// lookupErr, x.Err.
func errorNamed(e ast.Expr) bool {
	var name string
	switch v := e.(type) {
	case *ast.Ident:
		name = v.Name
	case *ast.SelectorExpr:
		name = v.Sel.Name
	default:
		return false
	}
	return name == "err" || name == "Err" || strings.HasSuffix(name, "err") || strings.HasSuffix(name, "Err")
}

// isHTTPStatus reports whether e is a net/http status constant.
func isHTTPStatus(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "http" && strings.HasPrefix(sel.Sel.Name, "Status")
}

// clientErrorStatus reports whether e is a constant 4xx status.
func clientErrorStatus(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "http" {
		return false
	}
	switch sel.Sel.Name {
	case "StatusBadRequest", "StatusUnauthorized", "StatusPaymentRequired", "StatusForbidden",
		"StatusNotFound", "StatusMethodNotAllowed", "StatusNotAcceptable", "StatusConflict",
		"StatusGone", "StatusLengthRequired", "StatusPreconditionFailed", "StatusRequestEntityTooLarge",
		"StatusUnsupportedMediaType", "StatusRequestedRangeNotSatisfiable", "StatusUnprocessableEntity",
		"StatusLocked", "StatusFailedDependency", "StatusPreconditionRequired", "StatusTooManyRequests",
		"StatusRequestHeaderFieldsTooLarge", "StatusUnavailableForLegalReasons":
		return true
	}
	return false
}

func exprString(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprString(v.X) + "." + v.Sel.Name
	case *ast.BasicLit:
		return v.Value
	default:
		return fmt.Sprintf("%T", e)
	}
}

// TestErrorTextGuardSeesTheClass runs the guard over a fixture: each line
// marked "flag" must be reported and nothing else.
func TestErrorTextGuardSeesTheClass(t *testing.T) {
	const src = `package p

var errFixed = errors.New("fixed text")

func h(w http.ResponseWriter, r *http.Request, err error, buf *turnBuffer, status int) {
	http.Error(w, err.Error(), http.StatusInternalServerError)              // flag
	http.Error(w, "save: "+err.Error(), http.StatusInternalServerError)     // flag
	http.Error(w, fmt.Sprintf("save: %v", err), http.StatusBadGateway)     // flag
	http.Error(w, err.Error(), status)                                      // flag
	http.Error(w, err.Error(), http.StatusBadRequest)
	http.Error(w, errFixed.Error(), http.StatusServiceUnavailable)
	http.Error(w, fmt.Errorf("%d left", 3).Error(), http.StatusInternalServerError)
	writeJSON(w, r, map[string]string{"message": err.Error()})              // flag
	writeJSONStatus(w, r, http.StatusConflict, map[string]string{"error": err.Error()})
	writeJSONStatus(w, r, http.StatusInternalServerError, map[string]string{"error": err.Error()}) // flag
	buf.Emit("turn.error", map[string]any{"message": err.Error()})          // flag
	emit("summary.error", map[string]any{"message": err.Error()})           // flag
	fail(http.StatusInternalServerError, "token: "+err.Error())             // flag
	fail(http.StatusInternalServerError, fmt.Errorf("persona: %w", err))
	resp.RolloutError = err.Error()                                         // flag
	_ = T{PartialError: fmt.Sprintf("%v", err)}                             // flag
	_ = a2abridge.NewErrorResponse(id, wire.ErrInternalError, err.Error(), nil)   // flag
	_ = a2abridge.NewErrorResponse(id, wire.ErrTaskNotFound, err.Error(), nil)
	writeInternalError(w, r, err)
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "fixture.go", src, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	g := errorTextGuard{fset: fset, fixed: fixedErrorVars(map[string]*ast.File{"fixture.go": f}), fields: true}
	got := map[int]bool{}
	for _, finding := range g.findings(f) {
		got[finding.line] = true
	}
	for i, line := range strings.Split(src, "\n") {
		want := strings.HasSuffix(strings.TrimSpace(line), "// flag")
		if got[i+1] != want {
			t.Errorf("line %d (%s): flagged = %v, want %v", i+1, strings.TrimSpace(line), got[i+1], want)
		}
	}
}
