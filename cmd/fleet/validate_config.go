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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver for the probe

	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
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
	fs := flag.NewFlagSet("validate-config", flag.ContinueOnError)
	var opts validateOptions
	fs.StringVar(&opts.bundlePath, "bundle-path", "", "client-config bundle dir (overrides FLEET_CLIENT_CONFIG_DIR; default config/default)")
	fs.BoolVar(&opts.skipNetworkChecks, "skip-network-checks", false, "skip the DB live probe, MCP HTTP ping, and OpenRouter key check (CI with no outbound access)")
	fs.BoolVar(&opts.jsonOutput, "json", false, "emit the report as JSON for CI parsing")
	if err := fs.Parse(args); err != nil {
		return opts, err
	}
	return opts, nil
}

// runChecks loads the bundle + config and runs every preflight check in the
// fixed order the issue lists. A bundle/config load failure is itself reported as
// a blocking failure of the relevant check (and dependent checks degrade to a
// blocking failure too, since they cannot run without it).
func runChecks(ctx context.Context, opts validateOptions) []checkResult {
	results := make([]checkResult, 0, 7)

	bundle, bundleErr := clientconfig.Load(clientconfig.Dir())

	// config.Load reads the env file (FLEET_ENV_FILE) and the process env. Register
	// the bundle's connector env-var names first — the SAME ordering the server boot
	// uses — so a .env-supplied credential survives the allowlist and the credential
	// check below sees it.
	if bundleErr == nil {
		config.RegisterAllowedEnvVars(bundle.EnvVarNames()...)
	}
	cfg, cfgErr := config.Load(os.Getenv("FLEET_ENV_FILE"))
	if cfgErr == nil && bundleErr == nil {
		cfg.MCPServers = bundle.MCPServerConfigs()
		cfg.HTTPTools = bundle.HTTPToolConfigs()
	}

	results = append(results, checkEnvVars(cfg, cfgErr))
	results = append(results, checkManifest(bundle, bundleErr, cfg))
	results = append(results, checkMCPServers(ctx, bundle, cfg, opts))
	results = append(results, checkDatabase(ctx, cfg, cfgErr, opts))
	results = append(results, checkCredentials(bundle, bundleErr))
	results = append(results, checkSandbox(ctx, cfg, bundle))
	results = append(results, checkModelAPI(ctx, cfg, cfgErr, opts))

	return results
}

// ── 1. env vars (blocking) ──

// checkEnvVars reuses cfg.Validate (the SAME required-field gate the server boots
// through: OPENROUTER_API_KEY unless MockMode, FLEET_SERVER_TOKEN, the
// conversation caps, DATABASE_URL, TLS) and then validates the optional numeric
// knobs that the loader silently defaults on a bad value, so a typo'd
// FLEET_MAX_COST_USD / FLEET_MAX_CONCURRENT_AGENTS is caught here rather than
// silently ignored.
func checkEnvVars(cfg *config.Config, cfgErr error) checkResult {
	res := checkResult{Name: "env_vars", Blocking: true}
	if cfgErr != nil {
		res.Status = statusFail
		res.Detail = "config load failed: " + cfgErr.Error()
		return res
	}
	var problems []string
	if err := cfg.Validate(); err != nil {
		problems = append(problems, err.Error())
	}
	problems = append(problems, validateOptionalEnvVars()...)

	if len(problems) > 0 {
		res.Status = statusFail
		res.Detail = strings.Join(problems, "; ")
		return res
	}
	res.Status = statusOK
	res.Detail = "required vars set; optional vars well-formed"
	return res
}

// validateOptionalEnvVars checks the optional numeric env vars that config.Load
// silently falls back to a default on when malformed. A SET-but-malformed value
// is an operator error worth surfacing: the spend/concurrency bounds must be
// positive, while the input-queue retention window is non-negative (zero
// explicitly disables it). Unset values are fine (the default applies). It
// reads the env directly (not cfg) so it can tell "unset" from "set to garbage
// that fell back to the default".
//
// NOTE: issue #248 also lists "FLEET_DEFAULT_RUNTIME a known flavor". fleet has
// no such process-level env var — the runtime flavor is a per-task field, not a
// boot config knob — so there is nothing to validate here for it. Validating a
// non-existent knob would be dishonest, so it is intentionally omitted.
func validateOptionalEnvVars() []string {
	var problems []string
	if raw, ok := lookupFleetEnv("MAX_COST_USD"); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err != nil || f <= 0 {
			problems = append(problems, "FLEET_MAX_COST_USD must be a positive float")
		}
	}
	if raw, ok := lookupFleetEnv("MAX_CONCURRENT_AGENTS"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(raw)); err != nil || n <= 0 {
			problems = append(problems, "FLEET_MAX_CONCURRENT_AGENTS must be a positive integer")
		}
	}
	if raw, ok := lookupFleetEnv("INPUT_QUEUE_RETENTION_DAYS"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(raw)); err != nil || n < 0 {
			problems = append(problems, "FLEET_INPUT_QUEUE_RETENTION_DAYS must be a non-negative integer")
		}
	}
	return problems
}

// lookupFleetEnv reads FLEET_<suffix> then the legacy CHAT_/CUTLASS_ prefixes,
// mirroring config.getenvFleet*'s precedence so the check sees the same value the
// loader would. Returns the raw value and whether any of the variants is set
// (non-empty).
func lookupFleetEnv(suffix string) (string, bool) {
	for _, prefix := range []string{"FLEET_", "CHAT_", "CUTLASS_"} {
		if v := strings.TrimSpace(os.Getenv(prefix + suffix)); v != "" {
			return v, true
		}
	}
	return "", false
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
	if len(missing) > 0 {
		res.Blocking = false
		res.Status = statusWarn
		res.Detail = fmt.Sprintf("%d/%d referenced vars present; absent (optional connectors disabled): %s",
			present, len(referenced), strings.Join(missing, ", "))
		return res
	}
	res.Status = statusOK
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
	containerBacked := sandboxIsContainerBacked(cfg)
	res.Blocking = containerBacked
	if !containerBacked {
		res.Status = statusOK
		res.Detail = "host executor compiled in + mock mode; container sandbox not required"
		return res
	}

	const podmanBin = "podman"
	if _, err := exec.LookPath(podmanBin); err != nil {
		res.Status = statusFail
		res.Detail = "podman not found in PATH"
		return res
	}
	// `podman info` FIRST: the runtime preflight below also shells out to podman,
	// so a broken rootless setup would otherwise be reported as "could not
	// resolve --runtime=…", blaming the runtime for a podman problem.
	infoCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := exec.CommandContext(infoCtx, podmanBin, "info").Run(); err != nil {
		res.Status = statusFail
		res.Detail = "podman info failed (rootless/daemon setup not accessible): " + err.Error()
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
		if err := sandbox.PreflightRuntime(ctx, podmanBin, rt); err != nil {
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
		if err := sandbox.PreflightAllowlistedNetwork(ctx, podmanBin); err != nil {
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
	//nolint:gosec // G204: podmanBin is the fixed "podman" binary and image is an operator-config-derived ref (FLEET_SANDBOX_IMAGE / bundle manifest), not request input — it cannot inject a subprocess.
	if err := exec.CommandContext(imgCtx, podmanBin, "image", "exists", image).Run(); err != nil {
		res.Status = statusFail
		res.Detail = fmt.Sprintf("sandbox image %q not present (build it with scripts/build-sandbox-image.sh or pull it)", image)
		return res
	}
	res.Status = statusOK
	res.Detail = fmt.Sprintf("podman ok; image %q present%s", image, networkHelperNote)
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

// checkModelAPI does a lightweight GET /api/v1/models against the OpenRouter base
// (or OPENROUTER_BASE_URL override) with the configured key, to verify the key
// authenticates. It is a WARNING: a 401 surfaces as a fail-status warning (bad
// key), a 200 is ok, and a timeout/transport error is a warning (transient
// network — not a config defect). Skipped by --skip-network-checks and in
// MockMode (no real key expected). Never prints the key.
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

	endpoint := openRouterModelsEndpoint()
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

// openRouterModelsEndpoint returns the /api/v1/models URL, honoring the
// OPENROUTER_BASE_URL override (E2E / self-hosted gateway) so the check hits the
// same origin the running server would.
func openRouterModelsEndpoint() string {
	if override := strings.TrimSpace(os.Getenv("OPENROUTER_BASE_URL")); override != "" {
		return strings.TrimRight(override, "/") + "/api/v1/models"
	}
	return "https://openrouter.ai/api/v1/models"
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
