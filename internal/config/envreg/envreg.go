// Package envreg is fleet's configuration registry (#2053): the ONE place an
// environment variable is declared, and the ONE way code outside
// internal/config reads one.
//
// Every setting fleet, its operator CLI, its harness binaries or its scripts
// read from the environment has exactly one Entry in the table (table.go):
// its canonical name, type, documented default, description, section, scope,
// whether it is a secret, its alias spellings (the bare PERSONA ones), whether
// the deployment env file may set it, the Helm
// value that sets it, and — for a typed knob the boot gate validates — its
// bounds. Everything else is derived:
//
//   - internal/config's env-file allowlist (allowedEnvVars);
//   - internal/config's typed-knob table (envKnobs: kind, bounds, scope);
//   - docs/CONFIGURATION.md and the config.env part of the Helm chart's
//     values.schema.json (both generated, both drift-checked by tests);
//   - `fleet config explain`, and the warnings boot and `fleet doctor` print
//     for a removed CHAT_/CUTLASS_ spelling (RetiredAlias, ADR-0106).
//
// # Reading the environment
//
// Get / Lookup read a REGISTERED name (canonical or alias). Reading a name
// that is not in the table is a programming error: in a test binary it
// panics, so the suite fails on the first read of an undeclared key; in a
// production binary it is logged once per name (and the value is still
// returned, so a missed registration can never take a running deployment
// down). TestNoUnregisteredLiteralReads walks every non-test Go file and
// fails on a literal key passed to Get/Lookup that is not registered, so the
// common case is caught at `go test` time without having to execute the read.
//
// Unregistered / LookupUnregistered read a name that comes from DATA, not
// code — a variable the client bundle's manifest names (connector
// credentials, webhook signing secrets, provider API keys, ${VAR}
// interpolation), a per-account suffix variant, or a name an operator passes
// on a flag. Those names cannot be declared here (fleet is client-agnostic);
// the bundle documents them. Passing a string literal to either is a test
// failure: a literal name belongs in the table.
//
// os.Getenv / os.LookupEnv / os.Environ are banned outside internal/config by
// a forbidigo rule in .golangci.yml; its short exception list is the
// wholesale environment copies (subprocess env plumbing and the secret
// redactors' value snapshots) and tests.
package envreg

import (
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
)

// CanonicalPrefix is the prefix every fleet-namespaced setting is spelled
// with.
const CanonicalPrefix = "FLEET_"

// RetiredPrefixes are the chat-server's and cutlass's prefixes. Until
// ADR-0106 a FLEET_<X> setting could also be spelled CHAT_<X> or CUTLASS_<X>;
// nothing reads those spellings any more. They are kept only so a leftover
// one is reported (RetiredAlias) instead of silently ignored, and so the MCP
// broker keeps a stale copy of a parent-owned secret away from connectors.
var RetiredPrefixes = []string{"CHAT_", "CUTLASS_"}

// retiredBare maps the removed spellings that are not CHAT_/CUTLASS_ +
// suffix to their canonical name.
var retiredBare = map[string]string{
	"DATA_DIR": "FLEET_DATA_DIR", // read by `fleet sched apikey` until ADR-0106
}

// Type is the value type of a setting, as the docs describe it. The typed
// knob kinds (Int … StrictBool) are the ones internal/config's knob parser
// understands; the rest are documentation types for string-valued settings.
type Type string

// Setting value types.
const (
	TypeString     Type = "string"
	TypeInt        Type = "int"
	TypeInt64      Type = "int64"
	TypeFloat      Type = "float"
	TypeBool       Type = "bool"        // 1/0, true/false, yes/no, on/off (case-insensitive)
	TypeStrictBool Type = "strict-bool" // strconv.ParseBool tokens only: 1/0/t/f/true/false
	TypeDuration   Type = "duration"    // Go duration: 90s, 5m, 1h30m
	TypeURL        Type = "url"
	TypeDSN        Type = "dsn" // a database connection string (always Secret; explain shows it with the credentials removed)
	TypeCSV        Type = "csv" // comma-separated list
	TypePath       Type = "path"
	TypeEnum       Type = "enum"   // one of Entry.Enum
	TypeBase64     Type = "base64" // standard base64 (keys)
)

// Scope says which process reads a setting.
type Scope string

// Setting scopes.
const (
	// ScopeServer: read by `fleet serve` (the server process, including its
	// in-process browser gateway, scheduler and agent runtime).
	ScopeServer Scope = "server"
	// ScopeCLI: read only by the operator CLI verbs (`fleet status`,
	// `fleet update`, `fleet chat`, …), never by the server.
	ScopeCLI Scope = "cli"
	// ScopeSandbox: shapes the sandbox (image, limits, backend). Read
	// host-side by the server; never shipped INTO a sandbox.
	ScopeSandbox Scope = "sandbox"
	// ScopeBroker: passed through to MCP servers by the credential broker
	// (connector wire keys); fleet itself does not interpret the value.
	ScopeBroker Scope = "broker"
	// ScopeScripts: read only by the shell scripts under scripts/
	// (bootstrap, update, doctor, backup) or the harness binaries.
	ScopeScripts Scope = "scripts"
	// ScopeTest: read only by tests and test harnesses.
	ScopeTest Scope = "test"
)

// Alias is another spelling of a setting.
type Alias struct {
	// Name is the alias spelling.
	Name string
	// EnvFile reports whether fleet's own env-file loader admits this
	// spelling. One it does not admit still reaches the server when something
	// exports it into the process environment — on the standard install,
	// systemd's EnvironmentFile= exports every line of the env file.
	EnvFile bool
}

// Knob marks an entry as a typed knob the boot gate validates
// (internal/config/knobs.go): a set value that does not parse as Type, or
// falls outside [Min, Max], refuses to boot. Only the knob kinds (int,
// int64, float, bool, strict-bool, duration) may carry one.
type Knob struct {
	// Min / Max are optional inclusive bounds.
	Min, Max *float64
	// ReadBy names the consumer of a knob Load does NOT consume (parsed at
	// its point of use elsewhere in the tree). Empty means config.Load
	// consumes it.
	ReadBy string
	// Lenient marks a knob whose consumer deliberately absorbs a malformed
	// value: boot does not refuse, validate-config reports an advisory. Why
	// is required with it.
	Lenient bool
	Why     string
}

// Entry declares one setting.
type Entry struct {
	// Name is the canonical spelling.
	Name string
	// Type is the value type.
	Type Type
	// Default is the documented default ("" = unset/empty). Prose in
	// parentheses means the default is not a value an operator could write
	// (decided at run time); such text must not name another setting — use
	// DefaultFrom for that (TestDerivedDefaultsAreStructured).
	Default string
	// DefaultFrom names the setting (canonical) this one INHERITS its default
	// from when no spelling of it is set: FLEET_METADATA_MODEL falls back to
	// FLEET_TITLE_MODEL's effective value. The parent is resolved the same
	// way (its own spellings, then its own inheritance, then its Default).
	DefaultFrom string
	// DefaultDerive turns the inherited value into this setting's default
	// (nil: the inherited value as is). ok=false falls back to Default.
	DefaultDerive func(inherited string) (string, bool)
	// DefaultFromCanonical: the reader inherits only the parent's CANONICAL
	// spelling read verbatim from the environment — not its aliases, its own
	// inheritance or its Default (the browser gateway reads FLEET_SERVER_TOKEN
	// with a plain lookup).
	DefaultFromCanonical bool
	// Description says what the setting does, in one or two sentences.
	Description string
	// Group is the docs section (one of the Group* constants).
	Group string
	// Scope says which process reads it.
	Scope Scope
	// Secret marks a credential or signing secret: always redacted by
	// `fleet config explain`.
	Secret bool
	// ProcessOnly: the deployment env file does not admit this name; it is
	// read from the process environment only (e.g. FLEET_ENV_FILE itself,
	// values systemd or Kubernetes inject).
	ProcessOnly bool
	// Aliases lists alias spellings, in resolution order after the canonical
	// name.
	Aliases []Alias
	// Enum lists the accepted values of a TypeEnum setting.
	Enum []string
	// Knob, when set, makes this a boot-validated typed knob.
	Knob *Knob
	// Helm names how the Helm chart sets this (a values.yaml path, or a
	// fixed value / the downward API in parentheses); empty when the chart
	// does not set it (operators pass it through config.env or
	// config.existingSecret).
	Helm string
}

// FileSettable reports whether the deployment env file admits the canonical
// spelling.
func (e *Entry) FileSettable() bool { return !e.ProcessOnly }

// Spellings returns every spelling of e in resolution order: the canonical
// name, then each alias.
func (e *Entry) Spellings() []string {
	out := make([]string, 0, 1+len(e.Aliases))
	out = append(out, e.Name)
	for _, a := range e.Aliases {
		out = append(out, a.Name)
	}
	return out
}

// registry is the built table: entries with derived aliases, and an index
// from every spelling to its entry.
type registry struct {
	entries []Entry
	byName  map[string]int // canonical name → index
	byAlias map[string]int // alias spelling → index
}

var reg = build(table)

// build indexes every spelling. A malformed table (duplicate spelling, a
// removed CHAT_/CUTLASS_ alias spelling, knob on a non-knob type) panics at
// package init: it is a programming error the first test catches.
func build(in []Entry) *registry {
	r := &registry{
		entries: make([]Entry, len(in)),
		byName:  make(map[string]int, len(in)),
		byAlias: map[string]int{},
	}
	seen := map[string]bool{}
	claim := func(name string) {
		if seen[name] {
			panic("envreg: duplicate spelling " + name)
		}
		seen[name] = true
	}
	for i, e := range in {
		if e.Knob != nil && !e.Type.IsKnobKind() {
			panic(fmt.Sprintf("envreg: %s has a Knob but type %q is not a knob kind", e.Name, e.Type))
		}
		claim(e.Name)
		r.byName[e.Name] = i
		for _, a := range e.Aliases {
			if _, bare := retiredBare[a.Name]; bare || retiredPrefixed(a.Name) {
				panic("envreg: " + e.Name + " declares alias " + a.Name + ", a spelling ADR-0106 removed")
			}
			claim(a.Name)
			r.byAlias[a.Name] = i
		}
		r.entries[i] = e
	}
	for _, e := range r.entries {
		seenFrom := map[string]bool{e.Name: true}
		for p := e.DefaultFrom; p != ""; {
			j, ok := r.byName[p]
			if !ok {
				panic("envreg: " + e.Name + " DefaultFrom names unregistered " + p)
			}
			if seenFrom[p] {
				panic("envreg: DefaultFrom cycle through " + e.Name)
			}
			seenFrom[p] = true
			p = r.entries[j].DefaultFrom
		}
	}
	return r
}

// retiredPrefixed reports whether name is CHAT_<X> or CUTLASS_<X> with a
// non-empty <X>.
func retiredPrefixed(name string) bool {
	for _, p := range RetiredPrefixes {
		if suffix, ok := strings.CutPrefix(name, p); ok && suffix != "" {
			return true
		}
	}
	return false
}

// IsKnobKind reports whether t is a type the typed-knob parser understands.
func (t Type) IsKnobKind() bool {
	switch t {
	case TypeInt, TypeInt64, TypeFloat, TypeBool, TypeStrictBool, TypeDuration:
		return true
	default:
		return false
	}
}

// All returns every entry in table order. The slice is a copy; the entries
// share their Alias/Enum backing arrays with the registry and must not be
// mutated.
func All() []Entry {
	out := make([]Entry, len(reg.entries))
	copy(out, reg.entries)
	return out
}

// Find returns the entry a spelling (canonical or alias) belongs to.
func Find(name string) (Entry, bool) {
	if i, ok := reg.byName[name]; ok {
		return reg.entries[i], true
	}
	if i, ok := reg.byAlias[name]; ok {
		return reg.entries[i], true
	}
	return Entry{}, false
}

// IsCanonical reports whether name is a canonical spelling.
func IsCanonical(name string) bool {
	_, ok := reg.byName[name]
	return ok
}

// IsRegistered reports whether name is any registered spelling.
func IsRegistered(name string) bool {
	_, c := reg.byName[name]
	_, a := reg.byAlias[name]
	return c || a
}

// AliasOf returns the Alias record and its entry when name is an alias
// spelling.
func AliasOf(name string) (Alias, Entry, bool) {
	i, ok := reg.byAlias[name]
	if !ok {
		return Alias{}, Entry{}, false
	}
	e := reg.entries[i]
	for _, a := range e.Aliases {
		if a.Name == name {
			return a, e, true
		}
	}
	return Alias{}, Entry{}, false
}

// EnvFileNames returns every spelling the deployment env file admits — the
// canonical name of each FileSettable entry plus each alias with EnvFile —
// sorted. internal/config derives its allowlist from it.
func EnvFileNames() []string {
	var out []string
	for i := range reg.entries {
		e := &reg.entries[i]
		if e.FileSettable() {
			out = append(out, e.Name)
		}
		for _, a := range e.Aliases {
			if a.EnvFile {
				out = append(out, a.Name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// RetiredAlias reports whether name is a spelling ADR-0106 removed, and the
// canonical name to set instead. It is CHAT_<X> or CUTLASS_<X> when FLEET_<X>
// is a registered setting and the name is not itself registered (CHAT_RATE_PER_MIN,
// CUTLASS_ALLOWED_DIRS and the other canonical CHAT_/CUTLASS_ names stay
// settings), or a bare spelling in retiredBare. Nothing reads such a name:
// boot and `fleet doctor` warn when one is set, and `fleet config explain`
// refuses it with the canonical name.
func RetiredAlias(name string) (canonical string, ok bool) {
	if IsRegistered(name) {
		return "", false
	}
	if c, ok := retiredBare[name]; ok {
		return c, true
	}
	for _, p := range RetiredPrefixes {
		if suffix, ok := strings.CutPrefix(name, p); ok && suffix != "" && IsCanonical(CanonicalPrefix+suffix) {
			return CanonicalPrefix + suffix, true
		}
	}
	return "", false
}

// RetiredAliasesOf returns the removed spellings RetiredAlias maps to
// canonical: its CHAT_/CUTLASS_ spellings (unless one is a setting of its
// own), then any bare one, sorted.
func RetiredAliasesOf(canonical string) []string {
	var out []string
	if suffix, ok := strings.CutPrefix(canonical, CanonicalPrefix); ok && suffix != "" {
		for _, p := range RetiredPrefixes {
			if c, ok := RetiredAlias(p + suffix); ok && c == canonical {
				out = append(out, p+suffix)
			}
		}
	}
	var bare []string
	for n, c := range retiredBare {
		if c == canonical {
			bare = append(bare, n)
		}
	}
	sort.Strings(bare)
	return append(out, bare...)
}

// RetiredSpellings returns name plus the CHAT_/CUTLASS_ spellings of the same
// suffix when name is FLEET_<X>, CHAT_<X> or CUTLASS_<X> (canonical first),
// and name alone otherwise. It is generic (any name, registered or not). The
// MCP broker uses it to keep a parent-owned name's stale retired spelling,
// which may still hold the same secret on a box, out of connector env.
func RetiredSpellings(name string) []string {
	suffix, ok := strings.CutPrefix(name, CanonicalPrefix)
	if !ok {
		for _, p := range RetiredPrefixes {
			if suffix, ok = strings.CutPrefix(name, p); ok {
				break
			}
		}
	}
	if !ok || suffix == "" {
		return []string{name}
	}
	out := make([]string, 0, 1+len(RetiredPrefixes))
	out = append(out, CanonicalPrefix+suffix)
	for _, p := range RetiredPrefixes {
		out = append(out, p+suffix)
	}
	return out
}

// Get returns the value of the registered variable name ("" when unset).
// See the package doc for what happens with an unregistered name.
func Get(name string) string {
	v, _ := Lookup(name)
	return v
}

// Lookup is os.LookupEnv for a registered name.
func Lookup(name string) (string, bool) {
	if !IsRegistered(name) {
		unregistered(name, "is not declared in the configuration registry (internal/config/envreg/table.go)")
	}
	return os.LookupEnv(name)
}

// Unregistered reads a variable whose NAME comes from data — the client
// bundle's manifest, a per-account suffix, an operator's flag — and so
// cannot be declared in the table. Never pass it a literal.
func Unregistered(name string) string {
	return os.Getenv(name)
}

// LookupUnregistered is os.LookupEnv for a data-named variable (see
// Unregistered).
func LookupUnregistered(name string) (string, bool) {
	return os.LookupEnv(name)
}

var warned sync.Map

// unregistered reports a read of an undeclared name: a panic in a test
// binary, one log line per name in production.
func unregistered(name, why string) {
	msg := fmt.Sprintf("envreg: environment variable %s %s", name, why)
	if testing.Testing() {
		panic(msg)
	}
	if _, dup := warned.LoadOrStore(name, true); !dup {
		log.Printf("BUG: %s; reading it anyway", msg)
	}
}
