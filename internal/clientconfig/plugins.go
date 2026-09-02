package clientconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Agent Plugins (https://agent-plugins.org, specification v1.0.0) support for
// the client bundle — ADR-0054, docs/AGENT-PLUGINS.md.
//
// An Agent Plugin is a portable directory that packages Agent Skills and MCP
// servers behind one required manifest:
//
//	<plugin>/
//	  plugin.json          # REQUIRED: $schema + name (+ optional metadata)
//	  skills/<name>/SKILL.md   # Agent Skills, exactly the format skills/ already uses
//	  mcp.json             # stdio / streamable-http (/ legacy sse) MCP servers
//	  com.example.client/  # client-owned extension dirs (ignored by fleet)
//
// fleet discovers plugins from two places: every immediate child directory of
// the bundle's `plugins/` dir, plus each directory listed in the manifest's
// `plugin_roots:` (absolute, or relative to the bundle root). A plugin is
// BUNDLE CONTENT: it ships in the same operator-owned checkout as mcp/ and
// skills/, and it inherits exactly the bundle's trust class — a plugin's
// stdio server is launched host-side by the credential broker the way a
// manifest `mcp_servers[]` entry is, and a plugin's skills are files the
// agent reads inside the sandbox. Nothing here adds a new host-side
// exception: the loader only TRANSLATES the portable format into the two
// bundle primitives fleet already governs (ServerDef and the skills tree).
//
// What this file maps, and how:
//
//   - Skills. `skills/*/SKILL.md` is parsed by the same ReadSkills the bundle
//     uses, then materialized into the merged skills tree (builtin_skills.go)
//     between the built-in pack and the bundle's own skills/: a plugin skill
//     overrides a built-in of the same name, the bundle's own skill overrides
//     a plugin's, and between two plugins the first by plugin name wins.
//     Downstream (prompt roster, sandbox mount, /skills API) needs no seam
//     change — the roster path is `skills/<name>/SKILL.md` as for any skill.
//   - MCP servers. Each valid mcp.json entry becomes one ServerDef appended to
//     the catalog: `stdio` → type stdio with the plugin root (or the declared
//     cwd) as the subprocess dir and PLUGIN_ROOT / PLUGIN_DATA set last in its
//     env; `streamable-http` → type http with literal headers. The entry is
//     always-on (the portable format has no credential gate — it MUST NOT
//     carry secrets — so there is nothing to gate on), and it then flows
//     through every existing gate: the child-side scope authorizer
//     (ADR-0042), the critical-tool audit suffixes, tool disclosure, and the
//     connections UI's always-on listing. Legacy `sse` entries are reported
//     and skipped: fleet speaks stdio and Streamable HTTP, which the spec
//     permits (sse support is OPTIONAL).
//
// Failure boundaries follow the specification exactly, because that is what
// makes a plugin PORTABLE: an unknown top-level plugin.json field is reported
// and ignored; a non-object `extensions` is reported and ignored; any other
// manifest violation rejects the whole plugin; a bad skill skips that skill; a
// bad mcp.json top level disables MCP for that plugin only; a bad server entry
// skips that entry only. Every file the loader reads or the broker executes
// must resolve (after symlinks) inside the plugin root — an escaping path is
// rejected at the narrowest boundary (plugin / component / skill / entry).
//
// The portable format has no field for fleet's per-server governance knobs
// (a `tools:` allowlist, a `probe:` canary, the Optional-server metadata), so
// those live where the spec puts client-specific data: the reverse-domain
// extension namespace `com.elcanotek.fleet` in plugin.json (spec §8.1), read
// by parseFleetExtension below and applied to the matching mcp.json entries.
// Every other client ignores that namespace, so a plugin carrying it stays
// portable.
//
// Honest scope: fleet's own `${FLEET_WORKSPACE}` / `${FLEET_TASK_ID}`
// spawn-time tokens are still substituted in a plugin server's env values (a
// documented deviation from the spec's "no other expansion" rule — see
// docs/AGENT-PLUGINS.md); a portable plugin has no reason to write them.

// Canonical schema identifiers for Agent Plugins 1.0.0 — the ONLY versions
// this loader recognizes. Per spec §5.2 a client selects its locally supported
// rules from the identifier and never fetches the schema while loading.
const (
	PluginManifestSchema = "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json"
	PluginMCPSchema      = "https://agent-plugins.org/schemas/1.0.0/mcp.schema.json"
)

// PluginsDirName is the fixed bundle subdirectory scanned for plugins.
const PluginsDirName = "plugins"

// FleetPluginExtensionNamespace is the reverse-domain namespace fleet owns in
// a plugin.json `extensions` object (spec §8.1). Its shape is fleet's to
// define; see fleetServerOverride for what is read. No extension DIRECTORY
// (`com.elcanotek.fleet/` at the plugin root) is read.
const FleetPluginExtensionNamespace = "com.elcanotek.fleet"

// fleetServerOverride is what the com.elcanotek.fleet extension may say about
// one mcp.json server, keyed by the server's mcp.json name under
// `extensions["com.elcanotek.fleet"].mcp_servers`:
//
//	"extensions": {
//	  "com.elcanotek.fleet": {
//	    "mcp_servers": {
//	      "validator": {
//	        "tools": ["validate", "explain"],          // per-server allowlist (ServerDef.Tools)
//	        "probe": {"tool": "validate", "contains": "ok", "args": {}},  // fleet mcp test --deep canary
//	        "optional": true, "enabled_by_default": false, "beta": false,
//	        "display_name": "…", "description": "…", "data_sources": ["s3://…"],
//	        "disabled": false                          // true drops the entry entirely
//	      }
//	    }
//	  }
//	}
//
// These are exactly the manifest ServerDef knobs a plugin author (or the
// bundle operator vendoring a third-party plugin) can set without a
// credential: no env, no gate, no account vars — the portable format forbids
// secrets and fleet does not smuggle them in through the side door. Because
// this is fleet's own namespace, its failure handling is fleet's to choose,
// and it is lenient to match the top level: an unknown key is reported and
// ignored; a wrong-typed override is reported and ignored for THAT server;
// nothing here can reject the plugin.
type fleetServerOverride struct {
	Tools            []string
	Probe            *ProbeDef
	Optional         *bool
	EnabledByDefault *bool
	Beta             *bool
	DisplayName      *string
	Description      *string
	DataSources      []string
	Disabled         bool
}

var fleetServerOverrideFields = map[string]bool{
	"tools": true, "probe": true, "optional": true, "enabled_by_default": true, "beta": true,
	"display_name": true, "description": true, "data_sources": true, "disabled": true,
}

// parseFleetExtension decodes extensions["com.elcanotek.fleet"]. It never
// rejects the plugin: every defect is a problem string and the affected
// override (or the whole extension, for a malformed top level) is ignored.
func parseFleetExtension(raw json.RawMessage) (map[string]fleetServerOverride, []string) {
	prefix := "extensions[" + FleetPluginExtensionNamespace + "]"
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil || top == nil {
		return nil, []string{prefix + " must be an object; ignored"}
	}
	var problems []string
	for k := range top {
		if k != "mcp_servers" {
			problems = append(problems, fmt.Sprintf("%s: unknown key %q ignored", prefix, k))
		}
	}
	rawServers, ok := top["mcp_servers"]
	if !ok {
		return nil, problems
	}
	var servers map[string]json.RawMessage
	if err := json.Unmarshal(rawServers, &servers); err != nil || servers == nil {
		return nil, append(problems, prefix+".mcp_servers must be an object keyed by mcp.json server name; ignored")
	}
	names := make([]string, 0, len(servers))
	for n := range servers {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make(map[string]fleetServerOverride, len(names))
	for _, name := range names {
		label := fmt.Sprintf("%s.mcp_servers[%q]", prefix, name)
		ov, oproblems, err := parseFleetServerOverride(label, servers[name])
		problems = append(problems, oproblems...)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v; override ignored", label, err))
			continue
		}
		out[name] = ov
	}
	return out, problems
}

// parseFleetServerOverride decodes one server's override object.
func parseFleetServerOverride(label string, raw json.RawMessage) (fleetServerOverride, []string, error) {
	var ov fleetServerOverride
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return ov, nil, errors.New("must be an object")
	}
	var problems []string
	for k := range fields {
		if !fleetServerOverrideFields[k] {
			problems = append(problems, fmt.Sprintf("%s: unknown key %q ignored", label, k))
		}
	}
	strList := func(key string) ([]string, error) {
		rawV, ok := fields[key]
		if !ok {
			return nil, nil
		}
		var l []string
		if err := json.Unmarshal(rawV, &l); err != nil {
			return nil, fmt.Errorf("%q must be an array of strings", key)
		}
		return l, nil
	}
	boolPtr := func(key string) (*bool, error) {
		rawV, ok := fields[key]
		if !ok {
			return nil, nil
		}
		var b bool
		if err := json.Unmarshal(rawV, &b); err != nil {
			return nil, fmt.Errorf("%q must be a boolean", key)
		}
		return &b, nil
	}
	strPtr := func(key string) (*string, error) {
		rawV, ok := fields[key]
		if !ok {
			return nil, nil
		}
		var str string
		if err := json.Unmarshal(rawV, &str); err != nil {
			return nil, fmt.Errorf("%q must be a string", key)
		}
		return &str, nil
	}
	var err error
	if ov.Tools, err = strList("tools"); err != nil {
		return ov, problems, err
	}
	if ov.DataSources, err = strList("data_sources"); err != nil {
		return ov, problems, err
	}
	if ov.Optional, err = boolPtr("optional"); err != nil {
		return ov, problems, err
	}
	if ov.EnabledByDefault, err = boolPtr("enabled_by_default"); err != nil {
		return ov, problems, err
	}
	if ov.Beta, err = boolPtr("beta"); err != nil {
		return ov, problems, err
	}
	disabled, err := boolPtr("disabled")
	if err != nil {
		return ov, problems, err
	}
	ov.Disabled = disabled != nil && *disabled
	if ov.DisplayName, err = strPtr("display_name"); err != nil {
		return ov, problems, err
	}
	if ov.Description, err = strPtr("description"); err != nil {
		return ov, problems, err
	}
	if rawP, ok := fields["probe"]; ok {
		var pf map[string]json.RawMessage
		if err := json.Unmarshal(rawP, &pf); err != nil || pf == nil {
			return ov, problems, errors.New("\"probe\" must be an object")
		}
		probe := &ProbeDef{}
		for k, v := range pf {
			switch k {
			case "tool":
				if err := json.Unmarshal(v, &probe.Tool); err != nil {
					return ov, problems, errors.New("\"probe.tool\" must be a string")
				}
			case "contains":
				if err := json.Unmarshal(v, &probe.Contains); err != nil {
					return ov, problems, errors.New("\"probe.contains\" must be a string")
				}
			case "args":
				var args map[string]interface{}
				if err := json.Unmarshal(v, &args); err != nil || args == nil {
					return ov, problems, errors.New("\"probe.args\" must be an object")
				}
				probe.Args = args
			default:
				problems = append(problems, fmt.Sprintf("%s: unknown key %q in probe ignored", label, k))
			}
		}
		if strings.TrimSpace(probe.Tool) == "" {
			return ov, problems, errors.New("\"probe.tool\" is required")
		}
		ov.Probe = probe
	}
	return ov, problems, nil
}

// applyFleetOverride copies one override onto the translated ServerDef. The
// probe is held to the same rule Bundle.validate applies to manifest servers:
// it must name a tool inside the allowlist when one is set.
func applyFleetOverride(sd *ServerDef, ov fleetServerOverride, label string, problems *[]string) {
	if tools := normalizeToolList(ov.Tools); len(tools) > 0 {
		sd.Tools = tools
	}
	if ov.Probe != nil {
		tool := strings.TrimSpace(ov.Probe.Tool)
		if len(sd.Tools) > 0 && !slices.Contains(sd.Tools, tool) {
			*problems = append(*problems, fmt.Sprintf("%s: probe.tool %q is not in the server's tools allowlist; probe ignored", label, tool))
		} else {
			probe := *ov.Probe
			sd.Probe = &probe
		}
	}
	if ov.Optional != nil {
		sd.Optional = *ov.Optional
	}
	if ov.EnabledByDefault != nil {
		sd.EnabledByDefault = *ov.EnabledByDefault
	}
	if ov.Beta != nil {
		sd.Beta = *ov.Beta
	}
	if ov.DisplayName != nil && strings.TrimSpace(*ov.DisplayName) != "" {
		sd.DisplayName = strings.TrimSpace(*ov.DisplayName)
	}
	if ov.Description != nil && strings.TrimSpace(*ov.Description) != "" {
		sd.Description = strings.TrimSpace(*ov.Description)
	}
	if len(ov.DataSources) > 0 {
		sd.DataSources = append([]string(nil), ov.DataSources...)
	}
}

// pluginDataDirName is the per-box state subdirectory that holds each plugin's
// persistent PLUGIN_DATA directory (spec §9.1), keyed by plugin name so it
// survives plugin updates.
const pluginDataDirName = "plugin-data"

const (
	pluginRootVar = "PLUGIN_ROOT"
	pluginDataVar = "PLUGIN_DATA"
)

// pluginNameRe is the spec §5.5 shape (the schema's pattern minus the
// look-ahead, which Go's RE2 lacks; the `--` / `..` bans are checked apart).
var pluginNameRe = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$`)

// pluginServerNameRe bounds a plugin's mcp.json server keys to the characters
// every provider accepts in a tool name: the agent addresses a server's tools
// as mcp_<server>_<tool>, so a key with a dot or a space would produce a tool
// name upstream rejects. The manifest's own servers carry no such rule (they
// are hand-named by the bundle author); plugin keys come from third parties.
var pluginServerNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// httpHeaderNameRe is RFC 7230 token syntax for a header field name.
var httpHeaderNameRe = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

// Plugin is one loaded Agent Plugin: the manifest identity plus what fleet
// actually took from it. Skills lists the skill folder names that made it
// into the merged roster; MCPServers lists the catalog names of the servers
// that made it into the MCP catalog. Both can be empty for a valid plugin.
type Plugin struct {
	// Name is the manifest `name` (spec §5.5 shape).
	Name string
	// Version / Description are the manifest metadata fields, verbatim (the
	// spec forbids rejecting a plugin over their format, so they are
	// unvalidated strings for display).
	Version     string
	Description string
	// Dir is the absolute, symlink-resolved plugin root — PLUGIN_ROOT.
	Dir string
	// DataDir is the plugin's persistent writable directory — PLUGIN_DATA.
	// Empty when it could not be created (stdio servers are then skipped).
	DataDir string
	// Skills are the folder names of the plugin's well-formed, contained
	// skills, in roster order.
	Skills []string
	// MCPServers are the catalog names of the servers loaded from mcp.json.
	MCPServers []string
}

// pluginManifest is the decoded plugin.json (spec §5).
type pluginManifest struct {
	Schema      string
	Name        string
	Version     string
	Description string
	Author      *pluginAuthor
	Homepage    string
	Repository  string
	License     string
	Keywords    []string
	Extensions  map[string]json.RawMessage
}

type pluginAuthor struct {
	Name  string
	Email string
	URL   string
}

// pluginManifestFields is the closed top-level field set of plugin.json.
var pluginManifestFields = map[string]bool{
	"$schema": true, "name": true, "version": true, "description": true,
	"author": true, "homepage": true, "repository": true, "license": true,
	"keywords": true, "extensions": true,
}

// skillOverlay is one plugin's contribution to the merged skills tree: the
// contained skill folders (by name) under its skills/ dir, and the plugin
// root every file copied out of it must resolve under.
type skillOverlay struct {
	Plugin    string
	Root      string
	SkillsDir string
	Names     []string
}

// pluginLoadResult is everything loadPlugins hands back to Load.
type pluginLoadResult struct {
	plugins  []Plugin
	servers  []ServerDef
	overlays []skillOverlay
	problems []string
}

// validPluginName reports whether name satisfies spec §5.5: 1–64 chars of
// [a-z0-9.-], alphanumeric at both ends, no "--" and no "..".
func validPluginName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	if strings.Contains(name, "--") || strings.Contains(name, "..") {
		return false
	}
	return pluginNameRe.MatchString(name)
}

// parsePluginManifest decodes plugin.json with the spec's two non-fatal
// deviations (unknown top-level field; non-object extensions) returned as
// warnings and everything else as a rejecting error.
func parsePluginManifest(raw []byte) (pluginManifest, []string, error) {
	var pm pluginManifest
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return pm, nil, fmt.Errorf("not a JSON object: %w", err)
	}
	if top == nil {
		return pm, nil, errors.New("must be a JSON object")
	}
	var warnings []string
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !pluginManifestFields[k] {
			warnings = append(warnings, fmt.Sprintf("unknown top-level field %q ignored (client data belongs under extensions)", k))
		}
	}
	str := func(key string, required bool) (string, error) {
		rawV, ok := top[key]
		if !ok {
			if required {
				return "", fmt.Errorf("required field %q is missing", key)
			}
			return "", nil
		}
		var s string
		if err := json.Unmarshal(rawV, &s); err != nil {
			return "", fmt.Errorf("field %q must be a string", key)
		}
		return s, nil
	}
	var err error
	if pm.Schema, err = str("$schema", true); err != nil {
		return pm, warnings, err
	}
	if pm.Schema != PluginManifestSchema {
		return pm, warnings, fmt.Errorf("unsupported $schema %q (fleet implements Agent Plugins 1.0.0: %s)", pm.Schema, PluginManifestSchema)
	}
	if pm.Name, err = str("name", true); err != nil {
		return pm, warnings, err
	}
	if !validPluginName(pm.Name) {
		return pm, warnings, fmt.Errorf("name %q is invalid (1-64 chars of a-z 0-9 . -, alphanumeric at both ends, no \"--\" or \"..\")", pm.Name)
	}
	for _, f := range []struct {
		key string
		dst *string
	}{
		{"version", &pm.Version}, {"description", &pm.Description}, {"homepage", &pm.Homepage},
		{"repository", &pm.Repository}, {"license", &pm.License},
	} {
		if *f.dst, err = str(f.key, false); err != nil {
			return pm, warnings, err
		}
	}
	if rawV, ok := top["keywords"]; ok {
		if err := json.Unmarshal(rawV, &pm.Keywords); err != nil {
			return pm, warnings, errors.New("field \"keywords\" must be an array of strings")
		}
	}
	if rawV, ok := top["author"]; ok {
		var a map[string]json.RawMessage
		if err := json.Unmarshal(rawV, &a); err != nil || a == nil {
			return pm, warnings, errors.New("field \"author\" must be an object")
		}
		pm.Author = &pluginAuthor{}
		for k, v := range a {
			var dst *string
			switch k {
			case "name":
				dst = &pm.Author.Name
			case "email":
				dst = &pm.Author.Email
			case "url":
				dst = &pm.Author.URL
			default:
				return pm, warnings, fmt.Errorf("field \"author\" has unknown member %q (only name, email, url are allowed)", k)
			}
			if err := json.Unmarshal(v, dst); err != nil {
				return pm, warnings, fmt.Errorf("field \"author.%s\" must be a string", k)
			}
		}
	}
	if rawV, ok := top["extensions"]; ok {
		var ext map[string]json.RawMessage
		if err := json.Unmarshal(rawV, &ext); err != nil || ext == nil {
			// Spec §8.1: report and ignore, keep loading components.
			warnings = append(warnings, "field \"extensions\" is not an object; ignored")
		} else {
			for ns, v := range ext {
				var obj map[string]json.RawMessage
				if err := json.Unmarshal(v, &obj); err != nil || obj == nil {
					return pm, warnings, fmt.Errorf("extensions[%q] must be an object", ns)
				}
			}
			pm.Extensions = ext
		}
	}
	return pm, warnings, nil
}

// withinDir reports whether path (already symlink-resolved and clean) is root
// itself or lies beneath it.
func withinDir(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// resolveContained resolves p through symlinks and returns the resolved path only
// when it stays inside root; otherwise an error naming the escape.
func resolveContained(p, root string) (string, error) {
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(resolved) {
		if resolved, err = filepath.Abs(resolved); err != nil {
			return "", err
		}
	}
	if !withinDir(resolved, root) {
		return "", fmt.Errorf("%s resolves outside the plugin root (%s)", p, resolved)
	}
	return resolved, nil
}

// expandPluginVars is the spec §9.2 expansion: one non-recursive textual pass
// replacing exact ${PLUGIN_ROOT} / ${PLUGIN_DATA} tokens. Any other
// placeholder-like text stays literal (strings.Replacer never rescans the
// text it substituted).
func expandPluginVars(s, root, data string) string {
	if !strings.Contains(s, "${PLUGIN_") {
		return s
	}
	return strings.NewReplacer("${"+pluginRootVar+"}", root, "${"+pluginDataVar+"}", data).Replace(s)
}

// pluginDataDir is <state>/plugin-data/<name>: created before any stdio server
// launches, writable by the fleet process the broker runs servers as, kept
// across plugin updates (the plugin root may be replaced wholesale; this dir
// is not under it). Sits beside skills-merged under the same trusted base.
func pluginDataDir(name string) (string, error) {
	base := fleetStateBase(pluginDataDirName)
	if err := ensureTrustedDir(base); err != nil {
		return "", fmt.Errorf("plugin-data root: %w", err)
	}
	dir := filepath.Join(base, name)
	if err := ensureTrustedDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// loadPlugins discovers and loads every Agent Plugin under the bundle's
// plugins/ dir and the configured extra roots. takenServerNames is the set of
// MCP-catalog / http_tool names already claimed by the manifest; a plugin
// server whose key collides is skipped (the manifest wins) and the set is
// extended with every plugin server accepted, so two plugins cannot both
// claim one name either. It never fails: every defect is a problem string.
func loadPlugins(bundleDir string, extraRoots []string, takenServerNames map[string]bool) pluginLoadResult {
	var res pluginLoadResult
	roots := []string{filepath.Join(bundleDir, PluginsDirName)}
	seenRoot := map[string]bool{filepath.Clean(roots[0]): true}
	for _, r := range extraRoots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if !filepath.IsAbs(r) {
			r = filepath.Join(bundleDir, r)
		}
		r = filepath.Clean(r)
		if seenRoot[r] {
			continue
		}
		seenRoot[r] = true
		roots = append(roots, r)
	}
	seenName := map[string]string{}
	for i, root := range roots {
		info, err := os.Stat(root)
		if err != nil {
			// The fixed plugins/ dir is optional (a bundle need not ship
			// plugins); a root the operator listed explicitly is not.
			if i > 0 {
				res.problems = append(res.problems, fmt.Sprintf("plugin_roots: %s: %v", root, err))
			}
			continue
		}
		if !info.IsDir() {
			res.problems = append(res.problems, fmt.Sprintf("%s exists but is not a directory; no plugins loaded from it", root))
			continue
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			res.problems = append(res.problems, fmt.Sprintf("%s: cannot read: %v", root, err))
			continue
		}
		for _, e := range entries {
			p := filepath.Join(root, e.Name())
			st, err := os.Stat(p) // follows a symlinked plugin dir on purpose; containment is checked against its resolved root
			if err != nil || !st.IsDir() {
				continue // a README or stray file beside the plugins is fine
			}
			label := p
			if rel, rerr := filepath.Rel(bundleDir, p); rerr == nil && !strings.HasPrefix(rel, "..") {
				label = rel
			}
			plugin, servers, overlay, problems, ok := loadOnePlugin(label, p, takenServerNames)
			res.problems = append(res.problems, problems...)
			if !ok {
				continue
			}
			if prev, dup := seenName[plugin.Name]; dup {
				res.problems = append(res.problems, fmt.Sprintf("%s: plugin name %q is already provided by %s; this copy is skipped", label, plugin.Name, prev))
				// Undo the server-name claims this copy made.
				for _, s := range servers {
					delete(takenServerNames, s.Name)
				}
				continue
			}
			seenName[plugin.Name] = label
			res.plugins = append(res.plugins, plugin)
			res.servers = append(res.servers, servers...)
			if overlay != nil {
				res.overlays = append(res.overlays, *overlay)
			}
		}
	}
	return res
}

// loadOnePlugin loads the plugin rooted at dir. ok=false means the plugin was
// rejected outright (manifest missing / invalid / escaping); the problems
// slice always explains itself.
func loadOnePlugin(label, dir string, taken map[string]bool) (Plugin, []ServerDef, *skillOverlay, []string, bool) {
	var problems []string
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return Plugin{}, nil, nil, []string{fmt.Sprintf("%s: %v", label, err)}, false
	}
	if root, err = filepath.Abs(root); err != nil {
		return Plugin{}, nil, nil, []string{fmt.Sprintf("%s: %v", label, err)}, false
	}

	manifestPath := filepath.Join(root, "plugin.json")
	realManifest, err := resolveContained(manifestPath, root)
	if err != nil {
		if os.IsNotExist(err) {
			return Plugin{}, nil, nil, []string{fmt.Sprintf("%s: no plugin.json at the plugin root (every Agent Plugin has one); not a plugin", label)}, false
		}
		return Plugin{}, nil, nil, []string{fmt.Sprintf("%s: plugin.json: %v; plugin rejected", label, err)}, false
	}
	if st, err := os.Stat(realManifest); err != nil || !st.Mode().IsRegular() {
		return Plugin{}, nil, nil, []string{fmt.Sprintf("%s: plugin.json is not a regular file; plugin rejected", label)}, false
	}
	raw, err := os.ReadFile(realManifest) // #nosec G304 — operator-owned bundle content, containment checked above.
	if err != nil {
		return Plugin{}, nil, nil, []string{fmt.Sprintf("%s: plugin.json: %v; plugin rejected", label, err)}, false
	}
	pm, warnings, err := parsePluginManifest(raw)
	for _, w := range warnings {
		problems = append(problems, fmt.Sprintf("%s/plugin.json: %s", label, w))
	}
	if err != nil {
		problems = append(problems, fmt.Sprintf("%s/plugin.json: %s; plugin rejected", label, err))
		return Plugin{}, nil, nil, problems, false
	}

	plugin := Plugin{Name: pm.Name, Version: pm.Version, Description: pm.Description, Dir: root}
	// fleet's own extension namespace (spec §8.1): per-server governance knobs
	// the portable format has no field for. Lenient by design — see
	// fleetServerOverride.
	var overrides map[string]fleetServerOverride
	if rawExt, ok := pm.Extensions[FleetPluginExtensionNamespace]; ok {
		var eproblems []string
		overrides, eproblems = parseFleetExtension(rawExt)
		for _, ep := range eproblems {
			problems = append(problems, fmt.Sprintf("%s/plugin.json: %s", label, ep))
		}
	}
	dataDir, dataErr := pluginDataDir(pm.Name)
	if dataErr != nil {
		problems = append(problems, fmt.Sprintf("%s: PLUGIN_DATA directory unavailable (%v); stdio MCP servers are skipped", label, dataErr))
	} else {
		plugin.DataDir = dataDir
	}

	// Skills (spec §7.1): fixed location skills/, immediate children only. The
	// overlay is recorded for every plugin (even one with no skills/ yet) so
	// Bundle.Skills can rediscover folders on read — a skill added to a plugin
	// after boot joins the roster like a bundle skill does.
	skillsPath := filepath.Join(root, "skills")
	names, sproblems := discoverPluginSkills(label, root, skillsPath)
	problems = append(problems, sproblems...)
	overlay := &skillOverlay{Plugin: pm.Name, Root: root, SkillsDir: skillsPath, Names: names}
	plugin.Skills = append([]string(nil), names...)

	// MCP servers (spec §7.2): fixed location mcp.json.
	var servers []ServerDef
	mcpPath := filepath.Join(root, "mcp.json")
	if _, err := os.Lstat(mcpPath); err == nil {
		realMCP, err := resolveContained(mcpPath, root)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s/mcp.json: %v; MCP disabled for this plugin", label, err))
		} else if st, err := os.Stat(realMCP); err != nil || !st.Mode().IsRegular() {
			problems = append(problems, fmt.Sprintf("%s/mcp.json is not a regular file; MCP disabled for this plugin", label))
		} else if raw, err := os.ReadFile(realMCP); err != nil { // #nosec G304 — bundle content, containment checked.
			problems = append(problems, fmt.Sprintf("%s/mcp.json: %v; MCP disabled for this plugin", label, err))
		} else {
			ctx := pluginServerContext{label: label, plugin: pm.Name, root: root, dataDir: dataDir, dataErr: dataErr, overrides: overrides}
			var mproblems []string
			servers, mproblems = parsePluginMCP(raw, ctx, taken)
			problems = append(problems, mproblems...)
			for _, s := range servers {
				plugin.MCPServers = append(plugin.MCPServers, s.Name)
			}
		}
	} else if !os.IsNotExist(err) {
		problems = append(problems, fmt.Sprintf("%s/mcp.json: %v; MCP disabled for this plugin", label, err))
	}

	return plugin, servers, overlay, problems, true
}

// pluginServerContext is what one mcp.json entry needs from its plugin.
type pluginServerContext struct {
	label     string
	plugin    string
	root      string
	dataDir   string
	dataErr   error
	overrides map[string]fleetServerOverride
}

// discoverPluginSkills lists the well-formed, contained skill folders under a
// plugin's skills/ dir (spec §7.1) in roster order. An absent skills/ is valid
// absence (no names, no problems); a skills path that is not a directory or
// escapes the root disables the component type with a report. Load calls it
// with reporting; Bundle.Skills calls it again on every read, so the roster
// follows the plugin's folder set without a restart.
func discoverPluginSkills(label, root, skillsPath string) (names []string, problems []string) {
	st, err := os.Stat(skillsPath)
	if err != nil {
		if !os.IsNotExist(err) {
			problems = append(problems, fmt.Sprintf("%s/skills: %v; skills disabled for this plugin", label, err))
		}
		return nil, problems
	}
	if !st.IsDir() {
		return nil, []string{fmt.Sprintf("%s/skills exists but is not a directory; skills disabled for this plugin", label)}
	}
	if _, err := resolveContained(skillsPath, root); err != nil {
		return nil, []string{fmt.Sprintf("%s/skills: %v; skills disabled for this plugin", label, err)}
	}
	skills, sproblems := ReadSkills(skillsPath)
	for _, sp := range sproblems {
		problems = append(problems, fmt.Sprintf("%s/%s", label, sp))
	}
	for _, sk := range skills {
		if _, ok := containedRegularFile(filepath.Join(skillsPath, sk.Dir, "SKILL.md"), root); !ok {
			problems = append(problems, fmt.Sprintf("%s/skills/%s: SKILL.md is not a regular file inside the plugin root; skill skipped", label, sk.Dir))
			continue
		}
		names = append(names, sk.Dir)
	}
	return names, problems
}

// livePluginSkillOverlays re-lists every plugin's skill folders from disk so a
// Skills() read reflects folders added or removed since Load. Discovery
// problems are not re-reported here (Load already did, once); a plugin whose
// skills/ became invalid simply contributes nothing until fixed.
func (b *Bundle) livePluginSkillOverlays() []skillOverlay {
	if len(b.pluginSkillOverlays) == 0 {
		return nil
	}
	out := make([]skillOverlay, 0, len(b.pluginSkillOverlays))
	for _, ov := range b.pluginSkillOverlays {
		fresh := ov
		fresh.Names, _ = discoverPluginSkills(ov.Plugin, ov.Root, ov.SkillsDir)
		out = append(out, fresh)
	}
	return out
}

// parsePluginMCP validates mcp.json in the spec's two stages: the closed top
// level first (a failure disables MCP for the plugin), then each server entry
// independently (a failure skips that entry). Accepted entries are returned
// as catalog-ready ServerDefs and their names are added to taken.
func parsePluginMCP(raw []byte, ctx pluginServerContext, taken map[string]bool) ([]ServerDef, []string) {
	disabled := func(msg string) ([]ServerDef, []string) {
		return nil, []string{fmt.Sprintf("%s/mcp.json: %s; MCP disabled for this plugin", ctx.label, msg)}
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil || top == nil {
		return disabled("not a JSON object")
	}
	for k := range top {
		if k != "$schema" && k != "mcpServers" {
			return disabled(fmt.Sprintf("unknown top-level field %q (only $schema and mcpServers are allowed)", k))
		}
	}
	var schema string
	if rawV, ok := top["$schema"]; !ok {
		return disabled("required field \"$schema\" is missing")
	} else if err := json.Unmarshal(rawV, &schema); err != nil {
		return disabled("field \"$schema\" must be a string")
	}
	if schema != PluginMCPSchema {
		// Covers both "unsupported version" and "version differs from
		// plugin.json" (§10.1): the only plugin.json version accepted is 1.0.0.
		return disabled(fmt.Sprintf("unsupported $schema %q (fleet implements Agent Plugins 1.0.0: %s, and it must match plugin.json's version)", schema, PluginMCPSchema))
	}
	var servers map[string]json.RawMessage
	if rawV, ok := top["mcpServers"]; !ok {
		return disabled("required field \"mcpServers\" is missing")
	} else if err := json.Unmarshal(rawV, &servers); err != nil || servers == nil {
		return disabled("field \"mcpServers\" must be an object")
	}
	names := make([]string, 0, len(servers))
	for n := range servers {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []ServerDef
	var problems []string
	usedOverrides := map[string]bool{}
	for _, name := range names {
		entryLabel := fmt.Sprintf("%s/mcp.json: server %q", ctx.label, name)
		if !pluginServerNameRe.MatchString(name) {
			problems = append(problems, entryLabel+": name must be 1-64 chars of letters, digits, '_' or '-' (it becomes part of a tool name); skipped")
			continue
		}
		if name == HTTPToolServerName {
			problems = append(problems, fmt.Sprintf("%s: name %q is reserved; skipped", entryLabel, name))
			continue
		}
		sd, err := parsePluginServer(name, servers[name], ctx)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v; skipped", entryLabel, err))
			continue
		}
		if ov, ok := ctx.overrides[name]; ok {
			usedOverrides[name] = true
			if ov.Disabled {
				// An explicit operator choice, not a defect: nothing to report.
				continue
			}
			applyFleetOverride(&sd, ov, entryLabel, &problems)
		}
		if taken[name] {
			problems = append(problems, fmt.Sprintf("%s: collides with an MCP server or http tool of the same name already in the catalog; skipped (the manifest and earlier plugins win)", entryLabel))
			continue
		}
		taken[name] = true
		out = append(out, sd)
	}
	unused := make([]string, 0, len(ctx.overrides))
	for name := range ctx.overrides {
		if !usedOverrides[name] {
			unused = append(unused, name)
		}
	}
	sort.Strings(unused)
	for _, name := range unused {
		problems = append(problems, fmt.Sprintf("%s/plugin.json: extensions[%s].mcp_servers[%q] names a server mcp.json does not declare (or one that was skipped as invalid); ignored", ctx.label, FleetPluginExtensionNamespace, name))
	}
	return out, problems
}

// jsonString reads fields[key] as a string; present is false when absent.
func jsonString(fields map[string]json.RawMessage, key string) (val string, present bool, err error) {
	rawV, ok := fields[key]
	if !ok {
		return "", false, nil
	}
	var str string
	if err := json.Unmarshal(rawV, &str); err != nil {
		return "", true, fmt.Errorf("field %q must be a string", key)
	}
	return str, true, nil
}

// jsonStringMap reads fields[key] as an object of strings (nil when absent).
func jsonStringMap(fields map[string]json.RawMessage, key string) (map[string]string, error) {
	rawV, ok := fields[key]
	if !ok {
		return nil, nil
	}
	var m map[string]string
	if err := json.Unmarshal(rawV, &m); err != nil || m == nil {
		return nil, fmt.Errorf("field %q must be an object of strings", key)
	}
	return m, nil
}

// allowFields rejects any member of fields outside allowed — the closed
// per-transport field set of spec §7.2.1 (a field from another variant makes
// the entry invalid, as does anything unknown).
func allowFields(fields map[string]json.RawMessage, typ string, allowed ...string) error {
	ok := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		ok[a] = true
	}
	for k := range fields {
		if !ok[k] {
			return fmt.Errorf("unknown field %q for type %q", k, typ)
		}
	}
	return nil
}

// parsePluginServer validates one server entry against its declared
// transport's closed field set and translates it into a ServerDef.
func parsePluginServer(name string, raw json.RawMessage, ctx pluginServerContext) (ServerDef, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return ServerDef{}, errors.New("entry must be an object")
	}
	typ, _, err := jsonString(fields, "type")
	if err != nil {
		return ServerDef{}, err
	}
	if typ == "" {
		return ServerDef{}, errors.New("required field \"type\" is missing")
	}
	base := ServerDef{
		Name:        name,
		Always:      true,
		DisplayName: fmt.Sprintf("%s (plugin %s)", name, ctx.plugin),
		Description: fmt.Sprintf("MCP server declared by Agent Plugin %q (mcp.json).", ctx.plugin),
		literalEnv:  true,
		plugin:      ctx.plugin,
	}
	switch typ {
	case "stdio":
		return parseStdioPluginServer(fields, ctx, base)
	case "streamable-http":
		return parseHTTPPluginServer(fields, base)
	case "sse":
		return ServerDef{}, errors.New("legacy HTTP+SSE transport is not supported (fleet speaks stdio and Streamable HTTP; spec support for sse is optional)")
	default:
		return ServerDef{}, fmt.Errorf("unknown type %q (want stdio, streamable-http, or sse)", typ)
	}
}

// parseStdioPluginServer handles the `stdio` variant: command resolution,
// the reserved-variable rule, placeholder expansion, and cwd containment.
func parseStdioPluginServer(fields map[string]json.RawMessage, ctx pluginServerContext, base ServerDef) (ServerDef, error) {
	if err := allowFields(fields, "stdio", "type", "command", "args", "env", "cwd"); err != nil {
		return ServerDef{}, err
	}
	if ctx.dataErr != nil {
		return ServerDef{}, fmt.Errorf("PLUGIN_DATA directory unavailable: %w", ctx.dataErr)
	}
	command, ok, err := jsonString(fields, "command")
	if err != nil {
		return ServerDef{}, err
	}
	if !ok || command == "" {
		return ServerDef{}, errors.New("required field \"command\" is missing or empty")
	}
	var args []string
	if rawV, ok := fields["args"]; ok {
		if err := json.Unmarshal(rawV, &args); err != nil {
			return ServerDef{}, errors.New("field \"args\" must be an array of strings")
		}
	}
	env, err := jsonStringMap(fields, "env")
	if err != nil {
		return ServerDef{}, err
	}
	for k := range env {
		if k == pluginRootVar || k == pluginDataVar {
			return ServerDef{}, fmt.Errorf("env must not set the reserved variable %s (the client supplies it)", k)
		}
	}
	cwd, _, err := jsonString(fields, "cwd")
	if err != nil {
		return ServerDef{}, err
	}
	if command, err = resolveStdioCommand(command, ctx.root); err != nil {
		return ServerDef{}, err
	}
	expand := func(s string) string { return expandPluginVars(s, ctx.root, ctx.dataDir) }
	for i := range args {
		args[i] = expand(args[i])
	}
	outEnv := make(map[string]string, len(env)+2)
	for k, v := range env {
		outEnv[k] = expand(v)
	}
	dir, err := resolveStdioCwd(cwd, expand(cwd), ctx)
	if err != nil {
		return ServerDef{}, err
	}
	// Reserved variables are set LAST so a plugin cannot shadow them (§9.1).
	outEnv[pluginRootVar] = ctx.root
	outEnv[pluginDataVar] = ctx.dataDir
	base.Type = "stdio"
	base.Command = command
	base.Args = args
	base.Env = outEnv
	base.dir = dir
	return base, nil
}

// resolveStdioCommand applies the spec's command rule: one executable token —
// a bare name (PATH lookup at spawn) or a ./-relative path resolved under the
// plugin root and required to exist there. No expansion applies to command.
func resolveStdioCommand(command, root string) (string, error) {
	switch {
	case strings.HasPrefix(command, "./"):
		resolved, err := resolveContained(filepath.Join(root, command[2:]), root)
		if err != nil {
			return "", fmt.Errorf("command %q: %w", command, err)
		}
		if st, err := os.Stat(resolved); err != nil || !st.Mode().IsRegular() {
			return "", fmt.Errorf("command %q does not resolve to a regular file under the plugin root", command)
		}
		return resolved, nil
	case strings.ContainsAny(command, `/\`) || strings.Contains(command, "${") || command == "." || command == "..":
		return "", fmt.Errorf("command %q must be a bare executable name or a ./-relative path (no absolute, ../, or placeholder commands)", command)
	}
	return command, nil
}

// resolveStdioCwd applies the spec's three cwd forms (§7.2.1): ./-relative and
// ${PLUGIN_ROOT}-rooted stay inside the plugin root; ${PLUGIN_DATA}-rooted
// stays inside the data dir and is created before launch. expanded is cwd
// after placeholder expansion. An empty cwd is the plugin root.
func resolveStdioCwd(cwd, expanded string, ctx pluginServerContext) (string, error) {
	if cwd == "" {
		return ctx.root, nil
	}
	var dir string
	switch {
	case strings.HasPrefix(cwd, "./"):
		resolved, err := resolveContained(filepath.Join(ctx.root, cwd[2:]), ctx.root)
		if err != nil {
			return "", fmt.Errorf("cwd %q: %w", cwd, err)
		}
		dir = resolved
	case cwd == "${"+pluginRootVar+"}" || strings.HasPrefix(cwd, "${"+pluginRootVar+"}/"):
		resolved, err := resolveContained(expanded, ctx.root)
		if err != nil {
			return "", fmt.Errorf("cwd %q: %w", cwd, err)
		}
		dir = resolved
	case cwd == "${"+pluginDataVar+"}" || strings.HasPrefix(cwd, "${"+pluginDataVar+"}/"):
		clean := filepath.Clean(expanded)
		if !withinDir(clean, ctx.dataDir) {
			return "", fmt.Errorf("cwd %q escapes the plugin data directory", cwd)
		}
		if err := os.MkdirAll(clean, 0o755); err != nil { //nolint:gosec // plugin state dir under the fleet data dir, non-secret; the server itself writes here
			return "", fmt.Errorf("cwd %q: %w", cwd, err)
		}
		dir = clean
	default:
		return "", fmt.Errorf("cwd %q must be ./-relative, ${PLUGIN_ROOT}-rooted, or ${PLUGIN_DATA}-rooted", cwd)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return "", fmt.Errorf("cwd %q is not a directory", cwd)
	}
	return dir, nil
}

// parseHTTPPluginServer handles the `streamable-http` variant: URL rules and
// literal, well-formed headers. fleet's type for it is "http".
func parseHTTPPluginServer(fields map[string]json.RawMessage, base ServerDef) (ServerDef, error) {
	if err := allowFields(fields, "streamable-http", "type", "url", "headers"); err != nil {
		return ServerDef{}, err
	}
	u, ok, err := jsonString(fields, "url")
	if err != nil {
		return ServerDef{}, err
	}
	if !ok || u == "" {
		return ServerDef{}, errors.New("required field \"url\" is missing or empty")
	}
	if err := validatePluginURL(u); err != nil {
		return ServerDef{}, err
	}
	headers, err := jsonStringMap(fields, "headers")
	if err != nil {
		return ServerDef{}, err
	}
	if err := validatePluginHeaders(headers); err != nil {
		return ServerDef{}, err
	}
	base.Type = "http"
	base.URL = u
	if len(headers) > 0 {
		base.Headers = headers
	}
	return base, nil
}

// containedRegularFile resolves p through symlinks and reports whether it is a
// regular file inside root — the per-file check the merged-tree copy applies
// to plugin skill contents.
func containedRegularFile(p, root string) (string, bool) {
	resolved, err := resolveContained(p, root)
	if err != nil {
		return "", false
	}
	st, err := os.Stat(resolved)
	if err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	return resolved, true
}

// validatePluginURL enforces spec §7.2.1 for a remote server URL: absolute
// http(s), no user info, no fragment, and https unless the host is loopback.
func validatePluginURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("url %q: %w", raw, err)
	}
	if !u.IsAbs() || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("url %q must be an absolute http or https URL", raw)
	}
	if u.User != nil {
		return fmt.Errorf("url %q must not carry user information", raw)
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return fmt.Errorf("url %q must not carry a fragment", raw)
	}
	if u.Scheme == "http" {
		host := u.Hostname()
		if host != "localhost" {
			ip := net.ParseIP(host)
			if ip == nil || !ip.IsLoopback() {
				return fmt.Errorf("url %q: a non-loopback endpoint must use https", raw)
			}
		}
	}
	return nil
}

// validatePluginHeaders enforces spec §7.2.1 for configured headers: token
// names, no duplicate names under different casing, no CR/LF in values.
// Values are visible package data, so nothing here is treated as a secret.
func validatePluginHeaders(h map[string]string) error {
	seen := map[string]string{}
	for name, val := range h {
		if !httpHeaderNameRe.MatchString(name) {
			return fmt.Errorf("header %q is not a valid HTTP header name", name)
		}
		lower := strings.ToLower(name)
		if prev, dup := seen[lower]; dup {
			return fmt.Errorf("headers %q and %q are the same header under different casing", prev, name)
		}
		seen[lower] = name
		if strings.ContainsAny(val, "\r\n") {
			return fmt.Errorf("header %q value contains a line break", name)
		}
	}
	return nil
}

// PluginProblems returns the human-readable problems the plugin loader
// reported (unknown manifest fields, skipped skills/servers, rejected
// plugins). Load logs them as warnings; `fleet validate-config` surfaces them
// as advisories. Empty means every discovered plugin loaded cleanly.
func (b *Bundle) PluginProblems() []string {
	return append([]string(nil), b.pluginProblems...)
}

// SkillOrigin says where a name in the merged skill roster came from.
type SkillOrigin struct {
	// Source is "bundle" (the bundle's own skills/), "plugin" (an Agent
	// Plugin's skills/), or "builtin" (fleet's embedded pack).
	Source string
	// Plugin is the plugin name when Source is "plugin".
	Plugin string
}

// SkillOrigin resolves the provenance of one roster name with the merge
// precedence the tree was built with: bundle > plugin (first by plugin name)
// > builtin. Used by GET /skills to badge each row.
func (b *Bundle) SkillOrigin(name string) SkillOrigin {
	dir := b.BundleSkillsDir
	if dir == "" {
		dir = b.SkillsDir
	}
	if dir != "" {
		if _, err := os.Stat(filepath.Join(dir, name, "SKILL.md")); err == nil {
			return SkillOrigin{Source: "bundle"}
		}
	}
	// Live, like the roster itself: the first plugin (by name) whose skills/
	// holds a contained SKILL.md for the name owns it.
	for _, ov := range b.pluginSkillOverlays {
		if _, ok := containedRegularFile(filepath.Join(ov.SkillsDir, name, "SKILL.md"), ov.Root); ok {
			return SkillOrigin{Source: "plugin", Plugin: ov.Plugin}
		}
	}
	return SkillOrigin{Source: "builtin"}
}

// PluginName returns the Agent Plugin this server was loaded from, or "" for
// a manifest-declared server.
func (s *ServerDef) PluginName() string {
	return s.plugin
}
