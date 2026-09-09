package agentcore

import (
	"fmt"
	"sort"
	"strings"
)

// The system prompt's "MCP Tools (live registry)" section tells the model
// which `mcp_*` tools exist THIS turn, so it neither hallucinates one nor gives
// up on a real one. It is rendered here, from the roster buildFantasyTools
// actually registered, and appended to the driver's system prompt by Run —
// not written by the driver up front. The driver cannot know two things the
// roster decides: which tools survived the persona/allowlist/opt-in gates, and
// whether the set was deferred behind the disclosure bridges (#506). Written
// early, the section listed names as callable that a deferred roster does not
// register; the model called one, the agent framework answered "tool not
// found", and the model concluded the connector was down (#1006, four
// connectors = 159 tools). Derived from the built roster, the section cannot
// disagree with the tool list.
//
// Prompt-cache contract (docs/PROMPT-CACHE-CONTRACT.md): the section is a pure
// function of the roster — names sorted, servers sorted, no counters or
// timestamps — so for the same conversation and connector set it is
// byte-identical turn after turn.

// liveRegistryHeading is referenced by name in the Browserbase skill and the
// docs; keep it stable.
const liveRegistryHeading = "## MCP Tools (live registry)"

// liveRegistrySection renders the section for this roster.
func (r toolRoster) liveRegistrySection() string {
	var sb strings.Builder
	sb.WriteString(liveRegistryHeading)
	sb.WriteString("\n\n")
	switch {
	case r.deferredMCP > 0:
		// Deferred: the names are real but NOT registered; only the bridges
		// are. Say so plainly and give the model an orientation (which
		// connectors, how many tools) without the 159-line list that costs
		// tokens and invites a direct call.
		fmt.Fprintf(&sb, "%d MCP tools are available this turn, but the roster is large, so they are NOT in your tool list by name. ", r.deferredMCP)
		sb.WriteString("They are reachable only through three bridge tools: `tool_search {query}` finds tools by keyword, `tool_describe {name}` returns a tool's parameters, and `tool_call {name, arguments}` invokes it. ")
		sb.WriteString("A direct `mcp_*` call will fail with \"tool not found\" — use `tool_call` instead. ")
		sb.WriteString("Tool names follow `mcp_<connector>_<tool>`; searching by the connector name or the task works. Connectors behind the bridges:\n\n")
		servers := make([]string, 0, len(r.deferredByServer))
		for s := range r.deferredByServer {
			servers = append(servers, s)
		}
		sort.Strings(servers)
		for _, s := range servers {
			n := r.deferredByServer[s]
			noun := "tools"
			if n == 1 {
				noun = "tool"
			}
			fmt.Fprintf(&sb, "- `%s` (%d %s)\n", PromptSafeName(s), n, noun)
		}
	case len(r.directMCP) > 0:
		sb.WriteString("These are the only `mcp_*` tools registered for this turn. Call exactly these names:\n\n")
		for _, n := range r.directMCP {
			fmt.Fprintf(&sb, "- `%s`\n", PromptSafeName(n))
		}
	default:
		sb.WriteString("No MCP tools are currently connected. Do not attempt to call any `mcp_*` tool — none will resolve.\n")
	}
	return sb.String()
}

// withLiveRegistry appends the roster's live-registry section to the driver's
// system prompt. The join is deterministic (trailing newlines of the base are
// normalized to exactly one blank line) so it is safe inside the cached prefix.
func withLiveRegistry(systemPrompt string, r toolRoster) string {
	base := strings.TrimRight(systemPrompt, "\n")
	if base == "" {
		return r.liveRegistrySection()
	}
	return base + "\n\n" + r.liveRegistrySection()
}

// PromptSafeName reduces a registration or tool name to the identifier grammar
// model APIs accept for tool names — ASCII letters, digits, `_`, `-` and `.`,
// at most 64 bytes — replacing anything else with `_`. A name that already fits
// comes back unchanged; one that does not could never have been a callable
// tool anyway, and must not reach the system prompt as written: a hosted
// connection's name is user-authored and a connection can be shared, so a name
// is a channel by which one user could place text into another user's SYSTEM
// prompt.
func PromptSafeName(name string) string {
	const maxLen = 64
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= maxLen {
			break
		}
	}
	return b.String()
}
