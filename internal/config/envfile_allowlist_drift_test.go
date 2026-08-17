package config

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

// #1107: env vars the loader reads but the .env allowlist omits are silently
// dropped when set only via FLEET_ENV_FILE — a bug class that has recurred
// three times (the per-store DSNs, the boot-read operator keys, then ~20
// loader knobs). This test makes a fourth recurrence impossible: it parses
// the loader source (this package plus cmd/fleet, which reads a handful of
// keys around config.Load), mechanically extracts every literal env key read
// through os.Getenv/os.LookupEnv, the direct-key helpers, and the
// FLEET_-prefixed helper family, and asserts each is in allowedEnvVars.
//
// Suffix-based helpers are expanded exactly the way lookupFleet derives the
// spellings: only the canonical FLEET_<suffix> must be allowlisted (the
// CHAT_/CUTLASS_ legacy twins are optional back-compat); getenvFleetOrBare
// additionally reads the bare suffix, so that spelling is asserted too.

// driftDirectKeyFuncs read their first string argument as a verbatim env key.
// os.Getenv / os.LookupEnv are matched separately (selector on the os package).
var driftDirectKeyFuncs = map[string]bool{
	"getenvDefault":      true, // internal/config
	"getenvInt":          true,
	"getenvBool":         true,
	"getEnvOrDefault":    true,
	"getEnvOrDefaultInt": true,
	"envIntDefault":      true, // cmd/fleet
}

// driftFleetSuffixFuncs read a FLEET_-prefixed knob by suffix; the value is
// the index of the suffix argument.
var driftFleetSuffixFuncs = map[string]int{
	"lookupFleet":         0,
	"getenvFleet":         0,
	"getenvFleetDefault":  0,
	"getenvFleetInt":      0,
	"getenvFleetInt64":    0,
	"getenvFleetDuration": 0,
	"getenvFleetFloat":    0,
	"getenvFleetBool":     0,
	"reloadFleetFloat":    1,
	"reloadFleetInt":      1,
}

// driftExemptKeys are reads that legitimately cannot come from the env file.
var driftExemptKeys = map[string]string{
	"FLEET_ENV_FILE": "names the env file itself; a value inside the file could never take effect",
	"NOTIFY_SOCKET":  "set by systemd socket activation, never operator config",
}

// collectEnvReads parses every non-test .go file under dir and returns the
// env keys read via literal arguments. Non-literal arguments (the alias
// machinery's computed prefix+suffix, the bundle-manifest loop in cmd/fleet)
// are skipped by construction: their concrete names are either derived from
// literals collected elsewhere or admitted at runtime via
// RegisterAllowedEnvVars.
func collectEnvReads(t *testing.T, dir string) map[string]bool {
	t.Helper()
	keys := map[string]bool{}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", filepath.Join(dir, name), err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				// os.Getenv / os.LookupEnv
				if pkg, ok := fn.X.(*ast.Ident); ok && pkg.Name == "os" &&
					(fn.Sel.Name == "Getenv" || fn.Sel.Name == "LookupEnv") {
					if key, ok := stringLitArg(call, 0); ok {
						keys[key] = true
					}
				}
			case *ast.Ident:
				if driftDirectKeyFuncs[fn.Name] {
					if key, ok := stringLitArg(call, 0); ok {
						keys[key] = true
					}
				}
				if idx, ok := driftFleetSuffixFuncs[fn.Name]; ok {
					if suffix, ok := stringLitArg(call, idx); ok {
						// Mirror lookupFleet's normalization of the suffix.
						suffix = strings.TrimLeft(suffix, "_")
						keys[canonicalPrefix+suffix] = true
					}
				}
				if fn.Name == "getenvFleetOrBare" {
					if suffix, ok := stringLitArg(call, 0); ok {
						suffix = strings.TrimLeft(suffix, "_")
						keys[canonicalPrefix+suffix] = true
						keys[suffix] = true // the bare historical spelling is read too
					}
				}
			}
			return true
		})
	}
	return keys
}

// stringLitArg returns call's idx-th argument when it is a string literal.
func stringLitArg(call *ast.CallExpr, idx int) (string, bool) {
	if idx >= len(call.Args) {
		return "", false
	}
	lit, ok := call.Args[idx].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

func TestEnvFileAllowlistCoversLoaderReads(t *testing.T) {
	keys := map[string]bool{}
	for _, dir := range []string{".", filepath.Join("..", "..", "cmd", "fleet")} {
		for k := range collectEnvReads(t, dir) {
			keys[k] = true
		}
	}

	// Guard the extraction itself: if a refactor renames the helper family or
	// moves the loader, an empty (or implausibly small) result must fail loudly
	// rather than pass vacuously. The loader reads well over 100 distinct keys.
	if len(keys) < 80 {
		t.Fatalf("extracted only %d env keys from the loader source; the drift test's extraction is broken", len(keys))
	}

	var missing []string
	for k := range keys {
		if _, exempt := driftExemptKeys[k]; exempt {
			continue
		}
		if !allowedEnvVars[k] {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	for _, k := range missing {
		t.Errorf("%s is read by the loader but missing from allowedEnvVars — a value set only in FLEET_ENV_FILE is silently dropped (#1107); add it to the allowlist in config.go", k)
	}
}
