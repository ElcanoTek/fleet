package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ── Config hot-reload (#286) ──
//
// A running fleet can re-read a small, explicitly safe SUBSET of its settings
// without a restart, so an operator can adjust a cost / token / iteration ceiling
// or the sampling temperature on the fly — via `kill -USR2 <pid>` or
// `POST /admin/reload-config`. Everything else (bind addresses, DB DSNs, auth
// secrets, the admission-semaphore size, TLS context) is bound into a listener,
// connection pool, or signing context at boot and CANNOT be swapped mid-run;
// reload reports any attempt to change one in ReloadResult.Skipped rather than
// silently ignoring it.
//
// Race-safety: the reloadable fields stay PUBLIC so existing construction (Load
// and the whole test suite) is unchanged, but at RUNTIME they MUST be read
// through the Live* getters below — Reload mutates them under a write lock from
// the signal / HTTP goroutine while turns read them. Direct field access is only
// safe before the process starts serving (boot), where it happens-before any
// turn; `make test-race` guards the runtime path.

// reloadState holds the synchronization primitives + boot snapshots backing
// hot-reload. It is a single pointer field on Config so (a) Config stays
// copy-safe for `go vet`'s copylocks check and (b) a test-built Config literal
// simply leaves it nil — the Live* getters then read the field directly, which
// is correct because such a Config is never concurrently reloaded.
type reloadState struct {
	mu        sync.RWMutex      // guards the reloadable Config fields at runtime
	serialize sync.Mutex        // serializes whole Reload calls (they edit process env)
	bootEnv   map[string]string // original process-env winners (∩ allowlist) — preserves boot precedence on reload
	bootVals  map[string]string // resolved boot strings of the watched non-reloadable settings
	// excludedEnv contains credential names whose runtime owner has moved to a
	// child process. Reload must not rehydrate them into this process from either
	// the env file or its boot-winner snapshot. Suffixed account variants are
	// excluded by the same uppercase-alphanumeric convention as env admission.
	excludedEnv map[string]bool
	// bootSecrets pins the boot values (nil = unset at boot) of the registered
	// per-request auth-secret env vars (#584). bootEnv only covers process-env
	// winners, so a FILE-sourced webhook/Slack signing secret would otherwise be
	// silently rotated by a reload of a drifted .env — Reload restores these and
	// reports the drift in Skipped instead.
	bootSecrets map[string]*string
}

// newReloadState captures the boot environment so a later Reload reproduces
// boot's precedence exactly, and snapshots the watched non-reloadable settings so
// a change to one can be reported as Skipped. Called once at the end of Load
// (after the process env is in its final, restored state).
func newReloadState(bootEnv map[string]string) *reloadState {
	rs := &reloadState{
		bootEnv:     make(map[string]string, len(bootEnv)),
		bootVals:    make(map[string]string, len(nonReloadableWatched)),
		bootSecrets: map[string]*string{},
		excludedEnv: map[string]bool{},
	}
	for k, v := range bootEnv {
		rs.bootEnv[k] = v
	}
	for _, w := range nonReloadableWatched {
		if v, ok := w.lookup(); ok {
			rs.bootVals[w.key] = v
		}
	}
	// Snapshot the registered auth-secret env vars in their FINAL boot state
	// (process env or env file, whichever won), remembering set vs unset so a
	// reload can restore either state exactly.
	for _, k := range nonReloadableSecretNames() {
		if v, ok := os.LookupEnv(k); ok {
			vc := v
			rs.bootSecrets[k] = &vc
		} else {
			rs.bootSecrets[k] = nil
		}
	}
	return rs
}

// ExcludeEnvVarsFromReload permanently removes names from this Config's
// process-env reload surface. It is used after a credential-owning child has
// inherited connector credentials: later env-file reloads must not silently
// reintroduce those credentials into the parent. Matching account-suffixed
// variants are excluded too. Names only are retained.
func (c *Config) ExcludeEnvVarsFromReload(names ...string) error {
	if c == nil {
		return errors.New("exclude environment from reload: nil config")
	}
	if c.reload == nil {
		return nil // literal/test Config values have no reload path to constrain
	}
	clean := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || strings.ContainsRune(name, '=') {
			return fmt.Errorf("invalid reload-excluded environment name %q", name)
		}
		clean = append(clean, name)
	}
	c.reload.serialize.Lock()
	defer c.reload.serialize.Unlock()
	for _, name := range clean {
		c.reload.excludedEnv[name] = true
		delete(c.reload.bootEnv, name)
	}
	return nil
}

func (rs *reloadState) envExcluded(name string) bool {
	if rs.excludedEnv[name] {
		return true
	}
	idx := strings.LastIndexByte(name, '_')
	if idx <= 0 || idx == len(name)-1 || !rs.excludedEnv[name[:idx]] {
		return false
	}
	for _, r := range name[idx+1:] {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// LiveMaxCostUSD returns the per-run cost ceiling, hot-reload-safe. A nil reload
// state (a Config built directly in a test, not via Load) reads the field
// directly — such a Config is never concurrently reloaded.
func (c *Config) LiveMaxCostUSD() float64 {
	if c.reload == nil {
		return c.MaxCostUSD
	}
	c.reload.mu.RLock()
	defer c.reload.mu.RUnlock()
	return c.MaxCostUSD
}

// LiveMaxTotalTokens returns the per-run token ceiling, hot-reload-safe.
func (c *Config) LiveMaxTotalTokens() int {
	if c.reload == nil {
		return c.MaxTotalTokens
	}
	c.reload.mu.RLock()
	defer c.reload.mu.RUnlock()
	return c.MaxTotalTokens
}

// LiveMaxIterations returns the per-turn iteration ceiling, hot-reload-safe.
func (c *Config) LiveMaxIterations() int {
	if c.reload == nil {
		return c.MaxIterations
	}
	c.reload.mu.RLock()
	defer c.reload.mu.RUnlock()
	return c.MaxIterations
}

// LiveTemperature returns the sampling temperature (interactive and scheduled
// runs share the one knob, #1079), hot-reload-safe.
func (c *Config) LiveTemperature() float64 {
	if c.reload == nil {
		return c.Temperature
	}
	c.reload.mu.RLock()
	defer c.reload.mu.RUnlock()
	return c.Temperature
}

// FieldChange records one reloadable setting whose value actually changed.
type FieldChange struct {
	Key string `json:"key"`
	Old string `json:"old"`
	New string `json:"new"`
}

// SkippedField records one non-reloadable setting the operator changed; it is
// ignored until a restart, with a human-readable reason why.
type SkippedField struct {
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

// ReloadError records a reloadable setting whose new value failed to parse or
// validate; the previous value is kept (a bad value never poisons a running
// process).
type ReloadError struct {
	Key   string `json:"key"`
	Error string `json:"error"`
}

// ReloadResult describes the outcome of a Reload.
type ReloadResult struct {
	ReloadedAt time.Time      `json:"reloaded_at"`
	Changed    []FieldChange  `json:"changed"`
	Skipped    []SkippedField `json:"skipped"`
	Errors     []ReloadError  `json:"errors"`
}

// watchedSetting is a non-reloadable env var whose change Reload surfaces in
// ReloadResult.Skipped (so the operator learns a restart is required).
type watchedSetting struct {
	key    string                // env var name, as reported back to the operator
	reason string                // why it cannot be changed without a restart
	lookup func() (string, bool) // resolves the current value (ok=false when unset)
}

// nonReloadableWatched lists the highest-impact settings that are bound at boot
// and cannot change mid-run. It is intentionally a curated subset, not the whole
// non-reloadable surface — enough to give an operator clear feedback when they
// changed something a reload won't pick up.
var nonReloadableWatched = []watchedSetting{
	{key: "FLEET_SERVER_ADDR", reason: "the TCP listener is bound at startup; restart to rebind",
		lookup: func() (string, bool) { return lookupFleet("SERVER_ADDR") }},
	{key: "DATABASE_URL / DB_*", reason: "the Postgres connection pool is opened once at startup; restart to reconnect",
		// Resolve the EFFECTIVE DSN the same way boot does (buildDatabaseURL),
		// so a change to the discrete DB_HOST/DB_PORT/... vars is detected too —
		// not just an explicit DATABASE_URL. The resolved value (which may carry a
		// password) stays in-process; only the key + reason are ever returned.
		lookup: func() (string, bool) { v := buildDatabaseURL(); return v, v != "" }},
	{key: "FLEET_SERVER_TOKEN", reason: "shared-secret auth; rotating it mid-run would invalidate in-flight sessions; restart to apply",
		lookup: func() (string, bool) { return lookupFleet("SERVER_TOKEN") }},
	{key: "ADMIN_API_KEY", reason: "admin auth secret; restart to apply",
		lookup: func() (string, bool) { v := os.Getenv("ADMIN_API_KEY"); return v, v != "" }},
	{key: "FLEET_MAX_CONCURRENT_AGENTS", reason: "sizes the admission semaphore + sandbox warm pool at startup; restart to resize",
		lookup: func() (string, bool) { return lookupFleet("MAX_CONCURRENT_AGENTS") }},
	{key: "FLEET_TLS_MODE", reason: "the TLS listener context is built at startup; restart to change",
		lookup: func() (string, bool) { v := os.Getenv("FLEET_TLS_MODE"); return v, v != "" }},
}

// Reload re-reads the reloadable settings from the env file (if given) and the
// process environment, applying any that changed and returning a diff. It is
// safe to call concurrently with running turns and with itself.
//
// Reload reproduces boot precedence exactly: a value the operator pinned in the
// process environment at startup still wins over the file; the file drives
// everything else. (Re-snapshotting the *current* env instead of the boot
// snapshot would wrongly promote file-only keys into the winners set and then
// freeze them against subsequent file edits.)
func (c *Config) Reload(envFile string) (ReloadResult, error) {
	if c.reload == nil {
		return ReloadResult{}, fmt.Errorf("config was not produced by Load; reload unavailable")
	}
	// Serialize whole reloads so two triggers (SIGUSR2 + HTTP) can't interleave
	// their process-env edits.
	c.reload.serialize.Lock()
	defer c.reload.serialize.Unlock()

	if envFile != "" {
		if err := loadEnvFileFiltered(envFile, func(name string) bool { return !c.reload.envExcluded(name) }); err != nil && !os.IsNotExist(err) {
			return ReloadResult{}, fmt.Errorf("reload env file %s: %w", envFile, err)
		}
	}
	for k, v := range c.reload.bootEnv {
		if c.reload.envExcluded(k) {
			continue
		}
		_ = os.Setenv(k, v)
	}

	result := ReloadResult{
		ReloadedAt: time.Now().UTC(),
		Changed:    []FieldChange{},
		Skipped:    []SkippedField{},
		Errors:     []ReloadError{},
	}

	c.restoreBootSecrets(&result)

	c.reload.mu.Lock()
	c.applyReloadableLocked(&result)
	c.reload.mu.Unlock()

	c.collectSkipped(&result)
	return result, nil
}

// applyReloadableLocked re-resolves each reloadable field and applies it. The
// caller MUST hold c.reload.mu for writing. Resolution mirrors the exact loader
// call for each field so reload and boot agree on precedence and defaults; the
// parse + range rules come from the shared envKnobs registry (knobs.go), so a
// value boot would reject is exactly the value reload rejects (#1119).
func (c *Config) applyReloadableLocked(result *ReloadResult) {
	reloadFleetFloat(result, "MAX_COST_USD", c.MaxCostUSD, func(v float64) { c.MaxCostUSD = v })
	reloadFleetInt(result, "MAX_TOTAL_TOKENS", c.MaxTotalTokens, func(v int) { c.MaxTotalTokens = v })
	reloadFleetInt(result, "MAX_ITERATIONS", c.MaxIterations, func(v int) { c.MaxIterations = v })
	// One temperature knob covers interactive and scheduled sampling (#1079);
	// the FLEET_/CHAT_/CUTLASS_ chain still picks up a legacy CUTLASS_TEMPERATURE
	// spelling as the last-resort alias.
	reloadFleetFloat(result, "TEMPERATURE", c.Temperature, func(v float64) { c.Temperature = v })
}

// restoreBootSecrets re-pins every registered per-request auth-secret env var
// to its boot value after the env-file re-apply (#584). Webhook/Slack signing
// secrets are read via os.Getenv on every verification, so a drifted .env copy
// would otherwise rotate a live secret mid-run — inbound webhooks start
// failing signature checks, an outage the operator never requested. Auth
// secrets are documented non-reloadable: the boot value (set OR unset) stays
// live and the drift is reported in Skipped (restart to rotate).
func (c *Config) restoreBootSecrets(result *ReloadResult) {
	for k, boot := range c.reload.bootSecrets {
		cur, ok := os.LookupEnv(k)
		if (boot == nil && !ok) || (boot != nil && ok && cur == *boot) {
			continue // no drift
		}
		if boot == nil {
			_ = os.Unsetenv(k)
		} else {
			_ = os.Setenv(k, *boot)
		}
		result.Skipped = append(result.Skipped, SkippedField{
			Key:    k,
			Reason: "per-request auth secret (webhook/Slack signing); the boot value stays live — restart to rotate",
		})
	}
}

// collectSkipped reports any watched non-reloadable setting whose resolved value
// differs from its boot snapshot (including set↔unset transitions).
func (c *Config) collectSkipped(result *ReloadResult) {
	for _, w := range nonReloadableWatched {
		cur, ok := w.lookup()
		boot, hadBoot := c.reload.bootVals[w.key]
		changed := (ok && (!hadBoot || cur != boot)) || (!ok && hadBoot)
		if changed {
			result.Skipped = append(result.Skipped, SkippedField{Key: w.key, Reason: w.reason})
		}
	}
}

// reloadFleetFloat resolves a FLEET_/CHAT_/CUTLASS_-prefixed float setting
// through the shared envKnobs registry (#1119) — the SAME parse + range rules
// boot enforces. When the value is set, well-formed, and differs from cur, it
// applies it via set and records the change; a parse/range failure records an
// Error and keeps cur; an unset (or blank) var is a no-op (the running value
// is retained).
func reloadFleetFloat(result *ReloadResult, suffix string, cur float64, set func(float64)) {
	raw, ok := lookupFleet(suffix)
	if !ok {
		return
	}
	key := canonicalPrefix + suffix
	k := envKnobByKey[key]
	// Mirror loadParser.knob's split so a registry bug names its actual shape.
	if k == nil {
		result.Errors = append(result.Errors, ReloadError{Key: key, Error: "BUG: reloadable knob missing from the envKnobs registry; add an entry in knobs.go"})
		return
	}
	if k.kind != kindFloat {
		result.Errors = append(result.Errors, ReloadError{Key: key, Error: fmt.Sprintf("BUG: envKnobs registry kind mismatch (registry %s, reload reads float); fix the entry in knobs.go", k.kind.label())})
		return
	}
	nv, isSet, err := k.parseFloat(raw)
	if err != nil {
		result.Errors = append(result.Errors, ReloadError{Key: key, Error: err.Error()})
		return
	}
	if isSet && nv != cur {
		result.Changed = append(result.Changed, FieldChange{Key: key, Old: formatFloat(cur), New: formatFloat(nv)})
		set(nv)
	}
}

// reloadFleetInt resolves a FLEET_/CHAT_/CUTLASS_-prefixed int setting through
// the shared envKnobs registry. Semantics otherwise match reloadFleetFloat.
func reloadFleetInt(result *ReloadResult, suffix string, cur int, set func(int)) {
	raw, ok := lookupFleet(suffix)
	if !ok {
		return
	}
	key := canonicalPrefix + suffix
	k := envKnobByKey[key]
	// Mirror loadParser.knob's split so a registry bug names its actual shape.
	if k == nil {
		result.Errors = append(result.Errors, ReloadError{Key: key, Error: "BUG: reloadable knob missing from the envKnobs registry; add an entry in knobs.go"})
		return
	}
	if k.kind != kindInt {
		result.Errors = append(result.Errors, ReloadError{Key: key, Error: fmt.Sprintf("BUG: envKnobs registry kind mismatch (registry %s, reload reads int); fix the entry in knobs.go", k.kind.label())})
		return
	}
	nv, isSet, err := k.parseInt(raw)
	if err != nil {
		result.Errors = append(result.Errors, ReloadError{Key: key, Error: err.Error()})
		return
	}
	if isSet && nv != cur {
		result.Changed = append(result.Changed, FieldChange{Key: key, Old: strconv.Itoa(cur), New: strconv.Itoa(nv)})
		set(nv)
	}
}

func formatFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
