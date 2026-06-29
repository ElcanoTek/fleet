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
//	                       #   sandbox{containerfile, tag, image}
//	  sandbox/             # the bundle's own Containerfile (build-on-box default)
//	  system_prompts/      # default.md (scheduled base), chat.md (interactive base)
//	  personas/            # *.yaml
//	  protocols/           # *.yaml|md
//	  skills/              # <name>/SKILL.md Agent Skills (progressive disclosure)
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
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/itchyny/gojq"

	"github.com/ElcanoTek/fleet/internal/config"
)

// HTTPToolServerName is the synthetic MCP-server name inline http_tools are
// registered under on the credentialed *mcp.Client. The leading underscore keeps
// it out of the namespace a bundle's own mcp_servers[] entries occupy; validate()
// rejects an MCP server that tries to claim it. The agent sees these tools as
// mcp__http_<name> — routed, gated, redacted, and brokered host-side exactly like
// any MCP tool.
const HTTPToolServerName = "_http"

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

// Bundle is the loaded, validated client configuration.
type Bundle struct {
	// Dir is the absolute path to the bundle root.
	Dir string

	Branding   Branding
	Models     Models
	EmptyState EmptyState

	// TaskTemplates is the bundle's catalog of pre-filled scheduled-task
	// configurations (manifest task_templates block), in manifest order. Empty
	// in the generic bundle's absence of the section; the shipped generic bundle
	// declares a handful of neutral starters. Surfaced read-only by the
	// orchestrator's GET /task-templates so the task-create UI can offer
	// "new task from a template" — the task itself is still created through the
	// existing POST /tasks path. See TaskTemplate.
	TaskTemplates []TaskTemplate

	// MCPCatalog is the declarative server catalog from the manifest, in
	// manifest order.
	MCPCatalog []ServerDef

	// AgentPolicyConfig carries the bundle's client-specific agent tool-behavior
	// lists (parallel-safe tools, critical-tool suffixes, substitute map). Empty
	// in the generic bundle. cmd/fleet translates it into agentcore.AgentPolicy.
	AgentPolicyConfig AgentPolicy

	// Personas carries the manifest's optional per-persona tool-permission
	// policies (#294), in manifest order. Empty in the generic bundle (defaults
	// unchanged). Look one up via PersonaToolPolicy. cmd/fleet translates each
	// into agentcore.PersonaToolPermissions and the drivers apply it as a
	// least-privilege NARROWING gate when building a run's tool roster.
	Personas []PersonaDef

	// HTTPTools is the manifest's inline REST-API tool catalog (the http_tools:
	// section), in manifest order. Each entry is registered as a native tool
	// alongside the MCP catalog — no MCP subprocess required. Empty in the generic
	// bundle (defaults unchanged). See HTTPToolDef.
	HTTPTools []HTTPToolDef

	// PricingConfig carries the bundle's optional custom model-pricing overrides
	// (#297). Empty in the generic bundle. cmd/fleet translates it into
	// agentcore.PricingConfig and installs it via agentcore.ConfigurePricing.
	PricingConfig PricingConfig

	// SandboxConfig is the bundle's resolved sandbox descriptor (Containerfile,
	// local tag, optional prebuilt image override). Access it via Sandbox().
	SandboxConfig Sandbox

	// sandboxDeclared reports whether the manifest carried an explicit sandbox:
	// block. Only a declared block enforces the Containerfile-exists invariant
	// in validate (a minimal/legacy bundle gets the conventional defaults without
	// being forced to ship a Containerfile).
	sandboxDeclared bool

	// Resolved absolute directories inside the bundle. These are the
	// same-path bind-mount sources and the source dirs the prompt/persona/
	// protocol loaders read.
	SystemPromptsDir string
	PersonasDir      string
	ProtocolsDir     string
	SkillsDir        string
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

// PersonaToolPermissions is the per-persona tool policy declared in the
// manifest's personas: block (#294). It is a least-privilege NARROWING gate
// layered on top of the existing server allowlist (Gate-2) and credential
// allowlist (Gate-3): it can only SUBTRACT from what a persona is already
// permitted to call, never add. It is a plain data struct (no dependency on
// internal/agentcore) so clientconfig stays a low-level package; cmd/fleet
// translates it into agentcore.PersonaToolPermissions.
//
//   - An absent block (both lists empty) means all tools are available
//     (backward compatible — existing bundles are unaffected).
//   - When Allow is non-empty, only listed tools are offered (default-deny).
//   - When only Deny is set, all tools except those are offered (default-allow
//     with exceptions).
//   - Deny takes precedence when a tool matches both lists.
//
// Pattern syntax (matched against the fantasy tool name, e.g. "bash" or
// "mcp_<server>_<tool>"):
//
//	bash                      exact native-tool name
//	mcp:server/tool           specific MCP tool (→ "mcp_<server>_<tool>")
//	mcp:server/*              all tools from one MCP server
//	prefix/*                  any tool whose fantasy name has the prefix
//	*                         all tools
type PersonaToolPermissions struct {
	Allow []string `yaml:"allow"`
	Deny  []string `yaml:"deny"`
}

// PersonaDef is one entry in the manifest's personas: block. Name matches the
// basename of a persona YAML file in personas/ (e.g. "code-reviewer" for
// personas/code-reviewer.yaml). A persona with no entry — or an entry with an
// empty tool_permissions block — keeps current behavior (sees all permitted
// tools).
type PersonaDef struct {
	Name            string                 `yaml:"name"`
	ToolPermissions PersonaToolPermissions `yaml:"tool_permissions"`
}

// PricingOverride is one entry in the manifest's pricing.overrides list: an
// operator-declared per-model rate. Rates are per MILLION tokens (the unit
// pricing pages publish). It is a plain data struct (no dependency on
// internal/agentcore) so clientconfig stays a low-level package; cmd/fleet
// translates it into agentcore.PricingOverride.
type PricingOverride struct {
	Model                          string  `yaml:"model"`
	InputCostPerMillionTokens      float64 `yaml:"input_cost_per_million_tokens"`
	OutputCostPerMillionTokens     float64 `yaml:"output_cost_per_million_tokens"`
	CacheReadCostPerMillionTokens  float64 `yaml:"cache_read_cost_per_million_tokens"`
	CacheWriteCostPerMillionTokens float64 `yaml:"cache_write_cost_per_million_tokens"`
}

// PricingConfig is the bundle's optional custom model-pricing block (#297). An
// operator on negotiated / enterprise rates declares per-model overrides here so
// cost accounting (and the cost ceiling) reflects their real spend instead of the
// OpenRouter-published price.
//
//   - Overrides: per-model rate table. A step whose model slug matches an entry
//     is priced locally from its token counts using these rates.
//   - Fallback: what to do for a model NOT listed in Overrides. "openrouter"
//     (default, and the value an absent/blank block resolves to) keeps the
//     existing behavior — trust the OpenRouter-returned cost. "zero" suppresses
//     cost for unlisted models (fully-private deployments).
//
// An absent pricing: block leaves the zero value, which cmd/fleet maps to the
// default (no overrides, OpenRouter fallback) — behavior identical to pre-#297.
type PricingConfig struct {
	Overrides []PricingOverride `yaml:"overrides"`
	Fallback  string            `yaml:"fallback"`
}

// Sandbox is the bundle's resolved execution-sandbox descriptor. The sandbox is
// a per-client CONFIG-BUNDLE artifact: each bundle ships its own
// sandbox/Containerfile flavor (and pins its own base digest). The default is
// BUILD-ON-BOX — scripts/build-sandbox-image.sh builds ContainerfileAbsPath into
// Tag, and the process consumes Tag. REGISTRY PUBLISH is opt-in: a client sets a
// non-empty Image (e.g. a prebuilt registry ref) in its manifest, which then
// WINS over Tag.
//
// The process does NOT build at startup. Bootstrap / build-sandbox-image.sh
// builds the image; the process only consumes the resolved ref
// (ResolvedImageRef).
type Sandbox struct {
	// ContainerfileAbsPath is the absolute path to the bundle's Containerfile
	// (manifest sandbox.containerfile resolved against the bundle dir; defaults
	// to <bundle>/sandbox/Containerfile when unset). Empty only when the
	// manifest explicitly blanks it AND supplies an Image override.
	ContainerfileAbsPath string

	// Tag is the local image tag the on-box build produces and the process
	// consumes when Image is empty (default localhost/fleet-sandbox:latest).
	Tag string

	// Image is the optional prebuilt image ref. When non-empty it is the
	// resolved ref (the opt-in registry-pull path); when empty the build-on-box
	// Tag is used.
	Image string
}

// ResolvedImageRef returns the image reference the fleet process should consume:
// Image when set (opt-in prebuilt/registry pull), else Tag (build-on-box).
func (s Sandbox) ResolvedImageRef() string {
	if strings.TrimSpace(s.Image) != "" {
		return strings.TrimSpace(s.Image)
	}
	return strings.TrimSpace(s.Tag)
}

// sandboxManifest is the on-disk YAML shape of the manifest's sandbox: block.
type sandboxManifest struct {
	Containerfile string `yaml:"containerfile"`
	Tag           string `yaml:"tag"`
	Image         string `yaml:"image"`
}

// Branding carries the white-label strings surfaced in the web UI + login.
type Branding struct {
	AppName          string `yaml:"app_name"`
	LoginTitle       string `yaml:"login_title"`
	LoginTagline     string `yaml:"login_tagline"`
	ShareTitle       string `yaml:"share_title"`
	ShareDescription string `yaml:"share_description"`
	// Colors lets a bundle theme the actual web UI (not just text) by overriding
	// the CSS custom properties globals.css defines. Served as a render-blocking
	// stylesheet by httpapi's /theme.css so the shell — including the pre-auth
	// login page — paints in the client's palette with no flash. An absent block
	// emits nothing and the built-in defaults stand. See BrandColors.
	Colors BrandColors `yaml:"colors"`
}

// BrandColors holds per-mode palette overrides. Light and Dark are keyed by a
// stable token name (e.g. "primary", "accent", "background") that httpapi maps
// to the corresponding --color-* custom property; unknown keys are ignored and
// values are validated at render time, so a sparse or typo'd block degrades to
// the defaults rather than breaking the UI. Maps (not a struct) keep the strict
// manifest decoder from rejecting a bundle that lists a token fleet doesn't yet
// theme.
type BrandColors struct {
	Light map[string]string `yaml:"light"`
	Dark  map[string]string `yaml:"dark"`
}

// Models carries advisory default model-tier hints a bundle may declare so its
// manifest is self-describing. It is NOT consumed by the running config and NOT
// exposed to the web — the operative model defaults are the agentcore.Default*
// constants, resolved from env + the per-turn slug. The field (and its `models:`
// manifest block) is retained only so the strict decoder still loads bundles
// that declare one; do not treat it as a live model-selection knob.
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

// TaskTemplate is one entry in the manifest's task_templates block: a named,
// described, pre-filled scheduled-task configuration the task-create UI offers
// as a starting point ("new task from a template"). It is purely read-through
// bundle config — fleet never persists a template and never creates a task FROM
// the backend; the UI seeds its form with Task's fields, the user edits them
// freely, and the resulting task is created through the ordinary POST /tasks
// path. A template therefore cannot grant any capability the create path does
// not already validate.
type TaskTemplate struct {
	Name        string           `yaml:"name"`
	Description string           `yaml:"description"`
	Icon        string           `yaml:"icon"`
	Task        TaskTemplateTask `yaml:"task"`
}

// TaskTemplateTask is the partial task payload a template carries — the subset
// of models.TaskCreate fields it makes sense to pre-fill in the create form.
//
// It deliberately mirrors only the EXISTING, form-editable TaskCreate fields, so
// every value a template sets maps to a real create-path field (honesty: no
// template knob promises a capability the task model lacks). Notable omissions
// and why:
//
//   - max_cost_usd / runtime_flavor: not fields on models.TaskCreate (the
//     issue's stale "current state" listing notwithstanding), so a template that
//     set them could not apply them — left out rather than feign support.
//   - scheduled_for / files: inherently per-invocation; a template seeds a
//     reusable shape, not a one-off run.
//   - credential_allowlist / mcp_selection / loop_config / worktree_config /
//     trigger_type / allow_task_creation / allow_recurring_task_creation:
//     security- or routing-sensitive knobs deliberately kept OUT of templates so
//     a shipped template can never silently widen a task's authority. The user
//     sets these explicitly in the form when they want them.
//
// Scalars that have a meaningful zero (priority, the bool flags) are plain
// values; optional fields whose ABSENCE should leave the form at its own default
// (model, fallback_model, max_iterations, max_retries) are pointers so an omitted
// YAML key is distinguishable from an explicit zero. The struct is serialized to
// the UI as opaque JSON; the Go side never interprets the values beyond parsing.
type TaskTemplateTask struct {
	Prompt                 string   `yaml:"prompt" json:"prompt,omitempty"`
	Model                  *string  `yaml:"model,omitempty" json:"model,omitempty"`
	FallbackModel          *string  `yaml:"fallback_model,omitempty" json:"fallback_model,omitempty"`
	MaxIterations          *int     `yaml:"max_iterations,omitempty" json:"max_iterations,omitempty"`
	MaxRetries             *int     `yaml:"max_retries,omitempty" json:"max_retries,omitempty"`
	Recurrence             string   `yaml:"recurrence,omitempty" json:"recurrence,omitempty"`
	Timezone               string   `yaml:"timezone,omitempty" json:"timezone,omitempty"`
	Priority               int      `yaml:"priority,omitempty" json:"priority,omitempty"`
	AllowNetwork           bool     `yaml:"allow_network,omitempty" json:"allow_network,omitempty"`
	InstructionSelfImprove bool     `yaml:"instruction_self_improve,omitempty" json:"instruction_self_improve,omitempty"`
	Persona                string   `yaml:"persona,omitempty" json:"persona,omitempty"`
	Description            string   `yaml:"description,omitempty" json:"description,omitempty"`
	Tags                   []string `yaml:"tags,omitempty" json:"tags,omitempty"`
	// SLA expectation (#274); omit for no SLA. The multipliers default to 1.5 /
	// 2.0 server-side when a template sets ExpectedDurationMinutes without them.
	ExpectedDurationMinutes *int     `yaml:"expected_duration_minutes,omitempty" json:"expected_duration_minutes,omitempty"`
	SLAWarnMultiplier       *float64 `yaml:"sla_warn_multiplier,omitempty" json:"sla_warn_multiplier,omitempty"`
	SLAFailMultiplier       *float64 `yaml:"sla_fail_multiplier,omitempty" json:"sla_fail_multiplier,omitempty"`
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

// HTTPToolDef is one inline HTTP tool in the manifest's http_tools: section.
// Each entry is registered as a native tool alongside the MCP catalog — no MCP
// server subprocess is required. Like an MCP server, an http_tool is
// BUNDLE-AUTHOR-DEFINED and therefore trusted: the manifest author decides which
// endpoint is called and which secrets back it.
//
// SECURITY — the credential boundary mirrors the MCP catalog exactly:
//
//   - Headers values may carry ${ENV_VAR} references. They are resolved from the
//     HOST process environment at CALL time (resolveEnvMap), inside whichever
//     process holds the credentialed client (the out-of-process mcp-broker under
//     issue #167, else the host-side manager). The resolved secret is applied to
//     the outbound request header and NEVER enters the sandbox, the model context,
//     or the logs — the model only ever supplies the declared input params and
//     sees the (redacted) response body.
//   - The HTTP request itself runs HOST-SIDE through the same MCP client/broker
//     seam every MCP tool call funnels through, so it is governed by the same
//     policy gate, output redaction, and isError handling — not a second path.
//
// URL and BodyTemplate may carry {param} tokens substituted from the model's
// declared input at call time (URL context is percent-encoded; body context is
// raw). InputSchema is the JSON Schema the model sees. ResponseJQ, when set, is a
// jq program applied to a JSON response body before it is returned to the model.
// Critical opts the tool into the existing critical-tool audit gate (its bare
// name is registered as a critical suffix — same semantics as
// AgentPolicy.CriticalToolSuffixes), for tools that write data or trigger side
// effects.
type HTTPToolDef struct {
	Name         string                 `yaml:"name"`
	Description  string                 `yaml:"description"`
	Method       string                 `yaml:"method"`        // GET | POST | PUT | PATCH | DELETE
	URL          string                 `yaml:"url"`           // may contain {param} tokens
	Headers      map[string]string      `yaml:"headers"`       // values support ${ENV_VAR}, resolved host-side at call time
	BodyTemplate string                 `yaml:"body_template"` // may contain {param} tokens
	InputSchema  map[string]interface{} `yaml:"input_schema"`
	ResponseJQ   string                 `yaml:"response_jq"` // optional jq program over a JSON response
	Critical     bool                   `yaml:"critical"`
}

// manifest is the on-disk YAML shape. Sandbox is a pointer so an absent block
// (a minimal/legacy bundle that never opted into the sandbox-as-config contract)
// is distinguishable from a present-but-empty one: only a DECLARED sandbox block
// enforces the Containerfile-exists invariant.
type manifest struct {
	Branding      Branding         `yaml:"branding"`
	Models        Models           `yaml:"models"`
	MCPServers    []ServerDef      `yaml:"mcp_servers"`
	HTTPTools     []HTTPToolDef    `yaml:"http_tools"`
	EmptyState    EmptyState       `yaml:"empty_state"`
	TaskTemplates []TaskTemplate   `yaml:"task_templates"`
	AgentPolicy   AgentPolicy      `yaml:"agent_policy"`
	Personas      []PersonaDef     `yaml:"personas"`
	Pricing       PricingConfig    `yaml:"pricing"`
	Sandbox       *sandboxManifest `yaml:"sandbox"`
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
	// Interpolate env references over the RAW bytes before YAML unmarshal so that
	// "env-or-default" config semantics — ${VAR:-default} / ${VAR:?message} —
	// resolve at load time. This restores the getEnvOrDefault("VAR","literal")
	// behavior the old internal/config carried, which had degenerated into bare
	// hardcoded literals once the catalog moved into the manifest.
	interpolated, err := interpolateManifest(raw, manifestPath)
	if err != nil {
		return nil, err
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

	b := &Bundle{
		Dir:               abs,
		Branding:          m.Branding,
		Models:            m.Models,
		EmptyState:        m.EmptyState,
		TaskTemplates:     m.TaskTemplates,
		MCPCatalog:        m.MCPServers,
		HTTPTools:         m.HTTPTools,
		AgentPolicyConfig: m.AgentPolicy,
		Personas:          m.Personas,
		PricingConfig:     m.Pricing,
		SandboxConfig:     resolveSandbox(m.Sandbox, abs),
		sandboxDeclared:   m.Sandbox != nil,
		SystemPromptsDir:  filepath.Join(abs, "system_prompts"),
		PersonasDir:       filepath.Join(abs, "personas"),
		ProtocolsDir:      filepath.Join(abs, "protocols"),
		SkillsDir:         filepath.Join(abs, "skills"),
		MCPDir:            filepath.Join(abs, "mcp"),
	}
	applyBrandingDefaults(&b.Branding)
	if err := b.validate(); err != nil {
		return nil, err
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

// DefaultBranding returns the neutral generic branding strings fleet renders
// when no client bundle is wired. It is the SAME source of truth
// applyBrandingDefaults uses for a sparse manifest, so the no-bundle and
// sparse-bundle UIs match (one source of truth rather than a hardcoded literal
// in the HTTP layer).
func DefaultBranding() Branding {
	var b Branding
	applyBrandingDefaults(&b)
	return b
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
	return Sandbox{
		ContainerfileAbsPath: filepath.Join(bundleDir, cf),
		Tag:                  tag,
		Image:                strings.TrimSpace(raw.Image),
	}
}

// Sandbox returns the bundle's resolved execution-sandbox descriptor.
func (b *Bundle) Sandbox() Sandbox {
	return b.SandboxConfig
}

// validate checks the MCP catalog for the structural invariants the spawn path
// relies on, plus the sandbox descriptor. Content dirs are NOT required to exist
// (a manifest-only bundle is valid); callers that read a specific file surface
// their own not-found errors.
func (b *Bundle) validate() error {
	// Sandbox: only a DECLARED sandbox block enforces the invariant (a minimal
	// bundle that never opted into the contract gets the conventional defaults
	// without being forced to ship a Containerfile). When no prebuilt Image
	// override is set the on-box build is the only way to materialize the image,
	// so the Containerfile MUST exist. When an Image override is present the
	// Containerfile is irrelevant (the process pulls/uses the prebuilt ref).
	if b.sandboxDeclared && b.SandboxConfig.Image == "" {
		cf := b.SandboxConfig.ContainerfileAbsPath
		if cf == "" {
			return fmt.Errorf("sandbox: containerfile is required when sandbox.image is empty")
		}
		if info, err := os.Stat(cf); err != nil {
			return fmt.Errorf("sandbox: containerfile %s: %w (set sandbox.image to use a prebuilt image instead)", cf, err)
		} else if info.IsDir() {
			return fmt.Errorf("sandbox: containerfile %s is a directory", cf)
		}
	}

	// Tool names share ONE namespace across mcp_servers[] (server names) and
	// http_tools[] (tool names): the agent addresses them as mcp_<server>_<tool>,
	// so a collision would make dispatch ambiguous. `seen` therefore tracks both.
	seen := map[string]bool{}
	for i := range b.MCPCatalog {
		s := &b.MCPCatalog[i]
		if strings.TrimSpace(s.Name) == "" {
			return fmt.Errorf("mcp_servers[%d]: name is required", i)
		}
		if s.Name == HTTPToolServerName {
			return fmt.Errorf("mcp_servers[%q]: name %q is reserved for inline http_tools", s.Name, HTTPToolServerName)
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
	if err := b.validateHTTPTools(seen); err != nil {
		return err
	}
	if err := b.validatePersonas(); err != nil {
		return err
	}
	if err := validatePricing(b.PricingConfig); err != nil {
		return err
	}
	return nil
}

// validatePersonas fails the load on a malformed personas[] entry — a blank
// name or a duplicate name. A typo'd or duplicated persona entry would
// otherwise silently fail to bind its tool policy (leaving the persona on the
// permissive default), which for a least-privilege gate is the dangerous
// direction: fail loud at startup instead. Empty tool_permissions blocks are
// allowed (they are the explicit "no narrowing" case).
func (b *Bundle) validatePersonas() error {
	seen := map[string]bool{}
	for i := range b.Personas {
		name := strings.TrimSpace(b.Personas[i].Name)
		if name == "" {
			return fmt.Errorf("personas[%d]: name is required", i)
		}
		if seen[name] {
			return fmt.Errorf("personas: duplicate persona name %q", name)
		}
		seen[name] = true
	}
	return nil
}

// validateHTTPTools fails the load on a malformed http_tools[] entry — a missing
// name/method/url, an unsupported method, a name that collides with an MCP server
// or another http_tool, an input_schema that is not a JSON-Schema object, or a
// response_jq that does not parse. The jq syntax check runs HERE (at Load) so a
// typo'd jq program fails startup loudly rather than at the first model call. The
// already-populated `seen` set enforces the single shared name namespace.
func (b *Bundle) validateHTTPTools(seen map[string]bool) error {
	for i := range b.HTTPTools {
		t := &b.HTTPTools[i]
		name := strings.TrimSpace(t.Name)
		if name == "" {
			return fmt.Errorf("http_tools[%d]: name is required", i)
		}
		if name == HTTPToolServerName {
			return fmt.Errorf("http_tools[%q]: name is reserved", name)
		}
		if seen[name] {
			return fmt.Errorf("http_tools: duplicate tool name %q (collides with an mcp_servers entry or another http_tool)", name)
		}
		seen[name] = true

		method := strings.ToUpper(strings.TrimSpace(t.Method))
		if method == "" {
			return fmt.Errorf("http_tools[%q]: method is required (GET|POST|PUT|PATCH|DELETE)", name)
		}
		if !httpToolMethods[method] {
			return fmt.Errorf("http_tools[%q]: unsupported method %q (want GET|POST|PUT|PATCH|DELETE)", name, t.Method)
		}
		t.Method = method // normalize so the executor sees a canonical verb
		if strings.TrimSpace(t.URL) == "" {
			return fmt.Errorf("http_tools[%q]: url is required", name)
		}
		// input_schema, when present, must be a JSON-Schema object so the model is
		// handed a well-formed tool parameter schema. An absent schema is allowed
		// (a no-parameter tool); the executor advertises an empty object schema.
		if t.InputSchema != nil {
			if typ, ok := t.InputSchema["type"].(string); ok && typ != "object" {
				return fmt.Errorf("http_tools[%q]: input_schema.type must be %q, got %q", name, "object", typ)
			}
		}
		if jq := strings.TrimSpace(t.ResponseJQ); jq != "" {
			if _, err := gojq.Parse(jq); err != nil {
				return fmt.Errorf("http_tools[%q]: response_jq does not parse: %w", name, err)
			}
		}
	}
	return nil
}

// validatePricing fails the load on a malformed pricing block (#297): an unknown
// fallback mode, an override missing its model slug, or a negative rate. This is
// the same fail-loud-at-startup posture as the rest of validate — a typo'd
// fallback or a sign-flipped rate would otherwise silently mis-account cost (and
// the cost ceiling that rides on it). An absent block (zero value) is valid: an
// empty fallback resolves to the OpenRouter default downstream.
func validatePricing(p PricingConfig) error {
	switch strings.ToLower(strings.TrimSpace(p.Fallback)) {
	case "", "openrouter", "zero":
	default:
		return fmt.Errorf("pricing.fallback: unknown value %q (want openrouter|zero)", p.Fallback)
	}
	for i, o := range p.Overrides {
		if strings.TrimSpace(o.Model) == "" {
			return fmt.Errorf("pricing.overrides[%d]: model is required", i)
		}
		for _, r := range []struct {
			name string
			val  float64
		}{
			{"input_cost_per_million_tokens", o.InputCostPerMillionTokens},
			{"output_cost_per_million_tokens", o.OutputCostPerMillionTokens},
			{"cache_read_cost_per_million_tokens", o.CacheReadCostPerMillionTokens},
			{"cache_write_cost_per_million_tokens", o.CacheWriteCostPerMillionTokens},
		} {
			if r.val < 0 {
				return fmt.Errorf("pricing.overrides[%q]: %s must not be negative (got %g)", o.Model, r.name, r.val)
			}
		}
	}
	return nil
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
		}
		switch s.Type {
		case "http":
			sc.URL = s.URL
			sc.Headers = resolveEnvMap(s.Headers, nil)
		default: // stdio
			sc.Command = s.Command
			sc.Args = append([]string(nil), s.Args...)
			sc.Env = resolveEnvMap(s.Env, s.OptionalEnv)
			sc.Dir = bundleDir
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
	if len(b.AgentPolicyConfig.CriticalToolSubstitutes) > 0 {
		p.CriticalToolSubstitutes = make(map[string][]string, len(b.AgentPolicyConfig.CriticalToolSubstitutes))
		for k, v := range b.AgentPolicyConfig.CriticalToolSubstitutes {
			p.CriticalToolSubstitutes[k] = append([]string(nil), v...)
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
	// Inline http_tools' header secrets must survive the .env-file allowlist too:
	// they are resolved host-side at call time exactly like an MCP server's headers.
	for i := range b.HTTPTools {
		for _, v := range b.HTTPTools[i].Headers {
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

// interpolateManifest performs a pre-unmarshal pass over the raw manifest bytes,
// expanding shell-style env references so the bundle can carry "env-or-default"
// config semantics (the getEnvOrDefault("VAR","literal") behavior the legacy
// internal/config had). It supports three POSIX-style forms:
//
//	${VAR}            Bare reference. If VAR is SET, substitute its value. If VAR
//	                  is UNSET, the token is LEFT INTACT (deferred): per-MCP-server
//	                  env/header values are resolved lazily at spawn time against
//	                  the live process env (after the .env file is loaded), where
//	                  an unset credential is legitimate (the server gates off or
//	                  optional_env drops the key). The pre-unmarshal pass therefore
//	                  must NOT hard-fail on an unset bare ${VAR} — that would make
//	                  loading any bundle impossible unless every connector secret
//	                  were exported up front. A value that MUST be present at load
//	                  uses the explicit ${VAR:?message} form instead.
//	${VAR:-default}   POSIX use-default. If VAR is set AND non-empty, use it; else
//	                  use default (empty env counts as unset). This is the restored
//	                  env-or-default form: env can override, the literal is kept.
//	${VAR:?message}   POSIX required. If VAR is unset OR empty, fail the load with
//	                  message (naming the var + the manifest path).
//
// Escaping: a literal "$${" emits "${" without triggering expansion, so a value
// that genuinely needs a literal ${...} can be written.
//
// Nested braces: the default/message body of a :- / :? form is scanned with
// brace-depth tracking, so a default that itself contains "${...}" (or any
// balanced braces) survives intact; expansion does NOT recurse into it.
//
// YAML-quoting requirement: a :- / :? default contains a ':' (and a URL default
// contains '://'), so the field MUST be quoted in YAML, e.g.
//
//	pubmatic_base_url: "${PUBMATIC_BASE_URL:-https://api.pubmatic.com}"
//
// An unquoted value would make the YAML parser read the ':' as a mapping
// separator. The interpolation runs on raw bytes before unmarshal, so the quotes
// remain around the substituted value and the YAML round-trips correctly.
func interpolateManifest(raw []byte, manifestPath string) ([]byte, error) {
	s := string(raw)
	var sb strings.Builder
	sb.Grow(len(s))
	for i := 0; i < len(s); {
		// Escape: "$${" -> literal "${" (consume one leading '$').
		if strings.HasPrefix(s[i:], "$${") {
			sb.WriteString("${")
			i += 3
			continue
		}
		if !strings.HasPrefix(s[i:], "${") {
			sb.WriteByte(s[i])
			i++
			continue
		}
		// Found "${": scan to the matching '}' tracking brace depth so nested
		// braces in a default body don't terminate the expression early.
		end, ok := matchBrace(s, i+1) // index of the '}' closing the '{' at i+1
		if !ok {
			return nil, fmt.Errorf("client config manifest %s: unterminated ${...} expression at offset %d", manifestPath, i)
		}
		expr := s[i+2 : end] // contents between "${" and "}"
		val, err := expandExpr(expr, manifestPath)
		if err != nil {
			return nil, err
		}
		if val.deferred {
			// Unset bare ${VAR}: leave the literal token in place for spawn-time
			// resolution.
			sb.WriteString(s[i : end+1])
		} else {
			sb.WriteString(val.text)
		}
		i = end + 1
	}
	return []byte(sb.String()), nil
}

// expandResult is the outcome of expanding one ${...} expression.
type expandResult struct {
	text     string // resolved replacement text (when deferred is false)
	deferred bool   // true => leave the literal ${VAR} token in place (unset bare ref)
}

// expandExpr resolves the body of a single ${...} expression (the text between
// the braces) into a replacement, implementing the ${VAR}, ${VAR:-default} and
// ${VAR:?message} forms.
func expandExpr(expr, manifestPath string) (expandResult, error) {
	// Find the first ":-" or ":?" operator at the TOP of the expression. The var
	// name itself never contains ':', so the first ':' (if any) starts the op.
	if idx := strings.IndexByte(expr, ':'); idx >= 0 && idx+1 < len(expr) {
		name := expr[:idx]
		op := expr[idx+1]
		body := expr[idx+2:]
		switch op {
		case '-': // ${VAR:-default}
			if v, ok := lookupNonEmpty(name); ok {
				return expandResult{text: v}, nil
			}
			return expandResult{text: body}, nil
		case '?': // ${VAR:?message}
			if v, ok := lookupNonEmpty(name); ok {
				return expandResult{text: v}, nil
			}
			msg := strings.TrimSpace(body)
			if msg == "" {
				msg = "required value is unset or empty"
			}
			return expandResult{}, fmt.Errorf("client config manifest %s: ${%s:?...}: %s", manifestPath, strings.TrimSpace(name), msg)
		}
		// Any other ':X' is not a form we support; fall through and treat the
		// whole expression as a bare name (which will almost certainly be unset,
		// hence deferred) rather than silently mangling it.
	}
	name := strings.TrimSpace(expr)
	if v, ok := lookupNonEmpty(name); ok {
		return expandResult{text: v}, nil
	}
	// Unset bare ${VAR}: defer to spawn-time resolution.
	return expandResult{deferred: true}, nil
}

// lookupNonEmpty reports the trimmed process-env value for name and whether it is
// set AND non-empty (empty env counts as unset, matching POSIX ${VAR:-default}
// and the legacy getEnvOrDefault, which treated an empty value as "use default").
func lookupNonEmpty(name string) (string, bool) {
	v := strings.TrimSpace(os.Getenv(strings.TrimSpace(name)))
	if v == "" {
		return "", false
	}
	return v, true
}

// matchBrace returns the index of the '}' that closes the '{' at position open
// (s[open] must be '{'), tracking nested '{' '}' so a brace inside a default body
// is balanced rather than terminating the expression.
func matchBrace(s string, open int) (int, bool) {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
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
