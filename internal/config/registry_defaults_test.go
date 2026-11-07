package config

import (
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/config/envreg"
)

// defaultHelpers are the env parse helpers that take a hard-coded fallback:
// helper name → (index of the key argument, index of the default argument,
// whether the key is a FLEET_ suffix). A default index of -1 means the
// helper's default is implicit (implicitDefault).
var defaultHelpers = map[string]struct {
	keyArg, defArg int
	suffix         bool
}{
	// internal/config — Load's typed readers and string helpers.
	"getenvInt": {0, 1, false}, "getenvBool": {0, 1, false}, "getenvDefault": {0, 1, false},
	"getenvFleetInt": {0, 1, true}, "getenvFleetInt64": {0, 1, true}, "getenvFleetFloat": {0, 1, true},
	"getenvFleetBool": {0, 1, true}, "getenvFleetDuration": {0, 1, true},
	"getenvFleetDefault": {0, 1, true}, "getenvFleetOrBare": {0, 1, true},
	// The exported point-of-use readers (cmd/fleet, admincli).
	"EnvKnobInt": {0, 1, false}, "EnvKnobBool": {0, 1, false},
	// internal/httpapi.
	"envDuration": {0, 1, false}, "envInt": {0, 1, false},
	// internal/agentcore's FLEET_-chain reader.
	"lookupFloatDefault": {0, 1, true}, "lookupBoolDefault": {0, 1, true},
	// harness binaries.
	"envOr": {0, 1, false},
	// internal/webpush: opt-out flags, default true.
	"envBoolDefaultTrue": {0, -1, false},
}

var implicitDefault = map[string]constant.Value{"envBoolDefaultTrue": constant.MakeBool(true)}

// pkgConsts collects every package-level constant expression of the Go
// package in dir (non-test files).
func pkgConsts(t *testing.T, dir string) map[string]ast.Expr {
	t.Helper()
	out := map[string]ast.Expr{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, e.Name()), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, s := range gd.Specs {
				vs, ok := s.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, n := range vs.Names {
					if i < len(vs.Values) {
						out[n.Name] = vs.Values[i]
					}
				}
			}
		}
	}
	return out
}

var timeUnits = map[string]time.Duration{
	"Nanosecond": time.Nanosecond, "Microsecond": time.Microsecond, "Millisecond": time.Millisecond,
	"Second": time.Second, "Minute": time.Minute, "Hour": time.Hour,
}

// evalConst evaluates a default expression made of literals, same-package
// constants, time units, arithmetic and conversions. ok is false for anything
// else (a call, another package's constant), which the test skips.
func evalConst(e ast.Expr, consts map[string]ast.Expr, depth int) (constant.Value, bool) {
	if depth > 10 {
		return nil, false
	}
	switch x := e.(type) {
	case *ast.BasicLit:
		v := constant.MakeFromLiteral(x.Value, x.Kind, 0)
		return v, v.Kind() != constant.Unknown
	case *ast.ParenExpr:
		return evalConst(x.X, consts, depth+1)
	case *ast.Ident:
		switch x.Name {
		case "true":
			return constant.MakeBool(true), true
		case "false":
			return constant.MakeBool(false), true
		}
		if c, ok := consts[x.Name]; ok {
			return evalConst(c, consts, depth+1)
		}
	case *ast.SelectorExpr:
		if pkg, ok := x.X.(*ast.Ident); ok && pkg.Name == "time" {
			if d, ok := timeUnits[x.Sel.Name]; ok {
				return constant.MakeInt64(int64(d)), true
			}
		}
	case *ast.UnaryExpr:
		if v, ok := evalConst(x.X, consts, depth+1); ok {
			return constant.UnaryOp(x.Op, v, 0), true
		}
	case *ast.BinaryExpr:
		l, okL := evalConst(x.X, consts, depth+1)
		r, okR := evalConst(x.Y, consts, depth+1)
		if !okL || !okR {
			return nil, false
		}
		if x.Op == token.SHL || x.Op == token.SHR {
			s, ok := constant.Uint64Val(r)
			if !ok {
				return nil, false
			}
			return constant.Shift(l, x.Op, uint(s)), true
		}
		return constant.BinaryOp(l, x.Op, r), true
	case *ast.CallExpr: // a conversion: float64(x), int64(x), time.Duration(x)
		if len(x.Args) == 1 {
			switch fn := x.Fun.(type) {
			case *ast.Ident:
				if fn.Name == "float64" || fn.Name == "int" || fn.Name == "int64" {
					return evalConst(x.Args[0], consts, depth+1)
				}
			case *ast.SelectorExpr:
				if pkg, ok := fn.X.(*ast.Ident); ok && pkg.Name == "time" && fn.Sel.Name == "Duration" {
					return evalConst(x.Args[0], consts, depth+1)
				}
			}
		}
	}
	return nil, false
}

// defaultMatches compares a code fallback with the registry's documented
// Default for entry e.
func defaultMatches(e envreg.Entry, code constant.Value) bool {
	d := e.Default
	isZero := false
	switch code.Kind() {
	case constant.String:
		s := constant.StringVal(code)
		return d == s
	case constant.Bool:
		if d == "" {
			return !constant.BoolVal(code)
		}
		b, err := strconv.ParseBool(d)
		return err == nil && b == constant.BoolVal(code)
	case constant.Int, constant.Float:
		f, _ := constant.Float64Val(constant.ToFloat(code))
		isZero = f == 0
		if d == "" {
			return isZero
		}
		if e.Type == envreg.TypeDuration {
			dur, err := time.ParseDuration(d)
			return err == nil && float64(dur) == f
		}
		want, err := strconv.ParseFloat(d, 64)
		return err == nil && want == f
	default: // Unknown, Complex: not a fallback a setting can have
		return false
	}
}

// TestRegistryDefaultsMatchCodeFallbacks — Codex round 2 (#2057): every
// registry entry read through a parse helper with a hard-coded fallback must
// document that fallback as its Default (FLEET_UPDATE_WAIT_SECONDS had none).
// Prose defaults ("(derived from …)") are skipped, as are fallbacks that are
// not constant (another setting, another package's constant).
func TestRegistryDefaultsMatchCodeFallbacks(t *testing.T) {
	root := filepath.Join("..", "..")
	var problems []string
	checked := 0
	constsByDir := map[string]map[string]ast.Expr{}
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
		dir := filepath.Dir(path)
		if constsByDir[dir] == nil {
			constsByDir[dir] = pkgConsts(t, dir)
		}
		consts := constsByDir[dir]
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
			h, ok := defaultHelpers[name]
			if !ok || h.keyArg >= len(call.Args) {
				return true
			}
			kv, ok := evalConst(call.Args[h.keyArg], consts, 0)
			if !ok || kv.Kind() != constant.String {
				return true
			}
			key := constant.StringVal(kv)
			if h.suffix {
				key = "FLEET_" + strings.TrimLeft(key, "_")
			}
			e, ok := envreg.Find(key)
			// An inherited default (DefaultFrom) is checked against the readers by
			// TestInheritedDefaultsMatchTheReaders; Load may hold a sentinel there.
			if !ok || strings.HasPrefix(e.Default, "(") || e.DefaultFrom != "" {
				return true
			}
			var code constant.Value
			if h.defArg < 0 {
				code = implicitDefault[name]
			} else if h.defArg < len(call.Args) {
				if code, ok = evalConst(call.Args[h.defArg], consts, 0); !ok {
					return true
				}
			}
			if code == nil {
				return true
			}
			checked++
			if !defaultMatches(e, code) {
				problems = append(problems, path+": "+e.Name+" falls back to "+code.ExactString()+" but the registry documents Default "+strconv.Quote(e.Default))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 80 {
		t.Fatalf("checked only %d fallbacks; the walk is broken", checked)
	}
	sort.Strings(problems)
	for _, p := range problems {
		t.Error(p)
	}
}

// Fallbacks parsed by hand rather than through a helper above, pinned
// against the registry here (each read site named).
func TestRegistryDefaultsOfHandParsedSettings(t *testing.T) {
	for key, want := range map[string]string{
		"FLEET_UPDATE_WAIT_SECONDS":                      "600",            // internal/admincli/service.go parseConfiguredWaitTimeout: 10 * time.Minute
		"FLEET_TASK_WALL_TIMEOUT":                        "4h",             // internal/runner DefaultTaskWallClockTimeout
		"FLEET_MAX_CONCURRENT_AGENTS":                    "8",              // internal/runner DefaultMaxConcurrentAgents
		"FLEET_NOTIFY_TIMEOUT":                           "10s",            // internal/notify defaultTimeout
		"FLEET_NOTIFY_RETRIES":                           "2",              // internal/notify defaultRetries
		"FLEET_SANDBOX_STATS_INTERVAL_SECONDS":           "10",             // internal/sandbox defaultStatsInterval
		"FLEET_SANDBOX_KATA_OVERHEAD_MB":                 "512",            // internal/sandbox DefaultKataOverheadMB
		"FLEET_MAX_TOOL_OUTPUT_BYTES":                    "65536",          // internal/agentcore defaultMaxToolOutputBytes
		"FLEET_MODEL_CACHE_TTL_MINUTES":                  "60",             // internal/agentcore defaultModelsCacheTTLMinutes
		"FLEET_RETRY_MAX_ATTEMPTS":                       "5",              // internal/agentcore defaultRetryMaxAttempts
		"FLEET_PROVIDER_FIRST_CHUNK_TIMEOUT_SECONDS":     "75",             // internal/agentcore defaultFirstChunkTimeout
		"FLEET_PROVIDER_FIRST_CHUNK_TIMEOUT_MAX_SECONDS": "180",            // internal/agentcore defaultFirstChunkTimeoutMax
		"FLEET_WORKSPACE_DOWNLOAD_MAX_BYTES":             "52428800",       // internal/sched/handlers defaultWorkspaceDownloadMaxBytes
		"FLEET_SCHED_RATE_LIMIT_PER_MINUTE":              "60",             // cmd/fleet schedRateLimits
		"FLEET_SCHED_RATE_LIMIT_PER_DAY":                 "500",            // cmd/fleet schedRateLimits
		"FLEET_SCHED_RATE_LIMIT_GLOBAL_PER_MINUTE":       "200",            // cmd/fleet schedRateLimits
		"FLEET_WEB_ADDR":                                 "127.0.0.1:3000", // cmd/fleet defaultWebAddr
		"FLEET_ORCHESTRATOR_ADDR":                        "127.0.0.1:8000", // cmd/fleet orchestratorAddr
		"FLEET_TIMEZONE":                                 "UTC",            // cmd/fleet timezone
		"FLEET_DEFAULT_TIMEZONE":                         "UTC",            // cmd/fleet defaultTaskTimezone
		"FLEET_OIDC_SCOPES":                              "openid email",   // internal/webui oidcFromEnv
		"FLEET_SMTP_PORT":                                "587",            // internal/notify Load
		"FLEET_SERVICE_NAME":                             "fleet",          // internal/admincli serviceName
	} {
		e, ok := envreg.Find(key)
		if !ok || e.Default != want {
			t.Errorf("%s: registry Default %q, the code falls back to %q", key, e.Default, want)
		}
	}
}
