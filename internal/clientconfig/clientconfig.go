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
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/goccy/go-yaml"
	"github.com/itchyny/gojq"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/mcp"
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

// Bundle is the loaded, validated client configuration.
type Bundle struct {
	// Dir is the absolute path to the bundle root.
	Dir string

	// connectorEnvVarNames and connectorAccountVarNames are captured from the
	// raw manifest before interpolation. Keeping the source names is required by
	// the out-of-process MCP broker: an already-exported secret is substituted
	// out of the parsed manifest, but the parent still needs its NAME so it can
	// remove that key after the broker child inherits the startup environment.
	connectorEnvVarNames     []string
	connectorAccountVarNames []string
	// manifestEnvVarNames inventories every ${VAR} source name the RAW manifest
	// references anywhere — url:, command:/args:, sandbox.image, providers, and
	// the connector env/header maps alike — captured before interpolation can
	// substitute an exported value out of the runtime manifest. EnvVarNames
	// merges them so the .env allowlist admits references from ALL interpolated
	// fields, not only env/header values (#1123). Names only, never values.
	manifestEnvVarNames []string
	// manifestDefaultOnlyEnvVarNames is the subset of manifestEnvVarNames
	// whose every reference carries a ${VAR:-...} default — see
	// EnvVarNamesDefaultOnly. Names only, never values.
	manifestDefaultOnlyEnvVarNames []string
	// scrubbedAlwaysOnServers preserves only the public availability rows after
	// ScrubConnectorRuntimeDefinitions removes the credential-bearing catalog.
	scrubbedAlwaysOnServers []AlwaysOnServer

	Branding   Branding
	Models     Models
	EmptyState EmptyState

	// BrandLogoPath is the absolute, symlink-resolved file backing
	// Branding.Logo, or "" when the bundle declares none. Set at load by
	// resolveBrandLogo so the HTTP layer serves a path it never has to
	// re-validate per request.
	BrandLogoPath string

	// BrandShareImagePath is the absolute, symlink-resolved file backing
	// branding.share_image, or "" when the bundle declares none. Set by
	// resolveBrandShareImage so the HTTP layer serves a path it never has to
	// re-validate per request.
	BrandShareImagePath string

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

	// HooksConfig carries the bundle's optional governed lifecycle hooks (#788).
	// nil/empty in the generic bundle. cmd/fleet translates it into
	// []agentcore.LifecycleHook at startup. Read via Bundle.Hooks().
	HooksConfig *HooksConfig

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
	// A2APeers is the manifest's a2a_peers: section (#1368) — remote A2A agents
	// this bundle's agents may delegate to. Runtime shape via A2APeerConfigs().
	A2APeers []A2APeerDef

	// WebhookTriggers is the manifest's inbound conversation-trigger catalog (the
	// webhook_triggers: section, #268), in manifest order. Each entry maps a
	// signed inbound webhook (POST /webhooks/{slug}) to a fresh conversation.
	// Empty in the generic bundle. Look one up via WebhookTrigger. See
	// WebhookTriggerDef.
	WebhookTriggers []WebhookTriggerDef

	// RemoteMCPCatalog is the manifest's curated directory of THIRD-PARTY hosted
	// MCP servers (the remote_mcp_catalog: section, #538), in manifest order.
	// Unlike MCPCatalog entries (bundle-author-defined, run host-side in the
	// credential-owning broker), these are pointers to services hosted and
	// operated by an external vendor: connecting one sends conversation-derived
	// tool traffic to that vendor under its own terms. The catalog is
	// informational — nothing connects until a user explicitly adds the server
	// through the per-user remote-MCP OAuth flow (#443). Empty in a bundle that
	// curates nothing.
	RemoteMCPCatalog []RemoteMCPCatalogEntry

	// Providers is the manifest's LLM-provider routing table (the providers:
	// section, #289), in precedence order. Empty in the generic bundle, which
	// keeps the historical single-OpenRouter behavior. cmd/fleet translates each
	// into an agentcore.ProviderConfig (resolving the API-key env host-side) and
	// hands them to the model resolver. See ProviderDef.
	Providers []ProviderDef
	// FallbackProviders is an ordered cross-provider retry chain (#703).
	FallbackProviders []string

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
	// PromptsDir is the bundle-owned, Git-trackable reusable prompt library.
	// Entries are read on demand so a config-repo pull is visible without a
	// fleet restart. See ReadPrompts and Bundle.Prompts.
	PromptsDir string
	SkillsDir  string
	// BundleSkillsDir is the bundle's OWN skills/ dir (the author-owned
	// source). SkillsDir may point at the merged bundle+builtin dir instead —
	// see builtin_skills.go. Validation always runs against this one.
	BundleSkillsDir string
	// skillsBuiltin/skillsHidden carry the manifest knobs so Skills() can
	// resync the merged dir on read (live-reload contract).
	skillsBuiltin bool
	skillsHidden  []string
	// pluginSkillOverlays are the Agent Plugin skill contributions merged
	// into SkillsDir (between the built-in pack and the bundle's own skills).
	pluginSkillOverlays []skillOverlay
	MCPDir              string
	// EvalsDir holds the bundle's eval & regression sets (#502): one YAML file
	// per set of golden cases, loaded by internal/evals.LoadSets. Optional like
	// every content dir — a bundle without evals/ simply has no eval sets. It is
	// a HOST-side dir (read by `fleet eval`), not bind-mounted into the sandbox.
	EvalsDir string

	// Plugins are the Agent Plugins (https://agent-plugins.org) loaded from the
	// bundle's plugins/ dir and the manifest's plugin_roots — see plugins.go
	// and ADR-0054. Their skills are already merged into SkillsDir and their
	// MCP servers appended to MCPCatalog; this is the inventory for the
	// operator surfaces (validate-config, /skills provenance).
	Plugins []Plugin
	// PluginRoots are the manifest's extra plugin roots, absolutized.
	PluginRoots    []string
	pluginProblems []string
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
	// CriticalToolTimeouts is an OPTIONAL per-tool approval default-deny window
	// (#225): a map from bare tool-name suffix (the same suffix form as
	// critical_tools) to seconds. It is additive and backward-compatible —
	// critical_tools stays a plain string list, so existing manifests are
	// unaffected. cmd/fleet threads it into agentcore.AgentPolicy.
	//
	//	agent_policy:
	//	  critical_tool_timeouts:
	//	    send_email: 600   # user reads the draft carefully
	//	    bash: 60          # risky shell commands decide fast
	CriticalToolTimeouts map[string]int `yaml:"critical_tool_timeouts"`
	// CriticalToolModes is an OPTIONAL per-tool approval MODE (#1153): a map
	// from bare tool-name suffix to "approve" (block on a card, the default and
	// the behavior of every tool before this existed) or "notify" (execute
	// immediately and post a card recording what happened).
	//
	// It exists because one global critical_tools list forces one policy onto
	// operations with wildly different undo stories. A card lands whenever the
	// agent reaches it — often many minutes into a run the user started and then
	// reasonably stopped watching — and then default-denies, so a long analysis
	// ending in a reversible page publish was routinely lost to nobody being at
	// the keyboard.
	//
	// A tool is only safe as `notify` when undoing it is cheap and complete.
	// Declare the undo in critical_tool_undo_hints so the record card can say
	// how; fleet refuses `notify` on the base email suffixes outright, because a
	// sent email has no undo at all.
	//
	//	agent_policy:
	//	  critical_tool_modes:
	//	    deploy_page: notify        # Pages keeps immutable versions
	//	    create_deal: approve       # audit-gated; unchanged
	//	  critical_tool_undo_hints:
	//	    deploy_page: "Undo with mcp_pages_rollback_page(slug, version_id)."
	CriticalToolModes map[string]string `yaml:"critical_tool_modes"`
	// CriticalToolUndoHints maps the same suffixes to a one-line, bundle-authored
	// statement of how to reverse the action, rendered on the record card. Fleet
	// does not know any client's undo verb and must not guess one: "we can always
	// roll back" is only true in practice if the card says how.
	CriticalToolUndoHints map[string]string `yaml:"critical_tool_undo_hints"`
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

	// Runtime is the OCI runtime passed to `podman run --runtime=<value>` for
	// every sandbox container (manifest sandbox.runtime). It selects the
	// ISOLATION posture of the mandatory sandbox without changing any other
	// invariant:
	//
	//	""        — Podman's configured default (crun/runc): rootless containers
	//	            sharing the host kernel. No extra host requirements (#217).
	//	"runc"    — explicit runc; same shared-kernel posture as the default.
	//	"kata"    — Kata Containers: every sandbox container is a dedicated KVM
	//	            VM with its own guest kernel — escape requires a hypervisor CVE,
	//	            not just a container-escape. Requires /dev/kvm + kata-runtime.
	//	"libkrun" — lightweight microVM (Apple Virtualization.framework on macOS,
	//	            libkrun on Linux); lower overhead than Kata. Normalized to the
	//	            Podman runtime name "krun" at consume time (see sandbox.NormalizeRuntime).
	//	"runsc"   — gVisor user-space kernel (syscall interception, not a VM).
	//
	// An empty value leaves the existing rootless-container default unchanged
	// (byte-for-byte the pre-#217 behaviour). The value is stored VERBATIM here;
	// the consuming layer (cmd/fleet) normalizes friendly aliases and applies the
	// boot-time preflight + Kata memory-overhead. An explicit FLEET_SANDBOX_RUNTIME
	// env var still wins over this manifest value, mirroring sandbox.image.
	Runtime string

	// NetworkAllowlist is the default sandbox egress allowlist (#211) used when
	// FLEET_DEFAULT_NETWORK_MODE=allowlisted: networked sandboxes may reach only
	// these domains (via the host egress proxy). Entries are exact domains or
	// "*."-prefixed wildcards (e.g. "pypi.org", "*.github.com"). From the
	// manifest sandbox.network_allowlist. Empty in allowlisted mode = deny all
	// egress (best-effort — see ADR-0012).
	NetworkAllowlist []string

	// Backend selects WHERE sandboxes run (#989 / ADR-0049): "" or "podman"
	// for the co-located rootless-Podman backend (the single-box default), or
	// "kubernetes" for ephemeral pods in a cluster (the split
	// control-plane/runner enterprise path). Stored VERBATIM; the consuming
	// layer (cmd/fleet) validates fail-closed and an explicit
	// FLEET_SANDBOX_BACKEND env var wins, mirroring sandbox.runtime.
	Backend string

	// Kubernetes carries the kubernetes-backend settings (manifest
	// sandbox.kubernetes). Meaningful only when the resolved backend is
	// "kubernetes"; each FLEET_SANDBOX_K8S_* env var overrides its field.
	Kubernetes KubernetesSandbox
}

// KubernetesSandbox is the resolved sandbox.kubernetes block: where sandbox
// pods run and what they mount. All fields are trusted operator config, same
// authority tier as sandbox.image / sandbox.runtime.
type KubernetesSandbox struct {
	// Namespace for sandbox pods (default applied at consume time:
	// "fleet-sandboxes" — kept separate from the control plane's namespace so
	// RBAC and the deny-all NetworkPolicy stay narrowly scoped).
	Namespace string `yaml:"namespace"`
	// WorkspaceClaim is the ReadWriteMany PVC (in Namespace) holding the
	// workspace root, mounted into every sandbox pod at the same absolute
	// path the control plane mounts it. Required for the kubernetes backend.
	WorkspaceClaim string `yaml:"workspace_claim"`
	// ServiceAccount stamped on sandbox pods (identity only — the token is
	// never mounted).
	ServiceAccount string `yaml:"service_account"`
	// ImagePullSecret for private sandbox-image registries.
	ImagePullSecret string `yaml:"image_pull_secret"`
	// RuntimeClass selects hypervisor isolation (e.g. kata) — the kubernetes
	// counterpart of sandbox.runtime, preflighted fail-closed (ADR-0010).
	RuntimeClass string `yaml:"runtime_class"`
	// SeccompProfile is a node-local Localhost seccomp profile path (relative
	// to the kubelet seccomp root); empty = RuntimeDefault.
	SeccompProfile string `yaml:"seccomp_profile"`
	// Kubeconfig selects out-of-cluster auth; empty = in-cluster.
	Kubeconfig string `yaml:"kubeconfig"`
	// NetworkPolicy is the deny-all NetworkPolicy name the boot preflight
	// requires to exist (default "fleet-sandbox-deny-all").
	NetworkPolicy string `yaml:"network_policy"`
	// OpenEgressPolicy is its counterpart for egress=open pods (default
	// "fleet-sandbox-open-egress"), required by the preflight whenever the
	// default network mode is open. The WAIVER for that requirement is
	// deliberately not here: it states something true of one cluster, and a
	// bundle is meant to travel between them.
	OpenEgressPolicy string `yaml:"open_egress_policy"`
	// BundleDocsInImage declares that the sandbox IMAGE carries this bundle's
	// supporting-doc dirs (protocols/, personas/, system_prompts/, skills/) at
	// the SAME absolute paths the control plane reads them from — the only way
	// a pod can see them, since it mounts just the workspace claim. Set it and
	// the fileop path anchors for those roots stay valid inside a pod, so
	// view_file works on `protocols/…` again; leave it false (the default) and
	// the anchors are dropped, which is what refuses those reads. A
	// declaration, not a probe: fleet cannot inspect an image's contents, so a
	// wrong declaration surfaces as a not-found read, never as a widened
	// boundary (reads only, still read-only, still inside the sandbox).
	// FLEET_SANDBOX_K8S_BUNDLE_DOCS_IN_IMAGE overrides it.
	BundleDocsInImage bool `yaml:"bundle_docs_in_image"`
	// NodeSelector pins sandbox pods to labeled nodes (a dedicated runner
	// pool). FLEET_SANDBOX_K8S_NODE_SELECTOR ("k=v,k=v") overrides it.
	NodeSelector map[string]string `yaml:"node_selector"`
	// Tolerations let sandbox pods schedule onto a tainted runner pool.
	// FLEET_SANDBOX_K8S_TOLERATIONS (a JSON array) overrides it.
	Tolerations []KubernetesToleration `yaml:"tolerations"`
}

// KubernetesToleration is the manifest shape of one sandbox-pod toleration
// (the four core/v1 fields fleet forwards).
type KubernetesToleration struct {
	Key      string `yaml:"key" json:"key,omitempty"`
	Operator string `yaml:"operator" json:"operator,omitempty"`
	Value    string `yaml:"value" json:"value,omitempty"`
	Effect   string `yaml:"effect" json:"effect,omitempty"`
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
	Containerfile    string             `yaml:"containerfile"`
	Tag              string             `yaml:"tag"`
	Image            string             `yaml:"image"`
	Runtime          string             `yaml:"runtime"`
	NetworkAllowlist []string           `yaml:"network_allowlist"`
	Backend          string             `yaml:"backend"`
	Kubernetes       *KubernetesSandbox `yaml:"kubernetes"`
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
	// Logo is a bundle-relative path to the mark the web shell renders in the
	// navigation rail (e.g. "assets/acme-mark.svg"). Colors alone cannot
	// white-label a deployment: the rail shows a logo on every page, so without
	// this a themed deployment wore fleet's mark next to its own app_name.
	//
	// The file is served by httpapi's /brand/logo, which resolves it against
	// Bundle.Dir at request time — fleet copies nothing into web/public, so a
	// bundle re-theme needs no web rebuild. Empty means the web falls back to
	// fleet's own mark.
	//
	// Validated at bundle load (see validateBrandLogo): the path must be
	// lexically local (no absolute path, no ".." escape), must resolve inside
	// the bundle after symlink resolution, must exist as a regular file, and
	// must carry an extension the HTTP layer knows a content type for. A bundle
	// naming a missing or unservable logo fails loudly at startup rather than
	// serving a broken image on every page.
	Logo string `yaml:"logo"`
	// ShareImage is a bundle-relative path to the image link-unfurl scrapers
	// show for this deployment (og:image / twitter:image), e.g.
	// "assets/acme-share.png". 1280x640 is the conventional size.
	//
	// It exists because the OG image was the last un-themable brand surface: a
	// checked-in web/public/share.png was the og:image for EVERY deployment, so
	// a link to a white-labeled instance unfurled in Slack, iMessage, Discord or
	// Teams wearing fleet's own marketing card — served from the client's own
	// domain, so nothing looked amiss to the unfurler. Omit the field and
	// fleet's neutral generic card stands.
	//
	// Validated at bundle load exactly like Logo (see resolveBrandImage): the
	// path must be lexically local, must resolve inside the bundle after symlink
	// resolution, must be a regular file, and must carry an extension the HTTP
	// layer knows a content type for.
	ShareImage string `yaml:"share_image"`
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
	Name        string           `yaml:"name" json:"name"`
	Description string           `yaml:"description" json:"description,omitempty"`
	Icon        string           `yaml:"icon" json:"icon,omitempty"`
	Task        TaskTemplateTask `yaml:"task" json:"task"`
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
	// Title pre-fills the task's display label. Omitted, the create form falls
	// back to the template's own name, which is what the operator picked it by.
	Title         string  `yaml:"title,omitempty" json:"title,omitempty"`
	Prompt        string  `yaml:"prompt" json:"prompt,omitempty"`
	Model         *string `yaml:"model,omitempty" json:"model,omitempty"`
	FallbackModel *string `yaml:"fallback_model,omitempty" json:"fallback_model,omitempty"`
	MaxIterations *int    `yaml:"max_iterations,omitempty" json:"max_iterations,omitempty"`
	MaxRetries    *int    `yaml:"max_retries,omitempty" json:"max_retries,omitempty"`
	Recurrence    string  `yaml:"recurrence,omitempty" json:"recurrence,omitempty"`
	Timezone      string  `yaml:"timezone,omitempty" json:"timezone,omitempty"`
	Priority      int     `yaml:"priority,omitempty" json:"priority,omitempty"`
	AllowNetwork  bool    `yaml:"allow_network,omitempty" json:"allow_network,omitempty"`
	// AllowDelegation is a pointer so an omitted key leaves the create form at
	// its default (true since #1043) instead of forcing an opt-out.
	AllowDelegation        *bool    `yaml:"allow_delegation,omitempty" json:"allow_delegation,omitempty"`
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

	// TLS, when set on an http server, hardens the connection: pin the trusted
	// CA, present a client certificate (mTLS), and/or pin the server's public
	// key (#280). Omitted = default system TLS verification. Ignored for stdio.
	TLS *ServerTLSDef `yaml:"tls"`

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

	// IdentityEnv names the env KEYS (must be keys of Env) that route identity
	// or money — owner/member/marketplace-account ids, seat-routing tokens. A
	// named-account variant spawn is REFUSED when any of these has a non-empty
	// default-seat value that the account's <VAR>_<ACCOUNT> overlay did not
	// override (agentcore's inherited-routing-identity guard, mirroring the
	// cutlass mcp_loader): suffixing the API key but not the owner id would
	// otherwise transact in the DEFAULT client's seat under the named account's
	// label. Optional; an absent list keeps the overrides>0 guard only.
	IdentityEnv []string `yaml:"identity_env"`

	// Optional marks a server users must opt into per conversation (chat's
	// Optional-server semantics). DisplayName/Description/Beta/EnabledByDefault
	// drive the settings-UI catalog rendering.
	Optional         bool   `yaml:"optional"`
	DisplayName      string `yaml:"display_name"`
	Description      string `yaml:"description"`
	Beta             bool   `yaml:"beta"`
	EnabledByDefault bool   `yaml:"enabled_by_default"`

	// DataSources names the external data this connector reads or writes —
	// bucket/endpoint identifiers like "s3://index-omnicom-reporting" or
	// "jmap://my.mailbux.com" — for display on the settings connections page,
	// so an operator can inventory what a deployment touches without reading
	// connector source. DECLARATIVE, not verified: the credential's scope
	// (IAM policy, API key) remains the authority on what is reachable.
	DataSources []string `yaml:"data_sources"`

	// Probe declares this server's canary for `fleet mcp test --deep`: ONE
	// read-only tool call that proves the server works end-to-end (credentials
	// accepted AND the upstream returns real data), one rung past the
	// auth-status convention. The bundle author vets the declared call for
	// side effects ONCE, here — the probe runner executes only what a manifest
	// declares, never auto-discovers tools to call. Absent = the server is
	// noted as unproven-beyond-handshake, never failed.
	Probe *ProbeDef `yaml:"probe"`

	// The three fields below are set ONLY by the Agent Plugins loader
	// (plugins.go) and never by the manifest (unexported, so strict YAML
	// decoding cannot reach them). dir is the subprocess working directory —
	// the plugin root or its declared cwd — in place of the bundle dir.
	// literalEnv says Env/Headers are already-final strings: the plugin spec
	// expands exactly ${PLUGIN_ROOT}/${PLUGIN_DATA} and forbids any other
	// expansion, so MCPServerConfigs must not run resolveEnvMap over them.
	// plugin names the source plugin for provenance (PluginName()).
	dir        string
	literalEnv bool
	plugin     string
}

// ProbeDef is one declared read-only canary call for `fleet mcp test --deep`.
// Assertions are deliberately minimal — the call must succeed and not be
// flagged isError, plus an optional Contains substring — because asserting on
// live upstream content makes the probe flaky as real-world state changes;
// the probe proves the pipe works, not that the data looks a particular way.
type ProbeDef struct {
	// Tool is the tool to call (required; must be advertised by the server,
	// and inside the server's tools: allowlist when one is set — the probe
	// must never exercise a tool the runtime itself would not expose).
	Tool string `yaml:"tool"`
	// Args is the literal argument object for the call (after the manifest's
	// usual env interpolation). Keep it minimal and read-only, e.g.
	// {maxResults: 1}.
	Args map[string]interface{} `yaml:"args"`
	// Contains optionally asserts the result's first text block contains this
	// substring (case-sensitive). Prefer shape-ish markers ("messages", an id
	// prefix) over content that changes.
	Contains string `yaml:"contains"`
}

// toConfig maps the manifest probe shape to the runtime config.MCPProbeConfig,
// or nil when absent (mirroring ServerTLSDef.toMCP).
func (p *ProbeDef) toConfig() *config.MCPProbeConfig {
	if p == nil {
		return nil
	}
	return &config.MCPProbeConfig{
		Tool:     strings.TrimSpace(p.Tool),
		Args:     p.Args,
		Contains: p.Contains,
	}
}

// ServerTLSDef is the manifest shape for per-server TLS hardening of an http
// MCP server (#280). All fields are optional paths/values on the fleet host;
// they map 1:1 to mcp.TLSOptions (see its doc for semantics). cert/key/ca files
// are read host-side at connect time and never enter the sandbox.
type ServerTLSDef struct {
	CACert       string `yaml:"ca_cert"`       // PEM CA bundle to verify the server against
	ClientCert   string `yaml:"client_cert"`   // PEM client cert for mTLS (with client_key)
	ClientKey    string `yaml:"client_key"`    // PEM client key for mTLS (with client_cert)
	PinnedSHA256 string `yaml:"pinned_sha256"` // hex SHA-256 of the server leaf public key (SPKI)
	ServerName   string `yaml:"server_name"`   // SNI / verified hostname override
}

// toMCP maps the manifest TLS shape to the runtime mcp.TLSOptions, or nil when
// the block is absent/empty (so an empty `tls:` is treated as "no hardening").
func (d *ServerTLSDef) toMCP() *mcp.TLSOptions {
	if d == nil {
		return nil
	}
	o := mcp.TLSOptions{
		CACertFile:     strings.TrimSpace(d.CACert),
		ClientCertFile: strings.TrimSpace(d.ClientCert),
		ClientKeyFile:  strings.TrimSpace(d.ClientKey),
		PinnedSHA256:   strings.TrimSpace(d.PinnedSHA256),
		ServerName:     strings.TrimSpace(d.ServerName),
	}
	if o.IsZero() {
		return nil
	}
	return &o
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
// declared input at call time. URL context is percent-encoded; body context is
// JSON-string-escaped when the body is JSON (a Content-Type header containing
// "json", or — with no Content-Type — a template starting with { or [) so a
// model-supplied value cannot inject fields into the outbound request (#600),
// and raw otherwise (declare a non-JSON Content-Type for form/plain-text
// bodies). InputSchema is the JSON Schema the model sees. ResponseJQ, when set, is a
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

// A2APeerDef is one remote A2A agent from the manifest's a2a_peers: section
// (#1368 — #1279 Phase 3): a peer a fleet agent may delegate work to over the
// A2A protocol. Registered as four tools on the synthetic A2AToolServerName
// server (<name>_send, _status, _wait, _cancel), so the model addresses them
// as mcp__a2a_<name>_send etc. — routed, gated, redacted, and brokered
// host-side exactly like http_tools and MCP tools.
//
// SECURITY — the definition is BUNDLE-AUTHOR-DEFINED and therefore trusted:
// peers are operator policy, never model choice. Headers carry the peer
// credential as ${ENV_VAR} references resolved host-side at call time (same
// boundary as http_tools). Description is the ONLY text about the peer the
// model ever sees — a remote agent card is never fetched into the roster,
// because remote card text would be a prompt-injection channel. What the
// peer sends BACK is untrusted external content and is rendered as such.
type A2APeerDef struct {
	Name        string            `yaml:"name"`        // ^[a-z0-9_]{1,32}$
	RPCURL      string            `yaml:"rpc_url"`     // the peer's JSON-RPC endpoint, http(s) only
	Description string            `yaml:"description"` // required; bundle-authored
	Headers     map[string]string `yaml:"headers"`     // values support ${ENV_VAR}, resolved host-side at call time
	// Critical opts <name>_send and <name>_cancel into the critical-tool audit
	// gate (chat: approval card; scheduled: confirm_audit accounting) —
	// delegating work off-box is a side effect with a cost on the remote side.
	Critical bool `yaml:"critical"`
}

// WebhookTriggerDef is one inbound conversation trigger from the manifest's
// webhook_triggers: section (#268). An external system (GitHub, Slack, CI, a
// Zapier hook) that presents a valid signature to POST /webhooks/{slug} starts
// a fresh interactive conversation under NotifyUser, seeded with a prompt
// rendered from PromptTemplate against the request payload. The turn runs
// through the SAME governed core (agentcore.Run) as any chat turn — this is an
// inbound I/O adapter, not a second agent loop.
//
// SECURITY — the trigger definition is BUNDLE-AUTHOR-DEFINED and therefore
// trusted (same trust level as an mcp_servers or http_tools entry). The inbound
// PAYLOAD, by contrast, is UNTRUSTED attacker-controllable input: it is exposed
// to PromptTemplate only as DATA ({{.payload…}}), never as template text, and
// the resulting prompt is untrusted model input bounded by the mandatory
// sandbox, the operator-chosen Persona, and the per-turn cost/iteration
// ceilings. See docs/adr/0016-webhook-triggered-conversations.md.
//
//   - Exactly one authentication method is configured per trigger:
//     HMACSecretEnv (GitHub-style HMAC-SHA256 over the raw body, verified against
//     HMACHeader) OR TokenSecretEnv (Slack v0 signing secret). The env-var NAME
//     is declared here; its VALUE is read host-side from the process env at
//     request time (registered via EnvVarNames → RegisterAllowedEnvVars so it
//     flows from .env), exactly like an MCP connector credential. The secret
//     never enters the sandbox, the model context, or the logs.
type WebhookTriggerDef struct {
	Slug           string `yaml:"slug"`             // URL-safe path segment; unique within the manifest
	Description    string `yaml:"description"`      // human-readable, for operator docs
	HMACSecretEnv  string `yaml:"hmac_secret_env"`  // env var holding the GitHub-style HMAC secret
	HMACHeader     string `yaml:"hmac_header"`      // signature header (default X-Hub-Signature-256)
	TokenSecretEnv string `yaml:"token_secret_env"` // env var holding the Slack signing secret
	Persona        string `yaml:"persona"`          // persona for the triggered conversation
	Model          string `yaml:"model"`            // model slug (optional; falls back to the server default)
	PromptTemplate string `yaml:"prompt_template"`  // Go text/template; {{.payload}} is the decoded JSON body
	NotifyUser     string `yaml:"notify_user"`      // email whose conversation store the trigger writes to (required)
}

// DefaultHMACHeader is the signature header a GitHub-style HMAC trigger reads
// when the manifest does not override HMACHeader.
const DefaultHMACHeader = "X-Hub-Signature-256"

// UsesSlack reports whether the trigger authenticates via the Slack v0 signing
// secret (TokenSecretEnv) rather than a GitHub-style HMAC (HMACSecretEnv).
func (t WebhookTriggerDef) UsesSlack() bool {
	return strings.TrimSpace(t.TokenSecretEnv) != "" && strings.TrimSpace(t.HMACSecretEnv) == ""
}

// SignatureHeader is the request header carrying the GitHub-style HMAC
// signature, defaulting to DefaultHMACHeader when the manifest omits it.
func (t WebhookTriggerDef) SignatureHeader() string {
	if h := strings.TrimSpace(t.HMACHeader); h != "" {
		return h
	}
	return DefaultHMACHeader
}

// ProviderDef is one LLM provider from the manifest's providers: block (#289).
// It selects a backend for a set of model slugs, letting a deployment route
// inference natively to Anthropic / OpenAI / a self-hosted Ollama endpoint
// instead of exclusively through OpenRouter (for failover, data residency, or to
// avoid the OpenRouter markup). A bundle with NO providers: block keeps the
// historical single-OpenRouter behavior unchanged.
//
// SECURITY — the credential boundary mirrors the MCP catalog: APIKeyEnv names an
// env var; its VALUE is read HOST-SIDE from the process env at boot (registered
// via EnvVarNames → RegisterAllowedEnvVars, so it flows from .env) and handed to
// the model resolver. The secret never enters the manifest, the sandbox, the
// model context, or the logs.
type ProviderDef struct {
	Name                string   `yaml:"name"`                  // routing name; unique within the manifest
	Type                string   `yaml:"type"`                  // openrouter | anthropic | openai | ollama
	APIKeyEnv           string   `yaml:"api_key_env"`           // env var holding the credential (not needed for ollama)
	BaseURL             string   `yaml:"base_url"`              // optional endpoint override
	Models              []string `yaml:"models"`                // slugs this provider serves; empty = catch-all
	ContextWindowTokens int      `yaml:"context_window_tokens"` // provider-local context; OpenRouter uses authoritative per-model metadata
}

// RemoteMCPCatalogEntry is one curated third-party hosted MCP server from the
// manifest's remote_mcp_catalog: section (#538). It is a DIRECTORY LISTING, not
// a connection: fleet never talks to the URL until a user explicitly adds it
// via the per-user remote-MCP flow (#443), which is where OAuth happens.
//
// TRUST — an entry here is deliberately weaker than an mcp_servers entry. A
// bundled (mcp_servers) connector is bundle-author-defined code that runs
// inside the mandatory sandbox with credentials brokered host-side. A catalog
// entry names a service HOSTED BY A THIRD PARTY: tool calls and their
// arguments (which can contain conversation content) travel to that vendor,
// governed by the vendor's own terms. The UI must label the two classes
// distinctly so a user knows what they are opting into; the bundle author
// curates the list but does not control the remote service.
type RemoteMCPCatalogEntry struct {
	Name        string   `yaml:"name"`         // stable identifier; unique within the manifest
	DisplayName string   `yaml:"display_name"` // human-readable label ("GitHub")
	Description string   `yaml:"description"`  // what the server does, one or two sentences
	URL         string   `yaml:"url"`          // the hosted MCP endpoint (https required)
	Vendor      string   `yaml:"vendor"`       // who operates the service ("GitHub, Inc.")
	DocsURL     string   `yaml:"docs_url"`     // vendor's documentation for the server (optional)
	RepoURL     string   `yaml:"repo_url"`     // source repository, so users can vet the server (optional)
	Category    string   `yaml:"category"`     // directory grouping slug ("development", "crm-sales", …); free-form kebab-case
	Tags        []string `yaml:"tags"`         // lowercase search keywords beyond name/description
	// Provenance is the trust tier of the HOSTING OPERATOR:
	//   official    — the endpoint is operated by the service's own vendor
	//                 (GitHub hosting GitHub's server).
	//   third_party — an identifiable company hosts access to OTHER vendors'
	//                 services (aggregators/integrators): that operator, not the
	//                 underlying vendor, sees the traffic and often holds the
	//                 delegated tokens.
	//   community   — an identifiable maintainer who is neither the service's
	//                 vendor nor a platform company hosts the endpoint.
	// Absent defaults to official — a legacy shim for pre-existing bundle
	// manifests, whose entries were all vendor-official; fleet's built-in
	// catalog requires it explicitly (CI-enforced). The UI badges the tiers
	// distinctly and gates non-official adds behind an operator-named consent
	// step.
	Provenance string `yaml:"provenance"`
	// Auth is a UI hint for what connecting takes: "oauth" (sign in with the
	// vendor), "api_key" (bring a key/token), "open" (no credentials needed),
	// or "tenant" (the URL carries a {placeholder} — per org/workspace/store,
	// so the user supplies their own endpoint). Optional; blank renders no hint.
	Auth string `yaml:"auth"`
	// SetupHint is one or two plain-language sentences of onboarding guidance
	// rendered VISIBLY on the directory card: where the URL or key comes from,
	// what must exist first (an app registration, a paid plan, a self-hosted
	// deployment), or which placeholder value to look up. Required in the
	// built-in catalog for every entry a user cannot one-click add (auth
	// "tenant" or "api_key") — a listing that names a prerequisite without
	// saying how to satisfy it is advertising, not onboarding.
	SetupHint string `yaml:"setup_hint"`
	// SetupURL links the vendor page that actually walks through connecting —
	// creating the API key, finding the tenant URL, registering the OAuth
	// client, or deploying the server. Distinct from DocsURL (what the server
	// does); the UI falls back to DocsURL when absent.
	SetupURL string `yaml:"setup_url"`
	// APIKeyHeader (auth "api_key" only) is the HTTP header NAME the vendor
	// expects the key under, e.g. "X-API-Key". Empty means the default shape:
	// "Authorization: Bearer <key>".
	APIKeyHeader string `yaml:"api_key_header"`
	// APIKeyQuery (auth "api_key" only) is the URL query-parameter NAME the
	// vendor expects the key under, e.g. "browserbaseApiKey" — for hosted
	// servers that authenticate in the URL rather than a header. The runtime
	// attaches the sealed key per-request; it is never persisted in a URL.
	APIKeyQuery string `yaml:"api_key_query"`
	// ClientRegistration is "manual" when the vendor's authorization server
	// does not support RFC 7591 dynamic client registration — the user must
	// bring their own OAuth client (a GCP OAuth client, an Entra app
	// registration, a vendor-issued client ID). The directory card then
	// collects a client ID (+ optional secret) up front instead of letting the
	// add fail mid-discovery. Empty means DCR is expected to work.
	ClientRegistration string `yaml:"client_registration"`
	// Featured surfaces the entry in the directory's Featured section — the
	// short, curated shelf of household-name connectors shown before the
	// category listing. Kept small on purpose (a test caps the built-in count)
	// so it stays a recommendation, not a second catalog.
	Featured bool `yaml:"featured"`
	// Builtin marks an entry inherited from fleet's embedded directory rather
	// than authored by this bundle's manifest. Never parsed from YAML.
	Builtin bool `yaml:"-"`
}

// manifest is the on-disk YAML shape. Sandbox is a pointer so an absent block
// (a minimal/legacy bundle that never opted into the sandbox-as-config contract)
// is distinguishable from a present-but-empty one: only a DECLARED sandbox block
// enforces the Containerfile-exists invariant.
type manifest struct {
	Branding        Branding                `yaml:"branding"`
	Models          Models                  `yaml:"models"`
	MCPServers      []ServerDef             `yaml:"mcp_servers"`
	HTTPTools       []HTTPToolDef           `yaml:"http_tools"`
	A2APeers        []A2APeerDef            `yaml:"a2a_peers"`
	WebhookTriggers []WebhookTriggerDef     `yaml:"webhook_triggers"`
	RemoteMCPs      []RemoteMCPCatalogEntry `yaml:"remote_mcp_catalog"`
	// RemoteMCPBuiltin toggles inheriting fleet's embedded hosted-server
	// directory (default TRUE — it is a listing only; nothing connects until a
	// user explicitly adds a server and completes OAuth). A bundle sets false
	// to curate from scratch. Pointer so absent ≠ explicit false.
	RemoteMCPBuiltin *bool `yaml:"remote_mcp_catalog_builtin"`
	// RemoteMCPCommunity additionally inherits the built-in entries whose
	// provenance is community (endpoints hosted by identifiable maintainers who
	// are not the service's vendor). Default FALSE: the silently-inherited
	// surface stays vendor-official + labeled aggregators; an operator opts a
	// bundle into the community tier deliberately.
	RemoteMCPCommunity bool `yaml:"remote_mcp_catalog_community"`
	// RemoteMCPHidden tombstones specific built-in entries by name so a bundle
	// can drop individual listings (or an operator can kill a compromised one
	// in a config-only change) without opting out of the whole directory.
	RemoteMCPHidden []string `yaml:"remote_mcp_catalog_hidden"`
	// SkillsBuiltin toggles inheriting fleet's embedded Agent Skills pack
	// (default TRUE); SkillsHidden tombstones individual built-in skills. The
	// skills analogues of the remote_mcp_catalog knobs — see builtin_skills.go.
	SkillsBuiltin *bool    `yaml:"skills_builtin"`
	SkillsHidden  []string `yaml:"skills_hidden"`
	// PluginRoots lists extra directories (absolute, or relative to the bundle
	// root) whose immediate children are Agent Plugins, in addition to the
	// fixed <bundle>/plugins/ dir. See plugins.go / docs/AGENT-PLUGINS.md.
	PluginRoots       []string         `yaml:"plugin_roots"`
	Providers         []ProviderDef    `yaml:"providers"`
	FallbackProviders []string         `yaml:"fallback_providers"`
	EmptyState        EmptyState       `yaml:"empty_state"`
	TaskTemplates     []TaskTemplate   `yaml:"task_templates"`
	AgentPolicy       AgentPolicy      `yaml:"agent_policy"`
	Personas          []PersonaDef     `yaml:"personas"`
	Pricing           PricingConfig    `yaml:"pricing"`
	Sandbox           *sandboxManifest `yaml:"sandbox"`
	Hooks             *HooksConfig     `yaml:"hooks"`
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
	// Parse only the connector-bearing sections before interpolation. This
	// source snapshot preserves env-var names even when their values are already
	// exported and the normal interpolation pass below substitutes the tokens
	// out of the runtime manifest.
	var rawConnectors struct {
		MCPServers []ServerDef   `yaml:"mcp_servers"`
		HTTPTools  []HTTPToolDef `yaml:"http_tools"`
		A2APeers   []A2APeerDef  `yaml:"a2a_peers"`
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
		SystemPromptsDir:               filepath.Join(abs, "system_prompts"),
		PersonasDir:                    filepath.Join(abs, "personas"),
		ProtocolsDir:                   filepath.Join(abs, "protocols"),
		PromptsDir:                     filepath.Join(abs, "prompts"),
		SkillsDir:                      filepath.Join(abs, "skills"),
		MCPDir:                         filepath.Join(abs, "mcp"),
		EvalsDir:                       filepath.Join(abs, "evals"),
	}
	applyBrandingDefaults(&b.Branding)
	if err := b.resolveBrandLogo(); err != nil {
		return nil, err
	}
	if err := b.resolveBrandShareImage(); err != nil {
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
	taken := map[string]bool{HTTPToolServerName: true}
	for i := range b.MCPCatalog {
		taken[b.MCPCatalog[i].Name] = true
	}
	for i := range b.HTTPTools {
		taken[b.HTTPTools[i].Name] = true
	}
	pl := loadPlugins(abs, m.PluginRoots, taken)
	b.Plugins = pl.plugins
	b.pluginProblems = pl.problems
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

// brandLogoContentTypes maps the image extensions a bundle logo may use to the
// content type /brand/logo serves it as. An allowlist rather than
// mime.TypeByExtension: the response type is what makes the browser render the
// bytes, so the set stays deliberately small and image-only — a bundle cannot
// turn the logo route into a general file server for HTML or scripts by naming
// a different extension.
var brandLogoContentTypes = map[string]string{
	".svg":  "image/svg+xml",
	".png":  "image/png",
	".webp": "image/webp",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".ico":  "image/x-icon",
}

// BrandLogoContentType returns the content type fleet serves a bundle logo
// under, or "" when the extension is not one it knows. The HTTP layer and the
// load-time validator share this one map so a bundle can never pass validation
// with a path the route would then refuse to serve.
func BrandLogoContentType(p string) string {
	return brandLogoContentTypes[strings.ToLower(filepath.Ext(p))]
}

// BrandLogoExtensions returns the supported logo extensions, sorted, for error
// messages and docs.
func BrandLogoExtensions() []string {
	out := make([]string, 0, len(brandLogoContentTypes))
	for ext := range brandLogoContentTypes {
		out = append(out, ext)
	}
	slices.Sort(out)
	return out
}

// resolveBrandLogo validates branding.logo and records the absolute file the
// HTTP layer serves. Empty is the norm (fleet's own mark stands) and is not an
// error.
//
// The path arrives from a manifest, so it is operator-authored rather than user
// input — but it is still resolved defensively, because the failure modes are
// silent and permanent: a "../../etc/passwd" would otherwise be served under an
// image content type on every page load, and a path that merely doesn't exist
// would render a broken mark on every page of a deployment that believes it is
// branded. Both fail at startup instead.
//
// filepath.IsLocal rejects an absolute path and any ".." escape lexically;
// EvalSymlinks then re-checks containment, so a symlink inside the bundle
// pointing outward cannot widen the reach either.
func (b *Bundle) resolveBrandLogo() error {
	rel := strings.TrimSpace(b.Branding.Logo)
	b.Branding.Logo = rel
	resolved, err := b.resolveBrandImage("branding.logo", rel)
	if err != nil {
		return err
	}
	b.BrandLogoPath = resolved
	return nil
}

// resolveBrandShareImage validates branding.share_image the same way, and
// records the absolute file /brand/share-image serves. Empty is the norm
// (fleet's own generic share card stands) and is not an error.
//
// This field exists because the OG image was the last un-themable brand surface:
// a checked-in web/public/share.png was the og:image for EVERY deployment, so
// pasting a link to a white-labeled instance into Slack, iMessage, Discord or
// Teams unfurled with another company's logo — served from the client's own
// domain, so nothing looked amiss to the unfurler.
func (b *Bundle) resolveBrandShareImage() error {
	rel := strings.TrimSpace(b.Branding.ShareImage)
	b.Branding.ShareImage = rel
	resolved, err := b.resolveBrandImage("branding.share_image", rel)
	if err != nil {
		return err
	}
	// Unlike the logo, the share image is only ever consumed by link-unfurl
	// scrapers, and none of them render SVG (or ICO) unfurls — the web proxy
	// passes through PNG/WebP/JPEG only and redirects anything else to fleet's
	// generic card. Rejecting the logo-only extensions HERE (after the shared
	// path/containment checks, so their errors keep precedence) keeps the
	// branding contract's promise that a bad value fails at startup instead of
	// every unfurl silently showing the wrong brand.
	if rel != "" {
		switch strings.ToLower(filepath.Ext(rel)) {
		case ".png", ".webp", ".jpg", ".jpeg":
		default:
			return fmt.Errorf("branding.share_image %q: unsupported extension (unfurl scrapers only render .png, .webp, .jpg, .jpeg)", rel)
		}
	}
	b.BrandShareImagePath = resolved
	return nil
}

// resolveBrandImage is the shared validator for every bundle-relative image path
// in branding:. field names it for error messages ("branding.logo"). An empty
// rel returns ("", nil) — declaring no image is the norm, not an error.
//
// The path arrives from a manifest, so it is operator-authored rather than user
// input — but it is still resolved defensively, because the failure modes are
// silent and permanent: a "../../etc/passwd" would otherwise be served under an
// image content type on every page load, and a path that merely doesn't exist
// would render a broken image on every page of a deployment that believes it is
// branded. Both fail at startup instead.
//
// filepath.IsLocal rejects an absolute path and any ".." escape lexically;
// EvalSymlinks then re-checks containment, so a symlink inside the bundle
// pointing outward cannot widen the reach either.
func (b *Bundle) resolveBrandImage(field, rel string) (string, error) {
	if rel == "" {
		return "", nil
	}
	if !filepath.IsLocal(rel) {
		return "", fmt.Errorf("%s %q: must be a bundle-relative path (no absolute path, no \"..\")", field, rel)
	}
	if BrandLogoContentType(rel) == "" {
		return "", fmt.Errorf("%s %q: unsupported extension (want one of %s)", field, rel, strings.Join(BrandLogoExtensions(), ", "))
	}
	root, err := filepath.EvalSymlinks(b.Dir)
	if err != nil {
		return "", fmt.Errorf("%s %q: resolve bundle dir: %w", field, rel, err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(b.Dir, rel))
	if err != nil {
		return "", fmt.Errorf("%s %q: %w", field, rel, err)
	}
	if resolved != root && !strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("%s %q: resolves outside the bundle (%s)", field, rel, resolved)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("%s %q: %w", field, rel, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s %q: not a regular file", field, rel)
	}
	return resolved, nil
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
		if s.Name == A2AToolServerName {
			return fmt.Errorf("mcp_servers[%q]: name %q is reserved for a2a_peers", s.Name, A2AToolServerName)
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
		// TLS placement is validated for EVERY server (after Type is normalized)
		// so a tls: block that could never apply fails the load loudly instead of
		// being silently dropped.
		if err := validateServerTLS(s.Name, s.Type, s.URL, s.TLS); err != nil {
			return err
		}
		// identity_env entries must name keys of THIS server's env map (and the
		// account overlay is stdio-only): a typo'd identity var would otherwise
		// silently never guard anything — for a wrong-seat/wrong-revenue guard
		// that is the dangerous direction, so fail loud at startup instead.
		for _, v := range s.IdentityEnv {
			trimmed := strings.TrimSpace(v)
			if trimmed == "" {
				return fmt.Errorf("mcp_servers[%q]: identity_env entries must be non-empty", s.Name)
			}
			if s.Type != "stdio" {
				return fmt.Errorf("mcp_servers[%q]: identity_env is only valid on a stdio server (accounts are env-suffixed; http servers reject account variants)", s.Name)
			}
			if _, ok := s.Env[trimmed]; !ok {
				return fmt.Errorf("mcp_servers[%q]: identity_env var %q is not a key of the server's env map", s.Name, trimmed)
			}
		}
		// The account-suffix convention is purely lexical, so its base vars
		// must not be underscore-prefixes of one another — fail the load
		// loudly rather than cross-wire credentials at spawn time (#1124).
		if err := validateAccountSuffixBases(s); err != nil {
			return err
		}
		// A declared probe must name a callable tool: empty is a broken
		// declaration, and a tool outside the tools: allowlist would have the
		// probe exercising a call the runtime itself never exposes — both fail
		// loud at load rather than silently at test time.
		if s.Probe != nil {
			probeTool := strings.TrimSpace(s.Probe.Tool)
			if probeTool == "" {
				return fmt.Errorf("mcp_servers[%q]: probe.tool is required", s.Name)
			}
			if len(s.Tools) > 0 && !slices.Contains(s.Tools, probeTool) {
				return fmt.Errorf("mcp_servers[%q]: probe.tool %q is not in the server's tools allowlist", s.Name, probeTool)
			}
		}
	}
	if err := b.validateHTTPTools(seen); err != nil {
		return err
	}
	if err := b.validateA2APeers(seen); err != nil {
		return err
	}
	if err := b.validatePersonas(); err != nil {
		return err
	}
	if err := b.validateWebhookTriggers(); err != nil {
		return err
	}
	if err := b.validateRemoteMCPCatalog(); err != nil {
		return err
	}
	if err := b.validateProviders(); err != nil {
		return err
	}
	if err := validatePricing(b.PricingConfig); err != nil {
		return err
	}
	if err := validateHooks(b.HooksConfig); err != nil {
		return err
	}
	return nil
}

// validateAccountSuffixBases rejects a stdio server whose account-suffix base
// vars contain one var that is an underscore-boundary prefix of another
// (#1124). The base set is every key of the server's env map (each is probed
// as <VAR>_<ACCOUNT> by creds.ApplyClientSuffix on a named-account spawn) plus
// account_vars (scanned by creds.AccountsFor to derive the account catalog).
// That convention is purely lexical, so declaring both FOO and FOO_BAR means:
//
//   - account "bar" silently overlays FOO with FOO_BAR's value — a DIFFERENT
//     credential injected under the wrong key; and
//   - FOO_BAR's tail surfaces as a phantom "bar" account of FOO in the
//     account catalog.
//
// Both are wrong-credential hazards, so the load fails with instructions to
// rename rather than leaving the ambiguity to spawn time. Matching is
// case-insensitive, mirroring creds.AccountsFor. Scope is per server — the
// set one ApplyClientSuffix/AccountsFor call actually operates over; http
// servers reject account variants outright and are skipped.
func validateAccountSuffixBases(s *ServerDef) error {
	if s.Type != "stdio" {
		return nil
	}
	bases := make(map[string]string, len(s.Env)+len(s.AccountVars)) // UPPER(name) → declared spelling
	names := make([]string, 0, len(s.Env)+len(s.AccountVars))
	collect := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		upper := strings.ToUpper(name)
		if _, dup := bases[upper]; dup {
			return
		}
		bases[upper] = name
		names = append(names, upper)
	}
	for name := range s.Env {
		collect(name)
	}
	for _, name := range s.AccountVars {
		collect(name)
	}
	slices.Sort(names) // deterministic error when several pairs conflict
	for _, upper := range names {
		for i := 0; i < len(upper); i++ {
			if upper[i] != '_' {
				continue
			}
			prefix, ok := bases[upper[:i]]
			if !ok {
				continue
			}
			return fmt.Errorf(
				"mcp_servers[%q]: env var %q is an underscore-prefix of sibling var %q — the account-suffix convention (<VAR>_<ACCOUNT>) would inject %s's value over %s for account %q and list %q as a credential account of %s; rename one of the vars so neither is a prefix of the other",
				s.Name, prefix, bases[upper], bases[upper], prefix, strings.ToLower(upper[i+1:]), strings.ToLower(upper[i+1:]), prefix)
		}
	}
	return nil
}

// webhookSlugShape bounds a manifest webhook trigger slug to the same URL-safe
// shape the orchestrator's task-trigger CLI enforces, so the two inbound-webhook
// surfaces share one slug grammar.
var webhookSlugShape = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,127}$`)

// validateWebhookTriggers fails the load on a malformed webhook_triggers[] entry
// (#268): a missing/duplicate/out-of-shape slug, a missing notify_user, or a
// missing authentication method. Like the rest of validate this is fail-loud at
// startup — a trigger with no secret env configured would otherwise silently
// reject every inbound call (fail-closed), and a trigger with no notify_user
// would have nowhere to create its conversation. An empty prompt_template is
// allowed (the raw payload still reaches the model via the default rendering).
func (b *Bundle) validateWebhookTriggers() error {
	seen := map[string]bool{}
	for i := range b.WebhookTriggers {
		t := &b.WebhookTriggers[i]
		slug := strings.TrimSpace(t.Slug)
		if slug == "" {
			return fmt.Errorf("webhook_triggers[%d]: slug is required", i)
		}
		if !webhookSlugShape.MatchString(slug) {
			return fmt.Errorf("webhook_triggers[%q]: slug must match %s", slug, webhookSlugShape.String())
		}
		if seen[slug] {
			return fmt.Errorf("webhook_triggers: duplicate slug %q", slug)
		}
		seen[slug] = true
		if strings.TrimSpace(t.NotifyUser) == "" {
			return fmt.Errorf("webhook_triggers[%q]: notify_user is required", slug)
		}
		hasHMAC := strings.TrimSpace(t.HMACSecretEnv) != ""
		hasToken := strings.TrimSpace(t.TokenSecretEnv) != ""
		if !hasHMAC && !hasToken {
			return fmt.Errorf("webhook_triggers[%q]: an authentication method is required (set hmac_secret_env or token_secret_env)", slug)
		}
		if hasHMAC && hasToken {
			return fmt.Errorf("webhook_triggers[%q]: set only one of hmac_secret_env or token_secret_env", slug)
		}
	}
	return nil
}

// validateRemoteMCPCatalog fails the load on a malformed remote_mcp_catalog[]
// entry (#538): a blank/duplicate name, a missing display_name/description
// (the UI renders these — a blank card is a curation bug), a non-https URL
// (the endpoint receives conversation-derived tool traffic; plaintext is never
// acceptable), or a name colliding with a bundled mcp_servers entry (the two
// classes render side by side and must stay distinguishable). Fail-loud at
// startup, like the rest of validate.
func (b *Bundle) validateRemoteMCPCatalog() error {
	seen := map[string]bool{}
	for i := range b.RemoteMCPCatalog {
		e := &b.RemoteMCPCatalog[i]
		name := strings.TrimSpace(e.Name)
		if name == "" {
			return fmt.Errorf("remote_mcp_catalog[%d]: name is required", i)
		}
		if seen[name] {
			return fmt.Errorf("remote_mcp_catalog: duplicate name %q", name)
		}
		seen[name] = true
		for _, s := range b.MCPCatalog {
			if s.Name == name {
				return fmt.Errorf("remote_mcp_catalog[%q]: name collides with bundled mcp_servers entry %q", name, name)
			}
		}
		if strings.TrimSpace(e.DisplayName) == "" {
			return fmt.Errorf("remote_mcp_catalog[%q]: display_name is required", name)
		}
		if strings.TrimSpace(e.Description) == "" {
			return fmt.Errorf("remote_mcp_catalog[%q]: description is required", name)
		}
		u := strings.TrimSpace(e.URL)
		if u == "" {
			return fmt.Errorf("remote_mcp_catalog[%q]: url is required", name)
		}
		if !strings.HasPrefix(u, "https://") {
			return fmt.Errorf("remote_mcp_catalog[%q]: url must be https:// (got %q)", name, e.URL)
		}
		if err := validateRemoteMCPEntryMeta(e); err != nil {
			return err
		}
	}
	return nil
}

// remoteMCPProvenances is the closed set of hosting-operator trust tiers a
// catalog entry may declare; see RemoteMCPCatalogEntry.Provenance.
var remoteMCPProvenances = map[string]bool{"official": true, "third_party": true, "community": true}

// remoteMCPAuths is the closed set of connection-hint values; see
// RemoteMCPCatalogEntry.Auth.
var remoteMCPAuths = map[string]bool{"oauth": true, "api_key": true, "open": true, "tenant": true}

// remoteMCPHeaderShape bounds api_key_header to an HTTP header-name token, so a
// catalog entry can never smuggle whitespace/CRLF into the per-user connect
// request that later replays it.
var remoteMCPHeaderShape = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// remoteMCPCategoryShape bounds a category slug to lowercase kebab-case so the
// UI's grouping/filter keys stay uniform. The set is open (a bundle may invent
// its own grouping) but the shape is not — "CRM Sales" vs "crm-sales" silently
// splitting one group into two is exactly the typo class validate exists for.
var remoteMCPCategoryShape = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// validateRemoteMCPEntryMeta fails the load on malformed directory metadata
// (category/tags/provenance/auth/repo_url). Absent provenance normalizes to
// "official" — a legacy shim for pre-existing bundle manifests, whose entries
// were all vendor-official; the built-in catalog's own test requires it
// explicitly so a new entry can never inherit trust by omission.
func validateRemoteMCPEntryMeta(e *RemoteMCPCatalogEntry) error {
	name := strings.TrimSpace(e.Name)
	if e.Provenance == "" {
		e.Provenance = "official"
	}
	if !remoteMCPProvenances[e.Provenance] {
		return fmt.Errorf("remote_mcp_catalog[%q]: unknown provenance %q (want official|third_party|community)", name, e.Provenance)
	}
	if e.Auth != "" && !remoteMCPAuths[e.Auth] {
		return fmt.Errorf("remote_mcp_catalog[%q]: unknown auth %q (want oauth|api_key|open|tenant)", name, e.Auth)
	}
	if e.Category != "" && !remoteMCPCategoryShape.MatchString(e.Category) {
		return fmt.Errorf("remote_mcp_catalog[%q]: category %q must be lowercase kebab-case", name, e.Category)
	}
	if r := strings.TrimSpace(e.RepoURL); r != "" && !strings.HasPrefix(r, "https://") {
		return fmt.Errorf("remote_mcp_catalog[%q]: repo_url must be https:// (got %q)", name, e.RepoURL)
	}
	if u := strings.TrimSpace(e.SetupURL); u != "" && !strings.HasPrefix(u, "https://") {
		return fmt.Errorf("remote_mcp_catalog[%q]: setup_url must be https:// (got %q)", name, e.SetupURL)
	}
	if h := strings.TrimSpace(e.APIKeyHeader); h != "" {
		if e.Auth != "api_key" {
			return fmt.Errorf("remote_mcp_catalog[%q]: api_key_header is only meaningful with auth: api_key", name)
		}
		if !remoteMCPHeaderShape.MatchString(h) {
			return fmt.Errorf("remote_mcp_catalog[%q]: api_key_header %q is not a valid header name", name, e.APIKeyHeader)
		}
	}
	if q := strings.TrimSpace(e.APIKeyQuery); q != "" {
		if e.Auth != "api_key" {
			return fmt.Errorf("remote_mcp_catalog[%q]: api_key_query is only meaningful with auth: api_key", name)
		}
		if strings.TrimSpace(e.APIKeyHeader) != "" {
			return fmt.Errorf("remote_mcp_catalog[%q]: api_key_header and api_key_query are mutually exclusive", name)
		}
		if !remoteMCPHeaderShape.MatchString(q) {
			return fmt.Errorf("remote_mcp_catalog[%q]: api_key_query %q is not a valid query-parameter name", name, e.APIKeyQuery)
		}
	}
	if e.ClientRegistration != "" && e.ClientRegistration != "manual" {
		return fmt.Errorf("remote_mcp_catalog[%q]: unknown client_registration %q (want manual or empty)", name, e.ClientRegistration)
	}
	for _, tag := range e.Tags {
		if strings.TrimSpace(tag) == "" {
			return fmt.Errorf("remote_mcp_catalog[%q]: empty tag", name)
		}
		if tag != strings.ToLower(tag) {
			return fmt.Errorf("remote_mcp_catalog[%q]: tag %q must be lowercase", name, tag)
		}
	}
	return nil
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

// providerTypes is the set of LLM provider backends the resolver can build (#289).
var providerTypes = map[string]bool{"openrouter": true, "anthropic": true, "openai": true, "ollama": true}

// validateProviders fails the load on a malformed providers[] entry (#289): a
// blank/duplicate name, an unknown type, or a missing api_key_env for a backend
// that requires one (every type except ollama, which is local). Like the rest of
// validate this is fail-loud at startup — a typo'd provider type or a missing
// credential env would otherwise only surface as a resolver error on the first
// turn that routed to it. An empty providers block is valid (single-OpenRouter
// default).
func (b *Bundle) validateProviders() error {
	seen := map[string]bool{}
	for i := range b.Providers {
		p := &b.Providers[i]
		name := strings.TrimSpace(p.Name)
		if name == "" {
			return fmt.Errorf("providers[%d]: name is required", i)
		}
		if seen[name] {
			return fmt.Errorf("providers: duplicate provider name %q", name)
		}
		seen[name] = true
		typ := strings.TrimSpace(p.Type)
		if typ == "" {
			return fmt.Errorf("providers[%q]: type is required (openrouter|anthropic|openai|ollama)", name)
		}
		if !providerTypes[typ] {
			return fmt.Errorf("providers[%q]: unknown type %q (want openrouter|anthropic|openai|ollama)", name, typ)
		}
		if typ != "ollama" && strings.TrimSpace(p.APIKeyEnv) == "" {
			return fmt.Errorf("providers[%q]: api_key_env is required for a %q provider", name, typ)
		}
		if p.ContextWindowTokens < 0 {
			return fmt.Errorf("providers[%q]: context_window_tokens must be positive when set", name)
		}
	}
	chainSeen := map[string]bool{}
	for i, raw := range b.FallbackProviders {
		name := strings.TrimSpace(raw)
		if name == "" {
			return fmt.Errorf("fallback_providers[%d]: provider name is required", i)
		}
		if !seen[name] {
			return fmt.Errorf("fallback_providers[%d]: unknown provider %q", i, name)
		}
		if chainSeen[name] {
			return fmt.Errorf("fallback_providers: duplicate provider %q", name)
		}
		chainSeen[name] = true
		b.FallbackProviders[i] = name
	}
	return nil
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

// validateServerTLS rejects a per-server TLS block that is malformed OR could
// never take effect (#280). A non-empty block is only meaningful on an https
// http server: http.Transport applies a TLSClientConfig solely to https
// requests, and stdio has no TLS transport at all — so a block on a plaintext
// http:// url or a stdio server would otherwise be SILENTLY ignored, leaving the
// operator believing a connection is pinned/mTLS-protected when it is not. We
// fail the load loudly instead. An absent or empty block (toMCP == nil) is a
// no-op and is allowed on any server.
//
// mTLS needs BOTH client_cert and client_key, and a public-key pin must be a
// well-formed SHA-256. File existence/parse is deliberately deferred to connect
// time (cert/key/CA files may be provisioned on the box separately from the
// bundle), where mcp.TLSOptions.build surfaces a clear, named error.
func validateServerTLS(name, serverType, url string, d *ServerTLSDef) error {
	if d.toMCP() == nil {
		return nil // absent or all-empty → nothing to apply or validate
	}
	if serverType != "http" {
		return fmt.Errorf("mcp_servers[%q]: tls is only valid on an http server (got type %q)", name, serverType)
	}
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(url)), "https://") {
		return fmt.Errorf("mcp_servers[%q]: tls hardening requires an https:// url (got %q) — it cannot apply to a plaintext connection", name, url)
	}
	cert := strings.TrimSpace(d.ClientCert)
	key := strings.TrimSpace(d.ClientKey)
	if (cert == "") != (key == "") {
		return fmt.Errorf("mcp_servers[%q]: tls mTLS requires both client_cert and client_key", name)
	}
	if p := strings.TrimSpace(d.PinnedSHA256); p != "" {
		if _, err := mcp.NormalizePinSHA256(p); err != nil {
			return fmt.Errorf("mcp_servers[%q]: tls %w", name, err)
		}
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
		if name == HTTPToolServerName || name == A2AToolServerName {
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

// validateA2APeers is the fail-fast Load-time check for a2a_peers[] (#1368):
// a valid MCP-safe name, an http(s) rpc_url with a host (never file:// or a
// URL with userinfo — the credential goes in headers, resolved host-side), a
// non-empty bundle-authored description (the only peer text the model sees),
// and no collision in the shared tool namespace — the peer name AND its four
// derived tool names are reserved there, so an http_tool or mcp_server cannot
// shadow mcp__a2a_<peer>_send.
func (b *Bundle) validateA2APeers(seen map[string]bool) error {
	for i := range b.A2APeers {
		p := &b.A2APeers[i]
		name := strings.TrimSpace(p.Name)
		if name == "" {
			return fmt.Errorf("a2a_peers[%d]: name is required", i)
		}
		if !a2aPeerNamePattern.MatchString(name) {
			return fmt.Errorf("a2a_peers[%q]: name must match %s (lowercase letters, digits, underscore; at most 32 chars)", name, a2aPeerNamePattern.String())
		}
		if name == HTTPToolServerName || name == A2AToolServerName {
			return fmt.Errorf("a2a_peers[%q]: name is reserved", name)
		}
		if seen[name] {
			return fmt.Errorf("a2a_peers: duplicate name %q (collides with an mcp_servers entry, an http_tool, or another peer)", name)
		}
		seen[name] = true
		for _, tool := range mcp.A2AToolNames(name) {
			if seen[tool] {
				return fmt.Errorf("a2a_peers[%q]: derived tool name %q collides with an mcp_servers entry or an http_tool", name, tool)
			}
			seen[tool] = true
		}
		p.Name = name

		rawURL := strings.TrimSpace(p.RPCURL)
		if rawURL == "" {
			return fmt.Errorf("a2a_peers[%q]: rpc_url is required", name)
		}
		u, err := url.Parse(rawURL)
		if err != nil {
			return fmt.Errorf("a2a_peers[%q]: rpc_url does not parse: %w", name, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("a2a_peers[%q]: rpc_url scheme must be http or https, got %q", name, u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("a2a_peers[%q]: rpc_url has no host", name)
		}
		if u.User != nil {
			return fmt.Errorf("a2a_peers[%q]: rpc_url must not carry userinfo; put the credential in headers as a ${ENV_VAR} reference", name)
		}
		p.RPCURL = rawURL
		if strings.TrimSpace(p.Description) == "" {
			return fmt.Errorf("a2a_peers[%q]: description is required (it is the only text about the peer the model sees)", name)
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
			sc.Dir = bundleDir
			if s.dir != "" {
				// An Agent Plugin server launches in its plugin root (or declared cwd).
				sc.Dir = s.dir
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
// across enable gates, account vars, identity_env, webhook/provider key names,
// and every ${VAR} interpolation site anywhere in the manifest (env/header
// values, url:, command:/args:, sandbox.image, providers, … — see
// manifestEnvVarNames, #1123). cmd/fleet passes these to
// config.RegisterAllowedEnvVars so the bundle's connector credentials survive
// the .env-file load while fleet's static allowlist stays client-agnostic.
func (b *Bundle) EnvVarNames() []string {
	seen := map[string]bool{}
	var out []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" || reservedRuntimeVar(name) || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	b.literalEnvVarNames(add)
	for i := range b.MCPCatalog {
		s := &b.MCPCatalog[i]
		for _, v := range s.Env {
			for _, name := range sourceEnvRefs(v) {
				add(name)
			}
		}
		for _, v := range s.Headers {
			for _, name := range sourceEnvRefs(v) {
				add(name)
			}
		}
	}
	// Inline http_tools' header secrets must survive the .env-file allowlist too:
	// they are resolved host-side at call time exactly like an MCP server's headers.
	for i := range b.HTTPTools {
		for _, v := range b.HTTPTools[i].Headers {
			for _, name := range sourceEnvRefs(v) {
				add(name)
			}
		}
	}
	// And a2a peers' header credentials (#1368), same seam.
	for i := range b.A2APeers {
		for _, v := range b.A2APeers[i].Headers {
			for _, name := range sourceEnvRefs(v) {
				add(name)
			}
		}
	}
	// Every ${VAR} reference anywhere else in the manifest (#1123) — url:,
	// command:/args:, sandbox.image, providers[].base_url, … interpolate at
	// load against the process env, so an env-file value for them must survive
	// the allowlist exactly like an env/header reference. Captured from the
	// raw manifest at Load (an exported value is substituted out of the parsed
	// fields, but its NAME must still be admitted).
	for _, name := range b.manifestEnvVarNames {
		add(name)
	}
	return out
}

// literalEnvVarNames feeds add() every env-var name the manifest names
// LITERALLY rather than via ${...} interpolation. Shared by EnvVarNames and
// EnvVarNamesDefaultOnly so the two views can never drift.
func (b *Bundle) literalEnvVarNames(add func(string)) {
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
		// identity_env keys must survive the .env allowlist: the variant guard
		// reads their <VAR>_<ACCOUNT> forms from the process env (suffix
		// admission requires the BASE name to be registered).
		for _, v := range s.IdentityEnv {
			add(v)
		}
	}
	// Webhook trigger signing secrets (#268) are named directly (not ${VAR}
	// references) and are read host-side from the process env at request time, so
	// their env-var names must survive the .env-file allowlist exactly like an MCP
	// connector credential.
	for i := range b.WebhookTriggers {
		add(b.WebhookTriggers[i].HMACSecretEnv)
		add(b.WebhookTriggers[i].TokenSecretEnv)
	}
	// LLM provider API-key env vars (#289) are named directly (not ${VAR}
	// references) and read host-side at boot, so their names must survive the
	// .env-file allowlist exactly like an MCP connector credential.
	for i := range b.Providers {
		add(b.Providers[i].APIKeyEnv)
	}
}

// EnvVarNamesDefaultOnly returns the subset of EnvVarNames whose EVERY
// manifest appearance is a default-carrying ${VAR:-...} reference — config
// knobs with a manifest fallback (e.g. the generic bundle's
// `image: "${FLEET_SANDBOX_IMAGE:-}"`), never a credential something waits
// on. A name that is also referenced bare or ${VAR:?}-required anywhere, or
// named literally (enable gates, account_vars, identity_env, webhook
// secrets, provider keys), is excluded. `fleet validate-config` uses this to
// report an absent default-carrying name as "manifest defaults in effect"
// instead of a missing credential (#1123 widened EnvVarNames to every
// interpolated field, which would otherwise warn forever on a pristine
// install). Names only, never values.
func (b *Bundle) EnvVarNamesDefaultOnly() []string {
	literal := map[string]bool{}
	b.literalEnvVarNames(func(name string) {
		if name = strings.TrimSpace(name); name != "" {
			literal[name] = true
		}
	})
	out := make([]string, 0, len(b.manifestDefaultOnlyEnvVarNames))
	for _, name := range b.manifestDefaultOnlyEnvVarNames {
		if !literal[name] {
			out = append(out, name)
		}
	}
	return out
}

// ConnectorEnvVarNames returns the base process-env names referenced by bundle
// MCP servers and inline HTTP tools. The inventory is captured from the raw
// manifest, before interpolation can replace an already-exported ${VAR} token
// with its value. Values are never retained here.
//
// This is narrower than EnvVarNames: provider keys and webhook signing secrets
// remain parent-owned and are deliberately excluded. Account-suffixed variants
// are represented by every stdio env key that ApplyClientSuffix may probe; use
// ConnectorEnvironmentKeys to expand those bases against a process environment.
func (b *Bundle) ConnectorEnvVarNames() []string {
	return append([]string(nil), b.connectorEnvVarNames...)
}

// ConnectorAccountEnvVarNames returns the stdio env keys whose
// <KEY>_<ACCOUNT> process-env variants may be consumed by account binding.
// These names are public identifiers only; values are never retained here.
func (b *Bundle) ConnectorAccountEnvVarNames() []string {
	return append([]string(nil), b.connectorAccountVarNames...)
}

// ConnectorEnvironmentKeys returns the exact keys in environ that belong to
// bundle connectors: every declared base name plus every <stdio-env-key>_<seat>
// variant recognized by the account suffix convention. Entries and returned
// values contain names only, never secret values. Matching account variants is
// case-insensitive, mirroring creds.AccountsFor.
func (b *Bundle) ConnectorEnvironmentKeys(environ []string) []string {
	bases := make(map[string]bool, len(b.connectorEnvVarNames))
	for _, name := range b.connectorEnvVarNames {
		bases[strings.ToUpper(name)] = true
	}
	accountBases := make([]string, 0, len(b.connectorAccountVarNames))
	for _, name := range b.connectorAccountVarNames {
		accountBases = append(accountBases, strings.ToUpper(name)+"_")
	}
	seen := map[string]bool{}
	var out []string
	for _, entry := range environ {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		upper := strings.ToUpper(key)
		matched := bases[upper]
		if !matched {
			for _, prefix := range accountBases {
				if strings.HasPrefix(upper, prefix) && len(upper) > len(prefix) {
					matched = true
					break
				}
			}
		}
		if matched && !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
}

// ScrubConnectorRuntimeDefinitions removes the interpolated connector sections
// from an already loaded bundle. Call only after all public metadata has been
// extracted and the credential-owning child has inherited the environment.
// Branding, providers, webhook triggers, remote directory entries, policies,
// templates, and content paths remain intact for parent-side consumers.
func (b *Bundle) ScrubConnectorRuntimeDefinitions() {
	if b == nil {
		return
	}
	b.scrubbedAlwaysOnServers = append([]AlwaysOnServer{}, b.AlwaysOnServers()...)
	for i := range b.MCPCatalog {
		b.MCPCatalog[i] = ServerDef{}
	}
	for i := range b.HTTPTools {
		b.HTTPTools[i] = HTTPToolDef{}
	}
	for i := range b.A2APeers {
		b.A2APeers[i] = A2APeerDef{}
	}
	b.MCPCatalog = nil
	b.HTTPTools = nil
	b.A2APeers = nil
}

func connectorEnvInventory(servers []ServerDef, httpTools []HTTPToolDef, a2aPeers []A2APeerDef) ([]string, []string) {
	seen := map[string]bool{}
	accountSeen := map[string]bool{}
	var names, accountNames []string
	add := func(dst *[]string, index map[string]bool, name string) {
		name = strings.TrimSpace(name)
		if name == "" || reservedRuntimeVar(name) || index[name] {
			return
		}
		index[name] = true
		*dst = append(*dst, name)
	}
	addRef := func(value string) {
		for _, name := range sourceEnvRefs(value) {
			add(&names, seen, name)
		}
	}
	for i := range servers {
		s := &servers[i]
		for _, name := range s.EnabledEnv {
			add(&names, seen, name)
		}
		for _, group := range s.EnabledGroups {
			for _, name := range group {
				add(&names, seen, name)
			}
		}
		for _, name := range s.AccountVars {
			add(&names, seen, name)
			add(&accountNames, accountSeen, name)
		}
		for _, name := range s.IdentityEnv {
			add(&names, seen, name)
		}
		// ApplyClientSuffix probes <ENV_KEY>_<ACCOUNT> for every stdio env key,
		// not only the smaller account_vars discovery list. Retain those keys as
		// account bases so the parent can scrub identity/config overrides too.
		for name := range s.Env {
			add(&names, seen, name)
			add(&accountNames, accountSeen, name)
		}
		walkSourceStrings(reflect.ValueOf(*s), addRef)
	}
	for i := range httpTools {
		walkSourceStrings(reflect.ValueOf(httpTools[i]), addRef)
	}
	for i := range a2aPeers {
		walkSourceStrings(reflect.ValueOf(a2aPeers[i]), addRef)
	}
	slices.Sort(names)
	slices.Sort(accountNames)
	return names, accountNames
}

// walkSourceStrings visits every string in the raw connector definitions,
// including nested probe args/input schemas and map keys. The manifest
// interpolator operates over the entire YAML section, so an inventory limited
// to today's documented header/env fields could silently miss a future
// interpolated connector field.
func walkSourceStrings(value reflect.Value, visit func(string)) {
	if !value.IsValid() {
		return
	}
	switch value.Kind() {
	case reflect.Interface, reflect.Pointer:
		if !value.IsNil() {
			walkSourceStrings(value.Elem(), visit)
		}
	case reflect.String:
		visit(value.String())
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			walkSourceStrings(value.Field(i), visit)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			walkSourceStrings(value.Index(i), visit)
		}
	case reflect.Map:
		iter := value.MapRange()
		for iter.Next() {
			walkSourceStrings(iter.Key(), visit)
			walkSourceStrings(iter.Value(), visit)
		}
	default:
		// Scalar non-strings and executable/channel/unsafe kinds carry no
		// manifest text and therefore cannot contain an env reference.
	}
}

// sourceEnvRefs extracts source variable names from ${VAR}, ${VAR:-default},
// and ${VAR:?message} without consulting the environment. It shares the
// scanner with the manifest-wide reference inventory (envRefsIn), so it
// mirrors the interpolator's brace/escape rules and expandExpr's operator
// parse exactly.
func sourceEnvRefs(value string) []string {
	refs := envRefsIn(value)
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		if ref.name != "" {
			out = append(out, ref.name)
		}
	}
	return out
}

// WebhookSecretEnvNames returns the env-var names holding the bundle's webhook
// trigger signing secrets (GitHub-style HMAC + Slack). cmd/fleet passes these
// to config.RegisterNonReloadableSecretEnvVars so a config hot-reload pins them
// to their boot values (#584): they are per-request AUTH secrets, read via
// os.Getenv at verification time, and a drifted .env-file copy must be reported
// as Skipped rather than silently rotating a live secret mid-run.
func (b *Bundle) WebhookSecretEnvNames() []string {
	seen := map[string]bool{}
	var out []string
	for i := range b.WebhookTriggers {
		for _, name := range []string{b.WebhookTriggers[i].HMACSecretEnv, b.WebhookTriggers[i].TokenSecretEnv} {
			name = strings.TrimSpace(name)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
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
		resolved := interpolate(v)
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

// interpolateManifest performs a pre-unmarshal pass over the raw manifest bytes,
// expanding shell-style env references so the bundle can carry "env-or-default"
// config semantics (the getEnvOrDefault("VAR","literal") behavior the legacy
// internal/config had). It supports three POSIX-style forms:
//
//	${VAR}            Bare reference. If VAR is SET, substitute its value. If VAR
//	                  is UNSET, the token is LEFT INTACT (deferred): per-MCP-server
//	                  env/header values are resolved lazily at spawn time against
//	                  the live process env, where an unset credential is
//	                  legitimate (the server gates off or optional_env drops the
//	                  key). The pre-unmarshal pass therefore must NOT hard-fail on
//	                  an unset bare ${VAR} — that would make loading any bundle
//	                  impossible unless every connector secret were exported up
//	                  front. Load DOES fail afterwards when a deferred token sits
//	                  in a field OUTSIDE those lazily-resolved maps (#1123):
//	                  nothing ever re-reads a url:/command:/sandbox.image, so the
//	                  literal token could never resolve. A value that MUST be
//	                  present at load uses the explicit ${VAR:?message} form.
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
//
// Connector env/header values (mcp_servers[].env/headers, http_tools[].headers)
// are special: this pass VALIDATES them — an unset ${VAR:?...} or a non-bare
// reserved token still fails the load — but its substitutions there are
// discarded. Load re-sources those maps from the raw manifest so they resolve
// against the live process env at catalog-build time (see
// interpolate/resolveEnvMap): an unset credential is legitimate there, the
// broker child resolves against its own environment, and keeping this pass's
// output would consume the $${ escape one pass early. Load folds the
// FLEET_ENV_FILE env file into the process env BEFORE running this pass
// (#1123), so every other field — and ${VAR:?...} validation — sees the same
// environment the runtime uses.
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

// reservedWorkspaceVar is the RESERVED spawn-time token name a bundle may use
// in an mcp_servers[].env value: ${FLEET_WORKSPACE} is substituted by the MCP
// spawn paths with a fleet-provided writable directory (per-run for a run's
// dedicated client, a stable per-deployment dir for shared spawns — see
// internal/agentcore's WorkspaceEnvToken, the consuming side of this
// contract; the name is inlined here to keep clientconfig dependency-free of
// agentcore). Both interpolation passes leave the bare token INTACT — it is
// never resolved from the process env (even if an operator exports a var of
// that name) and never blanked. Only the bare ${FLEET_WORKSPACE} spelling is
// reserved; any colon-suffixed (:-/:?) spelling fails the load (expandExpr).
const (
	reservedWorkspaceVar = "FLEET_WORKSPACE"
	reservedTaskIDVar    = "FLEET_TASK_ID"
)

func reservedRuntimeVar(name string) bool {
	return name == reservedWorkspaceVar || name == reservedTaskIDVar
}

// expandExpr resolves the body of a single ${...} expression (the text between
// the braces) into a replacement, implementing the ${VAR}, ${VAR:-default} and
// ${VAR:?message} forms.
func expandExpr(expr, manifestPath string) (expandResult, error) {
	// Reserved spawn-time tokens admit ONLY the bare spelling. Any
	// colon-suffixed form (:-, :?, or a typo'd op) would resolve the token from
	// the process env or a literal default — silently defeating the launch-time
	// substitution — so it fails the load.
	if idx := strings.IndexByte(expr, ':'); idx >= 0 {
		if name := strings.TrimSpace(expr[:idx]); reservedRuntimeVar(name) {
			return expandResult{}, fmt.Errorf(
				"client config manifest %s: ${%s}: %s is a RESERVED runtime token that fleet substitutes at MCP launch; only the bare ${%s} spelling is supported — it is never read from the process env and takes no default (see docs/MCP-BUNDLE-ENV.md)",
				manifestPath, expr, name, name)
		}
	}
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
	if reservedRuntimeVar(name) {
		// Reserved spawn-time token: always deferred, never read from the
		// process env (an operator-exported FLEET_WORKSPACE must not hijack it).
		return expandResult{deferred: true}, nil
	}
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

// interpolate resolves the ${...} references in an MCP env/header value
// against the process env at catalog-build time — AFTER config.Load has
// applied the .env file, which is why those values keep their raw manifest
// text through Load (see the re-source step there). It is the single
// substituting pass for those fields and supports the same forms as
// interpolateManifest: "$${" emits a literal "${" without expanding, ${VAR}
// substitutes the value (empty when unset — resolveEnvMap's optional_env drop
// keys on that), ${VAR:-default} falls back to the default, and ${VAR:?msg}
// substitutes the value (an unset var cannot reach here: interpolateManifest
// already failed the load). The reserved runtime tokens (reservedRuntimeVar)
// are preserved verbatim — never env-resolved, never blanked — for the MCP
// spawn paths to substitute. An unterminated "${" is copied through verbatim.
func interpolate(v string) string {
	if !strings.Contains(v, "${") {
		return v
	}
	var sb strings.Builder
	sb.Grow(len(v))
	for i := 0; i < len(v); {
		// Escape: "$${" -> literal "${" (consume one leading '$').
		if strings.HasPrefix(v[i:], "$${") {
			sb.WriteString("${")
			i += 3
			continue
		}
		if !strings.HasPrefix(v[i:], "${") {
			sb.WriteByte(v[i])
			i++
			continue
		}
		end, ok := matchBrace(v, i+1)
		if !ok {
			sb.WriteString(v[i:])
			break
		}
		sb.WriteString(resolveSpawnExpr(v[i+2:end], v[i:end+1]))
		i = end + 1
	}
	return sb.String()
}

// resolveSpawnExpr resolves one ${...} expression at catalog-build time (expr
// is the text between the braces, token the original "${...}" spelling). It
// mirrors expandExpr with the two load/spawn differences: an unset bare ${VAR}
// resolves to "" (there is no later pass to defer to), and a reserved runtime
// token is preserved verbatim in ANY spelling rather than erroring — the load
// pass already rejected non-bare reserved forms, so this is belt-and-braces
// against a value that never went through it.
func resolveSpawnExpr(expr, token string) string {
	head := expr
	if idx := strings.IndexByte(expr, ':'); idx >= 0 {
		head = expr[:idx]
	}
	if reservedRuntimeVar(strings.TrimSpace(head)) {
		return token
	}
	if idx := strings.IndexByte(expr, ':'); idx >= 0 && idx+1 < len(expr) {
		name := expr[:idx]
		op := expr[idx+1]
		body := expr[idx+2:]
		switch op {
		case '-': // ${VAR:-default}
			if v, ok := lookupNonEmpty(name); ok {
				return v
			}
			return body
		case '?': // ${VAR:?message}
			v, _ := lookupNonEmpty(name)
			return v
		}
		// Any other ':X' is not a form we support; treat the whole expression
		// as a bare name, matching expandExpr.
	}
	v, _ := lookupNonEmpty(expr)
	return v
}
