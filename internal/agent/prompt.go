package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"charm.land/fantasy"

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

// Manager owns the shared state reused across every chat turn: the MCP client
// connections, the optional-server gating sets, and the persona/protocol/
// system-prompt source files. Construct it once at server startup.
//
// This is the minimal subset needed by the interactive prompt/roster/optin
// helpers ported into the fleet monorepo. The full per-turn streaming loop
// (RunTurn) and provider/model wiring are out of scope for this package.
type Manager struct {
	config *config.Config

	mcpClient *mcp.Client
	allowlist mcpAllowlist

	// resolver loads + caches OpenRouter models per slug (nil in the
	// prompt/roster unit tests, which never run a turn). RunTurn / Summarize /
	// SuggestTitle resolve through it.
	resolver *agentcore.ModelResolver

	// native is the per-process native-tool template (DefaultTools); each turn
	// rebuilds a sandbox-bound variant via tools.NewTurnTools.
	native []fantasy.AgentTool

	// sandboxPool is the per-turn container warm pool. RunTurn Take()s one per
	// turn; SandboxPool() exposes it for the out-of-band approved-bash path.
	sandboxPool *sandbox.Pool

	// notesProvider supplies the admin-curated knowledge base injected into the
	// system prompt every turn (nil = no notes section).
	notesProvider agentcore.NotesProvider

	// noteProposer stages agent-proposed admin-notes edits (propose_note). Wired
	// once here so EVERY interactive RunTurn (web, ACP ingress, native-acp)
	// inherits the same propose_note guarantee — not per-entrypoint. Nil leaves
	// propose_note reporting "unavailable" (and the tool unregistered).
	noteProposer agentcore.NoteProposer

	// mcpToolRoster is the frozen list of `mcp_<server>_<tool>` names
	// that survived the initial MCP connection sweep and per-server
	// allowlists. Optional-server names are filtered from this roster per
	// turn before the system prompt is built, matching the actual tool set
	// registered for that conversation's opt-ins.
	mcpToolRoster []string

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

	// runtimes is the bundle's runtime-flavor catalog + the default flavor name.
	// A turn's requested flavor is validated against this; native-acp turns spawn
	// nativeAgentImage. Empty leaves every turn on native-inprocess.
	runtimes         []clientconfig.Runtime
	defaultRuntime   string
	nativeAgentImage string

	// limiter is the SHARED process-wide admission governor (interactive +
	// scheduled). RunTurn admits each chat turn through it so the box-wide
	// concurrency cap bounds chat too, with reserved headroom keeping chat ahead
	// of background work. Nil disables admission (the one-shot cutlass harness and
	// tests, where there is nothing to contend with).
	limiter *admission.Limiter
}

// SetRuntimes configures the runtime-flavor catalog + the native-agent image the
// Manager honors on a turn's requested flavor. Called at boot from the loaded
// client bundle (cmd/fleet). When unset, every turn runs native-inprocess.
func (m *Manager) SetRuntimes(runtimes []clientconfig.Runtime, defaultRuntime, nativeAgentImage string) {
	m.runtimes = runtimes
	m.defaultRuntime = defaultRuntime
	m.nativeAgentImage = nativeAgentImage
}

// resolveRuntime picks the effective flavor for a requested name: the requested
// flavor when it exists in the catalog, else the bundle default, else
// native-inprocess. Returns the resolved descriptor so RunTurn can route on the
// type (native-acp / external acp) and read the flavor's image/env/args.
func (m *Manager) resolveRuntime(requested string) clientconfig.Runtime {
	want := strings.TrimSpace(requested)
	if want == "" {
		want = m.defaultRuntime
	}
	for _, rt := range m.runtimes {
		if rt.Name == want {
			return rt
		}
	}
	return clientconfig.Runtime{Name: clientconfig.RuntimeNativeInprocess, Type: clientconfig.RuntimeTypeNativeInprocess}
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
	// Dir is the cwd the stdio subprocess launches in (the client-config bundle
	// root) so relative args like `mcp/foo.py` resolve there; "" inherits cwd.
	Dir string
	// AccountVars are the base credential env-var names whose `<VAR>_<ACCOUNT>`
	// suffixes name this server's provisioned credential seats (creds.AccountsFor).
	AccountVars []string

	// HTTP fields. If URL is set, we treat this as an HTTP MCP server.
	URL     string
	Headers map[string]string

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
}

// fastIOSystemPromptSection is the dedicated Fast.io guidance for
// turns where the fast.io MCP server is wired up. Lives here (not in
// system_prompts/default.md) because Fast.io is optional — deployments
// without FAST_IO_MCP_TOKEN should not see Fast.io references at all,
// or the model wastes context on tools it can't call.
//
// Includes (1) the read/write model overview, (2) the `fastio_find`
// discovery flow with the file-pick policy, (3) the upload flow via
// `fastio_upload_file`, (4) the protocol reference, and (5) two hard
// rules about what NOT to delegate to the user. Mirrors the prior
// in-default-md copy with the `fastio_find` doc added since the tool
// only exists now.
const fastIOSystemPromptSection = `## Fast.io: the shared read/write file store

Fast.io is the team's shared workspace — think of it as a OneDrive/Dropbox/Google Drive that *you* have hands-on access to via the ` + "`mcp_fast_io_*`" + ` tools. It is **not** a static archive and **not** something the user has to babysit through a browser. When the user says *"create a file for me in Fast.io"*, *"save this under KOC"*, *"update the master tracking sheet"*, or *"amend the KOC doc"*, **you do the entire find → download → edit → upload loop yourself.**

**Finding a file → use ` + "`fastio_find`" + `.** This native tool wraps storage search + bulk details into one round-trip and returns a tight markdown table — id, name, parent, modified, size, mimetype — sorted newest-first. It auto-promotes ELC codes in your query (` + "`fastio_find query=\"ABC plumbing ELC00109\"`" + ` also searches just ` + "`\"ELC00109\"`" + ` and unions the results), which fixes Fast.io keyword search's AND-tokenized blindspot for natural-language phrasing. **File-pick policy enforced by the tool's response:** 1 match → proceed; 2+ matches → STOP and ask the user which one (the response includes a recommended file and a pre-written question to quote back). Reach for raw ` + "`mcp_fast_io_storage action=search`" + ` only for parameters fastio_find doesn't expose (cursor pagination past 25 hits, semantic search via intelligence=true).

**Uploading a file you produced locally → use ` + "`fastio_upload_file`" + `.** This native tool takes a file path (not bytes), reads it from your workspace, and forwards it to Fast.io for you. The bytes never enter your context — no base64 in ` + "`run_python.vars`" + `, no ` + "`content_base64`" + ` in tool args, no length-mangling on the JSON round-trip. Required params: ` + "`path`" + ` (workspace-relative is fine) and ` + "`workspace_id`" + ` (19-digit, from ` + "`mcp_fast_io_workspace action=list`" + `). Optional: ` + "`filename`" + ` (defaults to basename), ` + "`parent_node_id`" + ` (defaults to root), ` + "`content_type`" + ` (auto-detected). Caps at 5 MB raw; for larger files drive ` + "`mcp_fast_io_upload`" + ` chunked blob flow yourself.

**Downloading a Fast.io file → ` + "`mcp_fast_io_download action=file-url`" + ` then ` + "`download_url`" + `.** The MCP returns a signed, short-lived URL; ` + "`download_url`" + ` GETs that URL and lands the bytes in this conversation's workspace in a single tool turn (no Python/curl detour). Pass the signed URL as ` + "`url`" + ` and an optional ` + "`filename`" + `; the native tool defaults ` + "`output_dir`" + ` to your scratch so the file is immediately readable by ` + "`bash`" + `/` + "`run_python`" + `/` + "`xlsx_workbook`" + `.

For everything else — workspace-id discovery, the required ` + "`profile_type`" + `/` + "`profile_id`" + ` on ` + "`storage`" + `/` + "`download`" + ` calls, the chunked blob flow for files over the native tool's 5 MB cap, the ` + "`web-import`" + ` shortcut for URL → Fast.io copies, share creation, and the "same-name upload overwrites in place" rule — read **` + "`protocols/fastio-mcp.md`" + ` once** at the start of any Fast.io task and follow it step by step.

**Persistence offer:** if a user-attached file looks like something they (or a teammate) will want to reference or edit again later — a source document, a dataset, anything they'd otherwise have to re-upload — proactively offer to drop it in Fast.io via ` + "`fastio_upload_file`" + `. Frame it as a one-line offer ("I can save this to Fast.io so we can update it from any chat going forward; want me to?") and only call the tool after they say yes. Don't bother offering for one-shot files (a quick snippet, a throwaway screenshot, the output of a single ad-hoc question).

Two hard rules that apply outside the protocol's mechanics:

- **Do not** tell the user to "open Fast.io in your browser, download the file, edit it, re-upload it." That is your job. The only time you ask the user to attach from their machine is when the source file genuinely lives behind an auth wall you can't bypass (SharePoint/Google login) — and even then, frame it as a one-step handoff ("attach it here and I'll save the update to Fast.io"), not a workflow they repeat every time.
- **Do not** describe Fast.io as a static repository, snapshot store, or "you do the edits, I just store" surface. From the user's perspective it is a live shared drive that you operate on their behalf.
`

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
func (m *Manager) fastIOEnabledForTurn(enabledOptIns []string) bool {
	for _, name := range m.activeMCPToolNames(enabledOptIns) {
		if strings.HasPrefix(name, "mcp_fast_io_") {
			return true
		}
	}
	return false
}

// activeMCPToolNames returns the prefixed (`mcp_<server>_<tool>`) tool names
// the system prompt advertises to the model for this turn. It starts from the
// frozen startup roster, then filters Optional servers unless the conversation
// opted in. This keeps prompt guidance aligned with buildFantasyTools.
func (m *Manager) activeMCPToolNames(enabledOptIns []string) []string {
	if len(m.optionalServers) == 0 {
		return m.mcpToolRoster
	}
	enabled := make(map[string]bool, len(enabledOptIns))
	for _, n := range enabledOptIns {
		enabled[strings.TrimSpace(n)] = true
	}
	out := make([]string, 0, len(m.mcpToolRoster))
	for _, name := range m.mcpToolRoster {
		optionalServer := ""
		for server := range m.optionalServers {
			if strings.HasPrefix(name, "mcp_"+server+"_") {
				optionalServer = server
				break
			}
		}
		if optionalServer != "" && !enabled[optionalServer] {
			continue
		}
		out = append(out, name)
	}
	return out
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
//  3. Runtime date context so the model doesn't infer "today" from stale data.
//  4. A one-line listing of protocol files the model may `view_file` when
//     referenced by name.
//  5. The per-conversation workspace path, so the agent can hand it to
//     MCP tools whose cwd is not per-conversation.
func (m *Manager) buildSystemPrompt(persona, conversationID string, memories []string, notes []agentcore.Note, enabledOptionalMCPServers []string) (string, error) {
	var sb strings.Builder

	fastIOOn := m.fastIOEnabledForTurn(enabledOptionalMCPServers)

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

	// 1a. Fast.io dedicated section — only when the fast.io MCP is
	// wired up. Kept out of default.md so deployments without
	// FAST_IO_MCP_TOKEN never see Fast.io guidance for tools they
	// can't call. The section name lives in the protocol table at
	// step 5 below (also gated on fastIOOn).
	if fastIOOn {
		sb.WriteString(fastIOSystemPromptSection)
		sb.WriteString("\n")
	}

	// 2. persona
	personaFile := persona
	if !strings.HasSuffix(strings.ToLower(personaFile), ".yaml") {
		personaFile += ".yaml"
	}
	personaPath := filepath.Join(m.personasDir, filepath.Base(personaFile))
	personaContent, err := os.ReadFile(personaPath) // #nosec G304 — dir is trusted, base() strips traversal.
	if err != nil {
		return "", fmt.Errorf("read persona %s: %w", persona, err)
	}
	fmt.Fprintf(&sb, "---\n\n# Persona: %s\n\n", strings.TrimSuffix(filepath.Base(personaFile), ".yaml"))
	sb.Write(personaContent)
	sb.WriteString("\n\n")

	// 3. runtime date
	sb.WriteString(runtimeDateContext(time.Now()))
	sb.WriteString("\n\n")

	if len(memories) > 0 {
		sb.WriteString("## User Memories\n\n")
		sb.WriteString("These memories are explicitly saved by this authenticated user and apply across their conversations. Treat them as durable user context, but do not reveal private memory contents unless relevant to the user's request.\n\n")
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
	sb.WriteString("- Professional context (\"I work with Kyle on trading\", \"My team uses GitHub\")\n")
	sb.WriteString("- Personal facts (\"I live in PST\", \"I have a dog named Max\")\n\n")
	sb.WriteString("Do NOT use for:\n")
	sb.WriteString("- Temporary or session-specific information\n")
	sb.WriteString("- Things the user is just mentioning in passing\n")
	sb.WriteString("- Factual questions the user asks (they're not telling you to remember it)\n")
	sb.WriteString("- When the user says \"remember this\" — they should use the manual Memories manager or you should propose it\n\n")
	sb.WriteString("When you call propose_memory, the user sees an inline card asking \"Save this memory?\" with Save/Don't Save buttons. Summarize what you proposed and wait for their response. Do NOT retry the tool.\n\n")

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
		sb.WriteString("You DO NOT pick the file extension — the model decides the output format (Nano Banana Pro returns JPEG; there is no API parameter to override) and the tool saves with the matching extension. ")
		sb.WriteString("Pass an optional `filename` slug WITHOUT an extension (e.g. `lumen-banner`); when omitted, the tool defaults to `image-<timestamp>`. Always reference the `path` returned by the tool when embedding the image in your reply via `![alt](path)` — the chat UI rewrites that to a workspace URL and renders inline. ")
		sb.WriteString("Default model is Google Nano Banana Pro (`google/gemini-3-pro-image-preview`, ~$0.14/image); cheaper alternatives: `model=google/gemini-3.1-flash-image-preview` (Nano Banana 2) or `model=google/gemini-2.5-flash-image` (Nano Banana). ")
		sb.WriteString("Use `reference_images` to edit / restyle existing images (including images the user attached this turn). ")
		sb.WriteString("Do NOT use this tool for charts, plots, or data visualizations — use `run_python` with matplotlib instead (free, deterministic, can read your data).\n\n")
	}

	// 6. MCP tool roster — what's actually wired up THIS turn. Prevents the
	//    model from confidently calling `mcp_email_search_emails` when the
	//    email subprocess failed to start (or the operator never set the
	//    AWS creds). Empty list = explicit "no MCP tools available" so the
	//    model doesn't hallucinate one.
	sb.WriteString("## MCP Tools (live registry)\n\n")
	mcpNames := m.activeMCPToolNames(enabledOptionalMCPServers)
	if len(mcpNames) == 0 {
		sb.WriteString("No MCP tools are currently connected. Do not attempt to call any `mcp_*` tool — none will resolve.\n\n")
	} else {
		sb.WriteString("These are the only `mcp_*` tools registered for this turn. Call exactly these names:\n\n")
		for _, n := range mcpNames {
			fmt.Fprintf(&sb, "- `%s`\n", n)
		}
		sb.WriteString("\n")
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
	if skills, _ := clientconfig.ReadSkills(m.skillsDir); len(skills) > 0 {
		sb.WriteString("## Skills\n")
		sb.WriteString("Skills are packaged, on-demand capabilities. Only each skill's name and description are listed here; when a task matches one, read its `SKILL.md` for the full instructions — it may bundle scripts you run via `bash`/`run_python` and reference files you read on demand. Do NOT read a skill's files unless the task calls for it.\n\n")
		for _, sk := range skills {
			fmt.Fprintf(&sb, "- **%s** (`%s`): %s\n", sk.Name, sk.Path, sk.Description)
		}
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
	return fmt.Sprintf("## Runtime Date Context\n\n"+
		"- Current UTC date: %s\n"+
		"- Treat the date above as 'today' for any rolling window. Do not infer 'today' from message history or tool output.\n",
		now.UTC().Format("2006-01-02"),
	)
}
