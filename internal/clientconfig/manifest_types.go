package clientconfig

import (
	"strings"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/mcp"
)

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
	// scrubbedConnectorMeta likewise keeps the public picker metadata
	// (ConnectorMeta) once the catalog is scrubbed.
	scrubbedConnectorMeta []ConnectorMeta
	connectorMetaPinned   bool

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

	// sandboxContainerfileNamed reports whether the manifest NAMED a
	// Containerfile (sandbox.containerfile). Only a named one must exist: a
	// bundle that ships none and names none customizes nothing, and its
	// sandbox is the release's generic image (docs/RELEASE-ARTIFACTS.md).
	sandboxContainerfileNamed bool

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
	pluginProblems []string // entry-level: decided by the bundle's own files
	// pluginRootProblems are about this machine (a plugin_roots dir missing or
	// unreadable here); see pluginLoadResult for why they are kept apart.
	pluginRootProblems []string
}

// NarrowableArgConfig is one critical_tool_narrowable_args entry: the
// top-level list argument a person may narrow on the card. A mapping (not a
// bare string) so the entry can grow without a breaking change.
type NarrowableArgConfig struct {
	Arg string `yaml:"arg"`
}

// NarrowableArgNames flattens critical_tool_narrowable_args to the
// suffix → argument-name map agentcore installs (nil when none is declared).
func (p AgentPolicy) NarrowableArgNames() map[string]string {
	if len(p.CriticalToolNarrowableArgs) == 0 {
		return nil
	}
	out := make(map[string]string, len(p.CriticalToolNarrowableArgs))
	for k, v := range p.CriticalToolNarrowableArgs {
		out[k] = v.Arg
	}
	return out
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
//   - CriticalToolAliases: symmetric equivalence classes of critical suffixes
//     that are the SAME action on the same server (#1604).
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
	// CriticalToolAliases is an OPTIONAL set of equivalence classes (#1604):
	// each key and the suffixes listed under it are one critical action exposed
	// under several names — the same write with a different transport. A typed
	// confirm_audit commitment declared on any member is authorized and
	// discharged by a successful call of any other member ON THE SAME
	// server/variant, in either direction; the record binding (deal_id,
	// deal_ids, values_digest) carries over unchanged and cross-server discharge
	// stays refused. Every member must also be listed in critical_tools — agentcore
	// logs and ignores one that is not.
	//
	// It exists because the audit binds a declaration to the exact tool name
	// (#715) while the transport is chosen after the payload is built: a Pages
	// write declared inline that went out as a staged upload left the inline
	// declaration outstanding, and runs whose data was live ended as failures.
	//
	//	agent_policy:
	//	  critical_tool_aliases:
	//	    update_page_data: [update_page_data_upload]
	//	    deploy_page: [deploy_page_upload]
	CriticalToolAliases map[string][]string `yaml:"critical_tool_aliases"`
	// EmailLastTools is an OPTIONAL list of critical suffixes whose run
	// reports by ONE summary email sent as its LAST outward step — a batch of
	// record writes followed by the email listing every outcome. Once a run
	// declares or attempts one, send_email is refused while a commitment on
	// one is unsettled, every such call is refused after that email has gone
	// out, and an abort (confirm_audit success=false) still owes — and may
	// send — the single failure-summary email. Members must be in
	// critical_tools and must not be email tools; agentcore logs and ignores
	// any that are not.
	EmailLastTools []string `yaml:"email_last_tools"`
	// SettleableCreateTools is an OPTIONAL list of record-CREATE suffixes
	// whose unbound commitment is SETTLED by a definitive failure (the tool
	// ran and reported success=false with no ambiguous-outcome marker): it no
	// longer holds the summary email back, and no longer blocks finish once
	// that email has gone out. Each member is implicitly an email_last_tools
	// member. Typed critical_actions entries for these tools may carry
	// deal_name to bind each unit to one record.
	//
	//	agent_policy:
	//	  email_last_tools: [update_deal, merge_geo]
	//	  settleable_create_tools: [execute_deal_from_prompt_inputs, create_deal]
	SettleableCreateTools []string `yaml:"settleable_create_tools"`
	// CriticalToolNoSessionApproval is an OPTIONAL list of critical suffixes
	// (same suffix matching as critical_tools) whose approval cards take one
	// decision per call: the web hides "apply my choice to all … calls in this
	// chat", the approve POST refuses a non-"once" scope, and staging ignores
	// any session policy for them. Members must be in critical_tools.
	//
	//	agent_policy:
	//	  critical_tool_no_session_approval: [create_deal, update_deal]
	CriticalToolNoSessionApproval []string `yaml:"critical_tool_no_session_approval"`
	// CriticalToolCardDescribers is an OPTIONAL map from a critical suffix to
	// a read-only "describer" tool suffix on the same server. When a matching
	// call is staged for approval, fleet calls the describer with the same
	// arguments and renders its structured JSON as a readable card (title,
	// records, before → after, flags), falling back to the generic arguments
	// card on any failure. The describer must be in parallel_safe_tools and
	// must not be critical. See docs/APPROVAL-CARD-DESCRIBERS.md.
	//
	//	agent_policy:
	//	  critical_tool_card_describers:
	//	    update_deal: describe_deal_update
	CriticalToolCardDescribers map[string]string `yaml:"critical_tool_card_describers"`
	// CriticalToolResume is an OPTIONAL list of critical suffixes (same
	// suffix matching as critical_tools) whose approval cards start one new
	// agent turn in the conversation once they are settled (approved with its
	// result recorded, declined, or timed out), so the agent can verify the
	// outcome and carry on without the user typing. Members must be in
	// critical_tools. Absent = no turn ever starts on its own, as before. See
	// docs/RESUME-AFTER-APPROVAL.md.
	//
	//	agent_policy:
	//	  critical_tool_resume: [create_deal, update_deal]
	CriticalToolResume []string `yaml:"critical_tool_resume"`
	// CriticalToolResumeMaxPerHour caps the automatic resumes one conversation
	// may start in a rolling hour (1-60; absent or 0 = 10).
	CriticalToolResumeMaxPerHour int `yaml:"critical_tool_resume_max_per_hour"`
	// CriticalToolGroupApproval is an OPTIONAL list of critical suffixes (same
	// suffix matching as critical_tools) whose approval cards, when one turn
	// stages two or more of them, render as one grouped card with a checkbox
	// per call: Approve all (the checked calls; the unchecked ones are
	// declined), One at a time, Cancel all. Each call still resolves through
	// its own approval row. Members must be in critical_tools and not notify
	// mode (docs/GROUPED-APPROVALS.md).
	//
	//	agent_policy:
	//	  critical_tool_group_approval: [execute_plan]
	CriticalToolGroupApproval []string `yaml:"critical_tool_group_approval"`
	// CriticalToolProgress is an OPTIONAL list of critical suffixes (same
	// suffix matching as critical_tools) whose approved calls ask their MCP
	// server for progress notifications, show them on the executing card
	// ("12 of 24 · message"), and send the conversation owner a browser push
	// when the call finishes. Members must be in critical_tools and not
	// notify mode (docs/APPROVAL-PROGRESS.md).
	//
	//	agent_policy:
	//	  critical_tool_progress: [execute_plan]
	CriticalToolProgress []string `yaml:"critical_tool_progress"`
	// CriticalToolResultCards is an OPTIONAL list of critical suffixes (same
	// suffix matching as critical_tools) whose result carries a structured
	// outcome card under the top-level `result_card` key. Fleet checks it
	// against a fixed schema and, when it passes, the settled approval card
	// (or notify record) renders it: a one-line outcome, a row per record
	// with its ids, status and link, and "Retry failed (N)". Members must be
	// in critical_tools (docs/APPROVAL-RESULT-CARDS.md).
	//
	//	agent_policy:
	//	  critical_tool_result_cards: [execute_plan]
	CriticalToolResultCards []string `yaml:"critical_tool_result_cards"`
	// CriticalToolNarrowableArgs is an OPTIONAL map from a critical suffix
	// (same suffix matching as critical_tools; the longest match wins) to the
	// ONE top-level list argument a person may narrow on its approval card:
	// the describer card's rows carry a `key` naming a staged value, the
	// person unticks rows, and fleet approves the call with that argument
	// replaced by the ticked values (a non-empty subset of the staged ones,
	// staged order, nothing else changed). The key must stage a card for an
	// MCP call (docs/APPROVAL-NARROWING.md, ADR-0089).
	//
	//	agent_policy:
	//	  critical_tool_narrowable_args:
	//	    execute_plan: {arg: deal_ids}
	CriticalToolNarrowableArgs map[string]NarrowableArgConfig `yaml:"critical_tool_narrowable_args"`
	// CriticalToolStop is an OPTIONAL list of critical suffixes (same suffix
	// matching as critical_tools) whose RUNNING approved call offers Stop:
	// fleet sends the MCP notifications/cancelled for the request and keeps
	// waiting for the server's answer, which a cooperating server gives after
	// its current item (with a stopped reason in its own result). Members
	// must be in critical_tools and not notify mode (docs/APPROVAL-STOP.md).
	//
	//	agent_policy:
	//	  critical_tool_stop: [execute_plan]
	CriticalToolStop []string `yaml:"critical_tool_stop"`
	// InteractiveHiddenTools is an OPTIONAL list of tool-name suffixes (same
	// suffix matching as critical_tools) whose MCP tools interactive chat
	// never sees or calls: not in the tool list, not findable through
	// tool_search, refused by tool_call and by a direct call, and not
	// reachable from a sub-agent a chat turn spawns. Scheduled and Operations
	// Center runs keep them. Use it for raw writes a scheduled protocol needs
	// but chat must reach through a reviewed flow (a plan with one approval
	// card) instead. Absent = chat and scheduled runs see the same tools. See
	// docs/INTERACTIVE-HIDDEN-TOOLS.md.
	//
	//	agent_policy:
	//	  interactive_hidden_tools: [ox_create_deal, ix_merge_deal_domains]
	InteractiveHiddenTools []string `yaml:"interactive_hidden_tools"`
	// UnattendedDisabledTools is an OPTIONAL list of fleet built-in names
	// (create_task, web_fetch, download_url) removed from every unattended
	// scheduled run, whatever the task's own grants say. Interactive chat is
	// unchanged. Use it for a deployment whose scheduled runs read outside
	// input (an inbox, vendor reports) and must not spawn follow-up tasks or
	// fetch arbitrary URLs on that input's word. See
	// docs/UNATTENDED-BUILTINS.md.
	//
	//	agent_policy:
	//	  unattended_disabled_tools: [create_task, web_fetch]
	UnattendedDisabledTools []string `yaml:"unattended_disabled_tools"`
	// BatchSecondsPerDeal is DERIVED, never read from agent_policy: Bundle.AgentPolicy
	// collects each mcp_servers[].batch_seconds_per_deal into it, keyed by
	// server name, so the per-record batch budget reaches agentcore on the same
	// path as the rest of the tool-behavior policy.
	BatchSecondsPerDeal map[string]int `yaml:"-"`
	// ApprovedCallTimeoutSeconds is DERIVED the same way: Bundle.AgentPolicy
	// collects each mcp_servers[].approved_call_timeout_seconds into it, keyed
	// by server name, so the approved-card call budget reaches agentcore on the
	// same path as batch_seconds_per_deal.
	ApprovedCallTimeoutSeconds map[string]int `yaml:"-"`
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
	// HouseToolsName names the Tools picker's "house" category: the
	// connectors the deployment's own team builds (e.g. "Elcano Tools"),
	// listed first so they stand out. Empty means "<app_name> Tools".
	HouseToolsName string `yaml:"house_tools_name"`
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
	// Support configures an optional prominent support/escalation contact
	// button in the authenticated web shell (e.g. a direct line to an
	// engineering lead or support team). Nil when omitted by the bundle, in
	// which case no support button is rendered. Documented in
	// docs/BRANDING.md ("Support contact").
	Support *SupportContact `yaml:"support"`
}

// SupportContact carries the optional bundle-configured support contact button
// and dialog copy surfaced via the member-gated /client-config endpoint.
type SupportContact struct {
	Label       string `yaml:"label" json:"label"`
	MobileLabel string `yaml:"mobile_label,omitempty" json:"mobile_label,omitempty"`
	Email       string `yaml:"email" json:"email"`
	Title       string `yaml:"title,omitempty" json:"title,omitempty"`
	Message     string `yaml:"message,omitempty" json:"message,omitempty"`
	Subject     string `yaml:"subject,omitempty" json:"subject,omitempty"`
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
	// empty AND listed in OptionalEnv are dropped from the spawned env. A value
	// may also carry fleet's reserved spawn-time tokens — ${FLEET_WORKSPACE},
	// ${FLEET_WORKSPACE_ROOT}, ${FLEET_TASK_ID}, and the interactive turn's
	// signed-in user's email ${FLEET_USER_EMAIL} — which are never
	// read from the process env and are substituted (or the key dropped) per
	// spawn path; see docs/MCP-BUNDLE-ENV.md.
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

	// BatchSecondsPerDeal is this server's per-record budget, in seconds, for a
	// tool call whose input carries a non-empty deal_ids array (a server-side
	// sequential batch). Fleet gives such a call this many seconds per listed
	// record, floored at the ordinary 5-minute MCP call timeout and capped at
	// 30 minutes; 0 (absent) keeps the engine default of 20 s per record. A
	// connector whose upstream paces requests (a per-minute rate limit makes
	// every record slower) declares its own pace here instead of the engine
	// knowing it by name. It governs the server's named-account variants too
	// (<server>_<account>). See docs/BATCH-CALLS.md.
	BatchSecondsPerDeal int `yaml:"batch_seconds_per_deal"`

	// ApprovedCallTimeoutSeconds is the budget, in seconds, for ONE call of
	// this server's tools that a person approved on a chat approval card
	// (the call runs outside the agent loop, after the click). 0 (absent)
	// keeps the engine default of 60 seconds, sized for an email send. A
	// server whose approved writes are slower (a deal create that round-trips
	// a vendor API several times) declares its own budget here; declaring it
	// also opts the server into deal_ids scaling on approved calls (the
	// batch_seconds_per_deal pace per listed record, capped at 30 minutes) and
	// into the early "still running" reply for a call that outlives the
	// request. It governs the server's named-account variants too
	// (<server>_<account>). See docs/APPROVED-CALL-BUDGET.md.
	ApprovedCallTimeoutSeconds int `yaml:"approved_call_timeout_seconds"`

	// Optional marks a server users must opt into per conversation (chat's
	// Optional-server semantics). DisplayName/Description/Beta/EnabledByDefault
	// drive the settings-UI catalog rendering.
	Optional         bool   `yaml:"optional"`
	DisplayName      string `yaml:"display_name"`
	Description      string `yaml:"description"`
	Beta             bool   `yaml:"beta"`
	EnabledByDefault bool   `yaml:"enabled_by_default"`

	// Category groups the connector in the chat Tools picker: a
	// ConnectorCategories ID ("ssps", "deal-management", …), never free text.
	// Empty lands it in Misc Tools with a startup warning; an unknown ID fails
	// the load.
	Category string `yaml:"category"`

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

// MaxBatchSecondsPerDeal is the largest batch_seconds_per_deal a manifest may
// declare: the engine caps a whole deal_ids batch call at 30 minutes, so a
// single record's budget can never usefully exceed it.
const MaxBatchSecondsPerDeal = 1800

// MaxApprovedCallTimeoutSeconds is the largest approved_call_timeout_seconds a
// manifest may declare: the same 30-minute ceiling the engine puts on any one
// MCP call.
const MaxApprovedCallTimeoutSeconds = 1800

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
// credential as ${ENV_VAR} references resolved host-side when the catalog is
// built (A2APeerConfigs — the same seam and boundary as http_tools). Description is the ONLY text about the peer the
// model ever sees — a remote agent card is never fetched into the roster,
// because remote card text would be a prompt-injection channel. What the
// peer sends BACK is untrusted external content and is rendered as such.
type A2APeerDef struct {
	Name        string            `yaml:"name"`        // ^[a-z0-9_]{1,32}$
	RPCURL      string            `yaml:"rpc_url"`     // the peer's JSON-RPC endpoint, http(s) only
	Description string            `yaml:"description"` // required; bundle-authored
	Headers     map[string]string `yaml:"headers"`     // values support ${ENV_VAR}, resolved host-side at catalog-build time
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
// inference natively to Anthropic / OpenAI / a self-hosted Ollama endpoint /
// any OpenAI-compatible endpoint / the Vercel AI Gateway / Azure OpenAI / the
// Gemini API instead of exclusively through OpenRouter (for failover, data
// residency, or to avoid the OpenRouter markup). A bundle with NO providers:
// block keeps the historical single-OpenRouter behavior unchanged.
//
// SECURITY — the credential boundary mirrors the MCP catalog: APIKeyEnv names an
// env var; its VALUE is read HOST-SIDE from the process env at boot (registered
// via EnvVarNames → RegisterAllowedEnvVars, so it flows from .env) and handed to
// the model resolver. The secret never enters the manifest, the sandbox, the
// model context, or the logs.
type ProviderDef struct {
	Name                string   `yaml:"name"`                  // routing name; unique within the manifest
	Type                string   `yaml:"type"`                  // openrouter | anthropic | openai | ollama | openaicompat | vercel | azure | google | bedrock
	APIKeyEnv           string   `yaml:"api_key_env"`           // env var holding the credential (optional for ollama and openaicompat)
	BaseURL             string   `yaml:"base_url"`              // endpoint override; required for openaicompat and azure
	Region              string   `yaml:"region"`                // AWS Region of a bedrock entry (default us-east-1); no other type takes one
	Models              []string `yaml:"models"`                // slugs this provider serves; empty = catch-all
	ContextWindowTokens int      `yaml:"context_window_tokens"` // provider-local context; OpenRouter uses authoritative per-model metadata
}

// RemoteMCPURLVariant is one alternative endpoint of a directory entry (see
// RemoteMCPCatalogEntry.URLVariants): a short id (lowercase letters, digits,
// hyphens — "eu") that names the variant in fixtures and secrets
// (FLEET_CATALOG_KEY_<ENTRY>_<ID>), a label for the card, and the https URL.
type RemoteMCPURLVariant struct {
	ID    string `yaml:"id"`
	Label string `yaml:"label"`
	URL   string `yaml:"url"`
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
	// APIKeyPrefix (auth "api_key" with api_key_header only) is the scheme the
	// vendor wants in front of the key under that header, sent verbatim:
	// PagerDuty's "Token token=" yields "Authorization: Token token=<key>".
	// The user pastes only the key; the entry carries the scheme, so no hint
	// has to ask anyone to type it. Trailing spaces are significant ("Token ").
	APIKeyPrefix string `yaml:"api_key_prefix"`
	// URLVariants lists alternative hosts for the same server — a vendor's
	// regional endpoints (PagerDuty's EU service region beside the US default
	// in URL) — each with a label the card shows. The card offers the choice
	// as a select; the chosen URL is what the connection stores, and nothing
	// else about the entry (auth, header, prefix) changes with it. One entry
	// per server, not one per region.
	URLVariants []RemoteMCPURLVariant `yaml:"url_variants"`
	// ClientRegistration is "manual" when the vendor's authorization server
	// does not support RFC 7591 dynamic client registration — the user must
	// bring their own OAuth client (a GCP OAuth client, an Entra app
	// registration, a vendor-issued client ID). The directory card then
	// collects a client ID (+ optional secret) up front instead of letting the
	// add fail mid-discovery. Empty means DCR is expected to work.
	ClientRegistration string `yaml:"client_registration"`
	// ClientSecret is "required" when the vendor's authorization server accepts
	// no public clients, so a manual client ID is useless without its secret:
	// the token exchange after the consent screen fails (GitHub, measured
	// against its live endpoint while verifying #1006 — no dynamic
	// registration, and a secretless exchange answers
	// incorrect_client_credentials). The card then makes the secret field
	// mandatory instead of "optional". Only meaningful with
	// client_registration: manual. Empty keeps the field optional; the add
	// path independently refuses a secretless manual client when the AS
	// metadata rules out public clients (RFC 8414 §2 default), so this flag
	// improves the form — it is not the only guard.
	ClientSecret string `yaml:"client_secret"`
	// OAuthClientID and OAuthClientSecret let an operator supply a shared
	// registered OAuth application (e.g. a workspace Slack app or GitHub OAuth
	// app) for a client_registration: manual directory entry, either in the
	// bundle manifest (typically as ${SLACK_MCP_CLIENT_ID} /
	// ${SLACK_MCP_CLIENT_SECRET}) or via the conventional environment
	// variables FLEET_OAUTH_CLIENT_ID_<STEM> / <STEM>_MCP_CLIENT_ID and
	// FLEET_OAUTH_CLIENT_SECRET_<STEM> / <STEM>_MCP_CLIENT_SECRET. When
	// configured, GET /mcp-catalog serves the entry as a one-click OAuth add
	// and POST /remote-mcp-servers injects the operator's client credentials
	// server-side so individual users only click Add and sign in at the
	// vendor. Never serialized to JSON (`json:"-"`).
	OAuthClientID     string `yaml:"oauth_client_id" json:"-"`
	OAuthClientSecret string `yaml:"oauth_client_secret" json:"-"`
	// oauthEnvResolved marks that resolveCatalogOAuthClients has snapshotted
	// any process-env OAuth client credentials at bundle Load time, so
	// EffectiveOAuthClient never falls back to os.Getenv at request time.
	oauthEnvResolved bool `yaml:"-" json:"-"`
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
