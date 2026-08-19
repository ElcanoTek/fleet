package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// #1119: the envKnobs registry (knobs.go) and the loader must not drift. The
// loadParser already fails every Load when a call names a knob the registry
// lacks (a runtime guarantee any Load-based test trips over), and this test
// closes the loop from SOURCE in both directions: every typed getenv*/reload*
// call in the package must have a registry entry of the SAME kind, and every
// registry entry must be consumed by such a call (no dead table rows).
//
// The extraction mirrors envfile_allowlist_drift_test.go: parse the non-test
// package sources and collect the typed-reader calls by name — plain calls
// (reloadFleetInt/Float) and loadParser method calls (lp.getenvFleetInt, …).

// knobDirectFuncs read their first string argument as a verbatim env key.
var knobDirectFuncs = map[string]knobKind{
	"getenvInt":  kindInt,
	"getenvBool": kindBool,
}

// knobFleetFuncs read a FLEET_-chain knob; the value is the kind. The suffix
// is always argument 0 for the loadParser methods and argument 1 for the
// reload helpers.
var knobFleetFuncs = map[string]struct {
	kind      knobKind
	suffixArg int
}{
	"getenvFleetInt":      {kindInt, 0},
	"getenvFleetInt64":    {kindInt64, 0},
	"getenvFleetFloat":    {kindFloat, 0},
	"getenvFleetBool":     {kindBool, 0},
	"getenvFleetDuration": {kindDuration, 0},
	"reloadFleetInt":      {kindInt, 1},
	"reloadFleetFloat":    {kindFloat, 1},
}

// collectKnobReads parses every non-test .go file in this package and returns
// canonical key → kind for each typed knob read with a literal key/suffix.
func collectKnobReads(t *testing.T) map[string]knobKind {
	t.Helper()
	reads := map[string]knobKind{}

	record := func(key string, kind knobKind) {
		if prev, ok := reads[key]; ok && prev != kind {
			t.Errorf("%s is read as both %s and %s — one knob must have one kind", key, prev.label(), kind.label())
		}
		reads[key] = kind
	}
	handle := func(name string, call *ast.CallExpr) {
		if kind, ok := knobDirectFuncs[name]; ok {
			if key, ok := knobStringLitArg(call, 0); ok {
				record(key, kind)
			}
		}
		if fn, ok := knobFleetFuncs[name]; ok {
			if suffix, ok := knobStringLitArg(call, fn.suffixArg); ok {
				record(canonicalPrefix+strings.TrimLeft(suffix, "_"), fn.kind)
			}
		}
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				handle(fn.Sel.Name, call)
			case *ast.Ident:
				handle(fn.Name, call)
			}
			return true
		})
	}
	return reads
}

// knobStringLitArg returns call's idx-th argument when it is a string literal.
func knobStringLitArg(call *ast.CallExpr, idx int) (string, bool) {
	if idx >= len(call.Args) {
		return "", false
	}
	lit, ok := call.Args[idx].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s := lit.Value
	if len(s) < 2 {
		return "", false
	}
	return s[1 : len(s)-1], true
}

func TestEnvKnobRegistryCoversLoaderReads(t *testing.T) {
	reads := collectKnobReads(t)

	// Guard the extraction itself: the loader consumes ~70 typed knobs; an
	// implausibly small result means the extraction broke (a helper rename,
	// say) and must fail loudly rather than pass vacuously.
	if len(reads) < 60 {
		t.Fatalf("extracted only %d typed knob reads; the coverage test's extraction is broken", len(reads))
	}

	var missing, mismatched []string
	for key, kind := range reads {
		k := envKnobByKey[key]
		switch {
		case k == nil:
			missing = append(missing, key)
		case k.kind != kind:
			mismatched = append(mismatched, key+" (registry "+k.kind.label()+", loader "+kind.label()+")")
		}
	}
	sort.Strings(missing)
	sort.Strings(mismatched)
	for _, key := range missing {
		t.Errorf("%s is read by the loader but has no envKnobs entry — add it in knobs.go so boot, hot-reload, and `fleet validate-config` all validate it (#1119)", key)
	}
	for _, m := range mismatched {
		t.Errorf("kind mismatch for %s — fix the envKnobs entry in knobs.go", m)
	}

	// The reverse direction: a registry row nothing reads is dead weight that
	// would make validate-config promise a check the loader never performs.
	var dead []string
	for i := range envKnobs {
		if _, ok := reads[envKnobs[i].key]; !ok {
			dead = append(dead, envKnobs[i].key)
		}
	}
	sort.Strings(dead)
	for _, key := range dead {
		t.Errorf("envKnobs entry %s is not consumed by any loader/reload read — remove it or wire the read through loadParser", key)
	}
}
