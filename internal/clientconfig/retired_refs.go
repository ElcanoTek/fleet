package clientconfig

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/ElcanoTek/fleet/internal/config/envreg"
)

// ADR-0106: a CHAT_/CUTLASS_ (or bare DATA_DIR) spelling of a FLEET_ setting
// was removed. Every lookup reads one as unset (envreg), so in the manifest it
// behaves exactly like any unset variable: ${X:-d} yields d, and a required
// reference fails the load with the rename spelled out (retiredRefError).
// `fleet update` refuses a bundle that still references one BEFORE installing
// (`fleet validate-config --retired-only`), so an upgrade cannot take a box
// down over it. This file is the ONE read-only scanner behind that refusal,
// `fleet doctor`, `fleet validate-config` and the load-time warnings.

// envNameFields are the typed manifest fields — by SCHEMA PATH, a list
// written "[]" — whose VALUE is the name of an environment variable fleet
// reads: the fields literalEnvVarNames admits to the .env allowlist and the
// readers resolve through envreg (a provider's API key, a connector's enable
// gates and account/identity variables, a webhook trigger's signing secret).
// It is the ONE list: the scanner checks literal names at exactly these
// paths (never at a same-named key inside an opaque pass-through section),
// and TestEnvNameFieldsCoverTheSchema walks the manifest types and fails when
// one gains a *_env / *_vars field that is in neither this list nor
// envKeyFields, or when a listed path no longer exists.
var envNameFields = map[string]bool{
	"providers[].api_key_env":             true,
	"mcp_servers[].enabled_env":           true,
	"mcp_servers[].enabled_groups":        true, // lists of names
	"mcp_servers[].account_vars":          true,
	"mcp_servers[].identity_env":          true, // their <VAR>_<ACCOUNT> forms are read
	"webhook_triggers[].hmac_secret_env":  true,
	"webhook_triggers[].token_secret_env": true,
}

// envKeyFields are *_env fields (by schema path) whose values are KEYS of a
// connector's own env map — the wire names fleet hands to the MCP server —
// not variables fleet reads, so a CHAT_/CUTLASS_ value there is legitimate.
var envKeyFields = map[string]string{
	"mcp_servers[].optional_env": "names keys of the connector's env map to drop when empty; never read from fleet's environment",
}

// RetiredRef is one use of a removed spelling in a client bundle manifest.
type RetiredRef struct {
	Name      string // the removed spelling
	Canonical string // the FLEET_ name to use instead
	Path      string // the manifest field, e.g. "mcp_servers[0].url"
	// Literal: the field's VALUE names the variable (api_key_env, enabled_env,
	// …) rather than a ${...} reference.
	Literal bool
}

// String is the one-line report. It never contains a value.
func (r RetiredRef) String() string {
	if r.Literal {
		return fmt.Sprintf("manifest.yaml %s names the removed variable %s; rename it to %s (ADR-0106)", r.Path, r.Name, r.Canonical)
	}
	return fmt.Sprintf("manifest.yaml %s references the removed variable ${%s}; rename it to ${%s} (ADR-0106)", r.Path, r.Name, r.Canonical)
}

// RetiredManifestRefs reads the manifest of the bundle in dir and returns every
// use of a removed spelling: each ${...} reference anywhere in it (a whole
// field or embedded in a larger string, in any section — servers, tools,
// personas, pricing, sandbox, and opaque pass-through blocks too, because the
// loader interpolates the whole manifest text), and each literal variable
// name at an envNameFields schema path. Read-only: it loads nothing, applies no env file and
// writes nothing. A missing manifest yields none. Sorted by path.
func RetiredManifestRefs(dir string) ([]RetiredRef, error) {
	if strings.TrimSpace(dir) == "" {
		dir = Dir()
	}
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.yaml")) // #nosec G304 — operator-supplied bundle path.
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return retiredRefsIn(raw)
}

// retiredRefsIn is RetiredManifestRefs over raw manifest bytes.
func retiredRefsIn(raw []byte) ([]RetiredRef, error) {
	sites, _, err := scanManifestEnvRefs(raw)
	if err != nil {
		return nil, err
	}
	var out []RetiredRef
	seen := map[string]bool{}
	add := func(r RetiredRef) {
		k := r.Path + "\x00" + r.Name
		if !seen[k] {
			seen[k] = true
			out = append(out, r)
		}
	}
	for _, s := range sites {
		if c, ok := envreg.RetiredAlias(s.ref.name); ok {
			add(RetiredRef{Name: s.ref.name, Canonical: c, Path: s.path})
		}
	}
	var tree any
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		return nil, err
	}
	walkEnvNameFields(tree, nil, func(path, name string) {
		if c, ok := envreg.RetiredAlias(name); ok {
			add(RetiredRef{Name: name, Canonical: c, Path: path, Literal: true})
		}
	})
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// walkEnvNameFields calls fn for every string at (or, for lists of lists,
// under) an envNameFields schema path, with its rendered path. The schema
// path drops list indices ("mcp_servers[].enabled_env"), so a same-named key
// anywhere else — inside an opaque pass-through block — never matches.
func walkEnvNameFields(node any, segs []string, fn func(path, name string)) {
	var collect func(n any, segs []string)
	collect = func(n any, segs []string) {
		switch v := n.(type) {
		case []any:
			for i, c := range v {
				collect(c, append(append([]string(nil), segs...), fmt.Sprintf("[%d]", i)))
			}
		case string:
			fn(renderManifestPath(segs), strings.TrimSpace(v))
		}
	}
	var walk func(n any, segs []string, schema string)
	walk = func(n any, segs []string, schema string) {
		switch v := n.(type) {
		case map[string]any:
			keys := make([]string, 0, len(v))
			for k := range v {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				kid := append(append([]string(nil), segs...), k)
				ks := k
				if schema != "" {
					ks = schema + "." + k
				}
				if envNameFields[ks] {
					collect(v[k], kid)
					continue
				}
				walk(v[k], kid, ks)
			}
		case []any:
			for i, c := range v {
				walk(c, append(append([]string(nil), segs...), fmt.Sprintf("[%d]", i)), schema+"[]")
			}
		}
	}
	walk(node, segs, "")
}

// warnRetiredManifestRefs logs each use at bundle load. A required ${...}
// reference also fails the load (with the same rename); the warning covers
// the uses that do not — a :- default, a lazily resolved connector env or
// header value, a literal env-name field (read as unset: no credential, the
// gate stays closed).
func warnRetiredManifestRefs(manifestPath string, raw []byte) {
	refs, err := retiredRefsIn(raw)
	if err != nil {
		return // the load reports the parse error itself
	}
	for _, r := range refs {
		slog.Warn("clientconfig: retired manifest reference — fleet never reads it, so it counts as unset", "manifest", manifestPath, "ref", r)
	}
}

// RetiredManifestRefWarnings is RetiredManifestRefs rendered one line each.
func RetiredManifestRefWarnings(dir string) ([]string, error) {
	refs, err := RetiredManifestRefs(dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.String())
	}
	return out, nil
}
