package main

// validate-config is `fleet validate-config`: a preflight verb (#248) that runs
// the boot-path checks that today only surface as cryptic runtime errors minutes
// after `systemctl start fleet` — a missing MCP executable, an unset credential
// gate, a wrong DATABASE_URL, podman absent. It reuses the SAME loaders the
// server boots through (clientconfig.Load, config.Load + cfg.Validate, the
// chatDSN/schedDSN/ensureDistinctDatabases logic) rather than reinventing them,
// so a green run here means the real boot path will get past these gates.
//
// It is a read-only diagnostic: it never starts the servers, never runs
// migrations, and — load-bearing invariant — never logs or prints a credential
// VALUE. Credential checks report only the env-var NAME and whether it is set.
//
// Exit code: 0 when every BLOCKING check passed, 1 otherwise. Warnings never
// change the exit code (a disabled/optional connector failing should not block a
// CI gate or a startup), matching issue #248's blocking-vs-warning split.

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver for the probe

	"github.com/ElcanoTek/fleet/internal/admincli"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/creds"
	"github.com/ElcanoTek/fleet/internal/sandbox"
)

// validateOptions are the parsed `fleet validate-config` flags.
type validateOptions struct {
	bundlePath        string
	skipNetworkChecks bool
	jsonOutput        bool
}

// checkStatus is one check's outcome. "ok" passed, "warn" failed but does not
// affect the exit code, "fail" is a blocking failure (exit 1).
type checkStatus string

const (
	statusOK   checkStatus = "ok"
	statusWarn checkStatus = "warn"
	statusFail checkStatus = "fail"
)

// checkResult is one preflight check's machine- and human-readable result. The
// JSON tags match issue #248's --json contract.
type checkResult struct {
	Name     string      `json:"name"`
	Status   checkStatus `json:"status"`
	Blocking bool        `json:"blocking"`
	Detail   string      `json:"detail,omitempty"`
}

// failed reports whether this result is a blocking failure (the only kind that
// changes the exit code).
func (r checkResult) failed() bool { return r.Status == statusFail && r.Blocking }

// validateReport is the top-level --json envelope.
type validateReport struct {
	Checks           []checkResult `json:"checks"`
	Passed           bool          `json:"passed"`
	BlockingFailures int           `json:"blocking_failures"`
}

// runValidateConfig is the `fleet validate-config` entry point. It parses flags,
// loads the bundle + config through the SAME loaders the server boots through,
// runs every check, prints the report, and returns the process exit code (0 when
// all blocking checks passed, 1 otherwise).
func runValidateConfig(args []string) int {
	opts, err := parseValidateFlags(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	// --bundle-path overrides FLEET_CLIENT_CONFIG_DIR for this run, exactly as a
	// boot would resolve it (clientconfig.Dir reads the env var).
	if strings.TrimSpace(opts.bundlePath) != "" {
		_ = os.Setenv(clientconfig.EnvDir, opts.bundlePath)
	}

	results := runChecks(context.Background(), opts)
	return emitReport(os.Stdout, results, opts.jsonOutput)
}

// parseValidateFlags parses the verb's flags. Defaults mirror the issue:
// --bundle-path defaults to the resolved bundle dir (FLEET_CLIENT_CONFIG_DIR or
// config/default), network checks are ON unless --skip-network-checks.
func parseValidateFlags(args []string) (validateOptions, error) {
	var opts validateOptions
	fs := newValidateFlagSet(&opts)
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	return opts, nil
}

// newValidateFlagSet is the verb's real flag surface, split from the parse so
// the top-level usage text (admincli.UsageText) can be checked against it —
// the usage once advertised two flags this set never defined.
func newValidateFlagSet(opts *validateOptions) *flag.FlagSet {
	fs := flag.NewFlagSet("validate-config", flag.ContinueOnError)
	fs.StringVar(&opts.bundlePath, "bundle-path", "", "client-config bundle dir (overrides FLEET_CLIENT_CONFIG_DIR; default config/default)")
	fs.BoolVar(&opts.skipNetworkChecks, "skip-network-checks", false, "skip the DB live probe, MCP HTTP ping, and OpenRouter key check (CI with no outbound access)")
	fs.BoolVar(&opts.jsonOutput, "json", false, "emit the report as JSON for CI parsing")
	return fs
}

// preflightEnvFile resolves the env file the in-binary preflight verbs
// (validate-config, eval, mcp test) load, and pins FLEET_ENV_FILE to it in the
// process env when the shell left it unset. The resolution is
// config.ResolveEnvFile's — $FLEET_ENV_FILE, else /etc/fleet/fleet.env on a
// provisioned box, else .env.local — the same order the operator CLI's
// credential writers use, so the file `fleet config set-openrouter-key` just
// wrote is the file `fleet validate-config` then checks. These verbs used to
// pass a bare os.Getenv("FLEET_ENV_FILE") to config.Load: on a provisioned box
// the unit UnsetEnvironment=s that variable, so the empty path loaded nothing
// and a healthy deployment preflighted as "OPENROUTER_API_KEY missing".
//
// Setting the variable (rather than only returning the path) matters because
// clientconfig.Load folds the SAME file into the process env for manifest
// interpolation, and reads the path from FLEET_ENV_FILE only (#1123) — without
// the pin, a ${CONNECTOR_KEY} in the manifest would still resolve against an
// empty env. It is the same in-process override these verbs already apply for
// --bundle-path (clientconfig.EnvDir).
//
// FLEET_CLIENT_CONFIG_DIR is pinned for exactly the same reason, and it is the
// same bug wearing a different variable name: the bundle dir also lives only in
// the deployment env file, clientconfig.Dir() reads it only from the process
// env, and the unit's UnsetEnvironment= keeps it out of an operator's shell. So
// on a provisioned box the bundle fell back to the RELATIVE default
// ("config/default", resolved against the caller's cwd) and a healthy
// deployment preflighted as "bundle load failed: stat /root/config/default: no
// such file or directory" — which then failed the manifest check, degraded
// mcp_servers and credentials to skipped, and failed the sandbox check too,
// because FLEET_SANDBOX_IMAGE arrives as a manifest default.
func preflightEnvFile() string {
	path := config.ResolveEnvFile("")
	if strings.TrimSpace(os.Getenv("FLEET_ENV_FILE")) == "" {
		_ = os.Setenv("FLEET_ENV_FILE", path)
	}
	pinBundleDirFromEnvFile(path)
	return path
}

// pinBundleDirFromEnvFile folds the env file's FLEET_CLIENT_CONFIG_DIR into the
// process env so clientconfig.Dir() can see it. Precedence is deliberate and
// matches config.Load's: an explicit --bundle-path (which every preflight verb
// pins into clientconfig.EnvDir BEFORE calling here) wins, then anything already
// in the process env, then the file, then clientconfig's own default. Only an
// unset variable is filled in, so this can never override an operator's choice.
//
// Every failure mode is a silent no-op on purpose: a missing file, an
// unreadable one (0600 and not ours), or a file with no such key all leave the
// caller exactly where it was, which is the pre-existing behaviour. Reporting
// them here would turn a diagnostic that is supposed to name the REAL problem
// into one that complains about its own bootstrap.
func pinBundleDirFromEnvFile(path string) {
	if strings.TrimSpace(os.Getenv(clientconfig.EnvDir)) != "" {
		return
	}
	values, err := creds.ReadEnvValues(path, clientconfig.EnvDir)
	if err != nil {
		return
	}
	if dir := strings.TrimSpace(values[clientconfig.EnvDir]); dir != "" {
		_ = os.Setenv(clientconfig.EnvDir, dir)
	}
}

// runChecks loads the bundle + config and runs every preflight check in the
// fixed order the issue lists. A bundle/config load failure is itself reported as
// a blocking failure of the relevant check (and dependent checks degrade to a
// blocking failure too, since they cannot run without it).
func runChecks(ctx context.Context, opts validateOptions) []checkResult {
	results := make([]checkResult, 0, 10)

	envFile := preflightEnvFile()
	bundle, bundleErr := clientconfig.Load(clientconfig.Dir())

	// config.Load reads the env file and the process env. Register the bundle's
	// connector env-var names first — the SAME ordering the server boot uses —
	// so a .env-supplied credential survives the allowlist and the credential
	// check below sees it.
	if bundleErr == nil {
		config.RegisterAllowedEnvVars(bundle.EnvVarNames()...)
	}
	cfg, cfgErr := config.Load(envFile)
	if cfgErr == nil && bundleErr == nil {
		cfg.MCPServers = bundle.MCPServerConfigs()
		cfg.HTTPTools = bundle.HTTPToolConfigs()
		cfg.A2APeers = bundle.A2APeerConfigs()
	}

	results = append(results, checkEnvVars(cfg, cfgErr))
	results = append(results, checkManifest(bundle, bundleErr, cfg))
	results = append(results, checkMCPServers(ctx, bundle, cfg, opts))
	results = append(results, checkMCPCatalog(bundle, bundleErr))
	results = append(results, checkManifestFiles(bundle, bundleErr))
	results = append(results, checkBundleSkills(bundle, bundleErr))
	results = append(results, checkAgentPolicy(bundle, bundleErr))
	results = append(results, checkDatabase(ctx, cfg, cfgErr, opts))
	results = append(results, checkCredentials(bundle, bundleErr))
	results = append(results, checkSandbox(ctx, cfg, bundle))
	results = append(results, checkModelAPI(ctx, cfg, cfgErr, opts))

	return results
}

// ── 1. env vars (blocking) ──

// checkEnvVars reuses cfg.Validate (the SAME required-field gate the server boots
// through: OPENROUTER_API_KEY unless MockMode, FLEET_SERVER_TOKEN, the
// conversation caps, DATABASE_URL, TLS) plus config.ValidateEnvKnobs — the
// registry-driven preflight of every numeric/bool/duration knob the BINARY
// parses: the ones config.Load consumes (#1119) and, since #1273, the ones
// parsed at their point of use elsewhere in the tree (the
// FLEET_SCHED_RATE_LIMIT_* trio, the sandbox Kata overhead, the agentcore
// thresholds, the SSE/notify/webpush knobs …). The registry
// (internal/config/knobs.go) is the same table config.Load parses through, so a
// typo'd knob (FLEET_MAX_COST_USD=5O, FLEET_LOCKDOWN_ONLY=enabled) is reported
// here exactly as the boot path would refuse it. Since #1119/#1273 the loader
// itself fails loud on all of them, so on a successful load the walk is a
// re-check; its real value is the FAILURE path — it needs no *Config, so knob
// problems are still reported when Load fails for an unrelated reason (a bad
// IP list / TLS mode) and the operator fixes everything in one pass.
//
// Documented-lenient knobs (config.ValidateLenientEnvKnobs — today only
// FLEET_OTEL_SAMPLE_RATIO) are ADVISORIES: their consumer deliberately absorbs
// a bad value, so reporting them as blocking would claim a boot failure that
// will not happen. They are still preflighted, so a typo'd tracing ratio is
// visible instead of silent.
func checkEnvVars(cfg *config.Config, cfgErr error) checkResult {
	res := checkResult{Name: "env_vars", Blocking: true}
	if cfgErr != nil {
		res.Status = statusFail
		detail := "config load failed: " + cfgErr.Error()
		// Walk the registry even though Load failed (see above); skip any
		// problem the load error already names verbatim.
		for _, p := range append(config.ValidateEnvKnobs(), config.ValidateLenientEnvKnobs()...) {
			if !strings.Contains(detail, p) {
				detail += "; " + p
			}
		}
		res.Detail = detail
		return res
	}
	var problems []string
	if err := cfg.Validate(); err != nil {
		problems = append(problems, err.Error())
	}
	problems = append(problems, config.ValidateEnvKnobs()...)
	advisories := config.ValidateLenientEnvKnobs()

	if len(problems) > 0 {
		res.Status = statusFail
		res.Detail = strings.Join(append(problems, advisories...), "; ")
		return res
	}
	if len(advisories) > 0 {
		// Flip Blocking with the status, as checkManifest/checkCredentials do:
		// failed() reads both fields, but the --json contract (#248) exposes
		// Blocking on its own and a consumer keying off it must not read an
		// advisory as must-fix.
		res.Blocking = false
		res.Status = statusWarn
		res.Detail = strings.Join(advisories, "; ")
		return res
	}
	res.Status = statusOK
	res.Detail = "required vars set; optional vars well-formed"
	return res
}

// ── 2. manifest bundle (blocking) ──

// checkManifest reports the bundle load (clientconfig.Load already validates the
// manifest schema + the MCP-catalog structural invariants) and then validates
// the referenced supporting-file paths the server reads at runtime: the two
// default personas and the system-prompt files. A referenced file that does not
// exist on disk is a blocking failure (the agent would fail the turn that needs
// it) — except the SCHEDULED default persona, whose reader degrades instead of
// failing; see the personaKnob table.
func checkManifest(bundle *clientconfig.Bundle, bundleErr error, cfg *config.Config) checkResult {
	res := checkResult{Name: "manifest", Blocking: true}
	if bundleErr != nil {
		res.Status = statusFail
		res.Detail = "bundle load failed: " + bundleErr.Error()
		return res
	}
	var problems, advisories []string

	manifestPath := filepath.Join(bundle.Dir, "manifest.yaml")
	// The interactive base system prompt (chat.md) and the scheduled base
	// (default.md) are the two files the engines always read.
	for _, name := range []string{"chat.md", "default.md"} {
		p := filepath.Join(bundle.SystemPromptsDir, name)
		if !fileExists(p) {
			problems = append(problems, fmt.Sprintf("system prompt %s missing", name))
		}
	}
	if cfg != nil {
		if miss := personaMiss(bundle, cfg.PersonaDefault, interactivePersonaKnob); miss != "" {
			problems = append(problems, miss)
		}
		if miss := personaMiss(bundle, cfg.Persona, scheduledPersonaKnob); miss != "" {
			advisories = append(advisories, miss)
		}
	}
	// Agent Plugin defects are advisory: the loader already applied the spec's
	// failure boundaries (a bad entry skips itself, never the bundle), so the
	// deployment runs — but the author should see what was dropped and why.
	for _, p := range bundle.PluginProblems() {
		advisories = append(advisories, "plugin: "+p)
	}

	if len(problems) > 0 {
		res.Status = statusFail
		res.Detail = strings.Join(append(problems, advisories...), "; ")
		return res
	}
	if len(advisories) > 0 {
		// Flip Blocking with the status, as checkCredentials does: failed() reads
		// both fields, but the --json contract (#248) exposes Blocking on its own,
		// and a consumer keying off it must not read an advisory as must-fix.
		res.Blocking = false
		res.Status = statusWarn
		res.Detail = strings.Join(advisories, "; ")
		return res
	}
	res.Status = statusOK
	res.Detail = manifestPath
	if n := len(bundle.Plugins); n > 0 {
		res.Detail = fmt.Sprintf("%s (%d agent plugin(s) loaded)", manifestPath, n)
	}
	return res
}

// personaKnob is one of the two default-persona settings a deployment's env file
// carries. They differ in SHAPE (config/default/README.md) and — the reason they
// are not checked at the same severity — in what their reader does with a miss.
type personaKnob struct {
	// role names the knob in the report line, so an operator reading "⚠ manifest"
	// knows which of the two is wrong.
	role string
	// env is the canonical variable that fixes the miss.
	env string
	// resolve turns the configured value into the filename its reader opens inside
	// personas/. Both reduce to a basename, but only the interactive reader
	// appends .yaml, so the two must not share one rule.
	resolve func(persona string) string
	// suggest renders one of the bundle's persona FILENAMES the way env takes it:
	// a bare name for FLEET_PERSONA_DEFAULT, a bundle-relative path for
	// FLEET_PERSONA. Suggesting the wrong shape would contradict the docs.
	suggest func(filename string) string
}

// The two knobs and their severities (#956).
//
// FLEET_PERSONA_DEFAULT is turn-fatal, so a miss is BLOCKING: it is the persona
// new interactive conversations start on (config.PersonaDefault), which
// agent.Manager.RunTurn passes to buildSystemPrompt, whose os.ReadFile miss
// RETURNS AN ERROR (internal/agent/prompt.go) and fails the turn. Nothing
// upstream catches it first — /api/personas hands the same name to the chat UI
// as the selected persona with no membership check against the roster beside it
// — so a deployment naming a persona the bundle does not ship fails every chat
// turn.
//
// FLEET_PERSONA only degrades, so a miss is ADVISORY: it is the scheduled
// driver's global persona (config.Persona) and its sole reader,
// scheduledrun.composeSystemPrompt, IGNORES the ReadFile error, dropping the
// domain-expertise block from the prompt but still running the task.
//
// Both are properties of the DEPLOYMENT rather than of the bundle under test,
// which is what keeps the advisory out of the exit code: unset, the loader falls
// back to assistant, and a red ✗ on every bundle that names its persona anything
// else teaches operators to ignore the whole report. The blocking one earns its
// ✗ despite that same argument, because for it the fallback is not a false
// alarm — an unset FLEET_PERSONA_DEFAULT against such a bundle really does break
// every chat turn.
var (
	interactivePersonaKnob = personaKnob{
		role: "interactive default persona",
		env:  "FLEET_PERSONA_DEFAULT",
		resolve: func(persona string) string {
			name := filepath.Base(persona)
			if !strings.HasSuffix(strings.ToLower(name), ".yaml") {
				name += ".yaml"
			}
			return name
		},
		suggest: func(filename string) string { return strings.TrimSuffix(filename, filepath.Ext(filename)) },
	}
	scheduledPersonaKnob = personaKnob{
		role:    "scheduled default persona",
		env:     "FLEET_PERSONA",
		resolve: filepath.Base,
		suggest: func(filename string) string { return "personas/" + filename },
	}
)

// personaMiss reports the configured persona when the bundle does not ship it,
// and "" when it does, resolving the value through knob.resolve — i.e. inside
// the bundle's personas/ dir, the way that knob's reader does. Resolving against
// the bundle ROOT instead, as this check used to, reported personas the bundle
// ships as missing.
func personaMiss(bundle *clientconfig.Bundle, persona string, knob personaKnob) string {
	persona = strings.TrimSpace(persona)
	if bundle == nil || persona == "" {
		return ""
	}
	name := knob.resolve(persona)
	if fileExists(filepath.Join(bundle.PersonasDir, name)) {
		return ""
	}
	offered := bundlePersonaFiles(bundle)
	if len(offered) == 0 {
		return fmt.Sprintf("%s %s not in personas/ — the bundle ships no personas", knob.role, name)
	}
	choices := make([]string, 0, len(offered))
	for _, filename := range offered {
		choices = append(choices, knob.suggest(filename))
	}
	return fmt.Sprintf("%s %s not in personas/ — set %s to one of: %s", knob.role, name, knob.env, strings.Join(choices, ", "))
}

// bundlePersonaFiles lists the persona files the bundle ships so a finding can
// name the choices, not just the miss. Only .yaml is offered: the persona
// rosters (agent.Manager.ListPersonas, listBundlePersonas) are .yaml-only and
// the interactive/per-task loaders force a ".yaml" suffix onto the configured
// name, so a .yml file can never back a chat persona — offering one here would
// hand the operator a remediation that loops. The one reader that opens its
// configured filename verbatim (the scheduled global persona,
// scheduledrun.composeSystemPrompt) could load a .yml, but steering an operator
// toward a file every other reader is blind to is not a fix. os.ReadDir
// already sorts by filename.
func bundlePersonaFiles(bundle *clientconfig.Bundle) []string {
	entries, err := os.ReadDir(bundle.PersonasDir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(strings.ToLower(e.Name()), ".yaml") {
			continue
		}
		names = append(names, e.Name())
	}
	return names
}

// ── 3. MCP servers (warning) ──

// checkMCPServers reports per-server reachability for the resolved (enabled) MCP
// catalog. For stdio servers it checks the command resolves on PATH (or as an
// absolute/bundle-relative file) and reuses the bundle's own script-arg path
// validation; for http servers it pings the URL unless --skip-network-checks.
// This is a WARNING check: a disabled or optional connector failing must not
// block startup, so the per-server problems are reported but never blocking.
func checkMCPServers(ctx context.Context, bundle *clientconfig.Bundle, cfg *config.Config, opts validateOptions) checkResult {
	res := checkResult{Name: "mcp_servers", Blocking: false}
	if bundle == nil || cfg == nil {
		res.Status = statusWarn
		res.Detail = "skipped (bundle/config not loaded)"
		return res
	}
	if len(cfg.MCPServers) == 0 {
		res.Status = statusOK
		res.Detail = "no enabled MCP servers"
		return res
	}

	// Reuse the bundle's script-arg path validation (catches a missing mcp/foo.py).
	scriptProblems := map[string]bool{}
	for _, p := range bundle.ValidateMCPArgPaths() {
		scriptProblems[p] = true
	}

	names := sortedServerNames(cfg.MCPServers)
	var perServer []string
	ok := true
	for _, name := range names {
		sc := cfg.MCPServers[name]
		if detail, good := probeMCPServer(ctx, name, sc, bundle.Dir, opts); good {
			perServer = append(perServer, name+": ok")
		} else {
			perServer = append(perServer, name+": "+detail)
			ok = false
		}
	}
	if len(scriptProblems) > 0 {
		ok = false
		for p := range scriptProblems {
			perServer = append(perServer, p)
		}
	}

	res.Detail = strings.Join(perServer, ", ")
	if ok {
		res.Status = statusOK
	} else {
		res.Status = statusWarn
	}
	return res
}

// probeMCPServer checks one resolved server. stdio: the command resolves on PATH
// or as a file under the bundle. http: a HEAD/GET ping unless network checks are
// skipped. Returns (detail, ok).
func probeMCPServer(ctx context.Context, name string, sc config.MCPServerConfig, bundleDir string, opts validateOptions) (string, bool) {
	if sc.Type == "http" {
		if opts.skipNetworkChecks {
			return "skipped (network)", true
		}
		return pingHTTP(ctx, sc.URL)
	}
	// stdio: resolve the command (an executable on PATH, an absolute path, or a
	// bundle-relative file like a venv interpreter).
	cmd := strings.TrimSpace(sc.Command)
	if cmd == "" {
		return "no command", false
	}
	if filepath.IsAbs(cmd) || strings.ContainsRune(cmd, os.PathSeparator) {
		p := cmd
		if !filepath.IsAbs(p) {
			p = filepath.Join(bundleDir, cmd)
		}
		if isExecutableFile(p) {
			return "ok", true
		}
		return fmt.Sprintf("command %q not found/executable", cmd), false
	}
	if _, err := exec.LookPath(cmd); err != nil {
		return fmt.Sprintf("command %q not on PATH", cmd), false
	}
	_ = name
	return "ok", true
}

// pingHTTP does a context-bounded GET against an MCP HTTP server's URL. Any HTTP
// response (even 4xx) means the endpoint is reachable — auth/path correctness is
// out of scope for a reachability ping. Returns (detail, ok).
func pingHTTP(ctx context.Context, rawURL string) (string, bool) {
	if strings.TrimSpace(rawURL) == "" {
		return "no url", false
	}
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "bad url", false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "unreachable", false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	return "ok", true
}

// ── 3b. MCP catalog structure (CI gate, non-blocking) ──

// checkMCPCatalog validates the STRUCTURE of the bundle's FULL MCP catalog —
// every declared server, including the ones an enable gate (enabled_env /
// enabled_groups, production secrets CI does not hold) turns off.
// checkMCPServers above only ever sees the enabled subset
// (Bundle.MCPServerConfigs skips !s.enabled()), so a CI gate on mcp_servers is
// nearly vacuous: the broken gated-off server is not even listed — a live
// fleet box reported "knowledge_base: ok, plugin_notes: ok" while its broken
// example_api entry went unexamined. Structure means what is true on ANY
// machine: names present, unique and provider-safe (they become part of
// mcp_<server>_<tool>), http URLs parse with an http(s) scheme AND a host,
// stdio servers name a command, every env-var name that gates or accounts the
// server — enabled_env, account_vars and each enabled_groups member — is
// clean and no group is empty, the bundle's own script-arg paths resolve
// (ValidateMCPArgPaths already walks this same unfiltered catalog — reuse,
// don't reimplement), and the Agent Plugin loader dropped nothing on the way
// in (PluginProblems).
//
// Deliberately NOT checked: whether a stdio Command resolves on PATH. That is
// INSTALLATION, not structure — it belongs to mcp_servers — and checking it
// here would pin the CI gate permanently red on every bundle whose servers
// need uvx/node, which a bare CI runner does not have. This check must be
// green on a machine with none of the bundles' tooling, or it is useless as
// a gate.
//
// Non-blocking BY DESIGN, and the reason is load-bearing: the workflow gate
// keys on status != "ok" (the Blocking flag feeds only validate-config's own
// exit code). Blocking:true here would newly fail `fleet validate-config` on
// operator boxes that are legitimately fine, and this change must not alter
// any existing box's exit code — CI reads the status, operators read the exit
// code, and only the former may change.
func checkMCPCatalog(bundle *clientconfig.Bundle, bundleErr error) checkResult {
	res := checkResult{Name: "mcp_catalog", Blocking: false}
	if bundle == nil || bundleErr != nil {
		res.Status = statusWarn
		res.Detail = "skipped (bundle not loaded)"
		return res
	}
	// No early return for an empty catalog. A bundle with no manifest servers
	// whose ONLY plugin server the loader rejected arrives here with an empty
	// MCPCatalog and a non-empty PluginProblems — returning ok before the
	// plugin fold below would hide exactly the connector that just vanished.
	// The loop is a no-op when empty; the ok detail reads "no servers
	// declared" at the end instead.
	var problems []string
	// Bundle-relative paths resolve against the absolute bundle dir, the same
	// way ValidateMCPArgPaths and the runtime do; fall back to the raw dir if
	// Abs fails so the check degrades to "relative to cwd" rather than skipping.
	bundleDir := bundle.Dir
	if abs, err := filepath.Abs(bundle.Dir); err == nil {
		bundleDir = abs
	}
	seen := make(map[string]bool, len(bundle.MCPCatalog))
	dupes := make(map[string]bool)
	for i := range bundle.MCPCatalog {
		s := &bundle.MCPCatalog[i]
		if strings.TrimSpace(s.Name) == "" {
			problems = append(problems, fmt.Sprintf("mcp_catalog[%d]: empty server name", i))
			continue
		}
		// The label names the server in every diagnostic — unless the name is
		// not even shaped like one. The manifest is ${VAR}-interpolated before
		// it gets here, and on an OPERATOR run preflightEnvFile loads the real
		// deployment env first, so `name: "${API_KEY}"` arrives as the key
		// itself. A name that fails the provider shape (a dot, a slash, a
		// space, over 64 chars) is exactly the shape a pasted secret takes, so
		// such an entry is identified by INDEX and its text is never printed.
		// A well-formed name is echoed, as checkMCPServers already does.
		label := fmt.Sprintf("mcp_catalog[#%d]", i)
		if clientconfig.ValidMCPServerName(s.Name) {
			label = fmt.Sprintf("mcp_catalog[%q]", s.Name)
		}
		if seen[s.Name] && !dupes[s.Name] {
			problems = append(problems, label+": duplicate server name")
			dupes[s.Name] = true
		}
		seen[s.Name] = true
		problems = append(problems, catalogServerProblems(s, label, bundleDir)...)
	}
	problems = append(problems, bundle.ValidateMCPArgPaths()...)
	// A plugin problem means part of the DECLARED bundle was dropped or rejected
	// before it reached MCPCatalog — an mcp.json server skipped as invalid, a
	// plugin.json the loader refused, a skill that would not parse. Walking only
	// the survivors would report "ok" over a connector that just vanished, and
	// checkManifest deliberately demotes these to advisories (a running box
	// must not be taken down by a plugin defect). Here the question is whether
	// the bundle is sound, so they count. Every one of them is decided by the
	// bundle's own files, not the machine — with one exception: the
	// PLUGIN_DATA-unavailable case is environmental, but it also means the
	// plugin's stdio servers were skipped and the catalog under test is
	// incomplete, so failing is still the honest answer (the CI workflow pins
	// FLEET_DATA_DIR to a fresh runner.temp dir so it cannot arise there).
	//
	// ENTRY problems only. Root-availability problems — an explicit
	// plugin_roots dir such as /opt/fleet/site-plugins that is missing or
	// unreadable HERE — are facts about the machine, not the bundle
	// (docs/AGENT-PLUGINS.md supports absolute roots precisely so a site can
	// mount plugins outside the repo). Folding them in would pin every PR of
	// such a bundle red for a configuration that is valid on the box. They stay
	// visible as `manifest` advisories, where the operator view belongs.
	for _, p := range bundle.PluginEntryProblems() {
		problems = append(problems, "plugin: "+p)
	}

	if len(problems) > 0 {
		res.Status = statusFail
		res.Detail = strings.Join(problems, "; ")
		return res
	}
	res.Status = statusOK
	if len(bundle.MCPCatalog) == 0 {
		res.Detail = "no servers declared"
		return res
	}
	res.Detail = fmt.Sprintf("%d server(s): structure ok%s", len(bundle.MCPCatalog), catalogAccountHeadroomNotes(bundle.MCPCatalog))
	return res
}

// catalogServerProblems is the per-server half of checkMCPCatalog: every
// structural rule for one named declaration, in one place, each rule a helper
// so the function stays readable as rules accumulate (they have — four review
// rounds' worth). label is the `mcp_catalog["name"]` prefix every problem
// carries. Nothing here touches the machine: no PATH lookup, no dial, no exec.
func catalogServerProblems(s *clientconfig.ServerDef, label, bundleDir string) []string {
	var problems []string
	// The name becomes part of every tool name (mcp_<server>_<tool>), and
	// upstream providers reject a dot or a space there. The loader does not
	// enforce this for manifest servers — see ValidMCPServerName for why — so a
	// credential-gated `sales.api` would pass boot and break the first turn
	// that enables it. Same rule as plugin server keys.
	if !clientconfig.ValidMCPServerName(s.Name) {
		problems = append(problems, label+": name must be 1-64 chars of letters, digits, '_' or '-' (it becomes part of the mcp_<server>_<tool> tool name)")
	}
	problems = append(problems, catalogToolNameBudgetProblems(s, label)...)
	problems = append(problems, catalogActivationProblems(s, label)...)
	if s.Type == "http" {
		problems = append(problems, catalogHTTPProblems(s, label)...)
	} else {
		problems = append(problems, catalogStdioProblems(s, label, bundleDir)...)
	}
	return append(problems, catalogVarNameProblems(s, label)...)
}

// catalogToolNameBudgetProblems: providers cap a tool name at
// MaxProviderToolNameLen (64) and the runtime emits mcp_<server>_<tool> with no
// truncation, so the budget is shared. Where the manifest declares a tools
// allowlist, every generated name is checked exactly; where it does not (tools
// are discovered at connect time), the server name must at least leave room
// for a one-character tool, or NO tool could ever be advertised. A too-long
// name is not a warning — once the server's credentials enable it, every
// model request that carries the tool fails.
func catalogToolNameBudgetProblems(s *clientconfig.ServerDef, label string) []string {
	var problems []string
	// The allowlist is matched EXACTLY against the tool names the server
	// advertises (MCPServerConfigs copies the strings verbatim), so a padded
	// " lookup " excludes the real lookup and a blank entry matches nothing —
	// a non-empty list of blanks silently filters every tool the server has.
	for i, tool := range s.Tools {
		switch {
		case strings.TrimSpace(tool) == "" || strings.TrimSpace(tool) != tool:
			problems = append(problems, fmt.Sprintf("%s: tools[%d] is blank or has surrounding whitespace; the allowlist is matched exactly and this entry can never match a real tool", label, i))
		case !clientconfig.ValidMCPServerName(tool):
			// The tool name is advertised inside mcp_<server>_<tool> verbatim,
			// and providers accept only letters, digits, '_' and '-' there — a
			// dot, slash or space in an allowlisted name fails every model
			// request once the connector is enabled. Same character rule as the
			// server name (ValidMCPServerName), so "checked exactly" is true of
			// the characters as well as the length. Not echoed: an allowlist
			// entry is manifest text and may be ${VAR}-interpolated.
			problems = append(problems, fmt.Sprintf("%s: tools[%d] contains characters providers reject in a tool name (allowed: letters, digits, '_', '-')", label, i))
		}
	}
	fixed := len(clientconfig.MCPToolNamePrefix) + len(s.Name) + 1 // "mcp_" + name + "_"
	if len(s.Tools) == 0 {
		if fixed+1 > clientconfig.MaxProviderToolNameLen {
			problems = append(problems, fmt.Sprintf("%s: name is %d chars; %s<name>_<tool> must fit in %d, which leaves no room for any tool name", label, len(s.Name), clientconfig.MCPToolNamePrefix, clientconfig.MaxProviderToolNameLen))
		}
	}
	for _, tool := range s.Tools {
		if n := fixed + len(tool); n > clientconfig.MaxProviderToolNameLen {
			problems = append(problems, fmt.Sprintf("%s: tool %q would be advertised as a %d-char name (%s%s_%s); providers cap tool names at %d", label, tool, n, clientconfig.MCPToolNamePrefix, s.Name, tool, clientconfig.MaxProviderToolNameLen))
		}
	}
	// A named seat is registered as <server>_<account> before the prefix and
	// tool are added (agentcore.RegisteredMCPName), so on a server that
	// declares account_vars the budget is ALSO shared with the account label.
	// Labels are operator input at `fleet mcp account set` time with no length
	// cap, so a secretless preflight cannot know them — and a fixed reservation
	// would be arbitrary: a 16-char one failed four real bundles whose seats
	// work today with `production`. So the exact headroom is computed instead:
	// it is a FAILURE only when even a one-character label cannot fit (a named
	// seat is then impossible on this server), and otherwise it is reported —
	// see catalogAccountHeadroom, which the ok detail carries — so the operator
	// knows the label cap for each server before they create the account.
	if len(s.AccountVars) > 0 {
		if h := catalogAccountHeadroom(s); h < 1 {
			problems = append(problems, fmt.Sprintf("%s: declares account_vars but %s%s_<account>_<longest tool> already reaches %d; no room for any account label, so a named seat can never be advertised", label, clientconfig.MCPToolNamePrefix, s.Name, clientconfig.MaxProviderToolNameLen))
		}
	}
	return problems
}

// catalogAccountHeadroom is the longest account label a server that declares
// account_vars can carry before mcp_<server>_<account>_<tool> exceeds the
// provider cap, measured against its longest allowlisted tool (or a
// one-character tool when no allowlist is declared). Negative or zero means
// no label fits at all.
func catalogAccountHeadroom(s *clientconfig.ServerDef) int {
	longest := 1
	for _, t := range s.Tools {
		if len(t) > longest {
			longest = len(t)
		}
	}
	// "mcp_" + name + "_" + account + "_" + tool
	return clientconfig.MaxProviderToolNameLen - (len(clientconfig.MCPToolNamePrefix) + len(s.Name) + 1 + 1 + longest)
}

// catalogAccountHeadroomNotes renders, for the ok detail, the label cap of
// every server that declares account_vars — the one fact about named seats a
// secretless preflight can state, and the number an operator needs when they
// pick an account label. Sorted by server name for a stable report.
func catalogAccountHeadroomNotes(catalog []clientconfig.ServerDef) string {
	var parts []string
	for i := range catalog {
		s := &catalog[i]
		if len(s.AccountVars) == 0 || strings.TrimSpace(s.Name) == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%d", s.Name, catalogAccountHeadroom(s)))
	}
	if len(parts) == 0 {
		return ""
	}
	sort.Strings(parts)
	return "; account label headroom (chars, per account_vars server): " + strings.Join(parts, ", ")
}

// catalogActivationProblems: a server that is not `always` and declares no
// enable gate has NO activation path — enabled() returns false for that
// combination on every box, so the server is declared, structurally perfect,
// and never offered anywhere. `optional` and `enabled_by_default` do not
// enable; they only shape the picker once a gate is satisfied.
func catalogActivationProblems(s *clientconfig.ServerDef, label string) []string {
	gated := len(s.EnabledEnv) > 0
	for _, g := range s.EnabledGroups {
		if len(g) > 0 {
			gated = true
		}
	}
	if s.Always || gated {
		return nil
	}
	return []string{label + ": no activation path — set always: true or declare enabled_env / enabled_groups (optional: true alone never enables a server)"}
}

// catalogHTTPProblems validates an http server's url and headers.
//
// None of the diagnostics echo the URL. A manifest url may carry userinfo or a
// signed query string, and this output lands in JSON reports, CI job
// summaries and journals — the no-credential-values rule applies to a
// malformed URL too. The server label is enough to find it. (*url.Error
// embeds the URL, so the parse failure is reported without the error text.)
func catalogHTTPProblems(s *clientconfig.ServerDef, label string) []string {
	var problems []string
	raw := strings.TrimSpace(s.URL)
	// MCPServerConfigs copies s.URL verbatim and net/http rejects a padded
	// request URL, so trimming here alone would pass a declaration the runtime
	// cannot dial.
	if raw != "" && s.URL != raw {
		problems = append(problems, label+": url has surrounding whitespace")
	}
	switch u, err := url.Parse(raw); {
	case raw == "":
		problems = append(problems, label+": http server has empty url")
	case err != nil:
		problems = append(problems, label+": url does not parse")
	case u.Scheme != "http" && u.Scheme != "https":
		// The scheme is not echoed either: `url: "${API_KEY}://host"` puts the
		// interpolated value exactly there.
		problems = append(problems, label+": url scheme is not http or https")
	case u.Hostname() == "":
		// url.Parse accepts "https://", "https:///mcp", the opaque "http:foo"
		// AND "https://:443/mcp" (Host ":443", Hostname "") without complaint;
		// none can be dialled, and the last has nothing to derive SNI or the
		// certificate name from. Hostname(), not Host, is the test.
		problems = append(problems, label+": url has no host")
	case u.Port() != "":
		// url.Parse keeps ":99999" as a string; the transport rejects it as an
		// invalid address only when it first dials.
		if n, perr := strconv.Atoi(u.Port()); perr != nil || n < 1 || n > 65535 {
			problems = append(problems, fmt.Sprintf("%s: url port %q is not in 1-65535", label, u.Port()))
		}
	}
	// Manifest headers get no validation in Load (plugin headers do, spec
	// §7.2.1) and net/http fails the request at send time on a bad name or a
	// value with a forbidden byte — visible only once the server's credentials
	// enable it. Same rule as plugin headers.
	if err := clientconfig.ValidateHTTPHeaders(s.Headers); err != nil {
		problems = append(problems, fmt.Sprintf("%s: %v", label, err))
	}
	// Named accounts are env-suffixed variants of a stdio spawn; there is no
	// such thing for an http server, and agentcore.resolveMCPVariant refuses
	// every named account whose base is http. But AccountsFor still publishes
	// the suffixed accounts for any server spec that declares account_vars,
	// so an http server with account_vars exposes seats that can never run.
	// The loader already rejects the sibling stdio-only field (identity_env)
	// on http servers; hold account_vars to the same rule here.
	if len(s.AccountVars) > 0 {
		problems = append(problems, label+": account_vars is stdio-only (accounts are env-suffixed spawn variants; an http server rejects every named account, so these seats could never run)")
	}
	return problems
}

// catalogStdioProblems validates the launch fields of a stdio server (the
// manifest's default type). Resolution of the command is deliberately NOT this
// check's business — see checkMCPCatalog — but the loader keeps every field
// verbatim and os/exec is unforgiving about what it is handed: a padded
// " python3 " resolves nowhere, and exec.Cmd.Start rejects a NUL anywhere in
// argv or the environment. Env values are checked pre-interpolation (${VAR}
// refs or literals); an env KEY with '=' or NUL cannot be represented in a
// process environment at all.
func catalogStdioProblems(s *clientconfig.ServerDef, label, bundleDir string) []string {
	var problems []string
	switch {
	case strings.TrimSpace(s.Command) == "":
		problems = append(problems, label+": stdio server has empty command")
	case s.Command != strings.TrimSpace(s.Command):
		// Not echoed: the manifest is ${VAR}-interpolated before it gets here,
		// so a command field can carry a value that came from the environment.
		problems = append(problems, label+": command has surrounding whitespace")
	case !filepath.IsAbs(s.Command) && strings.ContainsRune(s.Command, os.PathSeparator) && !s.FromPlugin():
		// A command WITH a path separator that is not absolute is a file the
		// bundle ships (./mcp/server, .venv/bin/python) — bundle content, not a
		// runner-installed dependency — and probeMCPServer already resolves it
		// against the bundle dir. So it IS structural: check it exists and is
		// executable, exactly as the runtime will. Bare names (python3, uvx)
		// stay exempt: that is installation. Absolute paths stay exempt: they
		// name the deployment box's filesystem, not the bundle's. Plugin
		// servers launch in the plugin root and were resolved by its loader.
		p := filepath.Clean(filepath.Join(bundleDir, s.Command))
		switch {
		case p != bundleDir && !strings.HasPrefix(p, bundleDir+string(os.PathSeparator)):
			// "../bin/server" joins to a path OUTSIDE the bundle. Whatever is
			// there on this machine, it is not shipped with the bundle — and
			// enough ".." makes the runner vouch for an unrelated host binary.
			problems = append(problems, fmt.Sprintf("%s: bundle-relative command %q escapes the bundle directory", label, s.Command))
		case !isExecutableFile(p):
			problems = append(problems, fmt.Sprintf("%s: bundle-relative command %q is not an executable file under the bundle", label, s.Command))
		}
	}
	if strings.IndexByte(s.Command, 0) >= 0 {
		problems = append(problems, label+": command contains a NUL byte")
	}
	for ai, a := range s.Args {
		if strings.IndexByte(a, 0) >= 0 {
			problems = append(problems, fmt.Sprintf("%s: args[%d] contains a NUL byte", label, ai))
		}
		// A relative arg with a path separator is a file the bundle ships (the
		// runtime spawns with cwd = bundle dir). ValidateMCPArgPaths checks
		// that a script arg EXISTS but only joins-and-stats, so "../shared/x.py"
		// passes when the file happens to sit elsewhere in the checkout — and
		// the gate would certify content the bundle does not ship. Same
		// containment as the command. Plugin servers launch in the plugin root
		// and were contained by its loader.
		if !s.FromPlugin() && !filepath.IsAbs(a) && strings.ContainsRune(a, os.PathSeparator) {
			if p := filepath.Clean(filepath.Join(bundleDir, a)); p != bundleDir && !strings.HasPrefix(p, bundleDir+string(os.PathSeparator)) {
				problems = append(problems, fmt.Sprintf("%s: args[%d] is a relative path that escapes the bundle directory", label, ai))
			}
		}
	}
	var badEnv []string
	for k, v := range s.Env {
		if strings.ContainsAny(k, "=\x00") || strings.IndexByte(v, 0) >= 0 {
			badEnv = append(badEnv, k)
		}
	}
	sort.Strings(badEnv) // map order is random; keep the report stable
	for _, k := range badEnv {
		problems = append(problems, fmt.Sprintf("%s: env %q cannot be passed to a process ('=' or NUL in the key, or NUL in the value)", label, k))
	}
	return problems
}

// catalogVarNameProblems validates every env var NAME that gates or accounts
// the server — enabled_env, account_vars, identity_env, and each member of
// every enabled_groups alternative. enabled() looks those up verbatim, so a
// padded " API_KEY" reads an unset var and leaves the connector silently
// disabled on every box; '=' and NUL cannot occur in a process-environment
// name at all (entries split at the first '='), so os.Getenv("API=KEY") can
// never find anything. identity_env is the sharp one: the loader trims the
// name for its own env-map lookup but propagates the padded original, and the
// named-account guard then reads the identity as unset and can let a variant
// inherit the default seat's routing identity. An EMPTY group is rejected
// outright: allSet(nil) is vacuously true, so `enabled_groups: [[]]` enables
// the server with no gate at all — the opposite of what a gated declaration
// means.
func catalogVarNameProblems(s *clientconfig.ServerDef, label string) []string {
	var problems []string
	varLists := make([][]string, 0, 3+len(s.EnabledGroups))
	varLists = append(varLists, s.EnabledEnv, s.AccountVars, s.IdentityEnv, s.OptionalEnv)
	// optional_env names keys of THIS server's env map whose empty value should
	// be DROPPED from the spawned environment rather than passed as "".
	// resolveEnvMap looks the name up exactly, so a typo'd or padded entry is
	// silently a no-op: the connector receives an empty variable it was meant
	// not to see, and one that distinguishes absent from empty fails only once
	// its credentials enable it. The spelling rule above catches padding; this
	// catches the typo.
	// Neither entry is echoed: on an operator run the manifest is interpolated
	// against the real deployment env first, so `optional_env: ["${API_KEY}"]`
	// arrives here as the key itself. The index finds it just as well.
	for i, v := range s.OptionalEnv {
		if _, ok := s.Env[v]; !ok && strings.TrimSpace(v) == v && v != "" {
			problems = append(problems, fmt.Sprintf("%s: optional_env[%d] is not a key of the server's env map, so it can never drop anything", label, i))
		}
	}
	// account_vars is deliberately NOT held to "must be a key of the env map".
	// It is documented as informational for seat discovery (creds.AccountsFor
	// scans <VAR>_<ACCOUNT> for the names listed here) while the overlay reads
	// Env's keys (ServerDef.AccountVars; docs/MCP-BUNDLE-ENV.md: "as env keys
	// or account_vars"), and two production bundles rely on listing the SOURCE
	// variables (OMNICOM_EMAIL_AWS_ACCESS_KEY_ID beside env key
	// AWS_ACCESS_KEY_ID). Whether that split contract fully works is a runtime
	// design question, not one a preflight should adjudicate.
	for gi, group := range s.EnabledGroups {
		if len(group) == 0 {
			problems = append(problems, fmt.Sprintf("%s: enabled_groups[%d] is empty (an empty group enables the server unconditionally)", label, gi))
		}
		varLists = append(varLists, group)
	}
	for _, vars := range varLists {
		for _, v := range vars {
			if v == "" || strings.TrimSpace(v) != v || strings.ContainsAny(v, "=\x00") {
				problems = append(problems, fmt.Sprintf("%s: env var name %q is empty, has surrounding whitespace, or contains '=' or NUL (the process environment cannot represent it)", label, v))
			}
		}
	}
	return problems
}

// ── 3c. Bundle files the engines always read (CI gate, non-blocking) ──

// checkManifestFiles is the bundle-intrinsic slice of checkManifest, split out
// so CI can gate it. checkManifest mixes two kinds of fact: files every box
// reads (the interactive base prompt chat.md and the scheduled base
// default.md — a missing chat.md fails every interactive turn) and defaults a
// BOX supplies (the persona chosen by FLEET_PERSONA, which a runner that sets
// no such variable legitimately lacks). Gating `manifest` in CI therefore
// pins half the bundle family red for a true statement about the runner,
// while leaving `manifest` ungated lets a deleted chat.md merge green because
// Load and mcp_catalog both still succeed without it. This check carries only
// the first kind, so the workflow can require it everywhere.
//
// It is a second check rather than a re-scoped `manifest` because `manifest`
// is BLOCKING and operators read its exit code: narrowing it would change
// what `fleet validate-config` refuses to start on every existing box.
// Non-blocking for the same reason as mcp_catalog — CI keys on the status.
func checkManifestFiles(bundle *clientconfig.Bundle, bundleErr error) checkResult {
	res := checkResult{Name: "manifest_files", Blocking: false}
	if bundle == nil || bundleErr != nil {
		res.Status = statusWarn
		res.Detail = "skipped (bundle not loaded)"
		return res
	}
	var problems []string
	for _, name := range []string{"chat.md", "default.md"} {
		if !fileExists(filepath.Join(bundle.SystemPromptsDir, name)) {
			problems = append(problems, fmt.Sprintf("system prompt %s missing", name))
		}
	}
	if len(problems) > 0 {
		res.Status = statusFail
		res.Detail = strings.Join(problems, "; ")
		return res
	}
	res.Status = statusOK
	res.Detail = "system prompts present (chat.md, default.md)"
	return res
}

// ── 3d. Bundle skills (CI gate, non-blocking) ──

// checkBundleSkills surfaces Bundle.ValidateSkills — a skill folder with no
// SKILL.md, bad frontmatter, a name/folder mismatch, an empty description — as
// a check result. Load deliberately does not fail on these: a defective skill
// is skipped from the roster and the problem is LOGGED, so a running box is
// not taken down by one bad skill. But "logged to stderr" is invisible to a
// CI gate that reads the JSON report, and a checked-in skill that quietly
// drops out of the roster is a bundle defect on every box. Decided entirely by
// the bundle's own files, so it belongs in the floor. Non-blocking for the
// same reason as the other floor checks: CI keys on the status, operators on
// the exit code, and this must not change any existing box's exit code.
// (Plugin skills are covered separately: the plugin loader records their
// defects as PluginEntryProblems, which mcp_catalog already folds in.)
func checkBundleSkills(bundle *clientconfig.Bundle, bundleErr error) checkResult {
	res := checkResult{Name: "bundle_skills", Blocking: false}
	if bundle == nil || bundleErr != nil {
		res.Status = statusWarn
		res.Detail = "skipped (bundle not loaded)"
		return res
	}
	if problems := bundle.ValidateSkills(); len(problems) > 0 {
		res.Status = statusFail
		res.Detail = strings.Join(problems, "; ")
		return res
	}
	res.Status = statusOK
	res.Detail = "bundle skills well-formed"
	return res
}

// ── 3e. Agent policy (CI gate, non-blocking) ──

// checkAgentPolicy surfaces what the boot-time agent-policy install would
// silently ignore in agent_policy.critical_tool_aliases (#1604): a member that
// is not a critical suffix (checked against the SAME merged list the audit gate
// uses — the base email suffixes, critical_tools, and the critical http_tools /
// a2a_peers names), or an entry left with fewer than two members. At boot each
// is one log line and the alias quietly does nothing, so a typo leaves exactly
// the wrong-variant wedge the alias was declared to end. The problems come from
// agentcore.CriticalToolAliasProblems, the same code ConfigureAgentPolicy runs,
// so this check and the running gate cannot disagree. Decided entirely by the
// bundle's own files, so it belongs in the floor, and non-blocking like the
// other floor checks: CI keys on the status, operators on the exit code.
func checkAgentPolicy(bundle *clientconfig.Bundle, bundleErr error) checkResult {
	res := checkResult{Name: "agent_policy", Blocking: false}
	if bundle == nil || bundleErr != nil {
		res.Status = statusWarn
		res.Detail = "skipped (bundle not loaded)"
		return res
	}
	p := bundle.AgentPolicy()
	problems := agentcore.CriticalToolAliasProblems(agentcore.AgentPolicy{
		CriticalToolSuffixes: p.CriticalToolSuffixes,
		CriticalToolAliases:  p.CriticalToolAliases,
	})
	if len(problems) > 0 {
		res.Status = statusFail
		res.Detail = strings.Join(problems, "; ")
		return res
	}
	res.Status = statusOK
	if n := len(p.CriticalToolAliases); n > 0 {
		res.Detail = fmt.Sprintf("critical_tool_aliases: %d entr%s, every member a critical suffix", n, map[bool]string{true: "y", false: "ies"}[n == 1])
	} else {
		res.Detail = "no critical_tool_aliases declared"
	}
	return res
}

// ── 4. database (blocking) ──

// checkDatabase validates the chat + sched DSNs and (unless --skip-network-checks)
// runs a SELECT 1 against each within a 5s budget. It reuses the SAME DSN
// resolution and the ensureDistinctDatabases invariant the server boots through,
// but does NOT run migrations — it is a read-only probe. The DB is BLOCKING (the
// issue lists it so); --skip-network-checks keeps it blocking but skips only the
// live probe, still validating the DSNs + the distinct-databases invariant.
func checkDatabase(ctx context.Context, cfg *config.Config, cfgErr error, opts validateOptions) checkResult {
	res := checkResult{Name: "database", Blocking: true}
	if cfgErr != nil || cfg == nil {
		res.Status = statusFail
		res.Detail = "config not loaded"
		return res
	}
	chat := chatDSN(cfg)
	sched := schedDSN()
	if strings.TrimSpace(chat) == "" {
		res.Status = statusFail
		res.Detail = "chat DSN is empty (set DATABASE_URL or FLEET_CHAT_DATABASE_URL)"
		return res
	}
	if err := ensureDistinctDatabases(chat, sched); err != nil {
		res.Status = statusFail
		res.Detail = err.Error()
		return res
	}
	if opts.skipNetworkChecks {
		res.Status = statusOK
		res.Detail = "DSNs valid + distinct (live probe skipped)"
		return res
	}
	// schedDSN may be empty (the sched layer then reads DATABASE_URL itself).
	effectiveSched := sched
	if strings.TrimSpace(effectiveSched) == "" {
		effectiveSched = strings.TrimSpace(os.Getenv("DATABASE_URL"))
	}
	if err := probeDB(ctx, chat); err != nil {
		res.Status = statusFail
		res.Detail = "chat DB: " + err.Error()
		return res
	}
	if strings.TrimSpace(effectiveSched) != "" {
		if err := probeDB(ctx, effectiveSched); err != nil {
			res.Status = statusFail
			res.Detail = "sched DB: " + err.Error()
			return res
		}
	}
	res.Status = statusOK
	res.Detail = "chat + sched DB reachable (SELECT 1)"
	return res
}

// probeDB opens a short-lived pool, pings, and runs SELECT 1 within a 5s budget,
// then closes it. It deliberately does NOT run migrations — the issue asks for a
// dry-run probe, not the self-migrating store.Open path.
func probeDB(ctx context.Context, dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer db.Close()
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var one int
	if err := db.QueryRowContext(probeCtx, "SELECT 1").Scan(&one); err != nil {
		return fmt.Errorf("SELECT 1: %w", err)
	}
	return nil
}

// ── 5. credentials (warning; blocking when a non-optional enabled server is missing a gate var) ──

// checkCredentials checks that the credential env-var NAMES the manifest's MCP
// catalog references are present in the process env. It NEVER reads or prints a
// VALUE — only the name and presence — honoring the host-side-credentials
// invariant. It is a WARNING by default (an absent optional credential just
// disables that connector), but escalates to BLOCKING when a NON-optional server
// is missing a required gate var (that server would silently fail to start).
func checkCredentials(bundle *clientconfig.Bundle, bundleErr error) checkResult {
	res := checkResult{Name: "credentials"}
	if bundleErr != nil || bundle == nil {
		res.Status = statusWarn
		res.Blocking = false
		res.Detail = "skipped (bundle not loaded)"
		return res
	}

	referenced := bundle.EnvVarNames()
	if len(referenced) == 0 {
		res.Status = statusOK
		res.Detail = "no credential vars referenced"
		return res
	}

	missing := missingEnvNames(referenced)
	// Split the absent names: one whose EVERY manifest occurrence carries a
	// ${VAR:-default} is a config knob with its manifest default in effect,
	// not a missing credential — EnvVarNames covers every interpolated field
	// (#1123), so the generic bundle's "${FLEET_SANDBOX_IMAGE:-}" would
	// otherwise warn on every pristine install forever.
	defaultOnly := map[string]bool{}
	for _, name := range bundle.EnvVarNamesDefaultOnly() {
		defaultOnly[name] = true
	}
	var missingCreds, defaultsInEffect []string
	for _, name := range missing {
		if defaultOnly[name] {
			defaultsInEffect = append(defaultsInEffect, name)
		} else {
			missingCreds = append(missingCreds, name)
		}
	}
	// A non-optional server whose gate var(s) are unset is a blocking failure.
	blockingMissing := requiredGateVarsMissing(bundle)

	present := len(referenced) - len(missing)
	if len(blockingMissing) > 0 {
		res.Blocking = true
		res.Status = statusFail
		res.Detail = fmt.Sprintf("%d/%d referenced vars present; required gate var(s) missing for non-optional server(s): %s",
			present, len(referenced), strings.Join(blockingMissing, ", "))
		return res
	}
	if len(missingCreds) > 0 {
		res.Blocking = false
		res.Status = statusWarn
		res.Detail = fmt.Sprintf("%d/%d referenced vars present; absent (optional connectors disabled): %s",
			present, len(referenced), strings.Join(missingCreds, ", "))
		if len(defaultsInEffect) > 0 {
			res.Detail += "; manifest defaults in effect: " + strings.Join(defaultsInEffect, ", ")
		}
		return res
	}
	res.Status = statusOK
	if len(defaultsInEffect) > 0 {
		res.Detail = fmt.Sprintf("%d/%d referenced vars present; manifest defaults in effect: %s",
			present, len(referenced), strings.Join(defaultsInEffect, ", "))
		return res
	}
	res.Detail = fmt.Sprintf("all %d referenced vars present", len(referenced))
	return res
}

// missingEnvNames returns the subset of names with no non-empty process-env
// value. Names only — never values.
func missingEnvNames(names []string) []string {
	var missing []string
	for _, n := range names {
		if strings.TrimSpace(os.Getenv(n)) == "" {
			missing = append(missing, n)
		}
	}
	return missing
}

// requiredGateVarsMissing returns the gate-var names of every NON-optional MCP
// server whose enable gate is not satisfied by the process env. These are
// blocking: a non-optional server the operator intends to ship would silently
// fail to enable. Optional servers are excluded (a user opts into those per
// conversation, so an absent credential just leaves them off). Returns names
// only, never values.
func requiredGateVarsMissing(bundle *clientconfig.Bundle) []string {
	seen := map[string]bool{}
	var missing []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		if strings.TrimSpace(os.Getenv(name)) == "" {
			seen[name] = true
			missing = append(missing, name)
		}
	}
	for i := range bundle.MCPCatalog {
		s := &bundle.MCPCatalog[i]
		if s.Optional || s.Always {
			continue
		}
		// An EnabledEnv (all-of) gate that is not fully satisfied means the
		// non-optional server will not enable. enabled_groups (any-of) is left to
		// the warning path: with multiple credential options, an operator may
		// legitimately provision only one group, so a partial group is not a hard
		// failure.
		if len(s.EnabledGroups) == 0 {
			for _, v := range s.EnabledEnv {
				add(v)
			}
		}
	}
	return missing
}

// ── 6. sandbox (blocking when container-backed) ──

// checkSandbox verifies the execution sandbox can be materialized. In a release
// build the host executor is NOT compiled in (sandbox.HostExecutorCompiledIn is
// false), so EVERY turn is container-backed and podman is mandatory — this check
// is then BLOCKING. When the host executor IS compiled in (the
// fleet_host_executor tag, tests/dev) AND MockMode is on, the container path is
// not required, so a missing podman degrades to a warning.
//
// When container-backed it checks: podman on PATH, `podman info` succeeds, and
// the resolved sandbox image exists locally (the same ref the boot path consumes
// via bundle.Sandbox().ResolvedImageRef() / cfg.SandboxImage). If a non-default
// OCI runtime is selected (FLEET_SANDBOX_RUNTIME or the bundle's sandbox.runtime
// — e.g. runsc/gVisor, kata, libkrun) podman must be able to resolve it, and the
// hypervisor-backed tiers (kata/krun) must additionally pass the same
// fail-closed KVM preflight the boot path runs (#217).
func checkSandbox(ctx context.Context, cfg *config.Config, bundle *clientconfig.Bundle) checkResult {
	res := checkResult{Name: "sandbox"}
	// A failed config load leaves cfg nil; the env_vars check already reports
	// that as the blocking failure, so degrade here instead of dereferencing.
	// (Reachable before #1119 only via a malformed IP list/TLS/network mode;
	// the loader failing loud on every malformed knob made it easy to hit.)
	if cfg == nil {
		res.Status = statusWarn
		res.Blocking = false
		res.Detail = "skipped (config not loaded)"
		return res
	}
	containerBacked := sandboxIsContainerBacked(cfg)
	res.Blocking = containerBacked
	if !containerBacked {
		res.Status = statusOK
		res.Detail = "host executor compiled in + mock mode; container sandbox not required"
		return res
	}

	// Kubernetes backend (#989): none of the podman checks apply — validate
	// the backend selection and run the same fail-closed cluster preflight the
	// boot path runs (apiserver reachability, RBAC, workspace claim, the
	// sealed-egress NetworkPolicy, the RuntimeClass when one is configured).
	backend, err := resolveValidateSandboxBackend(cfg, bundle)
	if err != nil {
		res.Status = statusFail
		res.Detail = err.Error()
		return res
	}
	if backend == sandbox.BackendKubernetes {
		return checkKubernetesSandbox(ctx, res, cfg, bundle)
	}

	const podmanBin = "podman"
	if _, err := exec.LookPath(podmanBin); err != nil {
		res.Status = statusFail
		res.Detail = "podman not found in PATH"
		return res
	}
	// Rootless podman keeps one image store PER USER, so EVERY podman probe
	// below must ask the store the SERVICE uses, not the one this shell has —
	// `fleet status` and `fleet doctor` already hop to the unit's User=; this
	// verb did not, and reported a present, runnable sandbox image as a
	// BLOCKING "not present" on a box those two called healthy. The runtime
	// and network-helper preflights below take the same context: a runtime
	// registered only in the fleet user's containers.conf booted fine while
	// these checks, run as root, failed it. One construction, shared with the
	// status probes through sandbox.ServiceStorePodmanExec.
	svcUser, svcHome := admincli.ServiceUserAndHome()
	execCtx, storeNote := sandbox.ServiceStorePodmanExec(svcUser, svcHome, os.Geteuid() == 0)
	execCtx.Binary = podmanBin
	// `podman info` FIRST: the runtime preflight below also shells out to podman,
	// so a broken rootless setup would otherwise be reported as "could not
	// resolve --runtime=…", blaming the runtime for a podman problem.
	infoCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := execCtx.CommandContext(infoCtx, "", "info").Run(); err != nil {
		res.Status = statusFail
		res.Detail = "podman info failed" + storeNote + " (rootless/daemon setup not accessible): " + err.Error()
		return res
	}
	// A non-default OCI runtime must be resolvable by podman and — for the
	// hypervisor-backed tiers (kata/krun) — actually able to deliver isolation.
	// Resolve the runtime the same way the boot path does (env wins, else the
	// bundle manifest) and run the real fail-closed preflight (#217), which asks
	// podman which binary it will exec rather than guessing from the name. A
	// separate PATH lookup here would be both weaker and wrong: it validates
	// whichever same-named binary is first on PATH, and it reports FAIL for a
	// perfectly good containers.conf that maps the name to an off-PATH binary.
	if rt := resolveSandboxRuntime(cfg, bundle); rt != "" {
		if err := sandbox.PreflightRuntime(ctx, execCtx, rt); err != nil {
			res.Status = statusFail
			res.Detail = err.Error()
			return res
		}
	}
	// Allowlisted egress needs a specific rootless network helper. Answering
	// "can this box run this config" is exactly what this verb is for, so run
	// the same fail-closed preflight the boot path runs (#211 / ADR-0012).
	networkHelperNote := ""
	if cfg.DefaultNetworkMode == sandbox.NetworkModeAllowlisted {
		if err := sandbox.PreflightAllowlistedNetwork(ctx, execCtx); err != nil {
			res.Status = statusFail
			res.Detail = err.Error()
			return res
		}
		// Say so on success too: an operator running this verb specifically to
		// check an allowlisted host should see the check ran, not just a generic
		// sandbox OK.
		networkHelperNote = "; allowlisted egress network helper present"
	}
	// Image existence: the SAME resolved ref the boot path consumes.
	image := resolveSandboxImage(cfg, bundle)
	if image == "" {
		res.Status = statusFail
		res.Detail = "no sandbox image resolved (set FLEET_SANDBOX_IMAGE or build the bundle image)"
		return res
	}
	imgCtx, imgCancel := context.WithTimeout(ctx, 10*time.Second)
	defer imgCancel()
	if err := execCtx.CommandContext(imgCtx, "", "image", "exists", image).Run(); err != nil {
		res.Status = statusFail
		res.Detail = fmt.Sprintf("sandbox image %q not present%s (build it with scripts/build-sandbox-image.sh or pull it)", image, storeNote)
		return res
	}
	res.Status = statusOK
	res.Detail = fmt.Sprintf("podman ok; image %q present%s%s", image, storeNote, networkHelperNote)
	return res
}

// resolveValidateSandboxBackend resolves the sandbox backend the same way the
// boot path does (sandbox.ResolveBackend: env FLEET_SANDBOX_BACKEND wins, else
// the bundle manifest's sandbox.backend, else podman; unrecognized = error).
func resolveValidateSandboxBackend(cfg *config.Config, bundle *clientconfig.Bundle) (string, error) {
	envBackend := ""
	if cfg != nil {
		envBackend = cfg.SandboxBackend
	}
	bundleBackend := ""
	if bundle != nil {
		bundleBackend = bundle.Sandbox().Backend
	}
	return sandbox.ResolveBackend(envBackend, bundleBackend)
}

// checkKubernetesSandbox validates the kubernetes sandbox backend: the image
// ref resolves, the podman-only knobs are unset, and the boot preflight's
// cluster checks pass. Image PRESENCE is not checked — pulls happen on the
// sandbox nodes' kubelets, which this process cannot see; a bad ref fails
// fast at the first pod start instead.
func checkKubernetesSandbox(ctx context.Context, res checkResult, cfg *config.Config, bundle *clientconfig.Bundle) checkResult {
	if rt := resolveSandboxRuntime(cfg, bundle); rt != "" {
		res.Status = statusFail
		res.Detail = fmt.Sprintf("FLEET_SANDBOX_RUNTIME=%q has no effect under the kubernetes backend — use FLEET_SANDBOX_K8S_RUNTIME_CLASS", rt)
		return res
	}
	// Boot refuses this knob too (internal/agent/manager.go, buildKubernetesSandboxPool).
	// It is read straight from the environment there — there is no config field for
	// the podman profile — so mirror that rather than inventing one, otherwise
	// validate-config reports OK on a config that cannot start.
	if v := strings.TrimSpace(os.Getenv("FLEET_SANDBOX_SECCOMP_PROFILE")); v != "" {
		res.Status = statusFail
		res.Detail = fmt.Sprintf("FLEET_SANDBOX_SECCOMP_PROFILE=%q has no effect under the kubernetes backend — install the profile on the sandbox nodes and use FLEET_SANDBOX_K8S_SECCOMP_PROFILE", v)
		return res
	}
	if cfg.DefaultNetworkMode == sandbox.NetworkModeAllowlisted {
		res.Status = statusFail
		res.Detail = "FLEET_DEFAULT_NETWORK_MODE=allowlisted is not supported under the kubernetes backend (the host egress proxy is unreachable from pods) — use lockdown or open"
		return res
	}
	// Boot refuses a pids ceiling too: a Pod spec has no per-pod pids limit, so
	// the knob would read as containment while imposing none. Reported here so
	// an operator sees it BEFORE the upgrade that starts refusing it.
	if cfg.SandboxPids > 0 {
		res.Status = statusFail
		res.Detail = fmt.Sprintf("FLEET_SANDBOX_PIDS=%d has no effect under the kubernetes backend (a Pod spec has no per-pod pids limit) — set the kubelet's podPidsLimit on the sandbox nodes and unset this knob", cfg.SandboxPids)
		return res
	}
	image := resolveSandboxImage(cfg, bundle)
	if image == "" {
		res.Status = statusFail
		res.Detail = "no sandbox image resolved (set FLEET_SANDBOX_IMAGE or the bundle manifest's sandbox.image — kubernetes nodes cannot consume a build-on-box tag)"
		return res
	}
	// Same env-wins-else-bundle resolution and fail-closed parse as the boot
	// path: an env value is parsed from its string form; with no env value
	// the bundle's structured knobs apply directly.
	k8s := bundle.Sandbox().Kubernetes
	nodeSelector := k8s.NodeSelector
	if strings.TrimSpace(cfg.SandboxK8sNodeSelector) != "" {
		parsed, err := sandbox.ParseK8sNodeSelector(cfg.SandboxK8sNodeSelector)
		if err != nil {
			res.Status = statusFail
			res.Detail = "FLEET_SANDBOX_K8S_NODE_SELECTOR: " + err.Error()
			return res
		}
		nodeSelector = parsed
	}
	var tolerations []sandbox.K8sToleration
	for _, tol := range k8s.Tolerations {
		tolerations = append(tolerations, sandbox.K8sToleration(tol))
	}
	if strings.TrimSpace(cfg.SandboxK8sTolerations) != "" {
		parsed, err := sandbox.ParseK8sTolerations(cfg.SandboxK8sTolerations)
		if err != nil {
			res.Status = statusFail
			res.Detail = "FLEET_SANDBOX_K8S_TOLERATIONS: " + err.Error()
			return res
		}
		tolerations = parsed
	}
	docsInImage := k8s.BundleDocsInImage
	if strings.TrimSpace(cfg.SandboxK8sBundleDocsInImage) != "" {
		parsed, err := sandbox.ParseK8sBundleDocsInImage(cfg.SandboxK8sBundleDocsInImage)
		if err != nil {
			res.Status = statusFail
			res.Detail = "FLEET_SANDBOX_K8S_BUNDLE_DOCS_IN_IMAGE: " + err.Error()
			return res
		}
		docsInImage = parsed
	}
	fill := func(env, bundleVal string) string {
		if strings.TrimSpace(env) != "" {
			return strings.TrimSpace(env)
		}
		return strings.TrimSpace(bundleVal)
	}
	backend, err := sandbox.NewKubernetesBackend(sandbox.KubernetesConfig{
		Namespace:                      fill(cfg.SandboxK8sNamespace, k8s.Namespace),
		WorkspaceClaim:                 fill(cfg.SandboxK8sWorkspaceClaim, k8s.WorkspaceClaim),
		ServiceAccount:                 fill(cfg.SandboxK8sServiceAccount, k8s.ServiceAccount),
		ImagePullSecret:                fill(cfg.SandboxK8sImagePullSecret, k8s.ImagePullSecret),
		RuntimeClassName:               fill(cfg.SandboxK8sRuntimeClass, k8s.RuntimeClass),
		SeccompLocalhostProfile:        fill(cfg.SandboxK8sSeccompProfile, k8s.SeccompProfile),
		KubeconfigPath:                 fill(cfg.SandboxK8sKubeconfig, k8s.Kubeconfig),
		NetworkPolicyName:              fill(cfg.SandboxK8sNetworkPolicy, k8s.NetworkPolicy),
		OpenEgressPolicyName:           fill(cfg.SandboxK8sOpenEgressPolicy, k8s.OpenEgressPolicy),
		DefaultNetworkMode:             cfg.DefaultNetworkMode,
		UnrestrictedEgressAcknowledged: cfg.SandboxK8sOpenEgressAcknowledged,
		NodeSelector:                   nodeSelector,
		Tolerations:                    tolerations,
	})
	if err != nil {
		res.Status = statusFail
		res.Detail = err.Error()
		return res
	}
	if err := backend.Preflight(ctx); err != nil {
		res.Status = statusFail
		res.Detail = err.Error()
		return res
	}
	res.Status = statusOK
	// bundle-doc reads are the one behavior an operator cannot infer from the
	// cluster state this check just proved, so it is reported either way.
	docs := "bundle docs NOT in the sandbox image — in-sandbox protocol/skill reads will not resolve"
	if docsInImage {
		docs = "bundle docs declared present in the sandbox image (unverifiable here — a wrong declaration reads as not-found)"
	}
	res.Detail = fmt.Sprintf("kubernetes backend ok; image %q, sandbox namespace %q (image pullability is checked at first pod start); %s", image, backend.Namespace(), docs)
	return res
}

// sandboxIsContainerBacked reports whether this binary will run agent tool calls
// in a container (the only sandbox path that needs podman). True for a release
// build (host executor not compiled in). When the host executor IS compiled in,
// it is container-backed UNLESS MockMode is on (the test/dev path that runs the
// host executor instead of a container).
func sandboxIsContainerBacked(cfg *config.Config) bool {
	if !sandbox.HostExecutorCompiledIn() {
		return true
	}
	if cfg != nil && cfg.MockMode {
		return false
	}
	return true
}

// resolveSandboxImage resolves the sandbox image ref the same way the boot path
// does: an explicit cfg.SandboxImage (FLEET_SANDBOX_IMAGE) wins, else the
// bundle's resolved ref (manifest sandbox.image, else sandbox.tag).
func resolveSandboxImage(cfg *config.Config, bundle *clientconfig.Bundle) string {
	if cfg != nil && strings.TrimSpace(cfg.SandboxImage) != "" {
		return strings.TrimSpace(cfg.SandboxImage)
	}
	if bundle != nil {
		return bundle.Sandbox().ResolvedImageRef()
	}
	return ""
}

// resolveSandboxRuntime resolves the OCI runtime the boot path will use, with
// the same precedence as the image (env FLEET_SANDBOX_RUNTIME wins, else the
// bundle manifest's sandbox.runtime), normalized to podman's runtime name
// ("libkrun" → "krun"). Empty means podman's configured default (#217).
func resolveSandboxRuntime(cfg *config.Config, bundle *clientconfig.Bundle) string {
	envRuntime := ""
	if cfg != nil {
		envRuntime = cfg.SandboxRuntime
	}
	bundleRuntime := ""
	if bundle != nil {
		bundleRuntime = bundle.Sandbox().Runtime
	}
	return sandbox.ResolveRuntime(envRuntime, bundleRuntime)
}

// ── 7. model / API key (warning) ──

// checkModelAPI does a lightweight GET /api/v1/key against the OpenRouter base
// (or OPENROUTER_BASE_URL override) with the configured key, to verify the key
// authenticates. /api/v1/key and not /api/v1/models: the models list is PUBLIC
// — it returns 200 with no Authorization header and with a garbage one — so
// probing it blessed any non-empty key with "authenticates" (#1264 found a
// 64-hex junk value passing this check and then failing the first real
// completion with a 401). /api/v1/key requires auth: 401 on a bad or missing
// key, 200 with the key's own metadata otherwise. It is a WARNING: a 401
// surfaces as a fail-status warning (bad key), a 200 is ok, and a
// timeout/transport error is a warning (transient network — not a config
// defect). Skipped by --skip-network-checks and in MockMode (no real key
// expected). Never prints the key.
func checkModelAPI(ctx context.Context, cfg *config.Config, cfgErr error, opts validateOptions) checkResult {
	res := checkResult{Name: "model_api", Blocking: false}
	if cfgErr != nil || cfg == nil {
		res.Status = statusWarn
		res.Detail = "skipped (config not loaded)"
		return res
	}
	if opts.skipNetworkChecks {
		res.Status = statusWarn
		res.Detail = "skipped (--skip-network-checks)"
		return res
	}
	if cfg.MockMode {
		res.Status = statusOK
		res.Detail = "skipped (mock mode)"
		return res
	}
	if strings.TrimSpace(cfg.OpenRouterAPIKey) == "" {
		res.Status = statusWarn
		res.Detail = "OPENROUTER_API_KEY unset; cannot verify"
		return res
	}

	endpoint := openRouterKeyEndpoint()
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		res.Status = statusWarn
		res.Detail = "could not build request: " + err.Error()
		return res
	}
	req.Header.Set("Authorization", "Bearer "+cfg.OpenRouterAPIKey)
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		res.Status = statusWarn
		res.Detail = "request failed (transient?): " + err.Error()
		return res
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		res.Status = statusWarn
		res.Detail = "OpenRouter rejected the API key (HTTP 401)"
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		res.Status = statusOK
		res.Detail = "API key authenticates"
	default:
		res.Status = statusWarn
		res.Detail = fmt.Sprintf("unexpected status %d", resp.StatusCode)
	}
	return res
}

// openRouterKeyEndpoint returns the /api/v1/key URL, honoring the
// OPENROUTER_BASE_URL override (E2E / self-hosted gateway) so the check hits the
// same origin the running server would. The fake-LLM seam serves this path
// with the same auth contract, so the check stays meaningful in E2E ladders.
func openRouterKeyEndpoint() string {
	if override := strings.TrimSpace(os.Getenv("OPENROUTER_BASE_URL")); override != "" {
		return strings.TrimRight(override, "/") + "/api/v1/key"
	}
	return "https://openrouter.ai/api/v1/key"
}

// ── output ──

// emitReport prints the results (human-readable or JSON) and returns the process
// exit code: 0 when every blocking check passed, 1 otherwise.
func emitReport(out io.Writer, results []checkResult, asJSON bool) int {
	blockingFailures := 0
	for _, r := range results {
		if r.failed() {
			blockingFailures++
		}
	}
	passed := blockingFailures == 0

	if asJSON {
		report := validateReport{Checks: results, Passed: passed, BlockingFailures: blockingFailures}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	} else {
		emitHuman(out, results, blockingFailures, passed)
	}

	if passed {
		return 0
	}
	return 1
}

// emitHuman prints the ✓/✗ per-check report and the summary line.
func emitHuman(out io.Writer, results []checkResult, blockingFailures int, passed bool) {
	for _, r := range results {
		fmt.Fprintf(out, "%s %s: %s\n", statusGlyph(r.Status), r.Name, r.Detail)
	}
	fmt.Fprintln(out)
	switch {
	case passed && warnCount(results) == 0:
		fmt.Fprintln(out, "All checks passed.")
	case passed:
		fmt.Fprintf(out, "All blocking checks passed (%d warning(s)).\n", warnCount(results))
	default:
		fmt.Fprintf(out, "%d blocking check(s) failed. Fix the above before starting Fleet.\n", blockingFailures)
	}
}

// statusGlyph maps a status to its report glyph: ✓ for ok, ✗ for a (blocking or
// non-blocking) failure, ⚠ for a warning.
func statusGlyph(s checkStatus) string {
	switch s {
	case statusOK:
		return "✓"
	case statusFail:
		return "✗"
	case statusWarn:
		return "⚠"
	default:
		return "?"
	}
}

// warnCount counts non-blocking warn/fail results (informational in the summary).
func warnCount(results []checkResult) int {
	n := 0
	for _, r := range results {
		if !r.failed() && (r.Status == statusWarn || r.Status == statusFail) {
			n++
		}
	}
	return n
}

// sortedServerNames returns the catalog server names sorted for stable output.
func sortedServerNames(m map[string]config.MCPServerConfig) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	// Small N (the enabled catalog), insertion sort keeps it dependency-free and
	// deterministic.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j-1] > names[j]; j-- {
			names[j-1], names[j] = names[j], names[j-1]
		}
	}
	return names
}

// ── small fs helpers ──

// fileExists reports whether path is an existing regular (non-dir) file. The
// path is always operator-config-derived (the bundle dir + a manifest/config
// reference, the latter reduced to a basename), never request input — this is a
// startup diagnostic with no HTTP surface.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// isExecutableFile reports whether path is an existing regular file with any
// execute bit set. The path is operator-config-derived (an MCP server's command
// from the bundle manifest), never request input.
func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}
