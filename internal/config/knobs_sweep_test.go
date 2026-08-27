package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// #1273: the #1119 registry covered only what config.Load consumes, so a knob
// parsed at its point of use somewhere else in the tree kept silently (or
// warn-)defaulting on a malformed value, and `fleet validate-config` could not
// preflight it at all. Fixing the ones that existed is half the job; this test
// is the other half — it makes a NEW ad-hoc out-of-loader knob fail the suite
// until it has a registry row.
//
// It walks every non-test .go file in the module and looks for the two shapes
// an ad-hoc typed env read actually takes:
//
//  1. A function that both reads a LITERAL env key (os.Getenv / os.LookupEnv,
//     or agentcore's EnvPrefix.lookup) and calls a typed parser
//     (strconv.Atoi/ParseInt/ParseFloat/ParseBool/ParseUint, time.ParseDuration).
//  2. A package-local env-parse HELPER — a func that reads one of its own string
//     parameters from the env and parses it (httpapi's envInt/envDuration,
//     webpush's envBoolDefaultTrue, agentcore's lookupBool/lookupFloatDefault) —
//     plus every call site of that helper with a literal key. Helpers are what
//     put the key literal and the parse in different functions, which is
//     exactly where a hand-maintained inventory drifts.
//
// Every key found that way must be in the envKnobs registry (any of the three
// classes) or in sweepExemptKeys with a written reason. Legacy CHAT_/CUTLASS_
// spellings resolve to their canonical FLEET_ row.
//
// The heuristic is deliberately over-eager: a false positive costs one exempt
// row with a reason, while a false negative is the bug class this test exists
// to prevent.

// sweepParseFuncs are the typed parsers whose presence makes a function (or a
// helper) a typed env reader.
var sweepParseFuncs = map[string]map[string]bool{
	"strconv": {"Atoi": true, "ParseInt": true, "ParseFloat": true, "ParseBool": true, "ParseUint": true},
	"time":    {"ParseDuration": true},
}

// sweepEnvReadFuncs are the env-reading calls whose literal argument is a key.
// os.Getenv/os.LookupEnv take a verbatim key; the EnvPrefix machinery's lookup
// takes a SUFFIX under the FLEET_ chain.
var sweepEnvReadFuncs = map[string]struct{ pkg, sel string }{
	"os.Getenv":    {"os", "Getenv"},
	"os.LookupEnv": {"os", "LookupEnv"},
}

// sweepExemptKeys are keys the sweep finds but that need no registry row, each
// with the reason it is not a typed knob of ours.
var sweepExemptKeys = map[string]string{
	// Read as an opaque string; the number parsed nearby belongs to something
	// else entirely (a PID, a /proc field, a `git rev-list` count).
	"FLEET_POD_UID":                 "opaque Kubernetes UID string, not a typed knob",
	"FLEET_OWNER_ID":                "opaque owner label string, not a typed knob",
	"FLEET_ROOT":                    "filesystem path",
	"FLEET_SERVICE_NAME":            "systemd unit name",
	"FLEET_SANDBOX_IMAGE":           "container image reference",
	"FLEET_SANDBOX_SECCOMP_PROFILE": "seccomp profile name/path",
	"FLEET_SCHED_DATABASE_URL":      "DSN string (validated by the loader as a DSN, not a number)",
	"DATABASE_URL":                  "DSN string (validated by the loader as a DSN, not a number)",
	"OPENROUTER_API_KEY":            "credential, presence-checked only",
	// Typed, but the loader owns it: these are loader knobs whose value is
	// ALSO re-read at a point of use. The registry row exists (as a loader
	// row), which is what matters; listing them here keeps the sweep from
	// demanding a scopeExternal duplicate.
	// (none currently — kept as documentation of the intended shape.)
}

// sweepSkipDirs are directory names never walked (no Go we own, or vendored).
var sweepSkipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "web": true,
	"docs": true, "deploy": true, "scripts": true, "evals": true,
}

// sweepFinding is one collected key plus where it came from, for the failure
// message.
type sweepFinding struct {
	key  string
	file string
	// kind is how the read spelled the name: "key" (a verbatim os.Getenv on the
	// canonical name) or "suffix" (through agentcore's EnvPrefix, which also
	// honors the CHAT_/CUTLASS_ aliases). It must match the row's fleet flag —
	// see TestExternalKnobAliasFlagMatchesItsReader.
	kind string
}

// isParseCall reports whether call is one of the typed parsers.
func isParseCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return sweepParseFuncs[pkg.Name][sel.Sel.Name]
}

// envReadKind classifies an env-reading call: "" (not one), "key" (the literal
// argument is a verbatim env key), or "suffix" (a FLEET_-chain suffix).
func envReadKind(call *ast.CallExpr) string {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	if pkg, ok := sel.X.(*ast.Ident); ok {
		if want, found := sweepEnvReadFuncs[pkg.Name+"."+sel.Sel.Name]; found &&
			want.pkg == pkg.Name && want.sel == sel.Sel.Name {
			return "key"
		}
	}
	// agentcore's EnvPrefix.lookup(suffix) — matched on the method name, since
	// the receiver is a local variable.
	if sel.Sel.Name == "lookup" {
		return "suffix"
	}
	return ""
}

// canonicalizeKey maps a collected key/suffix onto the registry's canonical
// spelling: a suffix gains the FLEET_ prefix, and the legacy CHAT_/CUTLASS_
// prefixes fold onto their FLEET_ twin (lookupFleet reads all three).
func canonicalizeKey(raw, kind string) string {
	raw = strings.TrimSpace(raw)
	if kind == "suffix" {
		return canonicalPrefix + strings.TrimLeft(raw, "_")
	}
	for _, legacy := range []string{"CHAT_", "CUTLASS_"} {
		if suffix, ok := strings.CutPrefix(raw, legacy); ok {
			if k := envKnobByKey[canonicalPrefix+suffix]; k != nil && k.fleet {
				return canonicalPrefix + suffix
			}
		}
	}
	return raw
}

// envHelper describes a package-local env-parse helper: the index of its key
// parameter and whether that parameter is a verbatim key or a FLEET_ suffix.
type envHelper struct {
	argIdx int
	kind   string
}

// collectEnvHelpers finds the env-parse helpers declared in one file set (one
// package directory): a func whose body parses AND reads one of its own string
// params from the env.
func collectEnvHelpers(files []*ast.File) map[string]envHelper {
	helpers := map[string]envHelper{}
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			// Map each string parameter name to its positional index.
			params := map[string]int{}
			idx := 0
			for _, field := range fn.Type.Params.List {
				for _, name := range field.Names {
					if id, ok := field.Type.(*ast.Ident); ok && id.Name == "string" {
						params[name.Name] = idx
					}
					idx++
				}
			}
			if len(params) == 0 {
				continue
			}
			var parses bool
			var found *envHelper
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if isParseCall(call) {
					parses = true
				}
				kind := envReadKind(call)
				if kind == "" || len(call.Args) == 0 {
					return true
				}
				// The env read must be of one of this func's own string params.
				arg, ok := call.Args[0].(*ast.Ident)
				if !ok {
					return true
				}
				if i, ok := params[arg.Name]; ok {
					found = &envHelper{argIdx: i, kind: kind}
				}
				return true
			})
			if parses && found != nil {
				helpers[fn.Name.Name] = *found
			}
		}
	}
	return helpers
}

// collectStringConsts maps package-level string const/var names to their value,
// so a read spelled through a named suffix (agentcore's
// modelsCacheTTLEnvSuffix) resolves like a literal.
func collectStringConsts(files []*ast.File) map[string]string {
	out := map[string]string{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || (gen.Tok != token.CONST && gen.Tok != token.VAR) {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					if v, err := strconv.Unquote(lit.Value); err == nil {
						out[name.Name] = v
					}
				}
			}
		}
	}
	return out
}

// keyArg returns call's idx-th argument as a key when it is a string literal or
// a name collectStringConsts resolved.
func keyArg(call *ast.CallExpr, idx int, consts map[string]string) (string, bool) {
	if s, ok := stringLitArg(call, idx); ok {
		return s, true
	}
	if idx >= len(call.Args) {
		return "", false
	}
	if id, ok := call.Args[idx].(*ast.Ident); ok {
		if v, ok := consts[id.Name]; ok {
			return v, true
		}
	}
	return "", false
}

// collectPackageKnobKeys returns every typed env key the given package's
// non-test sources read ad hoc.
func collectPackageKnobKeys(files []*ast.File, path string) []sweepFinding {
	helpers := collectEnvHelpers(files)
	consts := collectStringConsts(files)
	var out []sweepFinding

	record := func(raw, kind string) {
		if raw == "" {
			return
		}
		out = append(out, sweepFinding{key: canonicalizeKey(raw, kind), file: path, kind: kind})
	}

	// Shape 2: every call site of a package-local env-parse helper, anywhere in
	// the package (including package-level var initializers, which is where
	// httpapi's SSE knobs live).
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var name string
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				name = fn.Name
			case *ast.SelectorExpr:
				name = fn.Sel.Name
			}
			if h, ok := helpers[name]; ok {
				if key, ok := keyArg(call, h.argIdx, consts); ok {
					record(key, h.kind)
				}
			}
			return true
		})
	}

	// Shape 1: an env read whose VALUE reaches a typed parser inside the same
	// function. Requiring the flow (rather than just co-occurrence in one
	// function) is what keeps a config builder that reads a dozen string vars
	// next to one duration from reporting all thirteen. Two flows count: the
	// env read nested directly in the parse call, and the common
	// `v := <read>; … parse(v)` binding — attributed over EVERY key bound to
	// that name in the function, since a name reused for two knobs (notify's
	// `v`) must not lose one of them.
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			bound := map[string][]sweepFinding{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				assign, ok := n.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for i, lhs := range assign.Lhs {
					id, ok := lhs.(*ast.Ident)
					if !ok || i >= len(assign.Rhs) {
						continue
					}
					bound[id.Name] = append(bound[id.Name], envReadsIn(assign.Rhs[i], consts, path)...)
				}
				return true
			})
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !isParseCall(call) {
					return true
				}
				for _, arg := range call.Args {
					out = append(out, envReadsIn(arg, consts, path)...)
					if id, ok := arg.(*ast.Ident); ok {
						out = append(out, bound[id.Name]...)
					}
				}
				return true
			})
		}
		// …and the same nested flow in a package-level var/const initializer,
		// which has no enclosing function to scan.
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			ast.Inspect(gen, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !isParseCall(call) {
					return true
				}
				for _, arg := range call.Args {
					out = append(out, envReadsIn(arg, consts, path)...)
				}
				return true
			})
		}
	}
	return out
}

// envReadsIn returns the env keys read by any env-reading call inside expr
// (`os.Getenv("X")`, `strings.TrimSpace(os.Getenv("X"))`, `p.lookup(suffix)`).
func envReadsIn(expr ast.Expr, consts map[string]string, path string) []sweepFinding {
	var out []sweepFinding
	ast.Inspect(expr, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		kind := envReadKind(call)
		if kind == "" {
			return true
		}
		if key, ok := keyArg(call, 0, consts); ok {
			out = append(out, sweepFinding{key: canonicalizeKey(key, kind), file: path, kind: kind})
		}
		return true
	})
	return out
}

// moduleRoot walks up from this package to the module root.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		t.Fatalf("module root %s has no go.mod: %v", dir, err)
	}
	return dir
}

func TestEnvKnobRegistryCoversAdHocReadsAcrossTheRepo(t *testing.T) {
	root := moduleRoot(t)

	// Group the non-test sources by directory: helper discovery is
	// package-scoped, so a helper declared in one file is seen by the call site
	// in its sibling.
	byDir := map[string][]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (sweepSkipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return fs.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		dir := filepath.Dir(path)
		byDir[dir] = append(byDir[dir], path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(byDir) < 20 {
		t.Fatalf("walked only %d package dirs from %s; the sweep is broken", len(byDir), root)
	}

	selfDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve package dir: %v", err)
	}

	fset := token.NewFileSet()
	var findings []sweepFinding
	for dir, paths := range byDir {
		// internal/config is the registry's own home: its reads are the loader's
		// and are covered bidirectionally by knobs_coverage_test.go. Sweeping it
		// here would just rediscover the registry's own parse implementation.
		if dir == selfDir {
			continue
		}
		var files []*ast.File
		for _, p := range paths {
			f, perr := parser.ParseFile(fset, p, nil, 0)
			if perr != nil {
				t.Fatalf("parse %s: %v", p, perr)
			}
			files = append(files, f)
		}
		rel, _ := filepath.Rel(root, dir)
		findings = append(findings, collectPackageKnobKeys(files, rel)...)
	}

	// Guard the extraction: the tree really does contain a couple of dozen of
	// these reads, so an empty result means the sweep stopped working (a helper
	// shape changed, the walk broke) and must fail loudly, not pass vacuously.
	if len(findings) < 15 {
		t.Fatalf("extracted only %d ad-hoc typed env reads across the repo; the sweep is broken", len(findings))
	}

	missing := map[string]string{}
	for _, f := range findings {
		if _, exempt := sweepExemptKeys[f.key]; exempt {
			continue
		}
		if envKnobByKey[f.key] != nil {
			continue
		}
		missing[f.key] = f.file
	}
	keys := make([]string, 0, len(missing))
	for k := range missing {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Errorf("%s is parsed ad hoc in %s but has no envKnobs entry — add a scopeExternal row in knobs.go (strict, or lenient WITH a rationale) so boot and `fleet validate-config` validate it; if it is not a typed knob of ours, add it to sweepExemptKeys with a reason (#1273)", k, missing[k])
	}

	// The registry's `fleet` flag must match HOW the consumer spells the read:
	// a row marked fleet resolves the CHAT_/CUTLASS_ aliases too, so marking it
	// on a knob read with a plain os.Getenv("FLEET_…") would refuse boot over an
	// alias spelling nothing ever reads — and NOT marking it on an EnvPrefix
	// read would leave the alias spelling unvalidated.
	for _, f := range findings {
		k := envKnobByKey[f.key]
		if k == nil || k.scope != scopeExternal {
			continue
		}
		switch {
		case f.kind == "suffix" && !k.fleet:
			t.Errorf("%s is read through the FLEET_/CHAT_/CUTLASS_ alias chain in %s but its envKnobs row is not marked fleet — the alias spellings would go unvalidated", f.key, f.file)
		case f.kind == "key" && k.fleet:
			t.Errorf("%s is read with a plain os.Getenv on the canonical name in %s but its envKnobs row is marked fleet — boot would refuse over a CHAT_/CUTLASS_ spelling nothing reads", f.key, f.file)
		}
	}
}
