package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ElcanoTek/fleet/internal/admission"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// appendAgentNotes concatenates the admin-curated knowledge base after the
// (interactive-only) User Memories block. Identical text in both modes. A nil /
// empty slice renders nothing (back-compat for runs with no notes).
func appendAgentNotes(sb *strings.Builder, notes []agentcore.Note) {
	if len(notes) == 0 {
		return
	}
	sb.WriteString("## Agent Notes (Admin-Maintained Knowledge Base)\n\n")
	sb.WriteString("These notes are curated by administrators and apply across ALL agents and " +
		"conversations. Treat them as authoritative shared reference. If you discover information " +
		"that contradicts or extends a note, use the `propose_note` tool — do NOT assume your change " +
		"is live.\n\n")
	for _, n := range notes {
		fmt.Fprintf(sb, "### %s (`%s`)\n\n%s\n\n", n.Title, n.Slug, strings.TrimSpace(n.Body))
	}
}

// appendNoteProposalTool describes the propose_note tool, written symmetric to
// the ## Memory Proposal Tool block. Registered in both modes.
func appendNoteProposalTool(sb *strings.Builder) {
	sb.WriteString("## Note Proposal Tool\n\n")
	sb.WriteString("You have a `propose_note` tool for the shared, admin-curated knowledge base above. " +
		"Use it when you discover durable, cross-agent information (a corrected rate limit, a new API " +
		"quirk, a reusable playbook step) that should help every future run.\n\n")
	sb.WriteString("- An existing slug proposes an EDIT to that note; a new slug proposes a NEW note.\n")
	sb.WriteString("- Provide the FULL proposed markdown body, not a diff.\n")
	sb.WriteString("- Do NOT put secrets, credentials, or transient/per-conversation details in a note.\n\n")
	sb.WriteString("Your proposal is reviewed by an admin before it goes live; it does NOT take effect " +
		"immediately. After proposing, continue your task — do NOT retry the tool or assume the note is published.\n\n")
}

// Manager owns the shared state reused across every chat turn: the MCP call and
// discovery seams, the optional-server gating sets, and the persona/protocol/
// system-prompt source files. Construct it once at server startup.
//
// This is the minimal subset needed by the interactive prompt/roster/optin
// helpers ported into the fleet monorepo. The full per-turn streaming loop
// (RunTurn) and provider/model wiring are out of scope for this package.
type Manager struct {
	config *config.Config

	mcpClient *mcp.Client
	// mcpBroker/mcpCatalog decouple governed calls and public discovery from the
	// credentialed concrete client. Production may inject the out-of-process
	// broker while the local client remains the compatibility default.
	mcpBroker  agentcore.MCPBroker
	mcpCatalog []mcp.ServerTool
	// openMCPScope creates one isolated broker-owned client for an interactive
	// turn. Nil preserves the shared-client compatibility path.
	openMCPScope MCPScopeOpener
	// reloadMCP refreshes the credential-owning child and returns only public
	// state. Nil in broker mode makes operator reload fail explicitly.
	reloadMCP   MCPReloader
	mcpAccounts map[string][]string
	allowlist   mcpAllowlist

	// mcpGatingMu guards the MCP catalog/spec-derived gating fields below
	// (allowlist, mcpToolRoster, optionalServers, enabledMCPServers,
	// optionalServerMetadata, alwaysOnServerMetadata, mcpCatalog, and mcpAccounts) so a
	// hot reload (#218, ReloadMCPServers) can swap them race-free while turns
	// read them. Boot construction and unit-test literals set the fields
	// directly (single goroutine, happens-before any turn); every RUNTIME read
	// goes through the accessors that RLock this, and Reload assigns FRESH
	// maps/slices under Lock (never mutates a published one), so a reader may
	// use a returned snapshot lock-free. Mirrors config hot-reload (#286).
	mcpGatingMu sync.RWMutex

	// mcpReloadMu serializes ReloadMCPServers (#218) across its triggers (SIGHUP
	// + the admin HTTP endpoint), so one reload's live-client reload AND gating
	// swap complete as a unit before another starts — otherwise two overlapping
	// reloads could leave the client describing one manifest and the published
	// gating another (an optional server stuck always-on).
	mcpReloadMu sync.Mutex

	// resolver loads + caches models per slug (nil in the prompt/roster unit
	// tests, which never run a turn). RunTurn / Summarize / SuggestTitle
	// resolve through it. Admin-managed providers can rebuild it at runtime
	// (SetLLMProviders), so every runtime read goes through modelResolver()
	// under resolverMu — mirroring the MCP gating hot-reload pattern above.
	resolverMu sync.RWMutex
	resolver   *agentcore.ModelResolver

	// sandboxPool is the per-turn container warm pool. RunTurn Take()s one per
	// turn; SandboxPool() exposes it for the out-of-band approved-bash path.
	sandboxPool *sandbox.Pool

	// notesProvider supplies the admin-curated knowledge base injected into the
	// system prompt every turn (nil = no notes section).
	notesProvider agentcore.NotesProvider

	// noteProposer stages agent-proposed admin-notes edits (propose_note). Wired
	// once here so EVERY interactive RunTurn inherits the same propose_note
	// guarantee — not per-entrypoint. Nil leaves propose_note reporting
	// "unavailable" (and the tool unregistered).
	noteProposer agentcore.NoteProposer

	// mcpToolRoster is the frozen list of `mcp_<server>_<tool>` names
	// that survived the initial MCP connection sweep and per-server
	// allowlists. Optional-server names are filtered from this roster per
	// turn before the system prompt is built, matching the actual tool set
	// registered for that conversation's opt-ins.
	mcpToolRoster []string
	// enabledMCPServers is the complete bundle-server set. A scoped turn binds
	// every mandatory server and only the optional servers the conversation
	// enabled; using the UI opt-in list alone would silently drop mandatory MCP.
	enabledMCPServers map[string]bool

	// optionalServers lists the names of MCP servers marked Optional in
	// their spec. Referenced by buildFantasyTools on every turn to
	// decide whether a conversation's opt-in list should expose that
	// server's tools. Populated once in New() from the specs map.
	optionalServers mcpOptionalSet

	// optionalServerMetadata is a lightweight snapshot of each Optional
	// server's description + tool-count, used by MCPServerCatalog to
	// render the settings UI without re-walking spec structures on
	// every request.
	optionalServerMetadata []OptionalServerInfo
	// alwaysOnServerMetadata is the locked informational roster used by the
	// Operations task picker. Unlike the optional roster, it includes live
	// availability so a failed mandatory connector is not presented as healthy.
	alwaysOnServerMetadata []AlwaysOnServerInfo
	// connectorMeta is the Tools picker's view of every manifest connector,
	// enabled or not: category and whether its credentials are set. Set from
	// the bundle at boot (SetConnectorMeta) and replaced by an MCP reload, so a
	// reloaded manifest's categories and needs-setup rows match the reloaded
	// catalog. Guarded by mcpGatingMu.
	connectorMeta []clientconfig.ConnectorMeta

	// Source directories for persona + protocol + skill + system-prompt files.
	// These are read on every turn because operators may edit them in place
	// without a server restart (particularly useful for prompt iteration).
	personasDir      string
	protocolsDir     string
	skillsDir        string
	systemPromptsDir string

	// chatSystemPromptFile is the interactive base-prompt filename inside
	// systemPromptsDir (e.g. "chat.md"). The scheduled path uses its own base.
	chatSystemPromptFile string

	// limiter is the SHARED process-wide admission governor (interactive +
	// scheduled). RunTurn admits each chat turn through it so the box-wide
	// concurrency cap bounds chat too, with reserved headroom keeping chat ahead
	// of background work. Nil disables admission (the one-shot cutlass harness and
	// tests, where there is nothing to contend with).
	limiter *admission.Limiter

	// health is the long-lived provider circuit breaker (#267): it accumulates
	// per-model error frequency ACROSS interactive turns, so a degraded model is
	// skipped straight to the fallback on later turns and operators can query
	// per-model health. Passed into every interactive run's Deps.
	health *agentcore.ProviderHealthRegistry

	// personaPolicies is the per-persona tool allowlist (Gate-4, #294), keyed by
	// persona basename, resolved once from the bundle manifest's personas: block.
	// A persona with no entry inherits the permissive default (sees every tool the
	// server/credential gates permit). Nil/empty when the bundle declares no
	// personas: block — defaults unchanged.
	personaPolicies map[string]agentcore.PersonaToolPermissions

	// remoteMCP resolves a user's OAuth-connected remote (hosted) MCP servers and
	// mints their bearer tokens for the per-turn overlay (#443). It is the
	// in-process compatibility path; production can instead inject the opener.
	remoteMCP RemoteMCPResolver
	// openRemoteMCPOverlay keeps the credentialed remote client behind an
	// injected scope boundary. It takes precedence over remoteMCP when set.
	openRemoteMCPOverlay RemoteMCPOverlayOpener
}

// personaPolicy returns the per-persona tool allowlist for the named persona
// (#294), or nil when the persona has no manifest entry or its policy is empty
// (both cases mean "no narrowing"). nil-safe.
func (m *Manager) personaPolicy(persona string) *agentcore.PersonaToolPermissions {
	if m == nil || len(m.personaPolicies) == 0 {
		return nil
	}
	p, ok := m.personaPolicies[persona]
	if !ok || (len(p.Allow) == 0 && len(p.Deny) == 0) {
		return nil
	}
	return &p
}

// ProviderHealth returns a snapshot of per-model circuit-breaker state for the
// interactive engine (#267). nil-safe (returns nil before New wires it).
func (m *Manager) ProviderHealth() []agentcore.ModelHealth {
	if m == nil {
		return nil
	}
	return m.health.Snapshot()
}

// MCPServerSpec describes one MCP server to connect to. Either stdio
// (spawn a subprocess) or http (POST to a remote endpoint). Type is
// implied by which fields are populated — stdio uses Command/Args/Env,
// http uses URL/Headers.
type MCPServerSpec struct {
	Enabled bool

	// Stdio fields. If Command is set, we treat this as a stdio server.
	Command string
	Args    []string
	Env     map[string]string
	// Dir is the FALLBACK cwd for the stdio subprocess; "" inherits cwd. A
	// spawn with a fleet-managed workspace launches there instead
	// (agentcore.StdioCwd), so a server's relative output path stops landing
	// in the operator's bundle checkout.
	Dir string
	// DirPinned marks Dir as a contract (an Agent Plugin's plugin root) that a
	// workspace never overrides.
	DirPinned bool
	// AccountVars are the base credential env-var names whose `<VAR>_<ACCOUNT>`
	// suffixes name this server's provisioned credential seats (creds.AccountsFor).
	AccountVars []string

	// HTTP fields. If URL is set, we treat this as an HTTP MCP server.
	URL     string
	Headers map[string]string
	// TLS hardens an HTTP server's connection (CA pinning / mTLS / public-key
	// pin) when set in the manifest (#280); nil = default system TLS.
	TLS *mcp.TLSOptions

	// ToolAllowlist — empty means "register every tool the server
	// advertises". Non-empty restricts to the listed tool names.
	ToolAllowlist []string

	// Optional marks a server the user must explicitly opt into for a
	// given conversation. When true, the server's subprocess still
	// launches at startup (so we know what tools it advertises and can
	// render a useful catalog in the settings UI), but its tools are
	// filtered OUT of each turn unless the conversation's
	// optional_mcp_servers_enabled list contains this server's name.
	//
	// When false (the default), the server behaves like today — tools
	// are registered on every turn as long as the spec is Enabled.
	Optional bool

	// DisplayName is the prettified label shown in the settings UI.
	// Empty falls back to the spec's map key (e.g. "indexexchange"),
	// which is the wire id we use everywhere else (toggle keys,
	// optional_mcp_servers_enabled). Setting DisplayName is purely
	// cosmetic — internal references still go through Name.
	DisplayName string

	// Description is a short human-readable summary shown in the
	// settings UI when the server is Optional. Ignored for non-optional
	// servers today (they have no toggle). Two lines is fine; markup
	// renders as plain text. A "Try: …" sentence helps users discover
	// what the server is good for.
	Description string

	// Beta tags the server with a "BETA" badge in the settings UI.
	// Cosmetic only — no runtime gate. Indicates the connector is
	// still flaky / under active iteration so users approach it with
	// the right expectations.
	Beta bool

	// EnabledByDefault makes an Optional server start ON for brand-new
	// conversations (the Tools picker shows it toggled on; the user can
	// still turn it off). Only meaningful when Optional is true. The
	// default lives in the catalog the frontend seeds from, so turning a
	// server off pre-chat sticks — the backend never re-adds it. Used for
	// connectors we want first-class whenever their credentials are
	// configured (e.g. gamma).
	EnabledByDefault bool

	// DataSources: manifest-declared external data identifiers this connector
	// touches, for the settings-UI inventory (display-only).
	DataSources []string
}

// fastIOEnabledForTurn reports whether the fast.io MCP server has any
// tools wired up for this turn. Drives conditional system-prompt
// content: when fast.io is off (FAST_IO_MCP_TOKEN unset on the
// server), the dedicated `## Fast.io` section, the persistence-offer
// rule, and the `protocols/fastio-mcp.md` entry are all omitted so
// the agent doesn't see guidance about tools it can't call. Fast.io
// is intentionally an OPTIONAL surface — chat-server must run cleanly
// without it for deployments that don't license a Fast.io workspace.
//
// We probe by tool-name prefix rather than by config flag so the
// check stays accurate if a future allowlist change drops every
// fast.io tool but leaves the server registered.
func (m *Manager) fastIOEnabledForTurn(enabledOptIns, hidden []string) bool {
	for _, name := range m.activeMCPToolNames(enabledOptIns, hidden) {
		if strings.HasPrefix(name, "mcp_fast_io_") {
			return true
		}
	}
	return false
}

// activeMCPToolNames returns the prefixed (`mcp_<server>_<tool>`) tool names
// the system prompt advertises to the model for this turn. It starts from the
// frozen startup roster, then filters Optional servers unless the conversation
// opted in, and drops the tools hidden selects: the turn's
// agent_policy.interactive_hidden_tools snapshot, the SAME slice RunTurn puts
// on the turn's RunConfig for the run gate (agentcore/interactive_hidden.go),
// so the prompt and the tools cannot disagree within a turn. This keeps
// prompt guidance aligned with buildFantasyTools: a section gated on a
// connector's tools (the fast.io protocol entry) is not written for tools the
// model cannot call. nil hidden = nothing hidden.
func (m *Manager) activeMCPToolNames(enabledOptIns, hidden []string) []string {
	optionalServers, roster := m.mcpRosterSnapshot()
	if len(optionalServers) == 0 && len(hidden) == 0 {
		return roster
	}
	enabled := make(map[string]bool, len(enabledOptIns))
	for _, n := range enabledOptIns {
		enabled[strings.TrimSpace(n)] = true
	}
	out := make([]string, 0, len(roster))
	for _, name := range roster {
		if optionalServer := longestOptionalServerFor(name, optionalServers); optionalServer != "" && !enabled[optionalServer] {
			continue
		}
		if agentcore.InteractiveHiddenIn(hidden, name) {
			continue
		}
		out = append(out, name)
	}
	return out
}

// longestOptionalServerFor resolves which Optional server a prefixed
// `mcp_<server>_<tool>` roster name belongs to, or "" when none matches.
// Server names can extend each other — the `<server>_<account>` variant
// convention makes overlaps like "jira" / "jira_prod" real — and a first-match
// break over a map range made the winner a per-iteration coin flip, so whether
// a tool appeared in the system prompt could differ run to run, silently
// busting the byte-stable prefix the prompt cache requires
// (docs/PROMPT-CACHE-CONTRACT.md, #1125). The LONGEST matching server name
// wins: deterministic (two same-length prefixes of one name at the same offset
// are the same string), and it attributes a variant's tools to the variant's
// own toggle when it declares one, falling back to its base server's toggle
// when it does not.
//
// The rule itself is no longer duplicated here: it lives once in agentcore
// (longestServerKey, surfaced as OptionalServerForToolName) and is the SAME
// function agentcore's Gate-1 registration check resolves through, so the
// prompt roster and the registered tool set cannot drift apart for a variant
// seat the way they did before #1272.
func longestOptionalServerFor(name string, optionalServers mcpOptionalSet) string {
	return agentcore.OptionalServerForToolName(name, agentcore.MCPOptionalSet(optionalServers))
}

// ListPersonas returns available persona names (derived from *.yaml filenames).
func (m *Manager) ListPersonas() ([]string, error) {
	entries, err := os.ReadDir(m.personasDir)
	if err != nil {
		return nil, fmt.Errorf("read personas dir: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasSuffix(strings.ToLower(n), ".yaml") {
			continue
		}
		names = append(names, strings.TrimSuffix(n, filepath.Ext(n)))
	}
	sort.Strings(names)
	return names, nil
}

// ── system-prompt composition ──

// buildSystemPrompt composes the system prompt for a single turn:
//
//  1. The shared system_prompts/default.md (interactive chat operating model).
//  2. The chosen persona YAML, flagged as "Persona Context".
//  3. Runtime date context so the model doesn't infer "today" from stale data
//     (day-granular; restated again in the per-turn message tail — #1026).
//  4. A one-line listing of protocol files the model may `view_file` when
//     referenced by name.
//  5. The per-conversation workspace path, so the agent can hand it to
//     MCP tools whose cwd is not per-conversation.
//
// UserSkillPromptEntry is one user-authored skill in the prompt roster (the
// files live in the conversation workspace, materialized by the HTTP layer).
type UserSkillPromptEntry struct {
	Name        string
	Description string
	Path        string // workspace-relative, e.g. "user-skills/<name>/SKILL.md"
}

// buildSystemPrompt's hidden is the turn's interactive_hidden_tools snapshot
// (see activeMCPToolNames); nil hides nothing. The composition order is
// documented above, under "system-prompt composition".
//
// layer is the turn's user-persona layer (docs/USER-PERSONAS.md): persona is
// then the bundle persona the layer builds on, and the layer — nil for a plain
// bundle persona — is written directly after it, so it sits in the same stable
// prefix position for every turn of the conversation.
func (m *Manager) buildSystemPrompt(persona string, layer *UserPersonaLayer, conversationID string, memories []string, projectInstructions string, notes []agentcore.Note, enabledOptionalMCPServers []string, userSkills []UserSkillPromptEntry, hosted hostedMCPRoster, hidden []string) (string, error) {
	var sb strings.Builder

	fastIOOn := m.fastIOEnabledForTurn(enabledOptionalMCPServers, hidden)

	// 1. system prompt — the interactive base (bundle's chat.md by default).
	promptFile := m.chatSystemPromptFile
	if promptFile == "" {
		promptFile = "chat.md"
	}
	sp, err := os.ReadFile(filepath.Join(m.systemPromptsDir, filepath.Base(promptFile)))
	if err != nil {
		return "", fmt.Errorf("read system prompt: %w", err)
	}
	sb.Write(sp)
	sb.WriteString("\n\n")

	// 2. persona
	personaFile := persona
	if !strings.HasSuffix(strings.ToLower(personaFile), ".yaml") {
		personaFile += ".yaml"
	}
	// filepath.Base strips any directory components a caller-supplied persona name
	// might smuggle in, confining the read to personasDir (a trusted config dir).
	// The filepath.IsLocal check makes that confinement explicit and is the
	// barrier CodeQL's path-injection query recognizes, clearing the read below on
	// rescan; Base always yields a single component, so a valid persona passes.
	personaBase := filepath.Base(personaFile)
	if !filepath.IsLocal(personaBase) {
		return "", fmt.Errorf("invalid persona name %q", persona)
	}
	personaPath := filepath.Join(m.personasDir, personaBase)
	personaContent, err := os.ReadFile(personaPath) // #nosec G304 — personaBase is Base()+IsLocal-gated; personasDir is trusted config.
	if err != nil {
		return "", fmt.Errorf("read persona %s: %w", persona, err)
	}
	fmt.Fprintf(&sb, "---\n\n# Persona: %s\n\n", strings.TrimSuffix(filepath.Base(personaFile), ".yaml"))
	sb.Write(personaContent)
	sb.WriteString("\n\n")
	writeUserPersonaLayer(&sb, layer)

	// 3. runtime date
	sb.WriteString(runtimeDateContext(time.Now()))
	sb.WriteString("\n\n")

	// Project standing instructions (#509): the shared workspace's context,
	// injected before personal memories so project framing precedes user facts.
	if strings.TrimSpace(projectInstructions) != "" {
		sb.WriteString("## Project Instructions\n\n")
		sb.WriteString("This conversation belongs to a shared project. Follow these standing instructions from the project owner:\n\n")
		sb.WriteString(strings.TrimSpace(projectInstructions))
		sb.WriteString("\n\n")
	}

	if len(memories) > 0 {
		sb.WriteString("## User Memories\n\n")
		sb.WriteString("These memories are explicitly saved by this authenticated user and apply across their conversations. Treat them as durable user context, but do not reveal private memory contents unless relevant to the user's request. If two memories conflict, prefer the one with the more recent validity annotation; when in doubt, ask the user rather than assert a stale fact.\n\n")
		for _, memory := range memories {
			memory = strings.TrimSpace(memory)
			if memory == "" {
				continue
			}
			memory = strings.ReplaceAll(memory, "\r\n", "\n")
			memory = strings.ReplaceAll(memory, "\n", " ")
			fmt.Fprintf(&sb, "- %s\n", memory)
		}
		sb.WriteString("\n")
	}

	// 3b. Agent notes (admin-curated knowledge base) — injected directly after
	// the user-memories block, identical text in both modes.
	appendAgentNotes(&sb, notes)
	appendNoteProposalTool(&sb)

	// 4. Memory proposal tool instructions
	sb.WriteString("## Memory Proposal Tool\n\n")
	sb.WriteString("You have a `propose_memory` tool. Use it SPARINGLY — only when the user shares a durable preference, fact, or context that should persist across ALL future conversations.\n\n")
	sb.WriteString("Good candidates:\n")
	sb.WriteString("- User preferences (\"I prefer short answers\", \"Use Python for data tasks\")\n")
	sb.WriteString("- Professional context (\"I work with Jordan on trading\", \"My team uses GitHub\")\n")
	sb.WriteString("- Personal facts (\"I live in PST\", \"I have a dog named Max\")\n\n")
	sb.WriteString("Do NOT use for:\n")
	sb.WriteString("- Temporary or session-specific information\n")
	sb.WriteString("- Things the user is just mentioning in passing\n")
	sb.WriteString("- Factual questions the user asks (they're not telling you to remember it)\n")
	sb.WriteString("- When the user says \"remember this\" — they should use the manual Memories manager or you should propose it\n\n")
	sb.WriteString("When you call propose_memory, the user sees an inline card asking \"Save this memory?\" with Save/Don't Save buttons. Summarize what you proposed and wait for their response. Do NOT retry the tool.\n\n")

	// 4b. In-chat patrols and timers (chat_patrol)
	if !agentcore.InteractiveHiddenIn(hidden, tools.ChatPatrolToolName) {
		sb.WriteString("## In-Chat Patrols & Timers (`chat_patrol`)\n\n")
		sb.WriteString("You have a `chat_patrol` tool that attaches a recurring cron patrol or a one-shot timer directly to THIS chat conversation:\n")
		sb.WriteString("- When a patrol or timer fires, Fleet wakes you up right here in the same conversation with the full transcript, workspace files, persistent Python state, and enabled connectors intact.\n")
		sb.WriteString("- Use `chat_patrol` (`action=\"create\"`) whenever the user asks you to patrol every N hours in chat (e.g. `cron=\"0 */2 * * *\"` for every 2 hours), monitor deals/pacing/pages on a loop in this thread, or check back after a delay (`duration_seconds=1800` for 30 minutes).\n")
		sb.WriteString("- Because `chat_patrol` executes immediately without ending your turn, when a user asks to start a patrol, call `chat_patrol(action=\"create\", ...)` and also run the initial check in the same turn unless they asked you to wait.\n")
		sb.WriteString("- If `auto_approve` is false in the result and the patrol will invoke critical tools while the user is away, let the user know they can toggle **⚡ Auto-approve** in the Patrol bar or composer.\n")
		sb.WriteString("- Use `chat_patrol` (`action=\"list\"`, `\"update\"`, `\"pause\"`, `\"resume\"`, or `\"cancel\"`) to inspect or adjust this conversation's active patrol.\n\n")
	}

	// 5. Native optional tools — image generation is gated behind the same
	//    Tools picker users use for Optional MCP servers. We mention it here
	//    only when active so a chat without it enabled doesn't see the tool's
	//    description and try to call it (it won't be in the model's tool
	//    list either, but the static prompt previously listed it which led
	//    to "tool not found" errors).
	imageGenEnabled := false
	for _, n := range enabledOptionalMCPServers {
		if strings.TrimSpace(n) == OptionalNativeImageGenName {
			imageGenEnabled = true
			break
		}
	}
	if imageGenEnabled {
		sb.WriteString("## Image Generation\n\n")
		sb.WriteString("The `generate_image` tool is enabled for this conversation. Use it for photorealistic / illustrative output the user explicitly asks for — banner ads, brand creative, mockups, hero images. ")
		sb.WriteString("You DO NOT pick the file extension — the model decides the output format (Nano Banana 2.1 returns JPEG/PNG; there is no API parameter to override) and the tool saves with the matching extension. ")
		sb.WriteString("Pass an optional `filename` slug WITHOUT an extension (e.g. `lumen-banner`); when omitted, the tool defaults to `image-<timestamp>`. Always reference the `path` returned by the tool when embedding the image in your reply via `![alt](path)` — the chat UI rewrites that to a workspace URL and renders inline. ")
		sb.WriteString("Default model is Google Nano Banana 2.1 (`google/gemini-nano-banana-2.1`); alternatives: `model=google/gemini-3-pro-image` (Nano Banana Pro), `model=google/gemini-3.1-flash-image` (Nano Banana 2), `model=google/gemini-3.1-flash-lite-image` (Nano Banana 2 Lite), or `model=openai/gpt-5.4-image-2` (GPT-5.4 Image 2). ")
		sb.WriteString("Use `reference_images` to edit / restyle existing images (including images the user attached this turn). ")
		sb.WriteString("Do NOT use this tool for charts, plots, or data visualizations — use `run_python` with matplotlib instead (free, deterministic, can read your data).\n\n")
	}

	// 6. The MCP tool roster — "## MCP Tools (live registry)" — is NOT written
	//    here. It is appended by agentcore.Run once the tool roster exists
	//    (agentcore/live_registry.go), because only the roster knows which
	//    tools survived the gates and whether they were deferred behind
	//    tool_search/tool_describe/tool_call above the disclosure threshold.
	//    Written here, before the roster, the section listed 159 hosted names
	//    as callable that a deferred roster does not register, and the model
	//    gave up on a working connector (#1006).
	//
	//    What this builder does know and the roster does not: which hosted
	//    connectors the user set up that could NOT be mounted this turn
	//    (`hosted.skipped`, from the per-user overlay RunTurn opened before
	//    composing the prompt). That is the one case where "no such tool" is
	//    the wrong answer, so name them: the model sends the user to reconnect
	//    rather than improvising a workaround (the scheduled runner prepends
	//    the same notice, scheduledrun.withSkippedRemoteNotice).
	//    The reason is named per connector (F10): "reconnect" is the right
	//    advice only for a login the vendor refused; for a vendor that did not
	//    answer, that advice sends the user to redo a working login for
	//    nothing, so the model is told to say the connector is unavailable
	//    right now instead.
	writeHostedSkipNotice(&sb, hosted.skipped)
	if !agentcore.InteractiveHiddenIn(hidden, tools.EnableMCPConnectorsToolName) && agentcore.PersonaPermitsTool(m.personaPolicy(persona), tools.EnableMCPConnectorsToolName) {
		writeAvailableOptionalConnectorsSection(&sb, m.availableOptionalForPrompt(persona, enabledOptionalMCPServers, hosted, hidden))
	}

	// 5. protocol listing — skip fastio-mcp.md when fast.io is off
	// so the agent doesn't try to read it for tools it can't call.
	// The rich descriptions for the always-available protocols live
	// in default.md's protocol table; this listing is just a roster
	// of files the agent can `view_file` by path.
	if entries, err := os.ReadDir(m.protocolsDir); err == nil && len(entries) > 0 {
		sb.WriteString("## Protocols\n")
		sb.WriteString("Files under `protocols/` are reusable playbooks. Read one only when the user references it; do not re-read within the same conversation.\n\n")
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			lower := strings.ToLower(e.Name())
			if !strings.HasSuffix(lower, ".md") && !strings.HasSuffix(lower, ".yaml") && !strings.HasSuffix(lower, ".yml") {
				continue
			}
			// Per-protocol gates. Today only fastio-mcp.md is
			// optional; if more protocols ever become MCP-server-
			// dependent the gate set goes here.
			if lower == "fastio-mcp.md" && !fastIOOn {
				continue
			}
			fmt.Fprintf(&sb, "- `protocols/%s`\n", e.Name())
		}
		sb.WriteString("\n")
	}

	// 5b. skill listing — the Agent Skills standard (https://github.com/anthropics/skills).
	// Progressive disclosure: only each skill's name + description + path go in the
	// prompt (Level 1 metadata). The agent reads the skill's SKILL.md (Level 2) and
	// any bundled scripts/resources (Level 3) on demand, by path, when a task
	// matches — the skills/ dir is mounted read-only in the sandbox and symlinked
	// into the workspace, so `skills/<name>/...` paths resolve for bash/run_python.
	userSkillNames := make(map[string]bool, len(userSkills))
	for _, usk := range userSkills {
		userSkillNames[usk.Name] = true
	}
	if skills, _ := clientconfig.ReadSkills(m.skillsDir); len(skills) > 0 {
		var visibleSkills []clientconfig.Skill
		for _, sk := range skills {
			if !userSkillNames[sk.Name] {
				visibleSkills = append(visibleSkills, sk)
			}
		}
		if len(visibleSkills) > 0 {
			sb.WriteString("## Skills\n")
			sb.WriteString("Skills are packaged, on-demand capabilities. Only each skill's name and description are listed here; when a task matches one, read its `SKILL.md` for the full instructions — it may bundle scripts you run via `bash`/`run_python` and reference files you read on demand. Do NOT read a skill's files unless the task calls for it. When the user asks to edit, update, or customize a skill, read its `SKILL.md` and call `propose_skill` with the updated instructions so the user can approve and share it in Settings → Skills.\n\n")
			for _, sk := range visibleSkills {
				fmt.Fprintf(&sb, "- **%s** (`%s`): %s\n", sk.Name, sk.Path, sk.Description)
			}
			sb.WriteString("\n")
		}
	}

	// 5c. the user's own skills (docs/SKILLS.md phase 2) — same progressive
	// disclosure, but the files live in the conversation workspace (writable,
	// per-user) rather than the read-only bundle mount.
	if len(userSkills) > 0 {
		sb.WriteString("## Your user's skills\n")
		sb.WriteString("Skills this user authored, customized, or received via workspace sharing (any supporting reference files or scripts under `skills/<name>/` remain available on disk). Same rules as Skills above: read the SKILL.md only when the task matches, and if the user asks to edit or improve one, read its `SKILL.md` and call `propose_skill` with the updated instructions.\n\n")
		for _, sk := range userSkills {
			fmt.Fprintf(&sb, "- **%s** (`%s`): %s\n", sk.Name, sk.Path, sk.Description)
		}
		sb.WriteString("\n")
	}

	// Sub-agent delegation policy (#1043): appended in lockstep with the tool
	// registration — RunInteractiveTurn registers spawn_subagent from the same
	// LiveSubagentsEnabled flag the Manager passes as Subagent.Enabled, so the
	// prompt teaches the tool exactly when the roster carries it. Stable within
	// an enablement state (an admin toggle is a deliberate cache-busting act).
	if m.config.LiveSubagentsEnabled() {
		sb.WriteString(DelegationPromptSection())
		sb.WriteString("\n")
	}

	// 6. per-conversation absolute workspace path. Native tools
	// (bash, run_python) already cwd into this dir, so the agent can
	// use bare relative paths there. MCP subprocesses do NOT inherit
	// the per-conv cwd — they stay at the server root — so any MCP tool
	// that writes files needs to receive this ABSOLUTE path as
	// output_dir. Otherwise "./foo.csv" lands in the read-only server
	// root and the call fails.
	if conversationID != "" {
		if abs, err := filepath.Abs(tools.WorkspaceDirForConversation(conversationID)); err == nil {
			fmt.Fprintf(&sb, "## Working directory\n\nYour per-conversation scratch directory for this turn is:\n\n    %s\n\n", abs)
			sb.WriteString(
				"Whenever an MCP tool takes an `output_dir` (or equivalent output-path) argument, pass this absolute path. " +
					"MCP subprocesses run from the server root, not your scratch directory, and their built-in default output " +
					"locations are read-only on production hosts (systemd ProtectSystem=strict), so a missing `output_dir` " +
					"fails with `[Errno 30] Read-only file system`. " +
					"Bash and run_python already cwd into this dir — bare relative paths work there.\n\n",
			)
		}
	}

	return sb.String(), nil
}

// Day precision only — any finer granularity would change the system prompt
// every turn, which breaks implicit prefix caching (OpenAI) and advancing
// cache_control breakpoints (Anthropic). One refresh per UTC day is plenty
// for "today's date" reasoning.
func runtimeDateContext(now time.Time) string {
	today := now.UTC().Format("2006-01-02")
	from := now.UTC().AddDate(0, 0, -2).Format("2006-01-02")
	return fmt.Sprintf("## Runtime Date Context\n\n"+
		"- Current UTC date: %s\n"+
		"- runtime_today: %s\n"+
		"- freshness_window: %s .. %s\n"+
		"- Treat runtime_today as 'today' for any rolling window. Do not infer 'today' from message history, filenames, or prior tool output.\n"+
		"- For \"today\", \"latest\", or \"check again\", search freshness_window unless the user asked for a narrower historical range.\n",
		today, today, from, today,
	)
}

// writeHostedSkipNotice appends the "hosted connectors not mounted" section
// (see buildSystemPrompt step 6): every skipped connector named under the
// advice its reason class earns, and never dropped — an unrecognised class
// lands in the last group, which asserts nothing about the cause.
func writeHostedSkipNotice(sb *strings.Builder, skipped []skippedConnector) {
	if len(skipped) == 0 {
		return
	}
	sb.WriteString("## Hosted connectors not mounted this turn\n\n")
	sb.WriteString("Hosted connector(s) the user has set up that could NOT be mounted this turn. Their `mcp_*` tools are unavailable; if the user asks for them, say so plainly instead of working around it, with the reason below — do not tell the user to reconnect a connector listed as not responding.\n")
	groups := []struct {
		reason string
		text   string
	}{
		{SkipReasonNeedsReauth, "need reconnecting (the login expired or the vendor rejected the stored credential) — tell the user to reconnect them under Settings → Connections"},
		{SkipReasonUnreachable, "did not respond this turn (the vendor or the network, not the login) — tell the user they are unavailable right now and can be retried later"},
		{SkipReasonSeatNotConnected, "are pinned to an account that is not connected — tell the user to connect that account under Settings → Connections"},
		{SkipReasonOverlayCap, fmt.Sprintf("were left out because more hosted connectors are turned on than fleet mounts in one turn (at most %d; the connection itself is fine) — tell the user to turn off connectors this conversation does not need in the Connectors picker", maxOverlayServers)},
		{SkipReasonUnknown, "could not be mounted for a reason fleet could not classify — tell the user they are unavailable this turn and to check them under Settings → Connections; do not assert whether the login is the cause"},
	}
	for _, g := range groups {
		var names []string
		for _, sc := range skipped {
			reason := sc.reason
			switch reason {
			case SkipReasonNeedsReauth, SkipReasonUnreachable, SkipReasonSeatNotConnected, SkipReasonOverlayCap:
			default:
				reason = SkipReasonUnknown // anything else lands in the last group, never nowhere
			}
			if reason == g.reason {
				names = append(names, "`"+sc.name+"`")
			}
		}
		if len(names) > 0 {
			// Bounded (MaxSkipNoticeNames): the overlay cap can skip any number.
			sb.WriteString("- " + JoinSkipNoticeNames(names) + ": " + g.text + ".\n")
		}
	}
	sb.WriteString("\n")
}

// availableOptionalForPrompt filters the turn's connected optional MCP catalog
// to the connectors that are NOT yet enabled in this conversation and have at
// least one tool permitted by interactive_hidden_tools and the persona policy.
// Sorted deterministically by prompt-safe server name for prompt-cache byte
// stability (docs/PROMPT-CACHE-CONTRACT.md).
func (m *Manager) availableOptionalForPrompt(persona string, enabledOptIns []string, hosted hostedMCPRoster, hidden []string) []OptionalServerInfo {
	source := hosted.availableOptional
	if source == nil && m != nil {
		source = m.MCPServerCatalog()
	}
	if len(source) == 0 {
		return nil
	}
	enabled := make(map[string]bool, len(enabledOptIns))
	for _, n := range enabledOptIns {
		if clean := strings.ToLower(strings.TrimSpace(n)); clean != "" {
			enabled[clean] = true
		}
	}
	var policy *agentcore.PersonaToolPermissions
	if m != nil {
		policy = m.personaPolicy(persona)
	}
	out := make([]OptionalServerInfo, 0, len(source))
	seen := make(map[string]bool, len(source))
	for _, srv := range source {
		rawName := strings.ToLower(strings.TrimSpace(srv.Name))
		if rawName == "" || rawName == OptionalNativeImageGenName || enabled[rawName] {
			continue
		}
		safeName := promptSafeName(rawName)
		if safeName == "" || seen[safeName] {
			continue
		}
		visibleTools := 0
		if len(srv.Tools) > 0 {
			for _, t := range srv.Tools {
				full := t
				if !strings.HasPrefix(full, "mcp_") {
					full = "mcp_" + safeName + "_" + t
				}
				if agentcore.InteractiveHiddenIn(hidden, full) {
					continue
				}
				if !agentcore.PersonaPermitsTool(policy, full) {
					continue
				}
				visibleTools++
			}
			if visibleTools == 0 {
				continue
			}
		} else {
			if !srv.Remote && srv.ToolCount <= 0 {
				continue
			}
			probe := "mcp_" + safeName + "_probe"
			if agentcore.InteractiveHiddenIn(hidden, probe) || !agentcore.PersonaPermitsTool(policy, probe) {
				continue
			}
			visibleTools = srv.ToolCount
		}
		seen[safeName] = true
		entry := srv
		entry.Name = safeName
		entry.ToolCount = visibleTools
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

func sanitizeSingleLinePromptText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return strings.Join(strings.Fields(s), " ")
}

func writeAvailableOptionalConnectorsSection(sb *strings.Builder, available []OptionalServerInfo) {
	if len(available) == 0 {
		return
	}
	sb.WriteString("## Available Optional MCP Connectors (not currently enabled)\n\n")
	sb.WriteString("The following connected Optional MCP connectors are available for this user right now, but are currently toggled **OFF** in this conversation:\n")
	for _, srv := range available {
		label := sanitizeSingleLinePromptText(srv.DisplayName)
		desc := sanitizeSingleLinePromptText(srv.Description)
		var metaParts []string
		if label != "" && !strings.EqualFold(label, srv.Name) {
			metaParts = append(metaParts, label)
		}
		if srv.ToolCount > 0 {
			unit := "tools"
			if srv.ToolCount == 1 {
				unit = "tool"
			}
			metaParts = append(metaParts, fmt.Sprintf("%d %s", srv.ToolCount, unit))
		} else if srv.Remote {
			metaParts = append(metaParts, "connected remote MCP")
		}
		meta := ""
		if len(metaParts) > 0 {
			meta = " (" + strings.Join(metaParts, ", ") + ")"
		}
		if desc != "" {
			fmt.Fprintf(sb, "- **`%s`**%s: %s\n", srv.Name, meta, desc)
		} else {
			fmt.Fprintf(sb, "- **`%s`**%s\n", srv.Name, meta)
		}
	}
	sb.WriteString("\nIf the user's request needs data or actions from one of these connectors:\n")
	sb.WriteString("- Do **NOT** flail with `tool_search`, `bash`, or `run_python` workarounds trying to reach a connector that is turned off.\n")
	sb.WriteString("- Call `enable_mcp_connectors(connectors=[\"<name>\"], reason=\"<why it is needed>\")` right away.\n")
	sb.WriteString("- When auto-approve is on, Fleet enables the connector(s) immediately and automatically continues your turn with their tools mounted. When auto-approve is off, Fleet shows an approval card and automatically continues your turn as soon as the user clicks **Approve**.\n\n")
}

