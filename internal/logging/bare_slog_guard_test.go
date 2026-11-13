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

// Reasons a process-scope function may log through package-level slog: no
// request, run or conversation exists where it runs, so there is no
// correlation to carry.
const (
	reasonBoot     = "process boot: runs once in fleet serve's startup sequence, before any request or run"
	reasonConfig   = "configuration load or validation: runs at boot (and the hot-reload signal), before or outside any request or run"
	reasonSignal   = "signal handling and shutdown: a SIGHUP/SIGTERM-driven process transition, outside any request or run"
	reasonEnvKnob  = "reads a process-wide knob from the environment at construction time"
	reasonCLI      = "a separate harness binary's main (no request context exists)"
	reasonProcess  = "process-wide background work (shared cache refresh, warm pool, listener) not tied to any one request or run"
	reasonInternal = "marshals a Go value fleet built itself: a failure is a programming error, not request-specific"
)

// bareSlogAllowed lists the functions that may log through package-level
// slog, as "<dir>.<Func>" (methods as "<dir>.<Type>.<Method>"), each with
// its reason. Everything else under internal/ and cmd/ logs through
// logging.From(ctx) with the context of the request or run it serves.
var bareSlogAllowed = map[string]string{
	"cmd/fake-llm.main":                                 reasonCLI,
	"cmd/web-e2e-server.main":                           reasonCLI,
	"cmd/fleet.run":                                     reasonBoot,
	"cmd/fleet.serveChat":                               reasonBoot,
	"cmd/fleet.serveInBackground":                       reasonBoot,
	"cmd/fleet.buildWebServer":                          reasonBoot,
	"cmd/fleet.buildTaskNotifier":                       reasonBoot,
	"cmd/fleet.emailReplierFor":                         reasonBoot,
	"cmd/fleet.configureGuardrail":                      reasonBoot,
	"cmd/fleet.configurePIIRedaction":                   reasonBoot,
	"cmd/fleet.initTracing":                             reasonBoot,
	"cmd/fleet.installStoreCipher":                      reasonBoot,
	"cmd/fleet.logCentralAuth":                          reasonBoot,
	"cmd/fleet.logDiskGuard":                            reasonBoot,
	"cmd/fleet.resolveSandboxInto":                      reasonBoot,
	"cmd/fleet.runMCPBroker":                            reasonBoot,
	"cmd/fleet.seedBootstrapAdmins":                     reasonBoot,
	"cmd/fleet.setupRemoteMCP":                          reasonBoot,
	"cmd/fleet.startRedirectServer":                     reasonBoot,
	"cmd/fleet.toAgentcoreProviders":                    reasonBoot,
	"cmd/fleet.warnIfNoAdminKey":                        reasonBoot,
	"cmd/fleet.warnUncategorizedConnectors":             reasonBoot,
	"cmd/fleet.warnUndeclaredContextWindow":             reasonBoot,
	"cmd/fleet.wireA2A":                                 reasonBoot,
	"cmd/fleet.appendNotifySettingsOption":              reasonBoot,
	"cmd/fleet.appendWorkspaceSettingsOption":           reasonBoot,
	"cmd/fleet.awaitShutdown":                           reasonSignal,
	"cmd/fleet.performShutdown":                         reasonSignal,
	"cmd/fleet.logApprovalDrain":                        reasonSignal,
	"cmd/fleet.logChatDrain":                            reasonSignal,
	"cmd/fleet.logMCPReload":                            reasonSignal,
	"internal/agent.RegisterA2APeers":                   reasonBoot,
	"internal/agent.RegisterHTTPTools":                  reasonBoot,
	"internal/agent.StageSkillsForBackend":              reasonBoot,
	"internal/agent.absSupportingDocs":                  reasonBoot,
	"internal/agent.buildKubernetesSandboxPool":         reasonBoot,
	"internal/agent.buildSandboxPool":                   reasonBoot,
	"internal/agent.sandboxReadOnlyMounts":              reasonBoot,
	"internal/agent.warnPersistentMemoryBudget":         reasonBoot,
	"internal/agentcore.ConfigureAgentPolicy":           reasonConfig,
	"internal/agentcore.closeEmailLastOverEquivalents":  reasonConfig,
	"internal/agentcore.installInteractiveHiddenList":   reasonConfig,
	"internal/agentcore.installNarrowableArgs":          reasonConfig,
	"internal/agentcore.installResultCardSet":           reasonConfig,
	"internal/agentcore.installStopSet":                 reasonConfig,
	"internal/agentcore.installUnattendedDisabledSet":   reasonConfig,
	"internal/agentcore.modelsCache.ensureFresh":        reasonProcess,
	"internal/agentcore.toolInputSchemaProblem":         "compiles the JSON Schema metaschema once per process (sync.Once); the failure disables the check process-wide",
	"internal/clientconfig.Bundle.validate":             reasonConfig,
	"internal/clientconfig.Load":                        reasonConfig,
	"internal/clientconfig.mergeRemoteCatalog":          reasonConfig,
	"internal/clientconfig.warnRetiredManifestRefs":     reasonConfig,
	"internal/clientconfig.warnUserTokenAssembled":      reasonConfig,
	"internal/clientconfig.warnUserTokenFromEnv":        reasonConfig,
	"internal/config.Config.PinWorkspaceRoot":           reasonConfig,
	"internal/config.Config.applyReloadableLocked":      reasonConfig,
	"internal/config.Load":                              reasonConfig,
	"internal/config.decodeAES256KeyEnv":                reasonConfig,
	"internal/config.normalizePythonREPLMode":           reasonConfig,
	"internal/config.warnRetiredEnvAliases":             reasonConfig,
	"internal/config/envreg.unregistered":               "the configuration registry's own read of an undeclared variable: a programming-error report about the process, not a request",
	"internal/httpapi.New":                              reasonBoot,
	"internal/httpapi.envDuration":                      reasonEnvKnob,
	"internal/httpapi.envInt":                           reasonEnvKnob,
	"internal/httpapi.logIPFilter":                      reasonBoot,
	"internal/observability.Init":                       reasonBoot,
	"internal/runner.maxConcurrentFromEnv":              reasonEnvKnob,
	"internal/runner.wallTimeoutFromEnv":                reasonEnvKnob,
	"internal/sandbox.EgressProxy.Start":                reasonProcess,
	"internal/sandbox.Pool.Close":                       reasonSignal,
	"internal/sandbox.Pool.fill":                        reasonProcess,
	"internal/sched/db.marshalJSON":                     reasonInternal,
	"internal/sched/handlers.Handlers.CleanupTempFiles": "the periodic temp-upload sweep, a process-wide maintenance job",
	"internal/sched/scheduler.New":                      reasonBoot,
	"internal/sched/storage.Storage.SetTimezone":        reasonBoot,
	"internal/settings.NewService":                      reasonBoot,
}

// TestNoBareSlogOutsideProcessScope closes the "bare slog call that should
// carry the run context" class (#2074 review, three findings): every
// package-level slog.Debug/Info/Warn/Error/Log (and slog.Default().X) call in
// non-test code under internal/ and cmd/ fails, unless its function is a
// reviewed process-scope location in bareSlogAllowed. A call that serves a
// request or a run logs through logging.From(ctx), so the line carries its
// request_id, conv_id, turn_id or run_id. An allowlist entry that no longer
// matches a call fails too, so the list cannot rot.
func TestNoBareSlogOutsideProcessScope(t *testing.T) {
	root := filepath.Join("..", "..")
	seen := map[string]bool{}
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
			if strings.HasPrefix(rel, "internal/logging/") {
				return nil // the package that implements From and the handlers
			}
			for _, site := range bareSlogCalls(t, path) {
				key := filepath.ToSlash(filepath.Dir(rel)) + "." + site.fn
				if _, ok := bareSlogAllowed[key]; ok {
					seen[key] = true
					continue
				}
				bad = append(bad, rel+":"+strconv.Itoa(site.line)+" "+key)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for key := range bareSlogAllowed {
		if !seen[key] {
			bad = append(bad, "stale bareSlogAllowed entry (no bare slog call there any more): "+key)
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("package-level slog calls outside a process-scope location — log through logging.From(ctx) with the request's or run's context (thread ctx in), or add a reviewed bareSlogAllowed entry with its reason:\n  %s", strings.Join(bad, "\n  "))
	}
}

type bareSlogSite struct {
	fn   string
	line int
}

var bareSlogMethods = map[string]bool{
	"Debug": true, "Info": true, "Warn": true, "Error": true, "Log": true, "LogAttrs": true,
	"DebugContext": true, "InfoContext": true, "WarnContext": true, "ErrorContext": true,
}

// bareSlogCalls returns each package-level slog log call in path with its
// enclosing top-level function ("Func" or "Type.Method").
func bareSlogCalls(t *testing.T, path string) []bareSlogSite {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	slogName := importNames(f)["log/slog"]
	if slogName == "" {
		return nil
	}
	var out []bareSlogSite
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Body == nil {
			continue
		}
		name := fd.Name.Name
		if rt := receiverType(fd); rt != "" {
			name = rt + "." + name
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			se, ok := c.Fun.(*ast.SelectorExpr)
			if !ok || !bareSlogMethods[se.Sel.Name] {
				return true
			}
			switch x := se.X.(type) {
			case *ast.Ident: // slog.Info(...)
				if x.Name == slogName && x.Obj == nil {
					out = append(out, bareSlogSite{fn: name, line: fset.Position(c.Pos()).Line})
				}
			case *ast.CallExpr: // slog.Default().Info(...)
				if s2, ok := x.Fun.(*ast.SelectorExpr); ok && s2.Sel.Name == "Default" {
					if id, ok := s2.X.(*ast.Ident); ok && id.Name == slogName && id.Obj == nil {
						out = append(out, bareSlogSite{fn: name, line: fset.Position(c.Pos()).Line})
					}
				}
			}
			return true
		})
	}
	return out
}
