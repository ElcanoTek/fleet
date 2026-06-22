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
//	                       #   agent_policy{parallel/critical tool lists}
//	  system_prompts/      # default.md (scheduled base), chat.md (interactive base)
//	  personas/            # *.yaml
//	  protocols/           # *.yaml|md
//	  mcp/                 # the client's Python MCP servers + requirements.txt
//
// The MCP catalog is declarative: each entry names the subprocess command/args
// (args resolve relative to the bundle's mcp/ dir), an enable gate over process
// env vars, the per-subprocess env (each value supports ${VAR} interpolation
// from the process env), an optional tool allowlist, and the base credential
// vars used by the account-suffix scan (creds.ApplyClientSuffix / AccountsFor).
// Credential VALUES never live in the manifest — only the env-var NAMES do; the
// loader resolves them from the process environment at Load time.
package clientconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/ElcanoTek/fleet/internal/config"
)

// EnvDir is the environment variable naming the client bundle directory.
const EnvDir = "FLEET_CLIENT_CONFIG_DIR"

// DefaultDir is the generic bundle shipped in the repo, used when EnvDir is
// unset. Relative to the process working directory (cmd/fleet resolves it
// against the repo root the same way it resolves the legacy supporting dirs).
const DefaultDir = "config/default"

// Bundle is the loaded, validated client configuration.
type Bundle struct {
	// Dir is the absolute path to the bundle root.
	Dir string

	Branding   Branding
	Models     Models
	EmptyState EmptyState

	// MCPCatalog is the declarative server catalog from the manifest, in
	// manifest order.
	MCPCatalog []ServerDef

	// AgentPolicyConfig carries the bundle's client-specific agent tool-behavior
	// lists (parallel-safe tools, critical-tool suffixes, substitute map). Empty
	// in the generic bundle. cmd/fleet translates it into agentcore.AgentPolicy.
	AgentPolicyConfig AgentPolicy

	// Resolved absolute directories inside the bundle. These are the
	// same-path bind-mount sources and the source dirs the prompt/persona/
	// protocol loaders read.
	SystemPromptsDir string
	PersonasDir      string
	ProtocolsDir     string
	MCPDir           string
}

// AgentPolicy is the bundle's client-configurable agent tool-behavior policy. It
// is a plain data struct (no dependency on internal/agentcore) so clientconfig
// stays a low-level package; cmd/fleet translates it into agentcore.AgentPolicy.
//
//   - ParallelSafeTools: fully-prefixed MCP tool names (mcp_<server>_<tool>)
//     safe to dispatch concurrently within a single assistant turn.
//   - CriticalToolSuffixes: bare tool-name suffixes that require audit gating
//     before execution (the generic send_email/send_template_email base
//     suffixes are added unconditionally by agentcore, so the manifest lists
//     only the client-specific ones).
//   - CriticalToolSubstitutes: committed-suffix -> allowed executed substitute
//     suffixes that may discharge the commitment.
type AgentPolicy struct {
	ParallelSafeTools       []string            `yaml:"parallel_safe_tools"`
	CriticalToolSuffixes    []string            `yaml:"critical_tools"`
	CriticalToolSubstitutes map[string][]string `yaml:"critical_tool_substitutes"`
}

// Branding carries the white-label strings surfaced in the web UI + login.
type Branding struct {
	AppName          string `yaml:"app_name"`
	LoginTitle       string `yaml:"login_title"`
	LoginTagline     string `yaml:"login_tagline"`
	ShareTitle       string `yaml:"share_title"`
	ShareDescription string `yaml:"share_description"`
}

// Models carries the default model tiers. Informational/advisory — the running
// config still resolves the operative model from env + per-turn slug. Exposed
// so the bundle is self-describing and the web can show sensible defaults.
type Models struct {
	DefaultCore string `yaml:"default_core"`
	DefaultMax  string `yaml:"default_max"`
}

// EmptyState carries the chat empty-state catalog rendered by the web.
// Cards and ProtocolPills are passed through to the browser verbatim as opaque
// JSON (the shape is the web's ProtocolPill[]). The Go side never interprets
// them; it only validates that the manifest parsed and re-serializes them.
type EmptyState struct {
	Cards         []map[string]any `yaml:"cards"`
	ProtocolPills []map[string]any `yaml:"protocol_pills"`
}

// ServerDef is one declarative MCP server in the catalog.
type ServerDef struct {
	Name    string `yaml:"name"`
	Type    string `yaml:"type"` // "stdio" | "http"
	Command string `yaml:"command"`
	Args    []string

	// URL/Headers for http servers.
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"`

	// Enable gate. When Always is true the server is unconditionally enabled.
	// Otherwise the server is enabled iff EVERY var in EnabledEnv is non-empty
	// (after env interpolation), OR — if EnabledGroups is set — if ANY group's
	// vars are all non-empty (any-of-groups, e.g. token OR user+pass).
	Always        bool       `yaml:"always"`
	EnabledEnv    []string   `yaml:"enabled_env"`
	EnabledGroups [][]string `yaml:"enabled_groups"`

	// Env is the per-subprocess env. Each value may reference process-env vars
	// via ${VAR} (and a literal default tail). Keys whose resolved value is
	// empty AND listed in OptionalEnv are dropped from the spawned env.
	Env         map[string]string `yaml:"env"`
	OptionalEnv []string          `yaml:"optional_env"`

	// Tools is the per-server tool allowlist (empty = all advertised tools).
	Tools []string `yaml:"tools"`

	// AccountVars are the base credential vars the account-suffix scan uses to
	// derive the account catalog (creds.AccountsFor) and the per-account env
	// overlay (creds.ApplyClientSuffix). Informational for the catalog; the
	// actual overlay reads Env's keys.
	AccountVars []string `yaml:"account_vars"`

	// Optional marks a server users must opt into per conversation (chat's
	// Optional-server semantics). DisplayName/Description/Beta/EnabledByDefault
	// drive the settings-UI catalog rendering.
	Optional         bool   `yaml:"optional"`
	DisplayName      string `yaml:"display_name"`
	Description      string `yaml:"description"`
	Beta             bool   `yaml:"beta"`
	EnabledByDefault bool   `yaml:"enabled_by_default"`
}

// manifest is the on-disk YAML shape.
type manifest struct {
	Branding    Branding    `yaml:"branding"`
	Models      Models      `yaml:"models"`
	MCPServers  []ServerDef `yaml:"mcp_servers"`
	EmptyState  EmptyState  `yaml:"empty_state"`
	AgentPolicy AgentPolicy `yaml:"agent_policy"`
}

// Dir resolves the configured bundle directory: FLEET_CLIENT_CONFIG_DIR, else
// the generic default.
func Dir() string {
	if v := strings.TrimSpace(os.Getenv(EnvDir)); v != "" {
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
	var m manifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse manifest %s: %w", manifestPath, err)
	}

	b := &Bundle{
		Dir:               abs,
		Branding:          m.Branding,
		Models:            m.Models,
		EmptyState:        m.EmptyState,
		MCPCatalog:        m.MCPServers,
		AgentPolicyConfig: m.AgentPolicy,
		SystemPromptsDir:  filepath.Join(abs, "system_prompts"),
		PersonasDir:       filepath.Join(abs, "personas"),
		ProtocolsDir:      filepath.Join(abs, "protocols"),
		MCPDir:            filepath.Join(abs, "mcp"),
	}
	applyBrandingDefaults(&b.Branding)
	if err := b.validate(); err != nil {
		return nil, err
	}
	return b, nil
}

// applyBrandingDefaults fills neutral generic strings for any unset branding
// field so a sparse manifest still renders a coherent UI.
func applyBrandingDefaults(br *Branding) {
	if br.AppName == "" {
		br.AppName = "Fleet"
	}
	if br.LoginTitle == "" {
		br.LoginTitle = "Welcome aboard."
	}
	if br.LoginTagline == "" {
		br.LoginTagline = "Sign in to your workspace and pick up where you left off."
	}
	if br.ShareTitle == "" {
		br.ShareTitle = br.AppName
	}
	if br.ShareDescription == "" {
		br.ShareDescription = "An AI workspace with real tool use."
	}
}

// validate checks the MCP catalog for the structural invariants the spawn path
// relies on. Content dirs are NOT required to exist (a manifest-only bundle is
// valid); callers that read a specific file surface their own not-found errors.
func (b *Bundle) validate() error {
	seen := map[string]bool{}
	for i := range b.MCPCatalog {
		s := &b.MCPCatalog[i]
		if strings.TrimSpace(s.Name) == "" {
			return fmt.Errorf("mcp_servers[%d]: name is required", i)
		}
		if seen[s.Name] {
			return fmt.Errorf("mcp_servers: duplicate server name %q", s.Name)
		}
		seen[s.Name] = true
		switch s.Type {
		case "stdio", "":
			s.Type = "stdio"
			if strings.TrimSpace(s.Command) == "" {
				return fmt.Errorf("mcp_servers[%q]: stdio server requires a command", s.Name)
			}
		case "http":
			if strings.TrimSpace(s.URL) == "" {
				return fmt.Errorf("mcp_servers[%q]: http server requires a url", s.Name)
			}
		default:
			return fmt.Errorf("mcp_servers[%q]: unknown type %q (want stdio|http)", s.Name, s.Type)
		}
	}
	return nil
}

// MCPServerConfigs builds the runtime catalog (map[name]config.MCPServerConfig)
// from the manifest, resolving env values + the enable gate against the current
// process environment. Only enabled servers are returned. For stdio servers the
// command's relative args are resolved against the bundle's mcp/ dir parent so a
// bind-mounted bundle keeps the args correct; we keep args as the manifest
// supplied them (relative to the bundle root) because the agent process cwd is
// the bundle-aware root — see cmd/fleet.
//
// This REPLACES the formerly hardcoded internal/config catalog: the same Go
// struct + downstream behavior (tool allowlists, account suffixes via the env
// keys, command/args), now sourced from the bundle.
func (b *Bundle) MCPServerConfigs() map[string]config.MCPServerConfig {
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
		}
		switch s.Type {
		case "http":
			sc.URL = s.URL
			sc.Headers = resolveEnvMap(s.Headers, nil)
		default: // stdio
			sc.Command = s.Command
			sc.Args = append([]string(nil), s.Args...)
			sc.Env = resolveEnvMap(s.Env, s.OptionalEnv)
		}
		out[s.Name] = sc
	}
	return out
}

// AgentPolicy returns the bundle's agent tool-behavior policy (defensively
// copied). The generic bundle returns an empty policy, leaving agentcore on its
// base generic critical suffixes with no parallel-safe or DSP-specific tools.
func (b *Bundle) AgentPolicy() AgentPolicy {
	p := AgentPolicy{
		ParallelSafeTools:    append([]string(nil), b.AgentPolicyConfig.ParallelSafeTools...),
		CriticalToolSuffixes: append([]string(nil), b.AgentPolicyConfig.CriticalToolSuffixes...),
	}
	if len(b.AgentPolicyConfig.CriticalToolSubstitutes) > 0 {
		p.CriticalToolSubstitutes = make(map[string][]string, len(b.AgentPolicyConfig.CriticalToolSubstitutes))
		for k, v := range b.AgentPolicyConfig.CriticalToolSubstitutes {
			p.CriticalToolSubstitutes[k] = append([]string(nil), v...)
		}
	}
	return p
}

// EnvVarNames returns every process-env var name the manifest references —
// across enable gates, env interpolation, header interpolation, and account
// vars. cmd/fleet passes these to config.RegisterAllowedEnvVars so the bundle's
// connector credentials survive the .env-file load while fleet's static
// allowlist stays client-agnostic.
func (b *Bundle) EnvVarNames() []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	for i := range b.MCPCatalog {
		s := &b.MCPCatalog[i]
		for _, v := range s.EnabledEnv {
			add(v)
		}
		for _, group := range s.EnabledGroups {
			for _, v := range group {
				add(v)
			}
		}
		for _, v := range s.AccountVars {
			add(v)
		}
		for _, v := range s.Env {
			for _, name := range envRefs(v) {
				add(name)
			}
		}
		for _, v := range s.Headers {
			for _, name := range envRefs(v) {
				add(name)
			}
		}
	}
	return out
}

// envRefs extracts the ${VAR} names referenced in a manifest value.
func envRefs(v string) []string {
	var out []string
	for {
		start := strings.Index(v, "${")
		if start < 0 {
			return out
		}
		v = v[start+2:]
		end := strings.Index(v, "}")
		if end < 0 {
			return out
		}
		out = append(out, strings.TrimSpace(v[:end]))
		v = v[end+1:]
	}
}

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
		if strings.TrimSpace(os.Getenv(v)) == "" {
			return false
		}
	}
	return len(vars) > 0
}

// resolveEnvMap interpolates ${VAR} references against the process env and drops
// keys whose resolved value is empty AND listed in optional. A value with no
// ${...} reference is passed through literally.
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
		resolved := interpolate(v)
		if resolved == "" && opt[k] {
			continue
		}
		out[k] = resolved
	}
	return out
}

// interpolate replaces ${VAR} occurrences with the process-env value (empty
// when unset). A bare "${VAR}" with no surrounding text is the common case.
func interpolate(v string) string {
	if !strings.Contains(v, "${") {
		return v
	}
	var sb strings.Builder
	for {
		start := strings.Index(v, "${")
		if start < 0 {
			sb.WriteString(v)
			break
		}
		sb.WriteString(v[:start])
		end := strings.Index(v[start:], "}")
		if end < 0 {
			sb.WriteString(v[start:])
			break
		}
		name := v[start+2 : start+end]
		sb.WriteString(strings.TrimSpace(os.Getenv(name)))
		v = v[start+end+1:]
	}
	return sb.String()
}
