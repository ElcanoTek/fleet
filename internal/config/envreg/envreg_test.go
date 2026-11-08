package envreg

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// TestTableIsWellFormed checks every entry carries what the generated docs,
// `fleet config explain` and the knob parser need.
func TestTableIsWellFormed(t *testing.T) {
	groups := map[string]bool{}
	for _, g := range Groups {
		groups[g] = true
	}
	scopes := map[Scope]bool{ScopeServer: true, ScopeCLI: true, ScopeSandbox: true, ScopeBroker: true, ScopeScripts: true, ScopeTest: true}
	for _, e := range All() {
		if strings.TrimSpace(e.Description) == "" {
			t.Errorf("%s has no description", e.Name)
		}
		if !groups[e.Group] {
			t.Errorf("%s has unknown group %q", e.Name, e.Group)
		}
		if !scopes[e.Scope] {
			t.Errorf("%s has unknown scope %q", e.Name, e.Scope)
		}
		if e.Type == "" {
			t.Errorf("%s has no type", e.Name)
		}
		if (e.Type == TypeEnum) != (len(e.Enum) > 0) {
			t.Errorf("%s: an enum type needs Enum values and only an enum may have them", e.Name)
		}
		if e.Knob != nil && e.Knob.Lenient && strings.TrimSpace(e.Knob.Why) == "" {
			t.Errorf("%s is a lenient knob without a Why", e.Name)
		}
	}
}

func TestLookupPanicsOnUnregisteredNameInTests(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Get of an unregistered name must panic in a test binary")
		}
	}()
	_ = Get("FLEET_THIS_NAME_IS_NOT_REGISTERED")
}

// TestRetiredAliases pins ADR-0106: a removed CHAT_/CUTLASS_ spelling (and
// the bare DATA_DIR) is not registered, so nothing can read it, and
// RetiredAlias names the FLEET_ key to use; the canonical CHAT_/CUTLASS_
// settings are not retired.
func TestRetiredAliases(t *testing.T) {
	for name, want := range map[string]string{
		"CHAT_MAX_COST_USD":    "FLEET_MAX_COST_USD",
		"CUTLASS_MAX_COST_USD": "FLEET_MAX_COST_USD",
		"CHAT_SERVER_TOKEN":    "FLEET_SERVER_TOKEN",
		"CHAT_TLS_MODE":        "FLEET_TLS_MODE",
		"CUTLASS_TLS_DOMAIN":   "FLEET_TLS_DOMAIN",
		"CHAT_IMAGE_MODEL":     "FLEET_IMAGE_MODEL",
		"CUTLASS_LOG_FILE":     "FLEET_LOG_FILE",
		"DATA_DIR":             "FLEET_DATA_DIR",
	} {
		if IsRegistered(name) {
			t.Errorf("%s is still registered", name)
		}
		if got, ok := RetiredAlias(name); !ok || got != want {
			t.Errorf("RetiredAlias(%s) = %q, %v; want %q", name, got, ok, want)
		}
	}
	for _, name := range []string{
		"CHAT_RATE_PER_MIN", "CUTLASS_ALLOWED_DIRS", "CUTLASS_IMAGE_MODEL", "CHAT_DB_NAME",
		"FLEET_MAX_COST_USD", "PERSONA", "CHAT_NOT_A_SETTING", "CHAT_", "HOME",
	} {
		if c, ok := RetiredAlias(name); ok {
			t.Errorf("RetiredAlias(%s) = %q; it is not a removed spelling", name, c)
		}
	}
	for _, e := range All() {
		for _, a := range e.Aliases {
			if _, retired := RetiredAlias(a.Name); retired || retiredPrefixed(a.Name) {
				t.Errorf("%s declares alias %s, a spelling ADR-0106 removed", e.Name, a.Name)
			}
		}
	}
}

func TestRetiredSpellings(t *testing.T) {
	want := "FLEET_SERVER_TOKEN,CHAT_SERVER_TOKEN,CUTLASS_SERVER_TOKEN"
	for _, n := range []string{"FLEET_SERVER_TOKEN", "CHAT_SERVER_TOKEN", "CUTLASS_SERVER_TOKEN"} {
		if got := strings.Join(RetiredSpellings(n), ","); got != want {
			t.Errorf("RetiredSpellings(%s) = %s, want %s", n, got, want)
		}
	}
	if got := RetiredSpellings("OPENROUTER_API_KEY"); len(got) != 1 || got[0] != "OPENROUTER_API_KEY" {
		t.Errorf("RetiredSpellings(OPENROUTER_API_KEY) = %v", got)
	}
}

// ── repo walk: every literal env name read anywhere is registered ──

// keyFuncs take a verbatim env name as their first argument.
var keyFuncs = map[string]bool{
	"Get": true, "Lookup": true, // envreg.Get / envreg.Lookup
	"envOrFile": true, "envOr": true, // admincli, harness binaries
	"envDuration": true, "envInt": true, // internal/httpapi
	"envBoolDefaultTrue": true, // internal/webpush
	"getenvDefault":      true, // internal/config
	"getenvInt":          true, "getenvBool": true,
	"EnvKnobInt": true, "EnvKnobBool": true,
}

// suffixFuncs take a FLEET_ suffix as their first argument (or second, for
// the reload helpers).
var suffixFuncs = map[string]int{
	"lookupFleet": 0, "getenvFleet": 0, "getenvFleetDefault": 0, "getenvFleetOrBare": 0,
	"getenvFleetInt": 0, "getenvFleetInt64": 0, "getenvFleetFloat": 0, "getenvFleetBool": 0,
	"getenvFleetDuration": 0, "reloadFleetFloat": 1, "reloadFleetInt": 1,
	"lookup": 0, "lookupBool": 0, "lookupBoolDefault": 0, "lookupFloatDefault": 0, "hasExplicitFraction": 0, // agentcore fleetEnv
	"fleetEnv": 0, // internal/tools
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

// literalArg resolves call's idx-th argument when it is a string literal or a
// package-level string constant of the same file set.
func literalArg(call *ast.CallExpr, idx int, consts map[string]string) (string, bool) {
	if idx >= len(call.Args) {
		return "", false
	}
	switch a := call.Args[idx].(type) {
	case *ast.BasicLit:
		if a.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(a.Value)
		return s, err == nil
	case *ast.Ident:
		s, ok := consts[a.Name]
		return s, ok
	}
	return "", false
}

func calleeName(call *ast.CallExpr) (pkg, name string) {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return "", fn.Name
	case *ast.SelectorExpr:
		if id, ok := fn.X.(*ast.Ident); ok {
			return id.Name, fn.Sel.Name
		}
		return "", fn.Sel.Name
	}
	return "", ""
}

// fileConsts collects f's package-level string constants, so a read through
// a named constant (clientconfig.EnvDir's "FLEET_CLIENT_CONFIG_DIR") counts.
func fileConsts(f *ast.File) map[string]string {
	consts := map[string]string{}
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, n := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if s, err := strconv.Unquote(lit.Value); err == nil {
						consts[n.Name] = s
					}
				}
			}
		}
	}
	return consts
}

// readWalker checks one file's calls.
type readWalker struct {
	rel      string
	consts   map[string]string
	reads    int
	problems []string
}

func (w *readWalker) visit(n ast.Node) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return true
	}
	pkg, name := calleeName(call)
	switch {
	case pkg == "envreg" && (name == "Unregistered" || name == "LookupUnregistered"):
		if s, ok := literalArg(call, 0, w.consts); ok {
			w.problems = append(w.problems, w.rel+": envreg."+name+"("+strconv.Quote(s)+") — a literal name belongs in the registry; use envreg.Get")
		}
	case (name == "Get" || name == "Lookup") && pkg != "envreg":
		// Some other package's Get.
	case keyFuncs[name]:
		if s, ok := literalArg(call, 0, w.consts); ok {
			w.reads++
			if !IsRegistered(s) {
				w.problems = append(w.problems, w.rel+": "+name+"("+strconv.Quote(s)+") reads an unregistered name")
			}
		}
	default:
		w.checkSuffix(call, name)
	}
	return true
}

func (w *readWalker) checkSuffix(call *ast.CallExpr, name string) {
	idx, ok := suffixFuncs[name]
	if !ok {
		return
	}
	s, ok := literalArg(call, idx, w.consts)
	if !ok {
		return
	}
	s = strings.TrimLeft(s, "_")
	if s == "" || strings.ContainsAny(s, " ./-") || strings.ToUpper(s) != s {
		return // not an env suffix (some other "lookup" method)
	}
	w.reads++
	if !IsRegistered(CanonicalPrefix + s) {
		w.problems = append(w.problems, w.rel+": "+name+"("+strconv.Quote(s)+") reads FLEET_"+s+", which is not registered")
	}
}

// TestNoUnregisteredLiteralReads walks every non-test Go file in the module
// and fails on a literal env name passed to the registry accessor or to one
// of the repo's env-reading helpers that is not registered — and on ANY
// literal passed to Unregistered/LookupUnregistered, which exist only for
// names that come from data. This is the static half of "nothing reads env
// outside the registry"; Get's test-binary panic is the runtime half, and
// the forbidigo rule in .golangci.yml bans the os accessors outright.
func TestNoUnregisteredLiteralReads(t *testing.T) {
	root := moduleRoot(t)
	var problems []string
	reads := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "web", "docs", "deploy", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if perr != nil {
			return perr
		}
		rel, _ := filepath.Rel(root, path)
		w := &readWalker{rel: rel, consts: fileConsts(f)}
		ast.Inspect(f, w.visit)
		reads += w.reads
		problems = append(problems, w.problems...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if reads < 150 {
		t.Fatalf("found only %d literal env reads; the walk is broken", reads)
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}
