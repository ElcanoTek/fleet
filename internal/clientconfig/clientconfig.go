// Package clientconfig loads a pluggable CLIENT BUNDLE: the per-deployment
// branding, model defaults, MCP-server catalog, empty-state cards, and the
// resolved on-disk paths for system_prompts / personas / protocols / mcp.
//
// fleet itself ships NO client-specific content. At boot it loads a bundle
// from FLEET_CLIENT_CONFIG_DIR (default ./config/default, a GENERIC bundle that
// ships in the repo so fleet runs bare). A real deployment points the env var
// at a checked-out client repo (e.g. /root/elcano-config).
//
// Bundle layout:
//
//	<bundle>/
//	  manifest.yaml        # branding, models, mcp_servers[] (the catalog),
//	                       #   empty_state{cards[], protocol_pills[]},
//	                       #   agent_policy{parallel/critical tool lists},
//	                       #   sandbox{containerfile, tag, image, runtime}
//	  sandbox/             # the bundle's own Containerfile (build-on-box default)
//	  system_prompts/      # default.md (scheduled base), chat.md (interactive base)
//	  personas/            # *.yaml
//	  protocols/           # *.yaml|md
//	  prompts/             # *.yaml|yml|md|txt reusable prompt library entries
//	  skills/              # <name>/SKILL.md Agent Skills (progressive disclosure)
//	  plugins/             # <plugin>/plugin.json Agent Plugins (skills + mcp.json), plugins.go
//	  mcp/                 # the client's Python MCP servers + requirements.txt
//
// The execution SANDBOX is a per-client bundle artifact: each bundle ships its
// own sandbox/Containerfile flavor (and pins its own base digest). Bundle.Sandbox()
// resolves the descriptor — ResolvedImageRef() = the manifest's sandbox.image
// when set (opt-in registry/prebuilt) else sandbox.tag (build-on-box). fleet
// does not build at startup; bootstrap/build-sandbox-image.sh builds it.
//
// The MCP catalog is declarative: each entry names the subprocess command/args
// (args resolve relative to the bundle's mcp/ dir), an enable gate over process
// env vars, the per-subprocess env (each value supports ${VAR} interpolation
// from the process env), an optional tool allowlist, and the base credential
// vars used by the account-suffix scan (creds.ApplyClientSuffix / AccountsFor).
// Credential VALUES never live in the manifest — only the env-var NAMES do; the
// loader resolves them from the process environment at Load time.
//
// The manifest's http_tools[] section (issue #261) declares lightweight inline
// REST-API tools that register as native tools without a full MCP server. They are
// bundle-author-defined and trusted like mcp_servers, and share the SAME credential
// boundary: header ${ENV_VAR} secrets are resolved host-side and applied to the
// outbound request at call time, never entering the sandbox or the model context.
// See HTTPToolDef.
package clientconfig

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/config/envreg"
)

// HTTPToolServerName is the synthetic MCP-server name inline http_tools are
// registered under on the credentialed *mcp.Client. The leading underscore keeps
// it out of the namespace a bundle's own mcp_servers[] entries occupy; validate()
// rejects an MCP server that tries to claim it. The agent sees these tools as
// mcp__http_<name> — routed, gated, redacted, and brokered host-side exactly like
// any MCP tool.
const HTTPToolServerName = "_http"

// A2AToolServerName is the synthetic MCP-server name outbound a2a_peers[] tools
// are registered under (#1368). Same reservation rules as HTTPToolServerName;
// the agent sees a peer as mcp__a2a_<peer>_send / _status / _wait / _cancel.
// Mirrors mcp.A2AToolServerName (kept in sync; internal/mcp does not import
// this package).
const A2AToolServerName = "_a2a"

// a2aPeerNamePattern bounds a peer name: it is spliced into four tool names
// and matched by the critical-tool suffix gate, so it must be a plain,
// lowercase, MCP-safe identifier.
var a2aPeerNamePattern = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)

// httpToolMethods is the set of HTTP methods an http_tool may declare. Kept tight
// (no TRACE/CONNECT/OPTIONS/HEAD) to the verbs a REST tool actually needs.
var httpToolMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true,
}

// EnvDir is the environment variable naming the client bundle directory.
const EnvDir = "FLEET_CLIENT_CONFIG_DIR"

// DefaultDir is the generic bundle shipped in the repo, used when EnvDir is
// unset. Relative to the process working directory (cmd/fleet resolves it
// against the repo root the same way it resolves the legacy supporting dirs).
const DefaultDir = "config/default"

// Dir resolves the configured bundle directory: FLEET_CLIENT_CONFIG_DIR, else
// the generic default.
func Dir() string {
	if v := strings.TrimSpace(envreg.Get(EnvDir)); v != "" {
		return v
	}
	return DefaultDir
}

// Load reads + validates the bundle at dir (absolutized). A blank dir resolves
// via Dir(). It does NOT fail when the optional content dirs are absent — a
// minimal bundle may carry only a manifest — but a missing/invalid manifest.yaml
// is an error.
func Load(dir string) (*Bundle, error) {
	if strings.TrimSpace(dir) == "" {
		dir = Dir()
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve bundle dir %q: %w", dir, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("client config bundle %q: %w", abs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("client config bundle %q is not a directory", abs)
	}

	manifestPath := filepath.Join(abs, "manifest.yaml")
	raw, err := os.ReadFile(manifestPath) // #nosec G304 — operator-supplied bundle path.
	if err != nil {
		return nil, fmt.Errorf("read manifest %s: %w", manifestPath, err)
	}
	// Parse only the connector-bearing sections before interpolation. This
	// source snapshot preserves env-var names even when their values are already
	// exported and the normal interpolation pass below substitutes the tokens
	// out of the runtime manifest.
	var rawConnectors struct {
		MCPServers []ServerDef             `yaml:"mcp_servers"`
		HTTPTools  []HTTPToolDef           `yaml:"http_tools"`
		A2APeers   []A2APeerDef            `yaml:"a2a_peers"`
		RemoteMCPs []RemoteMCPCatalogEntry `yaml:"remote_mcp_catalog"`
	}
	if err := yaml.Unmarshal(raw, &rawConnectors); err != nil {
		return nil, fmt.Errorf("parse connector env inventory %s: %w", manifestPath, err)
	}
	connectorEnvVars, connectorAccountVars := connectorEnvInventory(rawConnectors.MCPServers, rawConnectors.HTTPTools, rawConnectors.A2APeers)
	// Inventory every ${...} reference in the raw manifest — url:, command:/
	// args:, sandbox.image, providers, and the connector maps alike — and where
	// each lives, then fold the FLEET_ENV_FILE env file into the process env
	// BEFORE interpolating (#1123). The names are registered with config's
	// .env allowlist first so the file's values for manifest-referenced vars
	// are admitted; the sites drive the unresolved-reference check after the
	// pass runs. The env-file path comes only from the process env, so there
	// is no circularity with the bundle being loaded here. A raw parse failure
	// fails the load HERE: the strict unmarshal below runs on the interpolated
	// bytes, where a substituted value can repair the syntax and would silently
	// skip the registration and the unresolved-reference check.
	refSites, refNames, err := scanManifestEnvRefs(raw)
	if err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", manifestPath, err)
	}
	config.RegisterAllowedEnvVars(refNames...)
	config.RegisterAllowedEnvVars(manualOAuthEnvNamesFor(rawConnectors.RemoteMCPs)...)
	if err := applyBootEnvFile(); err != nil {
		return nil, err
	}
	// Interpolate env references over the RAW bytes before YAML unmarshal so that
	// "env-or-default" config semantics — ${VAR:-default} / ${VAR:?message} —
	// resolve at load time. This restores the getEnvOrDefault("VAR","literal")
	// behavior the old internal/config carried, which had degenerated into bare
	// hardcoded literals once the catalog moved into the manifest.
	interpolated, err := interpolateManifest(raw, manifestPath)
	if err != nil {
		return nil, err
	}
	// With the env file applied, a bare ${VAR} still unset in a field OUTSIDE
	// the lazily-resolved connector env/header maps can never resolve — nothing
	// re-reads those fields after load, so the literal token would ship into a
	// url:, command:, or sandbox.image and fail confusingly at first use. Fail
	// the load instead, naming the field and the variable.
	if problems := unresolvedManifestRefs(refSites); len(problems) > 0 {
		return nil, fmt.Errorf("client config manifest %s: unresolved ${VAR} references:\n  %s",
			manifestPath, strings.Join(problems, "\n  "))
	}
	var m manifest
	// Strict parse: an unknown or duplicate key FAILS the load rather than being
	// silently dropped. A typo'd key (e.g. `tool:` for `tools:`, or `optional:`
	// misspelled) would otherwise leave a connector mis-configured — at worst
	// exposing a server's full tool surface when a `tools:` allowlist was meant
	// to scope it. Fail loud at startup instead.
	if err := yaml.UnmarshalWithOptions(interpolated, &m, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", manifestPath, err)
	}
	// Connector env/header values resolve against the LIVE process env at
	// catalog-build time (resolveEnvMap) — deliberately NOT here: an unset
	// credential is legitimate there (the server gates off, optional_env drops
	// the key, and the broker child resolves against its OWN environment), and
	// keeping the pass's output would consume the $${ escape one pass early
	// (fleet#706). Re-source those maps from the raw manifest; the pass above
	// still fail-loads an unset ${VAR:?...} or a non-bare reserved token
	// inside them.
	if len(rawConnectors.MCPServers) != len(m.MCPServers) || len(rawConnectors.HTTPTools) != len(m.HTTPTools) || len(rawConnectors.A2APeers) != len(m.A2APeers) {
		return nil, fmt.Errorf("client config manifest %s: env interpolation altered the connector list shape (mcp_servers %d -> %d, http_tools %d -> %d, a2a_peers %d -> %d); a substituted value must not inject YAML structure",
			manifestPath, len(rawConnectors.MCPServers), len(m.MCPServers), len(rawConnectors.HTTPTools), len(m.HTTPTools), len(rawConnectors.A2APeers), len(m.A2APeers))
	}
	for i := range m.MCPServers {
		m.MCPServers[i].Env = rawConnectors.MCPServers[i].Env
		m.MCPServers[i].Headers = rawConnectors.MCPServers[i].Headers
	}
	for i := range m.HTTPTools {
		m.HTTPTools[i].Headers = rawConnectors.HTTPTools[i].Headers
	}
	for i := range m.A2APeers {
		m.A2APeers[i].Headers = rawConnectors.A2APeers[i].Headers
	}

	b := &Bundle{
		Dir:                            abs,
		connectorEnvVarNames:           connectorEnvVars,
		connectorAccountVarNames:       connectorAccountVars,
		manifestEnvVarNames:            refNames,
		manifestDefaultOnlyEnvVarNames: defaultOnlyRefNames(refSites),
		Branding:                       m.Branding,
		Models:                         m.Models,
		EmptyState:                     m.EmptyState,
		TaskTemplates:                  m.TaskTemplates,
		MCPCatalog:                     m.MCPServers,
		HTTPTools:                      m.HTTPTools,
		A2APeers:                       m.A2APeers,
		WebhookTriggers:                m.WebhookTriggers,
		RemoteMCPCatalog:               m.RemoteMCPs,
		Providers:                      m.Providers,
		FallbackProviders:              m.FallbackProviders,
		AgentPolicyConfig:              m.AgentPolicy,
		HooksConfig:                    m.Hooks,
		Personas:                       m.Personas,
		PricingConfig:                  m.Pricing,
		SandboxConfig:                  resolveSandbox(m.Sandbox, abs),
		sandboxDeclared:                m.Sandbox != nil,
		sandboxContainerfileNamed:      m.Sandbox != nil && strings.TrimSpace(m.Sandbox.Containerfile) != "",
		SystemPromptsDir:               filepath.Join(abs, "system_prompts"),
		PersonasDir:                    filepath.Join(abs, "personas"),
		ProtocolsDir:                   filepath.Join(abs, "protocols"),
		PromptsDir:                     filepath.Join(abs, "prompts"),
		SkillsDir:                      filepath.Join(abs, "skills"),
		MCPDir:                         filepath.Join(abs, "mcp"),
		EvalsDir:                       filepath.Join(abs, "evals"),
	}
	if err := b.resolveBranding(); err != nil {
		return nil, err
	}
	if err := b.validate(); err != nil {
		return nil, err
	}
	// Agent Plugins (plugins.go, ADR-0054): translate each plugin's mcp.json
	// into catalog ServerDefs — after validate, so the manifest's own server
	// and http_tool names are known and win any collision — and collect its
	// skills for the merged tree built below. Never fails the load: every
	// defect is a warning, at the narrowest boundary the spec defines.
	for _, r := range m.PluginRoots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if !filepath.IsAbs(r) {
			r = filepath.Join(abs, r)
		}
		b.PluginRoots = append(b.PluginRoots, filepath.Clean(r))
	}
	taken := map[string]bool{HTTPToolServerName: true, A2AToolServerName: true}
	for i := range b.MCPCatalog {
		taken[b.MCPCatalog[i].Name] = true
	}
	for i := range b.HTTPTools {
		taken[b.HTTPTools[i].Name] = true
	}
	// a2a peers (#1368) share the tool namespace too: a plugin server named
	// like a peer would shadow its mcp__a2a_<peer>_* tools in the roster.
	for i := range b.A2APeers {
		taken[b.A2APeers[i].Name] = true
	}
	pl := loadPlugins(abs, m.PluginRoots, taken)
	b.Plugins = pl.plugins
	b.pluginProblems = pl.problems
	b.pluginRootProblems = pl.rootProblems
	b.pluginSkillOverlays = pl.overlays
	b.MCPCatalog = append(b.MCPCatalog, pl.servers...)
	// Inherit fleet's embedded hosted-server directory (after validate: bundle
	// entries are held to the manifest rules above; built-in entries are
	// CI-validated where they are authored, in builtin_remote_catalog.yaml).
	merged, err := mergeBuiltinRemoteCatalog(b.RemoteMCPCatalog, b.MCPCatalog, m.RemoteMCPBuiltin, m.RemoteMCPCommunity, m.RemoteMCPHidden)
	if err != nil {
		return nil, err
	}
	b.RemoteMCPCatalog = merged
	// Inherit fleet's embedded Agent Skills pack by materializing a merged
	// skills dir and pointing SkillsDir at it (bundle skills win collisions).
	// Degrades loudly to the bundle's own dir on failure — skills are a
	// capability, not a boot invariant.
	b.BundleSkillsDir = b.SkillsDir
	b.skillsBuiltin = m.SkillsBuiltin == nil || *m.SkillsBuiltin
	b.skillsHidden = m.SkillsHidden
	if mergedSkills, serr := materializeMergedSkills(b.BundleSkillsDir, b.skillsBuiltin, b.skillsHidden, b.pluginSkillOverlays); serr != nil {
		log.Printf("clientconfig: warning: builtin skills pack / plugin skills unavailable (%v) — using bundle skills only", serr)
	} else {
		b.SkillsDir = mergedSkills
	}
	// Warn (don't fail) on Agent Plugin defects: an unknown plugin.json field,
	// a skipped skill or server entry, a rejected plugin. The spec's failure
	// boundaries keep the rest loading; surface each so the author notices.
	for _, p := range b.pluginProblems {
		log.Printf("clientconfig: warning: plugin: %s", p)
	}
	if len(b.Plugins) > 0 {
		names := make([]string, 0, len(b.Plugins))
		for _, p := range b.Plugins {
			names = append(names, fmt.Sprintf("%s(skills=%d,mcp=%d)", p.Name, len(p.Skills), len(p.MCPServers)))
		}
		log.Printf("clientconfig: agent plugins loaded: %s", strings.Join(names, " "))
	}
	// Warn (don't fail) on stdio script-path args that don't resolve under the
	// bundle — a misspelled/missing `mcp/foo.py` would otherwise only surface as
	// a silent connector launch failure at runtime.
	for _, p := range b.ValidateMCPArgPaths() {
		log.Printf("clientconfig: warning: %s", p)
	}
	// Warn (don't fail) on malformed Agent Skills — a missing SKILL.md, bad
	// frontmatter, name/folder mismatch, or empty description. A defective skill
	// is skipped from the prompt roster but should not block the load; surface it
	// loudly so the author notices. A CI test asserts the shipped bundle is clean.
	for _, p := range b.ValidateSkills() {
		log.Printf("clientconfig: warning: %s", p)
	}
	return b, nil
}

// resolveSandbox turns the manifest's sandbox: block into a resolved Sandbox.
// The Containerfile path is resolved against the bundle dir; an unset
// containerfile defaults to the conventional <bundle>/sandbox/Containerfile.
// Tag defaults to the generic build-on-box tag. Image carries the optional
// prebuilt override verbatim (already env-interpolated by the manifest pass).
func resolveSandbox(sm *sandboxManifest, bundleDir string) Sandbox {
	var raw sandboxManifest
	if sm != nil {
		raw = *sm
	}
	cf := strings.TrimSpace(raw.Containerfile)
	if cf == "" {
		cf = "sandbox/Containerfile"
	}
	tag := strings.TrimSpace(raw.Tag)
	if tag == "" {
		tag = "localhost/fleet-sandbox:latest"
	}
	var allowlist []string
	for _, d := range raw.NetworkAllowlist {
		if d = strings.TrimSpace(d); d != "" {
			allowlist = append(allowlist, d)
		}
	}
	var k8s KubernetesSandbox
	if raw.Kubernetes != nil {
		k8s = *raw.Kubernetes
	}
	return Sandbox{
		ContainerfileAbsPath: filepath.Join(bundleDir, cf),
		Tag:                  tag,
		Image:                strings.TrimSpace(raw.Image),
		Runtime:              strings.TrimSpace(raw.Runtime),
		NetworkAllowlist:     allowlist,
		Backend:              strings.ToLower(strings.TrimSpace(raw.Backend)),
		Kubernetes:           k8s,
	}
}

// Sandbox returns the bundle's resolved execution-sandbox descriptor.
func (b *Bundle) Sandbox() Sandbox {
	return b.SandboxConfig
}

// AlwaysOnServer is the read-only view of a non-Optional enabled connector for
// the availability UI: these run in every turn with no opt-in decision, so the
// connections page renders them as visible-but-locked rows (an invisible
// always-on connector is a security-review smell — users should see what is
// implicitly wired into every conversation).
type AlwaysOnServer struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
	Description string `json:"description,omitempty"`
}

// AlwaysOnServers returns the bundle's enabled, non-Optional connectors.
func (b *Bundle) AlwaysOnServers() []AlwaysOnServer {
	if b == nil {
		return nil
	}
	if b.MCPCatalog == nil && b.scrubbedAlwaysOnServers != nil {
		return append([]AlwaysOnServer(nil), b.scrubbedAlwaysOnServers...)
	}
	var out []AlwaysOnServer
	for i := range b.MCPCatalog {
		sd := &b.MCPCatalog[i]
		if sd.Optional || !sd.enabled() {
			continue
		}
		out = append(out, AlwaysOnServer{Name: sd.Name, DisplayName: sd.DisplayName, Description: sd.Description})
	}
	return out
}

// ConnectorMeta is the public picker view of one manifest connector, enabled
// or not: its category, and whether its credentials are set. The Tools picker
// shows an unconfigured connector as "Needs setup" instead of dropping it, so
// people can find out it exists and who has to set it up. Tools is the
// manifest allowlist (the only tool names known without launching it). No
// env names or values: the copy rule is "no setup steps in the picker".
type ConnectorMeta struct {
	Name        string
	DisplayName string
	Description string
	Category    string
	Optional    bool
	Configured  bool
	Tools       []string
}

// ConnectorMeta returns every manifest connector's picker metadata in manifest
// order, Category resolved (Misc Tools when untagged).
func (b *Bundle) ConnectorMeta() []ConnectorMeta {
	if b == nil {
		return nil
	}
	if b.MCPCatalog == nil && b.scrubbedConnectorMeta != nil {
		return append([]ConnectorMeta(nil), b.scrubbedConnectorMeta...)
	}
	out := make([]ConnectorMeta, 0, len(b.MCPCatalog))
	for i := range b.MCPCatalog {
		sd := &b.MCPCatalog[i]
		cat := sd.Category
		if cat == "" {
			cat = MiscConnectorCategory
		}
		out = append(out, ConnectorMeta{
			Name:        sd.Name,
			DisplayName: sd.DisplayName,
			Description: sd.Description,
			Category:    cat,
			Optional:    sd.Optional,
			Configured:  sd.enabled(),
			Tools:       append([]string(nil), sd.Tools...),
		})
	}
	return out
}

// ConnectorCategories is the picker's category list for this deployment:
// the fixed list with the house category named from branding.
func (b *Bundle) ConnectorCategories() []ConnectorCategory {
	name := ""
	if b != nil {
		name = strings.TrimSpace(b.Branding.HouseToolsName)
		if name == "" && strings.TrimSpace(b.Branding.AppName) != "" {
			name = strings.TrimSpace(b.Branding.AppName) + " Tools"
		}
	}
	return connectorCategoriesNamed(name)
}

// UncategorizedConnectors names the manifest connectors with no category, for
// the startup warning that gets them tagged.
func (b *Bundle) UncategorizedConnectors() []string {
	if b == nil {
		return nil
	}
	var out []string
	for i := range b.MCPCatalog {
		if b.MCPCatalog[i].Category == "" {
			out = append(out, b.MCPCatalog[i].Name)
		}
	}
	return out
}

// WebhookTrigger returns the manifest webhook trigger for slug (defensively
// copied) and whether one exists (#268). The lookup is a linear scan over the
// manifest-order slice; the catalog is small and read once per inbound request.
// An unknown slug returns (zero, false), which the handler treats identically to
// a bad signature (timing-equalized 401) so a caller cannot enumerate slugs.
func (b *Bundle) WebhookTrigger(slug string) (WebhookTriggerDef, bool) {
	want := strings.TrimSpace(slug)
	if want == "" {
		return WebhookTriggerDef{}, false
	}
	for i := range b.WebhookTriggers {
		if strings.TrimSpace(b.WebhookTriggers[i].Slug) == want {
			return b.WebhookTriggers[i], true
		}
	}
	return WebhookTriggerDef{}, false
}

// MCPServerConfigs builds the runtime catalog (map[name]config.MCPServerConfig)
// from the manifest, resolving env values + the enable gate against the current
// process environment. Only enabled servers are returned. Manifest stdio args
// are kept verbatim (relative to the bundle root, e.g. `mcp/foo.py`); each stdio
// server's Dir is set to the bundle root so its subprocess launches there and
// the relative args resolve correctly — the fleet process cwd is NOT necessarily
// the bundle dir (under systemd it is /opt/fleet, while the bundle is the
// separate /opt/fleet/client checkout). See internal/mcp.AddStdioServer.
//
// This REPLACES the formerly hardcoded internal/config catalog: the same Go
// struct + downstream behavior (tool allowlists, account suffixes via the env
// keys, command/args), now sourced from the bundle.
func (b *Bundle) MCPServerConfigs() map[string]config.MCPServerConfig {
	// Absolutize the bundle dir so cmd.Dir is correct regardless of the spawning
	// process's cwd; fall back to the raw dir if Abs fails.
	bundleDir := b.Dir
	if abs, err := filepath.Abs(b.Dir); err == nil {
		bundleDir = abs
	}
	out := make(map[string]config.MCPServerConfig, len(b.MCPCatalog))
	for i := range b.MCPCatalog {
		s := &b.MCPCatalog[i]
		if !s.enabled() {
			continue
		}
		sc := config.MCPServerConfig{
			Type:          s.Type,
			Enabled:       true,
			ToolAllowlist: append([]string(nil), s.Tools...),
			AccountVars:   append([]string(nil), s.AccountVars...),
			// Carry the Optional-server metadata so the chat path can gate
			// optional connectors and render the settings-UI catalog. Dropping
			// these here was the bug behind the 128-tool ceiling overflow.
			Optional:         s.Optional,
			DisplayName:      s.DisplayName,
			Description:      s.Description,
			Beta:             s.Beta,
			EnabledByDefault: s.EnabledByDefault,
			DataSources:      append([]string(nil), s.DataSources...),
			Probe:            s.Probe.toConfig(),
		}
		switch s.Type {
		case "http":
			sc.URL = s.URL
			if s.literalEnv {
				sc.Headers = copyStringMap(s.Headers)
			} else {
				sc.Headers = resolveEnvMap(s.Headers, nil)
			}
			sc.TLS = s.TLS.toMCP()
		default: // stdio
			sc.Command = s.Command
			sc.Args = append([]string(nil), s.Args...)
			if s.literalEnv {
				// An Agent Plugin server: the loader already applied the spec's
				// only permitted expansion; ${VAR}-looking text must stay literal.
				sc.Env = copyStringMap(s.Env)
			} else {
				sc.Env = resolveEnvMap(s.Env, s.OptionalEnv)
			}
			// Absolutize bundle-relative script args (mcp/foo.py →
			// <bundle>/mcp/foo.py). Until now the subprocess started only
			// because its cwd WAS the bundle root, which is exactly what
			// forced every server's relative output path into the operator's
			// git checkout. Resolving here decouples "the script fleet
			// launches" from "the directory the server writes into", so the
			// cwd is free to be a managed workspace. Non-script args, absolute
			// args and Agent Plugin args (opaque by spec) are left untouched;
			// a script arg that does not resolve is left as-is so
			// ValidateMCPArgPaths still reports it as the warning it is.
			if s.plugin == "" {
				sc.Args = absolutizeScriptArgs(bundleDir, sc.Args)
			}
			// Dir is only the FALLBACK cwd; a spawn path with a
			// fleet-managed workspace launches the subprocess there instead
			// (agentcore.StdioCwd). Script args were absolutized above, so the
			// bundle root is no longer load-bearing for launching.
			sc.Dir = bundleDir
			if s.dir != "" {
				// An Agent Plugin server launches in its plugin root (or declared
				// cwd) — a spec contract, not a default, so it is pinned.
				sc.Dir = s.dir
				sc.DirPinned = true
			}
			sc.IdentityEnv = append([]string(nil), s.IdentityEnv...)
		}
		out[s.Name] = sc
	}
	return out
}

// HTTPToolConfigs builds the runtime inline-HTTP-tool catalog from the manifest's
// http_tools[] section, resolving each header's ${ENV_VAR} references against the
// current process env — exactly as MCPServerConfigs resolves an HTTP MCP server's
// headers. It is therefore called only in a process that legitimately holds the
// connector credentials (cmd/fleet, the mcp-broker, cutlass); the resolved secrets
// live in config.HTTPToolConfig.Headers and are applied to the outbound request
// host-side at call time, never entering the sandbox or the model context.
//
// Returns the slice in manifest order. Empty in the generic bundle (no http_tools)
// — the default, which registers no HTTP tools and changes nothing.
func (b *Bundle) HTTPToolConfigs() []config.HTTPToolConfig {
	if len(b.HTTPTools) == 0 {
		return nil
	}
	out := make([]config.HTTPToolConfig, 0, len(b.HTTPTools))
	for i := range b.HTTPTools {
		t := &b.HTTPTools[i]
		out = append(out, config.HTTPToolConfig{
			Name:         t.Name,
			Description:  t.Description,
			Method:       t.Method,
			URL:          t.URL,
			Headers:      resolveEnvMap(t.Headers, nil),
			BodyTemplate: t.BodyTemplate,
			InputSchema:  t.InputSchema,
			ResponseJQ:   t.ResponseJQ,
			Critical:     t.Critical,
		})
	}
	return out
}

// A2APeerConfigs resolves the manifest's a2a_peers[] into the credential-bearing
// runtime shape (#1368): header ${ENV_VAR} references are expanded against the
// HOST process env here, at catalog-build time — same seam as HTTPToolConfigs.
func (b *Bundle) A2APeerConfigs() []config.A2APeerConfig {
	if len(b.A2APeers) == 0 {
		return nil
	}
	out := make([]config.A2APeerConfig, 0, len(b.A2APeers))
	for i := range b.A2APeers {
		p := &b.A2APeers[i]
		out = append(out, config.A2APeerConfig{
			Name:        p.Name,
			Description: p.Description,
			RPCURL:      p.RPCURL,
			Headers:     resolveEnvMap(p.Headers, nil),
			Critical:    p.Critical,
		})
	}
	return out
}

// scriptExtensions are the arg suffixes ValidateMCPArgPaths treats as a script
// file path that must resolve under the bundle dir.
var scriptExtensions = map[string]bool{
	".py": true, ".js": true, ".mjs": true, ".cjs": true, ".ts": true, ".sh": true, ".rb": true,
}

// ValidateMCPArgPaths checks that every stdio server's relative script-path args
// (args ending in a known script extension, e.g. `mcp/foo.py`) resolve to a file
// under the bundle dir. It returns one human-readable problem per missing path;
// an empty slice means all paths resolve. This catches the deploy-time failure
// where a bundle ships `args: ["mcp/foo.py"]` but the file is absent or
// misspelled — the MCP subprocess would otherwise just fail to launch at runtime
// (see internal/mcp cmd.Dir). It is checked for ALL stdio servers regardless of
// the credential enable-gate, since a missing script is a defect whether or not
// the connector's creds happen to be set. Load logs any problems as warnings; a
// CI test asserts the shipped bundle returns none.
// absolutizeScriptArgs rewrites bundle-relative script args to absolute paths
// against bundleDir. It is deliberately conservative and mirrors
// ValidateMCPArgPaths' matching rules exactly: only a RELATIVE arg with a known
// script extension that actually resolves to a file under the bundle is
// rewritten. Everything else — flags, absolute paths, non-script values, and
// anything that does not exist — is passed through unchanged, so a typo still
// surfaces as the ValidateMCPArgPaths warning rather than being silently
// transformed into a different-looking one.
func absolutizeScriptArgs(bundleDir string, args []string) []string {
	if bundleDir == "" || len(args) == 0 {
		return args
	}
	var out []string
	for i, arg := range args {
		if filepath.IsAbs(arg) || !scriptExtensions[strings.ToLower(filepath.Ext(arg))] {
			continue
		}
		p := filepath.Join(bundleDir, arg)
		if info, err := os.Stat(p); err != nil || info.IsDir() {
			continue
		}
		if out == nil {
			out = append([]string(nil), args...)
		}
		out[i] = p
	}
	if out == nil {
		return args
	}
	return out
}

func (b *Bundle) ValidateMCPArgPaths() []string {
	bundleDir := b.Dir
	if abs, err := filepath.Abs(b.Dir); err == nil {
		bundleDir = abs
	}
	var problems []string
	for i := range b.MCPCatalog {
		s := &b.MCPCatalog[i]
		if s.Type == "http" {
			continue
		}
		if s.plugin != "" {
			// Agent Plugin args are opaque strings by spec (§4.1) and the
			// server launches in the plugin root, not the bundle — the
			// bundle-relative check below does not apply.
			continue
		}
		for _, arg := range s.Args {
			if filepath.IsAbs(arg) || !scriptExtensions[strings.ToLower(filepath.Ext(arg))] {
				continue
			}
			p := filepath.Join(bundleDir, arg)
			if info, err := os.Stat(p); err != nil || info.IsDir() {
				problems = append(problems, fmt.Sprintf(
					"mcp_servers[%q]: script arg %q does not resolve to a file under the bundle (looked for %s)",
					s.Name, arg, p))
			}
		}
	}
	return problems
}

// AgentPolicy returns the bundle's agent tool-behavior policy (defensively
// copied). The generic bundle returns an empty policy, leaving agentcore on its
// base generic critical suffixes with no parallel-safe or DSP-specific tools.
func (b *Bundle) AgentPolicy() AgentPolicy {
	p := AgentPolicy{
		ParallelSafeTools:    append([]string(nil), b.AgentPolicyConfig.ParallelSafeTools...),
		CriticalToolSuffixes: append([]string(nil), b.AgentPolicyConfig.CriticalToolSuffixes...),
		// The email-last lists (audit_email_last.go): without these copies the
		// boot paths and validate-config see nil and the gate is inert.
		EmailLastTools:                append([]string(nil), b.AgentPolicyConfig.EmailLastTools...),
		SettleableCreateTools:         append([]string(nil), b.AgentPolicyConfig.SettleableCreateTools...),
		CriticalToolNoSessionApproval: append([]string(nil), b.AgentPolicyConfig.CriticalToolNoSessionApproval...),
		CriticalToolResume:            append([]string(nil), b.AgentPolicyConfig.CriticalToolResume...),
		CriticalToolResumeMaxPerHour:  b.AgentPolicyConfig.CriticalToolResumeMaxPerHour,
		CriticalToolGroupApproval:     append([]string(nil), b.AgentPolicyConfig.CriticalToolGroupApproval...),
		CriticalToolProgress:          append([]string(nil), b.AgentPolicyConfig.CriticalToolProgress...),
		CriticalToolResultCards:       append([]string(nil), b.AgentPolicyConfig.CriticalToolResultCards...),
		CriticalToolStop:              append([]string(nil), b.AgentPolicyConfig.CriticalToolStop...),
		InteractiveHiddenTools:        append([]string(nil), b.AgentPolicyConfig.InteractiveHiddenTools...),
		UnattendedDisabledTools:       append([]string(nil), b.AgentPolicyConfig.UnattendedDisabledTools...),
	}
	// An http_tool flagged `critical: true` opts into the SAME critical-tool audit
	// gate as the manifest's critical_tools suffixes. The tool is registered as
	// mcp__http_<name>, and isCriticalTool matches on a trailing "_<suffix>", so its
	// bare name is the suffix that selects it. (One source of truth: the gate stays
	// agentcore's; this only contributes the names.)
	for i := range b.HTTPTools {
		if b.HTTPTools[i].Critical {
			if name := strings.TrimSpace(b.HTTPTools[i].Name); name != "" {
				p.CriticalToolSuffixes = append(p.CriticalToolSuffixes, name)
			}
		}
	}
	// An a2a peer flagged `critical: true` gates the two operations with side
	// effects — <peer>_send (work leaves the box, cost lands on the remote)
	// and <peer>_cancel. The registered names end in "_<peer>_send", so the
	// derived tool name is the suffix; status/wait are reads and stay free.
	for i := range b.A2APeers {
		if b.A2APeers[i].Critical {
			if name := strings.TrimSpace(b.A2APeers[i].Name); name != "" {
				p.CriticalToolSuffixes = append(p.CriticalToolSuffixes, name+"_send", name+"_cancel")
			}
		}
	}
	if len(b.AgentPolicyConfig.CriticalToolSubstitutes) > 0 {
		p.CriticalToolSubstitutes = make(map[string][]string, len(b.AgentPolicyConfig.CriticalToolSubstitutes))
		for k, v := range b.AgentPolicyConfig.CriticalToolSubstitutes {
			p.CriticalToolSubstitutes[k] = append([]string(nil), v...)
		}
	}
	if len(b.AgentPolicyConfig.CriticalToolTimeouts) > 0 {
		p.CriticalToolTimeouts = make(map[string]int, len(b.AgentPolicyConfig.CriticalToolTimeouts))
		for k, v := range b.AgentPolicyConfig.CriticalToolTimeouts {
			p.CriticalToolTimeouts[k] = v
		}
	}
	if len(b.AgentPolicyConfig.CriticalToolModes) > 0 {
		p.CriticalToolModes = make(map[string]string, len(b.AgentPolicyConfig.CriticalToolModes))
		for k, v := range b.AgentPolicyConfig.CriticalToolModes {
			p.CriticalToolModes[k] = v
		}
	}
	if len(b.AgentPolicyConfig.CriticalToolUndoHints) > 0 {
		p.CriticalToolUndoHints = make(map[string]string, len(b.AgentPolicyConfig.CriticalToolUndoHints))
		for k, v := range b.AgentPolicyConfig.CriticalToolUndoHints {
			p.CriticalToolUndoHints[k] = v
		}
	}
	if len(b.AgentPolicyConfig.CriticalToolCardDescribers) > 0 {
		p.CriticalToolCardDescribers = make(map[string]string, len(b.AgentPolicyConfig.CriticalToolCardDescribers))
		for k, v := range b.AgentPolicyConfig.CriticalToolCardDescribers {
			p.CriticalToolCardDescribers[k] = v
		}
	}
	if len(b.AgentPolicyConfig.CriticalToolNarrowableArgs) > 0 {
		p.CriticalToolNarrowableArgs = make(map[string]NarrowableArgConfig, len(b.AgentPolicyConfig.CriticalToolNarrowableArgs))
		for k, v := range b.AgentPolicyConfig.CriticalToolNarrowableArgs {
			p.CriticalToolNarrowableArgs[k] = v
		}
	}
	if len(b.AgentPolicyConfig.CriticalToolAliases) > 0 {
		p.CriticalToolAliases = make(map[string][]string, len(b.AgentPolicyConfig.CriticalToolAliases))
		for k, v := range b.AgentPolicyConfig.CriticalToolAliases {
			p.CriticalToolAliases[k] = append([]string(nil), v...)
		}
	}
	// A server's declared per-record batch budget (mcp_servers[].
	// batch_seconds_per_deal) and approved-card call budget
	// (approved_call_timeout_seconds) are server behavior the engine must not
	// know by name; they ride the policy keyed by the manifest server name.
	for i := range b.MCPCatalog {
		if secs := b.MCPCatalog[i].BatchSecondsPerDeal; secs > 0 {
			if p.BatchSecondsPerDeal == nil {
				p.BatchSecondsPerDeal = map[string]int{}
			}
			p.BatchSecondsPerDeal[b.MCPCatalog[i].Name] = secs
		}
		if secs := b.MCPCatalog[i].ApprovedCallTimeoutSeconds; secs > 0 {
			if p.ApprovedCallTimeoutSeconds == nil {
				p.ApprovedCallTimeoutSeconds = map[string]int{}
			}
			p.ApprovedCallTimeoutSeconds[b.MCPCatalog[i].Name] = secs
		}
	}
	return p
}

// PersonaToolPolicy returns the manifest tool-permission policy for the named
// persona (#294), defensively copied, and whether an entry exists. The name is
// the persona basename (with any directory / .yaml extension stripped) so a
// caller can pass either "code-reviewer" or "code-reviewer.yaml". A persona
// with no manifest entry returns (zero, false); the caller treats that as "no
// narrowing" (sees all permitted tools). An entry whose lists are both empty
// returns (zero-valued-but-present, true), which is functionally identical to
// no narrowing — the policy can only ever subtract.
func (b *Bundle) PersonaToolPolicy(name string) (PersonaToolPermissions, bool) {
	want := personaBaseName(name)
	if want == "" {
		return PersonaToolPermissions{}, false
	}
	for i := range b.Personas {
		if personaBaseName(b.Personas[i].Name) != want {
			continue
		}
		src := b.Personas[i].ToolPermissions
		return PersonaToolPermissions{
			Allow: append([]string(nil), src.Allow...),
			Deny:  append([]string(nil), src.Deny...),
		}, true
	}
	return PersonaToolPermissions{}, false
}

// personaBaseName normalizes a persona reference to its bare basename: it
// strips any directory and a trailing .yaml/.yml extension and trims spaces, so
// "personas/code-reviewer.yaml", "code-reviewer.yaml", and "code-reviewer" all
// resolve to the same key. The drivers identify personas by this basename when
// matching a run's persona against the manifest entries.
func personaBaseName(name string) string {
	base := filepath.Base(strings.TrimSpace(name))
	if base == "." || base == string(filepath.Separator) {
		return ""
	}
	if ext := filepath.Ext(base); ext == ".yaml" || ext == ".yml" {
		base = strings.TrimSuffix(base, ext)
	}
	return strings.TrimSpace(base)
}

// Pricing returns the bundle's custom model-pricing config (defensively copied),
// with the fallback normalized to lower-case (and a blank fallback left blank so
// the agentcore layer applies its OpenRouter default). The generic bundle ships
// no overrides, so this returns an empty config and cost accounting stays on the
// OpenRouter-returned price — identical to pre-#297 behavior.
func (b *Bundle) Pricing() PricingConfig {
	p := PricingConfig{Fallback: strings.ToLower(strings.TrimSpace(b.PricingConfig.Fallback))}
	if len(b.PricingConfig.Overrides) > 0 {
		p.Overrides = append([]PricingOverride(nil), b.PricingConfig.Overrides...)
	}
	return p
}

// FromPlugin reports whether this catalog entry came from an Agent Plugin's
// mcp.json rather than the manifest. Plugin servers launch in the plugin root,
// not the bundle, so bundle-relative path checks (script args, a
// ./bin/server command) do not apply to them — the plugin loader already
// resolved and contained those paths against the plugin root.
func (s *ServerDef) FromPlugin() bool { return s.plugin != "" }

// enabled evaluates the server's gate against the process env.
func (s *ServerDef) enabled() bool {
	if s.Always {
		return true
	}
	if len(s.EnabledGroups) > 0 {
		for _, group := range s.EnabledGroups {
			if allSet(group) {
				return true
			}
		}
		// When groups are declared they are the sole gate.
		if len(s.EnabledEnv) == 0 {
			return false
		}
	}
	if len(s.EnabledEnv) == 0 {
		// No gate declared and not Always: default OFF (the generic catalog is
		// empty, so this only affects a misconfigured manifest entry).
		return false
	}
	return allSet(s.EnabledEnv)
}

// allSet reports whether every named process-env var has a non-empty value.
func allSet(vars []string) bool {
	for _, v := range vars {
		if strings.TrimSpace(envreg.Unregistered(v)) == "" {
			return false
		}
	}
	return len(vars) > 0
}

// resolveEnvMap interpolates the ${...} references (all the forms interpolate
// supports) against the process env and drops keys whose resolved value is
// empty AND listed in optional. A value with no ${...} reference is passed
// through literally.
func resolveEnvMap(in map[string]string, optional []string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	opt := make(map[string]bool, len(optional))
	for _, k := range optional {
		opt[k] = true
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		resolved, assembled := interpolate(v)
		if assembled {
			// The value names a user token that the manifest did not write:
			// it was assembled from resolved fragments ("${A}${B}" with
			// A="${FLEET_USER_" and B="EMAIL}"). Substituting it at spawn
			// would fill a key the load checks never saw — an identity_env
			// key included — with whoever is at the keyboard. Drop the key.
			warnUserTokenAssembled(k)
			continue
		}
		if resolved == "" && opt[k] {
			continue
		}
		out[k] = resolved
	}
	return out
}

// copyStringMap returns a shallow copy of in (never nil), for values that are
// already final and must not be interpolated.
func copyStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
