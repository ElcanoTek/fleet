package clientconfig

// Manifest-wide ${VAR} reference handling (#1123).
//
// interpolateManifest expands env references over the RAW manifest bytes, so a
// reference can live in ANY field — url:, command:/args:, sandbox.image,
// providers[].base_url, branding — not only the connector env/header maps.
// Two consequences used to be silent:
//
//   - the pass ran BEFORE config.Load applied the FLEET_ENV_FILE env file, so
//     a ${VAR}/${VAR:-default} outside the lazily-resolved connector maps
//     interpolated against the pre-.env environment (baking the default, or
//     leaving a literal token nothing ever re-resolves);
//   - EnvVarNames inventoried references from env/header VALUES only, so a
//     ${VAR} in url: was never registered with the .env allowlist.
//
// This file closes both: Load scans the raw manifest for every reference (and
// where it lives), registers the names with config's .env allowlist, folds the
// env file into the process env BEFORE interpolating, and — after
// interpolation — fails the load for any bare ${VAR} that is still unset in a
// field outside the lazily-resolved connector maps, naming the field and the
// variable. The lazily-resolved maps (mcp_servers[].env/headers,
// http_tools[].headers) keep the fleet#706 contract unchanged: raw through
// load, resolved at catalog-build/spawn time, where an unset credential is
// legitimate.

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/goccy/go-yaml"
)

// ── early env-file application ──

var (
	bootEnvFileOnce sync.Once
	// bootEnvFileResult memoizes the once-per-process application outcome so
	// every Load in the process reports the same boot failure.
	bootEnvFileResult error //nolint:errname // memoized once-result, not a sentinel error value
)

// applyBootEnvFile folds the FLEET_ENV_FILE env file into the process env —
// once per process, before the first bundle's manifest interpolation — via
// config.BootstrapEnvFile, which applies Load's exact admission and precedence
// rules (allowlist-gated, process env wins, missing file is not an error).
// config.Load's own application afterwards is an idempotent re-read of the
// same file.
//
// The env-file PATH comes only from the process env (FLEET_ENV_FILE), never
// from the bundle, so there is no load-order circularity. Once per process —
// not per Load — because two callers rely on the boot snapshot staying fixed:
//
//   - the MCP broker child re-loads the bundle on catalog reload, and
//     credential env changes deliberately require a process restart — a reload
//     must not re-read a rotated env file;
//   - the parent process scrubs connector env after the broker boots, and a
//     later bundle load in the parent must never re-introduce those secrets.
func applyBootEnvFile() error {
	bootEnvFileOnce.Do(func() {
		bootEnvFileResult = config.BootstrapEnvFile(os.Getenv("FLEET_ENV_FILE"))
	})
	return bootEnvFileResult
}

// resetBootEnvFileForTest re-arms the once-per-process env-file application so
// package tests can exercise Load against fresh FLEET_ENV_FILE fixtures.
func resetBootEnvFileForTest() {
	bootEnvFileOnce = sync.Once{}
	bootEnvFileResult = nil
}

// ── reference scanning ──

// envRefForm classifies one ${...} spelling, mirroring expandExpr's parse.
type envRefForm int

const (
	envRefBare     envRefForm = iota // ${VAR} — substitutes when set, defers when unset
	envRefDefault                    // ${VAR:-default}
	envRefRequired                   // ${VAR:?message}
)

// envRef is one ${...} reference found in raw manifest text.
type envRef struct {
	name  string // trimmed variable name (for an unsupported ':X' op: the whole trimmed expression, matching expandExpr's fall-through)
	form  envRefForm
	token string // the original "${...}" spelling, for error messages
	body  string // the raw default body of a ${VAR:-...} form (empty otherwise) — checked for nested references
}

// envRefsIn scans raw manifest text with the interpolator's exact escape and
// brace rules ("$${" escapes without expanding; nested braces in a default
// body are balanced) and returns every ${...} reference in order. An
// unterminated "${" ends the scan — interpolateManifest is the pass that
// reports it as an error.
func envRefsIn(value string) []envRef {
	var out []envRef
	for i := 0; i < len(value); {
		if strings.HasPrefix(value[i:], "$${") {
			i += 3
			continue
		}
		if !strings.HasPrefix(value[i:], "${") {
			i++
			continue
		}
		end, ok := matchBrace(value, i+1)
		if !ok {
			return out
		}
		expr := value[i+2 : end]
		ref := envRef{form: envRefBare, name: strings.TrimSpace(expr), token: value[i : end+1]}
		if idx := strings.IndexByte(expr, ':'); idx >= 0 && idx+1 < len(expr) {
			switch expr[idx+1] {
			case '-':
				ref.form, ref.name, ref.body = envRefDefault, strings.TrimSpace(expr[:idx]), expr[idx+2:]
			case '?':
				ref.form, ref.name = envRefRequired, strings.TrimSpace(expr[:idx])
			}
			// Any other ':X' falls through as a bare whole-expression name,
			// exactly like expandExpr — it can only ever defer.
		}
		out = append(out, ref)
		i = end + 1
	}
	return out
}

// manifestRefSite is one ${...} reference at a specific manifest location.
type manifestRefSite struct {
	path string // rendered field path, e.g. "mcp_servers[0].url"
	ref  envRef
	lazy bool // inside a lazily-resolved connector map (env/headers) — resolved at catalog-build time, unset is legitimate
	// reservedOK marks an mcp_servers env VALUE — the only location the MCP
	// spawn paths substitute the reserved runtime tokens
	// (agentcore.ExpandWorkspaceEnv / ExpandTaskIDEnv operate on the env map
	// alone; header values pass through interpolate() verbatim, so a reserved
	// token there would ship on the wire as a literal forever).
	reservedOK bool
}

// scanManifestEnvRefs parses the RAW (pre-interpolation) manifest bytes into a
// generic YAML tree and returns every ${...} reference with its field path,
// plus the sorted, deduplicated variable names for the .env allowlist
// (reserved runtime tokens excluded). Scanning the raw bytes matters twice
// over: an already-exported value would be substituted out of the interpolated
// manifest, and an escaped "$${...}" literal would become indistinguishable
// from a deferred reference after the pass collapses the escape. A raw parse
// failure is a load error: the strict unmarshal in Load runs on the
// INTERPOLATED bytes, where a substituted value can repair the syntax — which
// would silently skip both the allowlist registration and the
// unresolved-reference check for that bundle.
func scanManifestEnvRefs(raw []byte) ([]manifestRefSite, []string, error) {
	var tree any
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		return nil, nil, err
	}
	var sites []manifestRefSite
	walkManifestNode(tree, nil, &sites)
	seen := map[string]bool{}
	var names []string
	for _, site := range sites {
		name := site.ref.name
		if name == "" || reservedRuntimeVar(name) || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	return sites, names, nil
}

// defaultOnlyRefNames returns the sorted names whose EVERY ${...} occurrence
// in the manifest carries a default (${VAR:-...}). Such a name always has a
// manifest-supplied fallback, so its absence from the environment is
// configuration with the default in effect, not a missing credential —
// `fleet validate-config` reports the two differently (via
// EnvVarNamesDefaultOnly, which additionally excludes literally-named vars).
// Reserved tokens and empty names are excluded, matching the allowlist
// inventory.
func defaultOnlyRefNames(sites []manifestRefSite) []string {
	candidates := map[string]bool{}
	disqualified := map[string]bool{}
	for _, site := range sites {
		name := site.ref.name
		if name == "" || reservedRuntimeVar(name) {
			continue
		}
		if site.ref.form == envRefDefault {
			candidates[name] = true
		} else {
			disqualified[name] = true
		}
	}
	var out []string
	for name := range candidates {
		if !disqualified[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// walkManifestNode visits every string scalar (map values AND keys — the
// interpolator rewrites both) in the generic YAML tree, recording ${...}
// references with their rendered path. Map keys are walked in sorted order so
// error output is deterministic.
func walkManifestNode(node any, segs []string, out *[]manifestRefSite) {
	switch v := node.(type) {
	case string:
		recordManifestRefs(v, segs, out)
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			kidSegs := append(append([]string(nil), segs...), k)
			recordManifestRefs(k, kidSegs, out)
			walkManifestNode(v[k], kidSegs, out)
		}
	case []any:
		for i, item := range v {
			kidSegs := append(append([]string(nil), segs...), fmt.Sprintf("[%d]", i))
			walkManifestNode(item, kidSegs, out)
		}
	default:
		// Non-string scalars (int/bool/float/nil/time) carry no manifest text.
	}
}

func recordManifestRefs(value string, segs []string, out *[]manifestRefSite) {
	if !strings.Contains(value, "${") {
		return
	}
	refs := envRefsIn(value)
	if len(refs) == 0 {
		return
	}
	path := renderManifestPath(segs)
	lazy := lazyResolvedManifestPath(segs)
	reservedOK := reservedSubstitutablePath(segs)
	for _, ref := range refs {
		*out = append(*out, manifestRefSite{path: path, ref: ref, lazy: lazy, reservedOK: reservedOK})
	}
}

// lazyResolvedManifestPath reports whether the path lies inside a connector
// map whose values deliberately keep their raw ${...} text through Load and
// resolve against the live process env at catalog-build/spawn time
// (resolveEnvMap): mcp_servers[i].env, mcp_servers[i].headers, and
// http_tools[i].headers. An unset bare reference there is legitimate — the
// server gates off, or optional_env drops the key.
func lazyResolvedManifestPath(segs []string) bool {
	if len(segs) < 3 || !strings.HasPrefix(segs[1], "[") {
		return false
	}
	switch segs[0] {
	case "mcp_servers":
		return segs[2] == "env" || segs[2] == "headers"
	case "http_tools":
		return segs[2] == "headers"
	}
	return false
}

// reservedSubstitutablePath reports whether the path is an mcp_servers env
// entry — the ONLY location the MCP spawn paths substitute the reserved
// runtime tokens (agentcore.ExpandWorkspaceEnv / ExpandTaskIDEnv operate on
// the env map alone). A reserved token anywhere else — INCLUDING the
// lazily-resolved header maps, whose spawn-time interpolate() preserves it
// verbatim — can never resolve and fails the load.
func reservedSubstitutablePath(segs []string) bool {
	return len(segs) >= 3 && strings.HasPrefix(segs[1], "[") &&
		segs[0] == "mcp_servers" && segs[2] == "env"
}

// hasUnescapedRef reports whether s contains a "${" that the interpolator's
// tokenizer would treat as the start of a reference (an "$${" escape does
// not count).
func hasUnescapedRef(s string) bool {
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], "$${") {
			i += 3
			continue
		}
		if strings.HasPrefix(s[i:], "${") {
			return true
		}
		i++
	}
	return false
}

// renderManifestPath renders path segments the way an operator reads the
// manifest: "mcp_servers[0].url", "sandbox.image", "providers[1].base_url".
func renderManifestPath(segs []string) string {
	if len(segs) == 0 {
		return "(document)"
	}
	var sb strings.Builder
	for i, seg := range segs {
		if i > 0 && !strings.HasPrefix(seg, "[") {
			sb.WriteByte('.')
		}
		sb.WriteString(seg)
	}
	return sb.String()
}

// unresolvedManifestRefs returns one problem line per ${...} reference that
// interpolation would leave as a literal token nothing ever re-resolves. Call
// it AFTER applyBootEnvFile so the env file counts. Three problem classes:
//
//   - an unset bare ${VAR} outside the lazily-resolved connector maps (a
//     ${VAR:-default} resolves to its default, an unset ${VAR:?msg} already
//     failed the load in interpolateManifest, and env/header values
//     re-resolve at catalog-build time);
//   - a bare reserved runtime token anywhere except an mcp_servers env value
//     — the MCP spawn paths substitute it only there, so even in the lazy
//     header maps it would ship on the wire verbatim;
//   - a ${VAR:-default} outside the lazy maps whose default body nests an
//     unescaped ${...} — the interpolator never expands a default body
//     (matchBrace only balances it), so the inner reference would ship as a
//     literal whenever the outer var is unset, and its name never reaches
//     the .env allowlist. Rejected as a spelling, set or unset: a config
//     that only breaks when the env changes is a landmine.
func unresolvedManifestRefs(sites []manifestRefSite) []string {
	var problems []string
	for _, site := range sites {
		if site.ref.form == envRefDefault && !site.lazy && hasUnescapedRef(site.ref.body) {
			problems = append(problems, fmt.Sprintf(
				"%s: %s nests an environment reference inside its default value; the interpolator never expands a default body, so the inner ${...} would ship as a literal — restructure the value, or write $${...} if the literal is intended",
				site.path, site.ref.token))
			continue
		}
		if site.ref.form != envRefBare {
			continue
		}
		if reservedRuntimeVar(site.ref.name) {
			if site.reservedOK {
				continue
			}
			problems = append(problems, fmt.Sprintf(
				"%s: %s is a reserved runtime token that fleet substitutes only in mcp_servers env values; it can never resolve in this field (see docs/MCP-BUNDLE-ENV.md)",
				site.path, site.ref.token))
			continue
		}
		if site.lazy {
			continue
		}
		if _, ok := lookupNonEmpty(site.ref.name); !ok {
			problems = append(problems, fmt.Sprintf(
				"%s: ${%s} is unset or empty in the process environment and the FLEET_ENV_FILE env file; set the variable, use ${%s:-default} for an optional value, or write $${...} for a literal",
				site.path, site.ref.name, site.ref.name))
		}
	}
	sort.Strings(problems)
	return slices.Compact(problems)
}
